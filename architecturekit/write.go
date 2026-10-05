package architecturekit

import (
	"context"
	"fmt"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// EventOn is an event together with the subject it is written to, for Write.
type EventOn struct {
	Subject string
	Event   Event
}

// Write writes events to one or several subjects at once, so that either all
// of them are written or none, under the given preconditions. Use it for what
// is not a decision on the state of a single subject, such as a result derived
// from many of them, which Execute does not cover, since a command acts on one
// subject.
//
// The events are written with the source of the store, and a failure is sorted
// into a category as with Execute, so a precondition that does not hold is
// ErrConflict. Write never decides again on a conflict, since there is nothing
// to decide.
//
// Like a command, a write declares at least one precondition, such as
// OnPristineSubject or one made with Require, or Unconditionally to write
// without any. OnStateRead has nothing to guard, since Write reads no state. That, an event without a subject or one
// that is nil, and the other mistakes Execute refuses in the preconditions of
// a command make Write fail with an error of the category ErrPermanent,
// without writing anything. A nil pointer of a concrete type counts as nil.
// So does an event whose data can not be encoded as JSON, for example because
// it holds a float NaN, as with Execute.
// Writing no events writes nothing and returns nil, but the preconditions are
// checked first, so a write of no events still declares them, for example
// with Unconditionally.
//
// Write knows no state, so unlike Execute, it does not refuse an event that a
// state reading its subject has no rule for. Reading the subject with that
// state fails afterwards, so every state that reads one of the subjects needs
// a rule for each event type written to it, an Evolve rule or Ignore.
func Write(
	ctx context.Context,
	store *Store,
	events []EventOn,
	preconditions ...Precondition,
) ([]eventsourcingdb.Event, error) {
	resolved, err := writePreconditions(preconditions)
	if err != nil {
		return nil, err
	}

	if len(events) == 0 {
		return nil, nil
	}

	candidates := make([]eventsourcingdb.EventCandidate, len(events))
	for i, event := range events {
		if event.Subject == "" || isNil(event.Event) {
			return nil, fmt.Errorf("%w: event %d of a write needs a subject and an event", ErrPermanent, i)
		}

		if candidates[i], err = store.candidateFor(event.Subject, event.Event); err != nil {
			return nil, err
		}
	}

	// The client writes without a context, so a context that has ended by
	// now must not lead to a write anyway.
	if ctx.Err() != nil {
		return nil, contextEnded(ctx, fmt.Sprintf("writing %d events, the first to %q", len(events), events[0].Subject))
	}

	written, err := store.client.WriteEvents(candidates, resolved)
	if err != nil {
		return nil, databaseFailure(err, fmt.Sprintf("writing %d events, the first to %q", len(events), events[0].Subject))
	}

	return written, nil
}

// writePreconditions checks the preconditions of a write the way
// checkPreconditions does those of a command, except that OnStateRead has
// nothing to refer to, and turns them into the ones the database checks.
func writePreconditions(declared []Precondition) ([]eventsourcingdb.Precondition, error) {
	if len(declared) == 0 {
		return nil, fmt.Errorf("%w: a write declares no preconditions, use Unconditionally to write without any",
			ErrPermanent)
	}

	resolved := make([]eventsourcingdb.Precondition, 0, len(declared))

	for _, precondition := range declared {
		switch precondition.kind {
		case requiredKind:
			if precondition.database == nil {
				return nil, fmt.Errorf("%w: a write requires a precondition that is nil", ErrPermanent)
			}
			resolved = append(resolved, precondition.database)
		case onStateReadKind:
			return nil, fmt.Errorf("%w: a write reads no state, so OnStateRead has nothing to guard, use Require instead",
				ErrPermanent)
		case unconditionallyKind:
			if len(declared) > 1 {
				return nil, fmt.Errorf("%w: a write combines Unconditionally with other preconditions", ErrPermanent)
			}
		default:
			return nil, fmt.Errorf("%w: a write declares a precondition not made with Require or Unconditionally",
				ErrPermanent)
		}
	}

	return resolved, nil
}
