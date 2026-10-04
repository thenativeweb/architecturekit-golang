package architecturekit_test

import (
	"context"
	"slices"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// countingState is the counter state, but it counts how many events it
// evolves, which shows how many events were read.
func countingState(evolved *atomic.Int64) *architecturekit.State[counter] {
	state := architecturekit.NewState(counter{})

	state.Evolve(func(current counter, event incremented) counter {
		evolved.Add(1)
		current.Total += event.By
		return current
	})

	state.Evolve(func(current counter, event reset) counter {
		evolved.Add(1)
		current.Total = 0
		return current
	})

	return state
}

// history holds a slice, so it may only be cached with a clone function.
type history struct {
	Values []int
}

func historyState(evolved *atomic.Int64) *architecturekit.State[history] {
	state := architecturekit.NewState(history{})

	state.Evolve(func(current history, event incremented) history {
		evolved.Add(1)
		current.Values = append(current.Values, event.By)
		return current
	})

	return state
}

func cloneHistory(current history) history {
	return history{Values: slices.Clone(current.Values)}
}

// countState and sumState are two different states of the same type, int:
// one counts the increments, the other one adds them up.
func countState() *architecturekit.State[int] {
	return architecturekit.NewState(0).
		Evolve(func(count int, _ incremented) int { return count + 1 })
}

func sumState() *architecturekit.State[int] {
	return architecturekit.NewState(0).
		Evolve(func(sum int, event incremented) int { return sum + event.By }).
		Evolve(func(int, reset) int { return 0 })
}

// cachedStore returns a store with a state cache on the test database.
func cachedStore(t *testing.T, maxSubjects int) *architecturekit.Store {
	t.Helper()

	return architecturekit.NewStore(rawClient(t), "https://thenativeweb.io", architecturekit.WithStateCache(maxSubjects))
}

func load[TState any](t *testing.T, store *architecturekit.Store, state *architecturekit.State[TState], subject string) TState {
	t.Helper()

	current, err := architecturekit.Load(context.Background(), store, state, subject)
	require.NoError(t, err)

	return current
}

func TestLoad(t *testing.T) {
	t.Run("reads the state of a subject", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})

		current := load(t, store, counterState(), subject)
		assert.Equal(t, 7, current.Total)
	})
}

func TestLoadWithoutStateCache(t *testing.T) {
	t.Run("allows two different states of the same type on the same subject", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})

		assert.Equal(t, 2, load(t, store, countState(), subject))
		assert.Equal(t, 7, load(t, store, sumState(), subject))
	})

	t.Run("reads all events every time", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		var evolved atomic.Int64
		state := countingState(&evolved)

		writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})

		load(t, store, state, subject)
		load(t, store, state, subject)

		assert.Equal(t, int64(4), evolved.Load())
	})
}

func TestLoadWithStateCache(t *testing.T) {
	t.Run("reads only the events written since", func(t *testing.T) {
		store := cachedStore(t, 10)
		subject := subjectFor(t)

		var evolved atomic.Int64
		state := countingState(&evolved)

		writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})
		load(t, store, state, subject)

		// These events are written past the store, as another process would.
		writeRaw(t, subject, incremented{By: 5})
		current := load(t, store, state, subject)

		assert.Equal(t, 12, current.Total)
		assert.Equal(t, int64(3), evolved.Load())
	})

	t.Run("shares the cached state between states built alike", func(t *testing.T) {
		store := cachedStore(t, 10)
		subject := subjectFor(t)

		var evolved atomic.Int64

		writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})
		load(t, store, countingState(&evolved), subject)

		// A state built anew for every command, as a function that returns it
		// does, is the same state as far as the cache is concerned.
		writeRaw(t, subject, incremented{By: 5})
		current := load(t, store, countingState(&evolved), subject)

		assert.Equal(t, 12, current.Total)
		assert.Equal(t, int64(3), evolved.Load(), "the second state must continue from the cached one")
	})

	t.Run("reports two different states of the same type on the same subject", func(t *testing.T) {
		store := cachedStore(t, 10)
		subject := subjectFor(t)

		writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})
		assert.Equal(t, 2, load(t, store, countState(), subject))

		_, err := architecturekit.Load(context.Background(), store, sumState(), subject)

		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorContains(t, err, "two different states of type int")
		assert.ErrorContains(t, err, subject, "the error must name the subject")
	})

	t.Run("hands a state the cached state of another one of the same type that is built alike", func(t *testing.T) {
		// The cache can not compare the Evolve functions, so it does not tell
		// these two apart, although one counts and the other one sums up. That
		// is why two different states need two different types.
		counting := architecturekit.NewState(0).
			Evolve(func(count int, _ incremented) int { return count + 1 })
		summing := architecturekit.NewState(0).
			Evolve(func(sum int, event incremented) int { return sum + event.By })

		store := cachedStore(t, 10)
		subject := subjectFor(t)

		writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})

		assert.Equal(t, 2, load(t, store, counting, subject))
		assert.Equal(t, 2, load(t, store, summing, subject), "the sum is 7, but the cache hands out the count")
	})

	t.Run("keeps two different states of the same type on different subjects apart", func(t *testing.T) {
		store := cachedStore(t, 10)
		counted := subjectFor(t) + "/counted"
		summed := subjectFor(t) + "/summed"

		writeRaw(t, counted, incremented{By: 3}, incremented{By: 4})
		writeRaw(t, summed, incremented{By: 3}, incremented{By: 4})

		assert.Equal(t, 2, load(t, store, countState(), counted))
		assert.Equal(t, 7, load(t, store, sumState(), summed))
	})

	t.Run("skips a state with references and no clone function", func(t *testing.T) {
		store := cachedStore(t, 10)
		subject := subjectFor(t)

		var evolved atomic.Int64
		state := historyState(&evolved)

		writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})

		load(t, store, state, subject)
		load(t, store, state, subject)

		assert.Equal(t, int64(4), evolved.Load(), "the state must not be cached")
	})

	t.Run("caches a state with a clone function", func(t *testing.T) {
		store := cachedStore(t, 10)
		subject := subjectFor(t)

		var evolved atomic.Int64
		state := historyState(&evolved).Clone(cloneHistory)

		writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})
		load(t, store, state, subject)

		writeRaw(t, subject, incremented{By: 5})
		current := load(t, store, state, subject)

		assert.Equal(t, []int{3, 4, 5}, current.Values)
		assert.Equal(t, int64(3), evolved.Load())
	})

	t.Run("returns a copy of the cached state", func(t *testing.T) {
		store := cachedStore(t, 10)
		subject := subjectFor(t)

		var evolved atomic.Int64
		state := historyState(&evolved).Clone(cloneHistory)

		writeRaw(t, subject, incremented{By: 3})

		first := load(t, store, state, subject)
		first.Values[0] = 999

		second := load(t, store, state, subject)
		assert.Equal(t, 3, second.Values[0], "changing a loaded state must not change the cache")
	})

	t.Run("reads an evicted subject again", func(t *testing.T) {
		store := cachedStore(t, 1)
		first := subjectFor(t) + "/first"
		second := subjectFor(t) + "/second"

		var evolved atomic.Int64
		state := countingState(&evolved)

		writeRaw(t, first, incremented{By: 1}, incremented{By: 2})
		writeRaw(t, second, incremented{By: 3})

		load(t, store, state, first)
		load(t, store, state, second)
		current := load(t, store, state, first)

		assert.Equal(t, 3, current.Total)
		assert.Equal(t, int64(5), evolved.Load(), "the first subject was evicted")
	})

	t.Run("works with FromLatest", func(t *testing.T) {
		store := cachedStore(t, 10)
		subject := subjectFor(t)

		var evolved atomic.Int64
		state := countingState(&evolved).FromLatest[reset]()

		writeRaw(t, subject, incremented{By: 10}, reset{}, incremented{By: 2})
		load(t, store, state, subject)

		writeRaw(t, subject, incremented{By: 1})
		current := load(t, store, state, subject)

		assert.Equal(t, 3, current.Total)
		assert.Equal(t, int64(3), evolved.Load(), "the reset and one increment first, then one more")
	})
}

func TestWithStateCache(t *testing.T) {
	t.Run("panics on a negative number of subjects", func(t *testing.T) {
		assert.PanicsWithValue(t,
			"architecturekit: WithStateCache needs a number of subjects that is not negative, not -1",
			func() { architecturekit.WithStateCache(-1) })
	})

	t.Run("turns the cache off with 0 subjects", func(t *testing.T) {
		var option architecturekit.StoreOption
		require.NotPanics(t, func() { option = architecturekit.WithStateCache(0) })

		store := architecturekit.NewStore(rawClient(t), "https://thenativeweb.io", option)
		subject := subjectFor(t)

		var evolved atomic.Int64
		state := countingState(&evolved)

		writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})

		load(t, store, state, subject)
		load(t, store, state, subject)

		assert.Equal(t, int64(4), evolved.Load(), "the state must be read in full every time")

		// Nor does the rule apply that a cache brings, that two different
		// states of the same type on the same subject fail.
		assert.Equal(t, 2, load(t, store, countState(), subject))
		assert.Equal(t, 7, load(t, store, sumState(), subject))
	})
}

func TestExecuteWithStateCache(t *testing.T) {
	t.Run("sees its own writes", func(t *testing.T) {
		store := cachedStore(t, 10)
		subject := subjectFor(t)
		ctx := context.Background()

		for range 2 {
			_, err := architecturekit.Execute(ctx, store, counterDecider(),
				increment{subject: subject, By: 2, Limit: 5})
			require.NoError(t, err)
		}

		// The total is 4, so another 2 exceed the limit. A cache that missed the
		// events it did not read itself would still see 0 or 2.
		_, err := architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: 2, Limit: 5})
		assert.ErrorIs(t, err, architecturekit.ErrDomain, "expected the limit to be exceeded")
	})
}
