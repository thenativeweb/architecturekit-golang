package architecturekit

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Envelope carries an event to a handler of a TypedProjection: the metadata of
// the event as the database recorded it, and its data, decoded into TEvent.
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
		var data TEvent
		if err := json.Unmarshal(event.Data, &data); err != nil {
			return fmt.Errorf("%w: decoding %q: %v", ErrPermanent, eventType, err)
		}

		return handle(ctx, Envelope[TEvent]{
			ID:          event.ID,
			Time:        event.Time,
			Source:      event.Source,
			Subject:     event.Subject,
			Type:        event.Type,
			TraceParent: event.TraceParent,
			TraceState:  event.TraceState,
			Data:        data,
		})
	}

	return p
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
func (p *TypedProjection) Apply(ctx context.Context, event eventsourcingdb.Event) error {
	upcasted, err := p.upcasters.apply(event)
	if err != nil {
		return err
	}

	for _, event := range upcasted {
		handle, isKnown := p.handlers[event.Type]
		if !isKnown {
			continue
		}

		if err := handle(ctx, event); err != nil {
			return err
		}
	}

	return nil
}
