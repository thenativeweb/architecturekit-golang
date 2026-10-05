// Package architecturekit provides building blocks for CQRS and event-sourced
// applications on top of the EventSourcingDB.
package architecturekit

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sync"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Event binds a Go type to an event type of the EventSourcingDB, so that the
// type string is written exactly once, on the event itself.
//
// Every event also has a JSON schema for its data, which the database checks
// each event of the type against (see RegisterSchemas). The kit derives it
// from the struct (see DeriveSchema), so that no event type can be forgotten.
// An event that needs another schema, e.g. one with constraints the struct
// can not express, returns it from a function Schema() map[string]any of its
// own, which takes precedence. One that the event has only from an embedded
// field describes the embedded type alone, so Evolve refuses it (see
// DeriveSchema).
type Event interface {
	EventType() string
}

// Command knows the subject it acts on, and the conditions under which its
// events may be written.
//
// Preconditions is where optimistic concurrency, idempotency and uniqueness
// live. Every command declares at least one (see Precondition), and the kit
// adds none of its own.
type Command interface {
	Subject() string
	Preconditions() []Precondition
}

// State is the state a command decides on, together with the rules that build
// that state from events.
//
// Deliberately not called a projection: a projection builds a read model for
// the query side, whereas this is the write side, holding just enough state
// for a decision.
type State[TState any] struct {
	initial TState
	evolve  map[string]func(TState, json.RawMessage) (TState, error)
	schemas []EventSchema

	// ignored holds the event types the state takes without changing.
	ignored map[string]bool

	// upcasters is the shared set the state refers to, or nil if it has none.
	upcasters *Upcasters

	// fromLatest is the event type from whose latest occurrence the state is
	// built, or an empty string to build it from the first event.
	fromLatest string

	// clone copies a state, for a store with a state cache, for Step, and for
	// the initial value at the start of every read, or is nil if the state has
	// none.
	clone func(TState) TState

	// isValue caches whether TState consists of values only, since that is
	// found by reflection and does not change.
	isValue     bool
	isValueOnce sync.Once

	// sharesInitial caches whether the initial value shares data with its
	// copies, since that is found by reflection and does not change either.
	sharesInitial     bool
	sharesInitialOnce sync.Once
}

// EventSchema holds an event schema for registration with the database.
type EventSchema struct {
	EventType string
	Schema    map[string]any
}

// NewState creates a state that starts out as initial. Every read starts from
// a copy of it, in Execute, Load, Replay, and ReplayStored alike, so that an
// Evolve rule never changes what the next read starts from.
//
// An initial value that consists of values only, or whose maps, slices,
// pointers and channels are nil, needs nothing else, since a copy shares no
// data with it. One that holds a map, a pointer or a channel that is not nil,
// or a slice with room for elements, at any depth, shares that with every
// copy, so the state needs a Clone function to copy it. Without one, every
// read fails with an error of the category ErrPermanent, rather than let an
// Evolve rule write into the initial value. A slice without room for
// elements, such as []string{}, needs none, since appending to it allocates a
// new array.
func NewState[TState any](initial TState) *State[TState] {
	return &State[TState]{
		initial: initial,
		evolve:  map[string]func(TState, json.RawMessage) (TState, error){},
		ignored: map[string]bool{},
	}
}

// Evolve defines how an event advances the state. The event type is read from
// TEvent instead of being passed as a string.
//
// Registering the same event type twice is a programming error, so it panics
// while the state is being built rather than silently overwriting a rule at
// run time.
func (s *State[TState]) Evolve[TEvent Event](evolve func(TState, TEvent) TState) *State[TState] {
	var zero TEvent
	eventType := zero.EventType()

	if _, exists := s.evolve[eventType]; exists {
		panic(fmt.Sprintf("architecturekit: event type %q is already registered on this state", eventType))
	}

	s.evolve[eventType] = func(state TState, data json.RawMessage) (TState, error) {
		var event TEvent
		if err := json.Unmarshal(data, &event); err != nil {
			return state, fmt.Errorf("%w: decoding %q: %v", ErrPermanent, eventType, err)
		}
		return evolve(state, event), nil
	}

	s.schemas = append(s.schemas, schemaFor[TEvent](eventType))

	return s
}

// Ignore lets the state take events of type TEvent without changing, for an
// event that belongs to the subject but matters for no decision, such as one
// that only records what happened. It says so, where an Evolve rule that
// returns the state unchanged would need a comment. Without either, reading
// such an event fails, since an event type without a rule usually points to a
// missing rule or a wrong subject.
//
// The data of an ignored event is not decoded. Its schema is still part of
// Schemas, since the event is still written to the subject.
//
// Ignoring an event type that has an Evolve rule, or ignoring it twice, is a
// programming error, so it panics while the state is being built.
func (s *State[TState]) Ignore[TEvent Event]() *State[TState] {
	var zero TEvent
	eventType := zero.EventType()

	if _, exists := s.evolve[eventType]; exists {
		panic(fmt.Sprintf("architecturekit: event type %q is already registered on this state", eventType))
	}

	s.evolve[eventType] = func(state TState, _ json.RawMessage) (TState, error) {
		return state, nil
	}
	s.ignored[eventType] = true
	s.schemas = append(s.schemas, schemaFor[TEvent](eventType))

	return s
}

// schemaFor returns the schema of an event type for registration. An event
// type whose schema can not be derived is a programming error, so it panics
// while the state is being built.
func schemaFor[TEvent Event](eventType string) EventSchema {
	schema, err := eventSchemaOf[TEvent]()
	if err != nil {
		panic(fmt.Sprintf("architecturekit: event type %q: %v", eventType, err))
	}

	return EventSchema{EventType: eventType, Schema: schema}
}

// UpcastWith runs the stored events through the given set of upcasters before
// any Evolve rule sees them, so that events of an older type arrive in their
// current shape (see Upcasters).
//
// Calling UpcastWith twice, or with nil, is a programming error, so it panics
// while the state is being built.
func (s *State[TState]) UpcastWith(upcasters *Upcasters) *State[TState] {
	if upcasters == nil {
		panic("architecturekit: UpcastWith needs a set of upcasters, not nil")
	}
	if s.upcasters != nil {
		panic("architecturekit: this state already has a set of upcasters")
	}

	s.upcasters = upcasters

	return s
}

// FromLatest builds the state from the latest event of type TEvent in a
// subject onwards, instead of from the first event, so that deciding on a long
// stream does not read all of it again every time. If the subject has no event
// of that type, the state is built from the first event, as usual.
//
// This yields the right state only if the Evolve rule for TEvent does not
// depend on the state before it, that is, if an event of that type carries
// everything the state needs from the events before it. Replay and
// ReplayStored start from the same event, so a test of a decider sees exactly
// what Execute sees.
//
// The database looks for the type under which the event is stored. If TEvent
// is the result of an upcaster, stored events of the older type are not found,
// and the state is built from the first event.
//
// Calling FromLatest for an event type without an Evolve rule, for one the
// state ignores, or calling it twice, is a programming error, so it panics
// while the state is being built.
func (s *State[TState]) FromLatest[TEvent Event]() *State[TState] {
	var zero TEvent
	eventType := zero.EventType()

	if _, isKnown := s.evolve[eventType]; !isKnown || s.ignored[eventType] {
		panic(fmt.Sprintf("architecturekit: event type %q has no Evolve rule on this state", eventType))
	}
	if s.fromLatest != "" {
		panic(fmt.Sprintf("architecturekit: this state already starts from the latest %q event", s.fromLatest))
	}

	s.fromLatest = eventType

	return s
}

// Clone lets a store with a state cache cache this state, although it holds
// slices, maps, pointers, channels, functions or interfaces. The function must
// return a copy that shares no data with the original, so that changing one of
// them never changes the other.
//
// Without it, such a state is not cached, and every command reads its events
// as without a cache, because commands that ran at the same time would
// otherwise share and change the same data. A state that consists of values
// only needs no Clone function.
//
// The same function lets Step and StepStored leave the given state unchanged,
// and copies the initial value at the start of every read. An initial value
// that holds a map, a pointer or a channel that is not nil, or a slice with
// room for elements, needs it, since reading would change it otherwise (see
// NewState).
//
// Calling Clone twice is a programming error, so it panics while the state is
// being built.
func (s *State[TState]) Clone(clone func(TState) TState) *State[TState] {
	if s.clone != nil {
		panic("architecturekit: this state already has a clone function")
	}

	s.clone = clone

	return s
}

// isCopyable reports whether copyOf returns a copy that shares no data with
// the original. Only then may a store cache the state, and only then can Step
// leave the given state unchanged.
func (s *State[TState]) isCopyable() bool {
	s.isValueOnce.Do(func() {
		s.isValue = isValueType(reflect.TypeFor[TState]())
	})

	return s.isValue || s.clone != nil
}

// copyOf returns a copy of a state that shares no data with it.
func (s *State[TState]) copyOf(state TState) TState {
	if s.clone != nil {
		return s.clone(state)
	}

	return state
}

// copyOfInitial returns a copy of the initial value that shares no data with
// it, for a read to start from. It fails permanently for an initial value that
// shares data with its copies if the state has no Clone function, since an
// Evolve rule would then write into the initial value of every later read.
func (s *State[TState]) copyOfInitial() (TState, error) {
	s.sharesInitialOnce.Do(func() {
		s.sharesInitial = sharesData(reflect.ValueOf(&s.initial).Elem())
	})

	if s.sharesInitial && s.clone == nil {
		var zero TState
		return zero, fmt.Errorf("%w: %s holds slices, maps, pointers or channels in its initial value, so it "+
			"needs a Clone function to start every read from a copy of the initial value", ErrPermanent, reflect.TypeFor[TState]())
	}

	return s.copyOf(s.initial), nil
}

// Schemas returns the schemas of all events the state evolves by, for
// registration with the database.
func (s *State[TState]) Schemas() []EventSchema {
	return s.schemas
}

// HasRule reports whether the state has a rule for an event type, an Evolve
// rule or Ignore, so that it can read events of that type. Execute refuses to
// write an event the state of the decider has no rule for, since the state
// could not read the subject any more, and the test fixture of
// architecturekittest uses HasRule to report such an event the same way.
//
// Upcasters play no part, since a decider returns events of current types: an
// older type that only an upcaster knows has no rule.
func (s *State[TState]) HasRule(eventType string) bool {
	_, isKnown := s.evolve[eventType]
	return isKnown
}

// checkRules fails permanently if the state has no rule for one of the events
// a decider returned for a subject. They are written to the subject that the
// same state reads for the next command, and the database keeps every event,
// so a single one without a rule would leave a subject the state can not read
// any more, until the code has a rule for it.
func (s *State[TState]) checkRules(subject string, events []Event) error {
	for _, event := range events {
		if !s.HasRule(event.EventType()) {
			return fmt.Errorf("%w: refusing to write an event of type %q to %q, since the state of the "+
				"decider has no rule for it and could not read the subject any more",
				ErrPermanent, event.EventType(), subject)
		}
	}

	return nil
}

// Decider connects a state with the decision made on it. Create one with
// NewDecider.
//
// The zero value, such as a variable that was declared but never set, has
// neither a state nor a decision, so its State returns nil. Handing it to
// Execute, to the test fixture of architecturekittest, or to Route of httpapi
// is a programming error, so they panic, while Handle fails with an error.
// Either way, the text names the mistake rather than a nil pointer.
type Decider[TCommand Command, TState any] struct {
	state  *State[TState]
	decide func(ctx context.Context, cmd TCommand, state TState) ([]Event, error)
}

// NewDecider creates a decider that decides on the given state with the given
// function. For every command, Execute reads the state of the subject of the
// command, and decide returns the events to write, or an error if the command
// is refused. The types of the command and the state come from the arguments,
// so that none of them has to be given:
//
//	var borrowBook = architecturekit.NewDecider(bookState,
//	  func(ctx context.Context, cmd BorrowBook, book Book) ([]architecturekit.Event, error) {
//	    if book.IsBorrowed {
//	      return nil, architecturekit.NewDomainError("book %s is already borrowed", cmd.BookID)
//	    }
//
//	    return []architecturekit.Event{BookBorrowed{BorrowedBy: cmd.ReaderID}}, nil
//	  })
//
// A nil state or a nil function is a programming error, so NewDecider panics,
// rather than the first command failing.
func NewDecider[TCommand Command, TState any](
	state *State[TState],
	decide func(ctx context.Context, cmd TCommand, state TState) ([]Event, error),
) Decider[TCommand, TState] {
	if state == nil {
		panic("architecturekit: NewDecider needs a state, not nil")
	}
	if decide == nil {
		panic("architecturekit: NewDecider needs a function that decides, not nil")
	}

	return Decider[TCommand, TState]{state: state, decide: decide}
}

// State returns the state the decider decides on, or nil for the zero value.
func (d Decider[TCommand, TState]) State() *State[TState] {
	return d.state
}

// Decide decides on a command, given the state, with the function the decider
// was created with, and returns what that function returns. The zero value
// has no such function, so it panics.
func (d Decider[TCommand, TState]) Decide(ctx context.Context, cmd TCommand, state TState) ([]Event, error) {
	return d.decide(ctx, cmd, state)
}

// Replay folds a sequence of events into a state. It is meant for tests, where
// the history is available as typed events.
func Replay[TState any](state *State[TState], history ...Event) (TState, error) {
	current, err := state.copyOfInitial()
	if err != nil {
		return current, err
	}

	if state.fromLatest != "" {
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].EventType() == state.fromLatest {
				history = history[i:]
				break
			}
		}
	}

	for _, event := range history {
		current, err = state.evolveBy(current, event)
		if err != nil {
			return current, err
		}
	}

	return current, nil
}

// ReplayStored folds stored events into a state, running the upcasters on the
// way, exactly as reading from the database would. It is meant for tests of
// upcasters, which need the raw shape of an older version.
func ReplayStored[TState any](
	state *State[TState],
	history ...eventsourcingdb.Event,
) (TState, error) {
	current, err := state.copyOfInitial()
	if err != nil {
		return current, err
	}

	if state.fromLatest != "" {
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Type == state.fromLatest {
				history = history[i:]
				break
			}
		}
	}

	for _, stored := range history {
		current, err = state.evolveByStored(current, stored)
		if err != nil {
			return current, err
		}
	}

	return current, nil
}

// Step advances a state by a single event, by the same rules as Replay. It
// starts from the given state instead of the initial one, and leaves that
// unchanged, so that the state before and after the event are both at hand,
// e.g. for a history that tells what an event changed.
//
// So that current stays unchanged, a state that holds slices, maps, pointers,
// channels, functions or interfaces needs a Clone function; without one, Step
// fails permanently. A state that consists of values only needs none. If the
// event fails, Step returns current.
func Step[TState any](state *State[TState], current TState, event Event) (TState, error) {
	if err := state.checkCopyable(); err != nil {
		return current, err
	}

	next, err := state.evolveBy(state.copyOf(current), event)
	if err != nil {
		return current, err
	}

	return next, nil
}

// StepStored advances a state by a single stored event, running the upcasters
// on the way, exactly as reading from the database would. If the upcasters
// turn the event into several, all of them are applied. Otherwise, it behaves
// like Step.
func StepStored[TState any](
	state *State[TState],
	current TState,
	stored eventsourcingdb.Event,
) (TState, error) {
	if err := state.checkCopyable(); err != nil {
		return current, err
	}

	next, err := state.evolveByStored(state.copyOf(current), stored)
	if err != nil {
		return current, err
	}

	return next, nil
}

// checkCopyable fails permanently for a state that Step can not leave
// unchanged.
func (s *State[TState]) checkCopyable() error {
	if s.isCopyable() {
		return nil
	}

	return fmt.Errorf("%w: %s holds slices, maps, pointers, channels, functions or interfaces, so it needs a "+
		"Clone function to be stepped without changing the given state", ErrPermanent, reflect.TypeFor[TState]())
}

// evolveBy applies the rule for a typed event.
func (s *State[TState]) evolveBy(current TState, event Event) (TState, error) {
	evolve, isKnown := s.evolve[event.EventType()]
	if !isKnown {
		return current, fmt.Errorf("%w: no rule for event type %q",
			ErrPermanent, event.EventType())
	}

	data, err := json.Marshal(event)
	if err != nil {
		return current, fmt.Errorf("%w: encoding %q: %v",
			ErrPermanent, event.EventType(), err)
	}

	return evolve(current, data)
}

// evolveByStored runs the upcasters on a stored event and applies the rules for
// the events they return.
func (s *State[TState]) evolveByStored(current TState, stored eventsourcingdb.Event) (TState, error) {
	upcasted, err := s.upcasters.apply(stored)
	if err != nil {
		return current, err
	}

	for _, event := range upcasted {
		evolve, isKnown := s.evolve[event.Type]
		if !isKnown {
			return current, fmt.Errorf("%w: no rule for event type %q",
				ErrPermanent, event.Type)
		}

		current, err = evolve(current, event.Data)
		if err != nil {
			return current, err
		}
	}

	return current, nil
}
