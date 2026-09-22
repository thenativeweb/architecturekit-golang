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

type upcasters map[string]Upcaster

// apply runs the chain for one stored event and returns what the Evolve rules
// should see. An event without an upcaster passes through untouched.
func (u upcasters) apply(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
	return u.applyWithBudget(event, maxUpcastSteps)
}

func (u upcasters) applyWithBudget(
	event eventsourcingdb.Event,
	budget int,
) ([]eventsourcingdb.Event, error) {
	upcast, hasUpcaster := u[event.Type]
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
