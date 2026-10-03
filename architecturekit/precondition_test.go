package architecturekit_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// interrupted decides like the given decider, but has someone else write to
// the subject after Execute has read the state and before it writes, which is
// exactly the moment another process can get in between.
func interrupted(
	t *testing.T,
	store *architecturekit.Store,
	decider architecturekit.Decider[increment, counter],
) architecturekit.Decider[increment, counter] {
	t.Helper()

	decide := decider.Decide
	decider.Decide = func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
		_, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: cmd.subject, By: 100})
		assert.NoError(t, err, "failed to write in between")

		return decide(ctx, cmd, current)
	}

	return decider
}

func TestExecuteOnStateRead(t *testing.T) {
	t.Run("writes if nothing changed", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		ctx := context.Background()

		// The first command finds the subject pristine, the second one finds the
		// event of the first.
		for range 2 {
			_, err := architecturekit.Execute(ctx, store, counterDecider(),
				increment{subject: subject, By: 1}.onStateRead())
			require.NoError(t, err)
		}

		assert.Equal(t, 2, totalIn(t, store, subject))
	})

	t.Run("conflicts if someone wrote in between", func(t *testing.T) {
		t.Run("on a pristine subject", func(t *testing.T) {
			store := requireStore(t)
			subject := subjectFor(t)

			_, err := architecturekit.Execute(context.Background(), store, interrupted(t, store, counterDecider()),
				increment{subject: subject, By: 1}.onStateRead())

			assert.ErrorIs(t, err, architecturekit.ErrConflict)
			assert.Equal(t, 100, totalIn(t, store, subject), "only the write in between may have happened")
		})

		t.Run("on a populated subject", func(t *testing.T) {
			store := requireStore(t)
			subject := subjectFor(t)
			ctx := context.Background()

			_, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: subject, By: 1})
			require.NoError(t, err)

			_, err = architecturekit.Execute(ctx, store, interrupted(t, store, counterDecider()),
				increment{subject: subject, By: 1}.onStateRead())

			assert.ErrorIs(t, err, architecturekit.ErrConflict)
			assert.Equal(t, 101, totalIn(t, store, subject), "only the write in between may have happened")
		})

		t.Run("with a state cache", func(t *testing.T) {
			store := cachedStore(t, 10)
			subject := subjectFor(t)
			ctx := context.Background()

			// The first command leaves the state in the cache.
			_, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: subject, By: 1}.onStateRead())
			require.NoError(t, err)

			_, err = architecturekit.Execute(ctx, store, interrupted(t, store, counterDecider()),
				increment{subject: subject, By: 1}.onStateRead())

			assert.ErrorIs(t, err, architecturekit.ErrConflict)
			assert.Equal(t, 101, totalIn(t, store, subject), "only the write in between may have happened")
		})

		t.Run("with a state that starts from the latest event of a type", func(t *testing.T) {
			store := requireStore(t)
			subject := subjectFor(t)

			writeRaw(t, subject, incremented{By: 5}, reset{}, incremented{By: 1})

			_, err := architecturekit.Execute(context.Background(), store, interrupted(t, store, counterFromLatestDecider()),
				increment{subject: subject, By: 1}.onStateRead())

			assert.ErrorIs(t, err, architecturekit.ErrConflict)
			assert.Equal(t, 101, totalIn(t, store, subject), "only the write in between may have happened")
		})
	})

	t.Run("admits only one of concurrent commands", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		ctx := context.Background()

		_, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: subject, By: 1})
		require.NoError(t, err)

		// Every command waits in Decide until all of them have read the state, so
		// that they all decide on the same one, as concurrent processes may.
		const concurrent = 4
		var haveRead sync.WaitGroup
		haveRead.Add(concurrent)

		decider := counterDecider()
		decide := decider.Decide
		decider.Decide = func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
			haveRead.Done()
			haveRead.Wait()

			return decide(ctx, cmd, current)
		}

		var waitGroup sync.WaitGroup
		results := make([]error, concurrent)
		for i := range concurrent {
			waitGroup.Go(func() {
				_, results[i] = architecturekit.Execute(ctx, store, decider, increment{subject: subject, By: 1}.onStateRead())
			})
		}
		waitGroup.Wait()

		succeeded := 0
		for i, err := range results {
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, architecturekit.ErrConflict):
			default:
				assert.NoError(t, err, "command %d failed unexpectedly", i)
			}
		}

		assert.Equal(t, 1, succeeded, "exactly one command must succeed")
		assert.Equal(t, 2, totalIn(t, store, subject))
	})

	t.Run("sees what other processes wrote through the cache", func(t *testing.T) {
		cached := cachedStore(t, 10)
		other := requireStore(t)
		subject := subjectFor(t)
		ctx := context.Background()

		_, err := architecturekit.Execute(ctx, cached, counterDecider(), increment{subject: subject, By: 1}.onStateRead())
		require.NoError(t, err)

		// Another process writes, which the cache does not know about.
		_, err = architecturekit.Execute(ctx, other, counterDecider(), increment{subject: subject, By: 1}.onStateRead())
		require.NoError(t, err)

		// The cached store reads what was written since, and so decides on the
		// current state.
		_, err = architecturekit.Execute(ctx, cached, counterDecider(), increment{subject: subject, By: 1}.onStateRead())
		require.NoError(t, err)

		assert.Equal(t, 3, totalIn(t, cached, subject))
	})

	t.Run("combines with a revision of the caller", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		ctx := context.Background()

		written, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: subject, By: 1})
		require.NoError(t, err)
		seen := written[0].ID

		byCallerAndOnStateRead := func(eventID string) increment {
			return increment{subject: subject, By: 1}.declaring(
				architecturekit.Require(eventsourcingdb.NewIsSubjectOnEventIDPrecondition(subject, eventID)),
				architecturekit.OnStateRead(),
			)
		}

		written, err = architecturekit.Execute(ctx, store, counterDecider(), byCallerAndOnStateRead(seen))
		require.NoError(t, err, "a current revision must be accepted")

		_, err = architecturekit.Execute(ctx, store, counterDecider(), byCallerAndOnStateRead(seen))
		assert.ErrorIs(t, err, architecturekit.ErrConflict, "a stale revision must conflict")

		_, err = architecturekit.Execute(ctx, store, counterDecider(), byCallerAndOnStateRead(written[0].ID))
		assert.NoError(t, err, "a current revision must be accepted")
	})
}

func TestExecuteWithInvalidPreconditions(t *testing.T) {
	tests := []struct {
		name          string
		preconditions []architecturekit.Precondition
	}{
		{name: "none", preconditions: nil},
		{name: "unconditionally combined with another one", preconditions: []architecturekit.Precondition{
			architecturekit.Unconditionally(),
			architecturekit.OnStateRead(),
		}},
		{name: "a zero value", preconditions: []architecturekit.Precondition{{}}},
		{name: "a requirement of nothing", preconditions: []architecturekit.Precondition{architecturekit.Require(nil)}},
	}

	t.Run("rejects them before reading", func(t *testing.T) {
		// The store can not reach a database, so a transient error would show
		// that Execute has tried to read.
		store := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
					increment{subject: "/test/invalid", By: 1}.declaring(test.preconditions...))

				assert.ErrorIs(t, err, architecturekit.ErrPermanent)
				assert.NotErrorIs(t, err, architecturekit.ErrTransient, "nothing may have been read")
			})
		}
	})

	t.Run("rejects them with the same error as CheckPreconditions and the test fixture", func(t *testing.T) {
		store := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				cmd := increment{subject: "/test/invalid", By: 1}.declaring(test.preconditions...)

				_, err := architecturekit.Execute(context.Background(), store, counterDecider(), cmd)
				require.ErrorIs(t, err, architecturekit.ErrPermanent, "Execute has to refuse the command")

				assert.Equal(t, err, architecturekit.CheckPreconditions(cmd))
				assert.Equal(t, err.Error(), fixtureRefusal(t, counterDecider(), cmd))
			})
		}
	})
}

func TestCheckPreconditions(t *testing.T) {
	t.Run("accepts what Execute accepts", func(t *testing.T) {
		for _, cmd := range []increment{
			{subject: "/test/valid"},
			increment{subject: "/test/valid"}.onStateRead(),
			increment{subject: "/test/valid"}.pristine(),
			increment{subject: "/test/valid"}.declaring(
				architecturekit.Require(eventsourcingdb.NewIsSubjectPopulatedPrecondition("/test/valid")),
				architecturekit.OnStateRead(),
			),
		} {
			assert.NoError(t, architecturekit.CheckPreconditions(cmd))
		}
	})
}

func TestPrecondition(t *testing.T) {
	t.Run("tells how it was made", func(t *testing.T) {
		pristine := eventsourcingdb.NewIsSubjectPristinePrecondition("/books/42")

		required := architecturekit.Require(pristine)
		database, ok := required.Database()
		assert.True(t, ok)
		assert.Equal(t, pristine, database)
		assert.False(t, required.IsOnStateRead(), "a requirement is neither OnStateRead nor unconditional")
		assert.False(t, required.IsUnconditional(), "a requirement is neither OnStateRead nor unconditional")

		_, ok = architecturekit.OnStateRead().Database()
		assert.False(t, ok, "OnStateRead must only be OnStateRead")
		assert.True(t, architecturekit.OnStateRead().IsOnStateRead(), "OnStateRead must only be OnStateRead")

		_, ok = architecturekit.Unconditionally().Database()
		assert.False(t, ok, "Unconditionally must only be unconditional")
		assert.True(t, architecturekit.Unconditionally().IsUnconditional(), "Unconditionally must only be unconditional")
	})
}
