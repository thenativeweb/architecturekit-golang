package architecturekit

import (
	"context"
	"fmt"
	"strings"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Store reads from and writes to the EventSourcingDB.
type Store struct {
	client *eventsourcingdb.Client
	source string
}

// NewStore creates a store that writes events with the given source.
func NewStore(client *eventsourcingdb.Client, source string) *Store {
	return &Store{client: client, source: source}
}

// fold reads the stream and folds it into a state as it goes. Events are not
// collected, so even long streams need constant memory only.
func fold[TState any](
	ctx context.Context,
	store *Store,
	subject string,
	state *State[TState],
) (TState, error) {
	current := state.initial

	for event, err := range store.client.ReadEvents(ctx, subject, eventsourcingdb.ReadEventsOptions{Recursive: false}) {
		if err != nil {
			return current, fmt.Errorf("%w: reading %q: %v", ErrTransient, subject, err)
		}

		upcasted, err := state.upcasters.apply(event)
		if err != nil {
			return current, err
		}

		for _, event := range upcasted {
			evolve, isKnown := state.evolve[event.Type]
			if !isKnown {
				// An unexpected event type in an aggregate's stream points to a wrong
				// subject or a missing rule. Skipping it silently would hide that.
				return current, fmt.Errorf("%w: no rule for event type %q on subject %q",
					ErrPermanent, event.Type, subject)
			}

			current, err = evolve(current, event.Data)
			if err != nil {
				return current, err
			}
		}
	}

	return current, nil
}

// write appends the events under the given preconditions and returns them as
// the database recorded them, including their IDs.
func (s *Store) write(
	subject string,
	events []Event,
	preconditions []eventsourcingdb.Precondition,
) ([]eventsourcingdb.Event, error) {
	candidates := make([]eventsourcingdb.EventCandidate, len(events))
	for i, event := range events {
		candidates[i] = eventsourcingdb.EventCandidate{
			Source:  s.source,
			Subject: subject,
			Type:    event.EventType(),
			Data:    event,
		}
	}

	written, err := s.client.WriteEvents(candidates, preconditions)
	if err == nil {
		return written, nil
	}

	// The client exports no typed error, which leaves nothing but the status
	// code inside the error text.
	if strings.Contains(err.Error(), "'409'") {
		return nil, fmt.Errorf("%w on %q: %v", ErrConflict, subject, err)
	}

	return nil, fmt.Errorf("%w: writing %q: %v", ErrPermanent, subject, err)
}

// RegisterSchemas registers the schemas of the given events with the database.
// The call is idempotent: an already known event type counts as success,
// because otherwise an application could not be started a second time.
//
// The EventSourcingDB does not distinguish whether a re-registered schema is
// the same or a different one; it answers 409 either way. A later change to a
// schema therefore goes unnoticed here.
func (s *Store) RegisterSchemas(schemas ...[]EventSchema) error {
	registered := map[string]bool{}

	for _, group := range schemas {
		for _, schema := range group {
			if registered[schema.EventType] {
				continue
			}
			registered[schema.EventType] = true

			err := s.client.RegisterEventSchema(schema.EventType, schema.Schema)
			if err == nil || isAlreadyRegistered(err) {
				continue
			}

			return fmt.Errorf("%w: registering schema for %q: %v",
				ErrPermanent, schema.EventType, err)
		}
	}

	return nil
}

// isAlreadyRegistered reports whether the event type is known to the database
// already. The client exports no typed error for this either.
func isAlreadyRegistered(err error) bool {
	return strings.Contains(err.Error(), "'409'")
}

// Execute loads the state, lets the decider decide, and appends the resulting
// events. It does not retry: a conflict is reported, and the caller decides
// what to do about it.
//
// All preconditions come from the command. The kit adds none of its own, so a
// command that needs optimistic concurrency has to say so, and the caller has
// to supply the event ID it read.
func Execute[TCommand Command, TState any](
	ctx context.Context,
	store *Store,
	decider Decider[TCommand, TState],
	cmd TCommand,
) ([]eventsourcingdb.Event, error) {
	subject := cmd.Subject()

	state, err := fold(ctx, store, subject, decider.State)
	if err != nil {
		return nil, err
	}

	events, err := decider.Decide(ctx, cmd, state)
	if err != nil {
		return nil, err
	}
	if len(events) == 0 {
		return nil, nil
	}

	var preconditions []eventsourcingdb.Precondition
	if preconditioned, ok := any(cmd).(Preconditioned); ok {
		preconditions = preconditioned.Preconditions()
	}

	return store.write(subject, events, preconditions)
}
