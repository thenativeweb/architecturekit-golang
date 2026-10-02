package architecturekit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Envelope carries an event to a handler of a TypedProjection, or out of
// Decode: the metadata of the event as the database recorded it, and its data,
// decoded into TEvent.
//
// The integrity fields of the database, such as the hash and the signature,
// are deliberately missing, because they describe the stored data, and an
// upcaster may have changed it on the way.
type Envelope[TEvent Event] struct {
	ID          string
	Time        time.Time
	Source      string
	Subject     string
	Type        string
	TraceParent *string
	TraceState  *string
	Data        TEvent
}

// TypedProjection is a projection that hands every event to the handler
// registered for its type, with the data already decoded. Create it with
// NewProjection, and register a handler per event type with On.
//
// It is the read side's counterpart of State: the event type is taken from the
// Go type of the handler, so it is written exactly once, on the event, and the
// type string and the decoded type can not drift apart.
//
// Events without a handler are skipped, because a projection usually reads far
// more events than it cares about. A projection that has to see every event,
// such as one that logs them, implements Projection directly instead.
type TypedProjection struct {
	handlers  map[string]func(ctx context.Context, event eventsourcingdb.Event) error
	upcasters *Upcasters
}

// NewProjection creates a projection without any handlers.
//
// The result is a Projection like any other, so it can be tracked, run, and
// tested as usual. To make it resumable, embed it in a type that adds the
// Checkpoint and SaveCheckpoint functions.
func NewProjection() *TypedProjection {
	return &TypedProjection{
		handlers: map[string]func(ctx context.Context, event eventsourcingdb.Event) error{},
	}
}

// On registers the handler for the events of type TEvent. The event type is
// read from TEvent instead of being passed as a string.
//
// If the data of an event can not be decoded into TEvent, Apply returns an
// error of the category ErrPermanent. An error of the handler itself is
// returned unchanged.
//
// Registering the same event type twice is a programming error, so it panics
// while the projection is being built rather than silently overwriting a
// handler.
func (p *TypedProjection) On[TEvent Event](
	handle func(ctx context.Context, event Envelope[TEvent]) error,
) *TypedProjection {
	var zero TEvent
	eventType := zero.EventType()

	if _, exists := p.handlers[eventType]; exists {
		panic(fmt.Sprintf("architecturekit: event type %q already has a handler on this projection", eventType))
	}

	p.handlers[eventType] = func(ctx context.Context, event eventsourcingdb.Event) error {
		envelope, err := Decode[TEvent](event)
		if err != nil {
			return err
		}

		return handle(ctx, envelope)
	}

	return p
}

// Decode turns a stored event into an envelope of the given type, the same one
// a typed projection hands to its handler: the metadata of the event, and its
// data decoded into TEvent. Use it wherever events come as they are stored,
// such as the ones Execute returns.
//
// An event of another type than TEvent, or one whose data can not be decoded
// into it, makes Decode fail with an error of the category ErrPermanent, since
// decoding it again yields the same result. Checking the type matters: decoding
// into the wrong struct would otherwise quietly leave its fields empty.
func Decode[TEvent Event](event eventsourcingdb.Event) (Envelope[TEvent], error) {
	var data TEvent

	if eventType := data.EventType(); event.Type != eventType {
		return Envelope[TEvent]{}, fmt.Errorf("%w: event %s is of type %q, not %q",
			ErrPermanent, event.ID, event.Type, eventType)
	}

	if err := json.Unmarshal(event.Data, &data); err != nil {
		return Envelope[TEvent]{}, fmt.Errorf("%w: decoding event %s of type %q: %v",
			ErrPermanent, event.ID, event.Type, err)
	}

	return Envelope[TEvent]{
		ID:          event.ID,
		Time:        event.Time,
		Source:      event.Source,
		Subject:     event.Subject,
		Type:        event.Type,
		TraceParent: event.TraceParent,
		TraceState:  event.TraceState,
		Data:        data,
	}, nil
}

// UpcastWith runs the stored events through the given set of upcasters before
// any handler sees them, so that the projection sees the same events as every
// state that uses the same set (see Upcasters).
//
// Calling UpcastWith twice, or with nil, is a programming error, so it panics
// while the projection is being built.
func (p *TypedProjection) UpcastWith(upcasters *Upcasters) *TypedProjection {
	if upcasters == nil {
		panic("architecturekit: UpcastWith needs a set of upcasters, not nil")
	}
	if p.upcasters != nil {
		panic("architecturekit: this projection already has a set of upcasters")
	}

	p.upcasters = upcasters

	return p
}

// Apply satisfies Projection. It upcasts the event, and hands every resulting
// event to the handler registered for its type.
//
// An upcaster may split a stored event into several events, which all carry
// the ID of the stored event. So that a view can still tell them apart, Apply
// hands every handler a context that holds the position of its event among
// them: 0 for the first, 1 for the second, and so on. A stored event that is
// not split is at position 0. The functions of InMemoryView read the position
// from the context they get, so a handler hands its context on to the view,
// or one derived from it, but never one of its own.
func (p *TypedProjection) Apply(ctx context.Context, event eventsourcingdb.Event) error {
	upcasted, err := p.upcasters.apply(event)
	if err != nil {
		return err
	}

	for part, event := range upcasted {
		handle, isKnown := p.handlers[event.Type]
		if !isKnown {
			continue
		}

		if err := handle(withPart(ctx, part), event); err != nil {
			return err
		}
	}

	return nil
}

// partKey is the key under which Apply puts the position of an event among the
// ones an upcaster split a stored event into, its part, into the context of a
// handler.
//
// A view needs the part because the parts share the ID of the stored event. It
// compares the ID first, and the part only for the same ID, so a later part of
// an event is newer than an earlier one, but older than every later event.
// That way every part changes an item once, and applying the stored event
// again changes nothing, just as for an event that is not split. The revision
// a view hands out stays the ID of the event, without the part, so that it
// still fits a precondition.
//
// The key is not exported, since only InMemoryView reads it so far. A view of
// one's own, such as one in a database, may get a function to read it later
// on, which can be added without changing anything else.
type partKey struct{}

// withPart returns a context that holds the part of an event.
func withPart(ctx context.Context, part int) context.Context {
	return context.WithValue(ctx, partKey{}, part)
}

// partOf reads the part of an event from a context. A context without one,
// such as one that does not come from a typed projection, stands for part 0,
// which is what every event that is not split is.
func partOf(ctx context.Context) int {
	part, _ := ctx.Value(partKey{}).(int)

	return part
}
