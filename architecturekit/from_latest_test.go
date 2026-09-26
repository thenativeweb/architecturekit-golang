package architecturekit_test

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// counterFromLatestState starts from the latest reset. The rule for reset sets
// the total to zero, whatever it was before, so the events before the latest
// reset do not matter for the state.
func counterFromLatestState() *architecturekit.State[counter] {
	return counterState().FromLatest[reset]()
}

func counterFromLatestDecider() architecturekit.Decider[increment, counter] {
	decider := counterDecider()
	decider.State = counterFromLatestState()

	return decider
}

func TestReplayWithFromLatestStartsFromTheLatestEventOfThatType(t *testing.T) {
	// annotated has no rule on the counter state, so replaying it would fail.
	// Starting from the latest reset skips it.
	current, err := architecturekit.Replay(counterFromLatestState(),
		annotated{Note: "before the reset"},
		incremented{By: 3},
		reset{},
		incremented{By: 2},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if current.Total != 2 {
		t.Fatalf("got %d, want 2", current.Total)
	}
}

func TestReplayWithFromLatestReplaysEverythingIfTheTypeIsMissing(t *testing.T) {
	current, err := architecturekit.Replay(counterFromLatestState(),
		incremented{By: 3},
		incremented{By: 4},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if current.Total != 7 {
		t.Fatalf("got %d, want 7", current.Total)
	}
}

func TestReplayStoredWithFromLatestStartsFromTheLatestEventOfThatType(t *testing.T) {
	current, err := architecturekit.ReplayStored(counterFromLatestState(),
		stored(annotated{}.EventType(), `{"note":"before the reset"}`),
		stored(reset{}.EventType(), `{}`),
		stored(incremented{}.EventType(), `{"by":2}`),
		stored(reset{}.EventType(), `{}`),
		stored(incremented{}.EventType(), `{"by":5}`),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if current.Total != 5 {
		t.Fatalf("got %d, want 5", current.Total)
	}
}

func TestFromLatestPanicsWithoutEvolveRule(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("expected a panic for an event type without a rule")
		}
		message, ok := recovered.(string)
		if !ok || !strings.Contains(message, reset{}.EventType()) {
			t.Fatalf("panic should name the event type, got %v", recovered)
		}
	}()

	architecturekit.NewState(counter{}).FromLatest[reset]()
}

func TestFromLatestPanicsWhenCalledTwice(t *testing.T) {
	defer func() {
		recovered := recover()
		if recovered == nil {
			t.Fatal("expected a panic for a second call")
		}
		message, ok := recovered.(string)
		if !ok || !strings.Contains(message, reset{}.EventType()) {
			t.Fatalf("panic should name the event type, got %v", recovered)
		}
	}()

	counterFromLatestState().FromLatest[incremented]()
}

func TestExecuteWithFromLatestReadsFromTheLatestEventOfThatType(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	ctx := context.Background()

	// annotated has no rule on the counter state, so reading it would fail
	// the command. Starting from the latest reset never reads it.
	writeRaw(t, subject,
		annotated{Note: "before the reset"},
		incremented{By: 10},
		reset{},
		incremented{By: 2},
	)

	_, err := architecturekit.Execute(ctx, store, counterDecider(),
		increment{subject: subject, By: 1, Limit: 3})
	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("reading from the first event should fail on the annotated event, got %v", err)
	}

	written, err := architecturekit.Execute(ctx, store, counterFromLatestDecider(),
		increment{subject: subject, By: 1, Limit: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(written) != 1 {
		t.Fatalf("expected one written event, got %d", len(written))
	}
}

func TestExecuteWithFromLatestReadsEverythingIfTheTypeIsMissing(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	ctx := context.Background()

	writeRaw(t, subject,
		incremented{By: 2},
		incremented{By: 1},
	)

	// The total is 3, so another increment exceeds the limit. Reading nothing
	// would see a total of 0 and let it pass.
	_, err := architecturekit.Execute(ctx, store, counterFromLatestDecider(),
		increment{subject: subject, By: 1, Limit: 3})
	if !errors.Is(err, architecturekit.ErrDomain) {
		t.Fatalf("expected the limit to be exceeded, got %v", err)
	}
}

// writeRaw writes events past the framework, so that a stream can contain
// events the state has no rule for.
func writeRaw(t *testing.T, subject string, events ...architecturekit.Event) {
	t.Helper()

	candidates := make([]eventsourcingdb.EventCandidate, len(events))
	for i, event := range events {
		candidates[i] = eventsourcingdb.EventCandidate{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    event.EventType(),
			Data:    event,
		}
	}

	if _, err := rawClient(t).WriteEvents(candidates, nil); err != nil {
		t.Fatalf("failed to write events: %v", err)
	}
}
