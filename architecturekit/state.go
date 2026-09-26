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
type Event interface {
	EventType() string
}

// SchemaProvider is optional. Events that implement it get their schema
// registered with the EventSourcingDB on startup.
type SchemaProvider interface {
	Schema() map[string]any
}

// Command knows the subject it acts on.
type Command interface {
	Subject() string
}

// Preconditioned is optional. A command that implements it decides under which
// conditions its events may be appended, and it can build those conditions
// from its own fields.
//
// This is where optimistic concurrency, idempotency and uniqueness live. The
// kit adds no preconditions of its own.
type Preconditioned interface {
	Preconditions() []eventsourcingdb.Precondition
}

// State is the state a command decides on, together with the rules that build
// that state from events.
//
// Deliberately not called a projection: a projection builds a read model for
// the query side, whereas this is the write side, holding just enough state
// for a decision.
type State[TState any] struct {
	initial   TState
	evolve    map[string]func(TState, json.RawMessage) (TState, error)
	schemas   []EventSchema
	upcasters upcasters

	// fromLatest is the event type from whose latest occurrence the state is
	// built, or an empty string to build it from the first event.
	fromLatest string

	// clone copies a state, for a store with a state cache, or is nil if the
	// state has none.
	clone func(TState) TState

	// isValue caches whether TState consists of values only, since that is
	// found by reflection and does not change.
	isValue     bool
	isValueOnce sync.Once
}

// EventSchema holds an event schema for registration with the database.
type EventSchema struct {
	EventType string
	Schema    map[string]any
}

// NewState creates a state that starts out as initial.
func NewState[TState any](initial TState) *State[TState] {
	return &State[TState]{
		initial:   initial,
		evolve:    map[string]func(TState, json.RawMessage) (TState, error){},
		upcasters: upcasters{},
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

	if provider, ok := any(zero).(SchemaProvider); ok {
		s.schemas = append(s.schemas, EventSchema{EventType: eventType, Schema: provider.Schema()})
	}

	return s
}

// Upcast translates stored events of an older type into a newer shape, before
// any Evolve rule sees them. The result is never written back.
//
// Upcasters are chained: if the result carries a type that has an upcaster of
// its own, that one runs too, so only one step per version is needed instead
// of one per pair of versions.
func (s *State[TState]) Upcast(from string, upcast Upcaster) *State[TState] {
	if _, exists := s.upcasters[from]; exists {
		panic(fmt.Sprintf("architecturekit: event type %q already has an upcaster", from))
	}

	s.upcasters[from] = upcast

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
// Calling FromLatest for an event type without an Evolve rule, or calling it
// twice, is a programming error, so it panics while the state is being built.
func (s *State[TState]) FromLatest[TEvent Event]() *State[TState] {
	var zero TEvent
	eventType := zero.EventType()

	if _, isKnown := s.evolve[eventType]; !isKnown {
		panic(fmt.Sprintf("architecturekit: event type %q has no Evolve rule on this state", eventType))
	}
	if s.fromLatest != "" {
		panic(fmt.Sprintf("architecturekit: this state already starts from the latest %q event", s.fromLatest))
	}

	s.fromLatest = eventType

	return s
}

// Clone lets a store with a state cache cache this state, although it holds
// slices, maps or pointers. The function must return a copy that shares no
// data with the original, so that changing one of them never changes the
// other.
//
// Without it, such a state is not cached, and every command reads its events
// as without a cache, because commands that ran at the same time would
// otherwise share and change the same data. A state that consists of values
// only needs no Clone function.
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

// isCacheable reports whether a store may cache the state.
func (s *State[TState]) isCacheable() bool {
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

// Schemas returns the event schemas that can be registered.
func (s *State[TState]) Schemas() []EventSchema {
	return s.schemas
}

// Decider connects a state with the decision made on it.
type Decider[TCommand Command, TState any] struct {
	State  *State[TState]
	Decide func(ctx context.Context, cmd TCommand, state TState) ([]Event, error)
}

// Replay folds a sequence of events into a state. It is meant for tests, where
// the history is available as typed events.
func Replay[TState any](state *State[TState], history ...Event) (TState, error) {
	current := state.initial

	if state.fromLatest != "" {
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].EventType() == state.fromLatest {
				history = history[i:]
				break
			}
		}
	}

	for _, event := range history {
		evolve, isKnown := state.evolve[event.EventType()]
		if !isKnown {
			return current, fmt.Errorf("%w: no rule for event type %q",
				ErrPermanent, event.EventType())
		}

		data, err := json.Marshal(event)
		if err != nil {
			return current, fmt.Errorf("%w: encoding %q: %v",
				ErrPermanent, event.EventType(), err)
		}

		current, err = evolve(current, data)
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
	current := state.initial

	if state.fromLatest != "" {
		for i := len(history) - 1; i >= 0; i-- {
			if history[i].Type == state.fromLatest {
				history = history[i:]
				break
			}
		}
	}

	for _, stored := range history {
		upcasted, err := state.upcasters.apply(stored)
		if err != nil {
			return current, err
		}

		for _, event := range upcasted {
			evolve, isKnown := state.evolve[event.Type]
			if !isKnown {
				return current, fmt.Errorf("%w: no rule for event type %q",
					ErrPermanent, event.Type)
			}

			current, err = evolve(current, event.Data)
			if err != nil {
				return current, err
			}
		}
	}

	return current, nil
}
