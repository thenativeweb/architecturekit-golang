package architecturekit_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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
	return architecturekit.NewDecider(counterFromLatestState(), counterDecider().Decide)
}

func TestReplayWithFromLatest(t *testing.T) {
	t.Run("starts from the latest event of that type", func(t *testing.T) {
		// annotated has no rule on the counter state, so replaying it would fail.
		// Starting from the latest reset skips it.
		current, err := architecturekit.Replay(counterFromLatestState(),
			annotated{Note: "before the reset"},
			incremented{By: 3},
			reset{},
			incremented{By: 2},
		)
		require.NoError(t, err)
		assert.Equal(t, 2, current.Total)
	})

	t.Run("replays everything if the type is missing", func(t *testing.T) {
		current, err := architecturekit.Replay(counterFromLatestState(),
			incremented{By: 3},
			incremented{By: 4},
		)
		require.NoError(t, err)
		assert.Equal(t, 7, current.Total)
	})
}

func TestReplayStoredWithFromLatest(t *testing.T) {
	t.Run("starts from the latest event of that type", func(t *testing.T) {
		current, err := architecturekit.ReplayStored(counterFromLatestState(),
			stored(annotated{}.EventType(), `{"note":"before the reset"}`),
			stored(reset{}.EventType(), `{}`),
			stored(incremented{}.EventType(), `{"by":2}`),
			stored(reset{}.EventType(), `{}`),
			stored(incremented{}.EventType(), `{"by":5}`),
		)
		require.NoError(t, err)
		assert.Equal(t, 5, current.Total)
	})
}

func TestFromLatest(t *testing.T) {
	t.Run("panics without evolve rule", func(t *testing.T) {
		defer func() {
			recovered := recover()
			require.NotNil(t, recovered, "expected a panic for an event type without a rule")
			message, ok := recovered.(string)
			require.True(t, ok, "panic should name the event type, got %v", recovered)
			assert.Contains(t, message, reset{}.EventType(), "panic should name the event type")
		}()

		architecturekit.NewState(counter{}).FromLatest[reset]()
	})

	t.Run("panics on a pointer as the event type, also if its EventType function has a pointer receiver", func(t *testing.T) {
		assert.PanicsWithValue(t,
			"architecturekit: FromLatest needs the event type architecturekit_test.reset, not the pointer *architecturekit_test.reset",
			func() { counterState().FromLatest[*reset]() })
		assert.PanicsWithValue(t,
			"architecturekit: FromLatest needs the event type architecturekit_test.pointed, not the pointer *architecturekit_test.pointed",
			func() { counterState().FromLatest[*pointed]() })
	})

	t.Run("panics on an interface as the event type while the state is being built, also on one of its own", func(t *testing.T) {
		assert.PanicsWithValue(t,
			"architecturekit: FromLatest needs a concrete event type, not the interface architecturekit.Event",
			func() { counterState().FromLatest[architecturekit.Event]() })
		assert.PanicsWithValue(t,
			"architecturekit: FromLatest needs a concrete event type, not the interface architecturekit_test.counterEvent",
			func() { counterState().FromLatest[counterEvent]() })
	})

	t.Run("panics when called twice", func(t *testing.T) {
		defer func() {
			recovered := recover()
			require.NotNil(t, recovered, "expected a panic for a second call")
			message, ok := recovered.(string)
			require.True(t, ok, "panic should name the event type, got %v", recovered)
			assert.Contains(t, message, reset{}.EventType(), "panic should name the event type")
		}()

		counterFromLatestState().FromLatest[incremented]()
	})
}

func TestExecuteWithFromLatest(t *testing.T) {
	t.Run("reads from the latest event of that type", func(t *testing.T) {
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
		assert.ErrorIs(t, err, architecturekit.ErrPermanent,
			"reading from the first event should fail on the annotated event")

		written, err := architecturekit.Execute(ctx, store, counterFromLatestDecider(),
			increment{subject: subject, By: 1, Limit: 3})
		require.NoError(t, err)
		assert.Len(t, written, 1)
	})

	t.Run("reads everything if the type is missing", func(t *testing.T) {
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
		assert.ErrorIs(t, err, architecturekit.ErrDomain, "expected the limit to be exceeded")
	})
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

	_, err := rawClient(t).WriteEvents(context.Background(), candidates, nil)
	require.NoError(t, err)
}
