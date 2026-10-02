package architecturekit

import (
	"fmt"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// maxUpcastSteps bounds the chain, so that an upcaster which does not change
// the event type fails loudly instead of looping forever.
const maxUpcastSteps = 16

// Upcaster translates a stored event into one or more events of a newer shape.
// It works on the raw event, because the Go type of the old shape may be gone.
type Upcaster func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error)

// Upcasters is the set of upcasters for the event types of an application.
//
// Upcasting belongs to the event types, not to whoever reads them, so the set
// is registered once and shared: every state and every projection that reads
// these events refers to it with UpcastWith. That way the write side and the
// read side always see the same events.
//
// Register all upcasters before the first event is read.
type Upcasters struct {
	byType map[string]Upcaster
}

// NewUpcasters creates an empty set of upcasters.
func NewUpcasters() *Upcasters {
	return &Upcasters{byType: map[string]Upcaster{}}
}

// Upcast translates stored events of an older type into a newer shape, before
// any Evolve rule or projection handler sees them. The result is never written
// back.
//
// Upcasters are chained: if the result carries a type that has an upcaster of
// its own, that one runs too, so only one step per version is needed instead
// of one per pair of versions. A chain ends after 16 steps: an event that still
// has an upcaster then makes reading fail with an error of the category
// ErrPermanent, which catches an upcaster that keeps its event type and would
// otherwise run forever. So does an error that an upcaster returns.
//
// Registering the same event type twice is a programming error, so it panics
// while the set is being built rather than silently overwriting an upcaster.
func (u *Upcasters) Upcast(from string, upcast Upcaster) *Upcasters {
	if _, exists := u.byType[from]; exists {
		panic(fmt.Sprintf("architecturekit: event type %q already has an upcaster", from))
	}

	u.byType[from] = upcast

	return u
}

// apply runs the chain for one stored event and returns what the Evolve rules
// or projection handlers should see. An event without an upcaster passes
// through untouched, and so does every event if there is no set at all.
func (u *Upcasters) apply(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
	if u == nil {
		return []eventsourcingdb.Event{event}, nil
	}

	return u.applyWithBudget(event, maxUpcastSteps)
}

func (u *Upcasters) applyWithBudget(
	event eventsourcingdb.Event,
	budget int,
) ([]eventsourcingdb.Event, error) {
	upcast, hasUpcaster := u.byType[event.Type]
	if !hasUpcaster {
		return []eventsourcingdb.Event{event}, nil
	}

	if budget == 0 {
		return nil, fmt.Errorf("%w: upcasting %q exceeded %d steps, check for an upcaster that keeps its event type",
			ErrPermanent, event.Type, maxUpcastSteps)
	}

	produced, err := upcast(event)
	if err != nil {
		return nil, fmt.Errorf("%w: upcasting %q: %v", ErrPermanent, event.Type, err)
	}

	var result []eventsourcingdb.Event
	for _, next := range produced {
		further, err := u.applyWithBudget(next, budget-1)
		if err != nil {
			return nil, err
		}
		result = append(result, further...)
	}

	return result, nil
}
