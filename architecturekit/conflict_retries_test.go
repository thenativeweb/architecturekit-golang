package architecturekit_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// interfered counts the decisions of an interfering decider.
type interfered struct {
	decisions int
}

// interferingDecider decides like counterDecider, but for its first decisions,
// as many as times says, somebody else writes to the subject while it decides,
// so that writing what it decided conflicts.
func interferingDecider(t *testing.T, times int) (architecturekit.Decider[increment, counter], *interfered) {
	t.Helper()

	counted := &interfered{}
	decide := counterDecider().Decide

	return architecturekit.NewDecider(counterState(),
		func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
			counted.decisions++

			if counted.decisions <= times {
				_, err := rawClient(t).WriteEvents(context.Background(), []eventsourcingdb.EventCandidate{{
					Source:  "https://thenativeweb.io",
					Subject: cmd.subject,
					Type:    (incremented{}).EventType(),
					Data:    incremented{By: 100},
				}}, nil)
				require.NoError(t, err)
			}

			return decide(ctx, cmd, current)
		}), counted
}

func retryingStore(t *testing.T, retries int) *architecturekit.Store {
	t.Helper()

	return architecturekit.NewStore(rawClient(t), "https://thenativeweb.io", architecturekit.WithConflictRetries(retries))
}

func TestWithConflictRetries(t *testing.T) {
	t.Run("decides again on a conflict and writes then", func(t *testing.T) {
		store := retryingStore(t, 2)
		subject := subjectFor(t)
		decider, counted := interferingDecider(t, 1)

		written, err := architecturekit.Execute(context.Background(), store, decider,
			increment{subject: subject, By: 1}.onStateRead())
		require.NoError(t, err)
		assert.Len(t, written, 1)
		assert.Equal(t, 2, counted.decisions)

		current, err := architecturekit.Load(context.Background(), store, counterState(), subject)
		require.NoError(t, err)
		assert.Equal(t, 101, current.Total, "the second decision builds on what was written in between")
	})

	t.Run("decides on the new state", func(t *testing.T) {
		decider, counted := interferingDecider(t, 1)

		_, err := architecturekit.Execute(context.Background(), retryingStore(t, 2), decider,
			increment{subject: subjectFor(t), By: 1, Limit: 50}.onStateRead())

		assert.ErrorIs(t, err, architecturekit.ErrDomain, "what was written in between exceeds the limit")
		assert.Equal(t, 2, counted.decisions)
	})

	t.Run("reports the conflict once the retries are used up", func(t *testing.T) {
		decider, counted := interferingDecider(t, 10)

		_, err := architecturekit.Execute(context.Background(), retryingStore(t, 2), decider,
			increment{subject: subjectFor(t), By: 1}.onStateRead())

		assert.ErrorIs(t, err, architecturekit.ErrConflict)
		assert.Equal(t, 3, counted.decisions, "one decision and two more")
	})

	t.Run("reports a conflict right away without the option", func(t *testing.T) {
		store := architecturekit.NewStore(rawClient(t), "https://thenativeweb.io")
		decider, counted := interferingDecider(t, 1)

		_, err := architecturekit.Execute(context.Background(), store, decider,
			increment{subject: subjectFor(t), By: 1}.onStateRead())

		assert.ErrorIs(t, err, architecturekit.ErrConflict)
		assert.Equal(t, 1, counted.decisions)
	})

	t.Run("never decides again on a revision the caller hands over", func(t *testing.T) {
		store := retryingStore(t, 2)
		subject := subjectFor(t)

		seeded, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1})
		require.NoError(t, err)

		decider, counted := interferingDecider(t, 1)
		_, err = architecturekit.Execute(context.Background(), store, decider,
			increment{subject: subject, By: 1}.onEventID(seeded[0].ID))

		assert.ErrorIs(t, err, architecturekit.ErrConflict, "the caller has to learn about the conflict")
		assert.Equal(t, 1, counted.decisions)
	})

	t.Run("stops deciding again once the context has ended", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		decider, counted := interferingDecider(t, 10)
		decide := decider.Decide
		decider = architecturekit.NewDecider(decider.State(),
			func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
				cancel()
				return decide(ctx, cmd, current)
			})

		_, err := architecturekit.Execute(ctx, retryingStore(t, 2), decider,
			increment{subject: subjectFor(t), By: 1}.onStateRead())

		// The context ended while deciding, so nothing is written at all, and
		// there is no conflict left to decide again on.
		assert.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, architecturekit.ErrConflict)
		assert.Equal(t, 1, counted.decisions)
	})

	t.Run("panics on a negative number of retries", func(t *testing.T) {
		assert.PanicsWithValue(t,
			"architecturekit: WithConflictRetries needs a number of retries that is not negative, not -1",
			func() { architecturekit.WithConflictRetries(-1) })
	})
}
