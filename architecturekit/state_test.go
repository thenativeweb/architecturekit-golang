package architecturekit_test

import (
	"errors"
	"strings"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

func TestReplayReturnsInitialStateForEmptyHistory(t *testing.T) {
	current, err := architecturekit.Replay(counterState())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if current.Total != 0 {
		t.Fatalf("got %d, want 0", current.Total)
	}
}

func TestReplayFoldsEventsInOrder(t *testing.T) {
	current, err := architecturekit.Replay(counterState(),
		incremented{By: 3},
		incremented{By: 4},
		reset{},
		incremented{By: 5},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if current.Total != 5 {
		t.Fatalf("got %d, want 5", current.Total)
	}
}

func TestReplayFailsOnUnknownEventType(t *testing.T) {
	state := architecturekit.NewState(counter{})
	state.Evolve(func(current counter, event incremented) counter { return current })

	_, err := architecturekit.Replay(state, reset{})
	if err == nil {
		t.Fatal("expected an error for an event type without a rule")
	}
	if !strings.Contains(err.Error(), reset{}.EventType()) {
		t.Fatalf("error should name the event type, got %q", err.Error())
	}
}

func TestEvolvePanicsOnDuplicateEventType(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("expected a panic for a duplicate event type")
		}
		message, ok := recovered.(string)
		if !ok || !strings.Contains(message, incremented{}.EventType()) {
			t.Fatalf("panic should name the event type, got %v", recovered)
		}
	}()

	state := architecturekit.NewState(counter{})
	state.Evolve(func(current counter, event incremented) counter { return current })
	state.Evolve(func(current counter, event incremented) counter { return current })
}

func TestEvolveIsChainable(t *testing.T) {
	state := architecturekit.NewState(counter{}).
		Evolve(func(current counter, event incremented) counter {
			current.Total += event.By
			return current
		})

	current, err := architecturekit.Replay(state, incremented{By: 7})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if current.Total != 7 {
		t.Fatalf("got %d, want 7", current.Total)
	}
}

func TestSchemasCollectsOnlyEventsThatProvideOne(t *testing.T) {
	schemas := counterState().Schemas()

	if len(schemas) != 1 {
		t.Fatalf("got %d schema(s), want 1", len(schemas))
	}
	if schemas[0].EventType != (incremented{}).EventType() {
		t.Fatalf("got %q", schemas[0].EventType)
	}
	if schemas[0].Schema == nil {
		t.Fatal("schema must not be nil")
	}
}

func TestDomainErrorCarriesItsMessage(t *testing.T) {
	err := architecturekit.NewDomainError("limit of %d would be exceeded", 10)

	if err.Error() != "limit of 10 would be exceeded" {
		t.Fatalf("got %q", err.Error())
	}

	var domainError *architecturekit.DomainError
	if !errors.As(err, &domainError) {
		t.Fatal("NewDomainError must produce a *DomainError")
	}
}
