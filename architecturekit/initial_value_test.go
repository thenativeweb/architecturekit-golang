package architecturekit_test

import (
	"context"
	"maps"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// seen remembers every amount a subject was incremented by. Its initial value
// holds a map already, so that its Evolve rule writes into the map it starts
// from.
type seen struct {
	Amounts map[int]bool
}

func seenState() *architecturekit.State[seen] {
	return architecturekit.NewState(seen{Amounts: map[int]bool{}}).
		Evolve(func(current seen, event incremented) seen {
			current.Amounts[event.By] = true
			return current
		})
}

func cloneSeen(current seen) seen {
	return seen{Amounts: maps.Clone(current.Amounts)}
}

// seenDecider increments a subject only by an amount it has not seen yet.
func seenDecider(state *architecturekit.State[seen]) architecturekit.Decider[increment, seen] {
	return architecturekit.NewDecider(state,
		func(_ context.Context, cmd increment, current seen) ([]architecturekit.Event, error) {
			if current.Amounts[cmd.By] {
				return nil, architecturekit.NewDomainError("%d has been seen already", cmd.By)
			}
			return []architecturekit.Event{incremented{By: cmd.By}}, nil
		})
}

// listState appends every amount to a slice that the initial value holds, so
// whether a read shares data with the initial value depends on the capacity
// of that slice.
func listState(initial []int) *architecturekit.State[history] {
	return architecturekit.NewState(history{Values: initial}).
		Evolve(func(current history, event incremented) history {
			current.Values = append(current.Values, event.By)
			return current
		})
}

func TestInitialValue(t *testing.T) {
	t.Run("is not changed by Replay", func(t *testing.T) {
		state := seenState().Clone(cloneSeen)

		first, err := architecturekit.Replay(state, incremented{By: 3})
		require.NoError(t, err)
		assert.Equal(t, map[int]bool{3: true}, first.Amounts)

		empty, err := architecturekit.Replay(state)
		require.NoError(t, err)
		assert.Empty(t, empty.Amounts, "the first replay changed the initial value")

		second, err := architecturekit.Replay(state, incremented{By: 4})
		require.NoError(t, err)
		assert.Equal(t, map[int]bool{4: true}, second.Amounts, "the first replay changed the initial value")
		assert.Equal(t, map[int]bool{3: true}, first.Amounts, "the second replay changed the first state")
	})

	t.Run("is not changed by ReplayStored", func(t *testing.T) {
		state := seenState().Clone(cloneSeen)

		_, err := architecturekit.ReplayStored(state, stored((incremented{}).EventType(), `{"by":3}`))
		require.NoError(t, err)

		empty, err := architecturekit.ReplayStored(state)
		require.NoError(t, err)
		assert.Empty(t, empty.Amounts, "the first replay changed the initial value")
	})

	t.Run("is not changed by replays at the same time", func(t *testing.T) {
		state := seenState().Clone(cloneSeen)

		// Without a copy, all of them write into the same map, which the race
		// detector reports, and which the runtime may stop with a fatal error.
		var waitGroup sync.WaitGroup
		for i := range 8 {
			waitGroup.Go(func() {
				for j := range 200 {
					current, err := architecturekit.Replay(state, incremented{By: i*1000 + j})
					assert.NoError(t, err)
					assert.Len(t, current.Amounts, 1)
				}
			})
		}
		waitGroup.Wait()
	})

	t.Run("is not changed by Load or Execute", func(t *testing.T) {
		for _, withCache := range []bool{false, true} {
			t.Run("cache "+strconv.FormatBool(withCache), func(t *testing.T) {
				store := requireStore(t)
				if withCache {
					store = cachedStore(t, 10)
				}

				state := seenState().Clone(cloneSeen)
				decider := seenDecider(state)
				first := subjectFor(t) + "/first"
				second := subjectFor(t) + "/second"

				_, err := architecturekit.Execute(context.Background(), store, decider, increment{subject: first, By: 3})
				require.NoError(t, err)
				assert.Equal(t, map[int]bool{3: true}, load(t, store, state, first).Amounts)

				// The second subject has no events, so it is the initial value.
				assert.Empty(t, load(t, store, state, second).Amounts, "reading the first subject changed the initial value")

				_, err = architecturekit.Execute(context.Background(), store, decider, increment{subject: second, By: 3})
				require.NoError(t, err, "the second subject has not seen 3")
				assert.Equal(t, 3, totalIn(t, store, second))
			})
		}
	})

	t.Run("is not changed for the shape of a cached state", func(t *testing.T) {
		store := cachedStore(t, 10)
		subject := subjectFor(t)

		// A state built anew for every command is the same state for the cache,
		// as long as the initial value the cache has kept is not changed.
		for _, by := range []int{1, 2, 3} {
			_, err := architecturekit.Execute(context.Background(), store, seenDecider(seenState().Clone(cloneSeen)),
				increment{subject: subject, By: by})
			require.NoError(t, err, "incrementing by %d", by)
		}

		assert.Equal(t, map[int]bool{1: true, 2: true, 3: true}, load(t, store, seenState().Clone(cloneSeen), subject).Amounts)
	})

	t.Run("that holds a map needs a Clone function", func(t *testing.T) {
		state := seenState()

		_, err := architecturekit.Replay(state, incremented{By: 3})
		require.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorContains(t, err, "architecturekit_test.seen", "the error has to name the state")
		assert.ErrorContains(t, err, "needs a Clone function", "the error has to point to Clone")

		_, err = architecturekit.ReplayStored(state, stored((incremented{}).EventType(), `{"by":3}`))
		assert.ErrorIs(t, err, architecturekit.ErrPermanent)

		// It fails every time, not only the first time.
		_, err = architecturekit.Replay(state)
		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
	})

	t.Run("that holds a map without a Clone function makes Load and Execute fail before reading", func(t *testing.T) {
		// The store can not reach a database, so a transient error would show
		// that reading has begun.
		store := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

		_, err := architecturekit.Load(context.Background(), store, seenState(), "/test/seen")
		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.NotErrorIs(t, err, architecturekit.ErrTransient, "nothing may have been read")
		assert.ErrorContains(t, err, "needs a Clone function")

		_, err = architecturekit.Execute(context.Background(), store, seenDecider(seenState()),
			increment{subject: "/test/seen", By: 3})
		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.NotErrorIs(t, err, architecturekit.ErrTransient, "nothing may have been read")
	})

	t.Run("whose map is nil needs no Clone function", func(t *testing.T) {
		state := architecturekit.NewState(seen{}).
			Evolve(func(current seen, event incremented) seen {
				if current.Amounts == nil {
					current.Amounts = map[int]bool{}
				}
				current.Amounts[event.By] = true
				return current
			})

		first, err := architecturekit.Replay(state, incremented{By: 3})
		require.NoError(t, err)
		second, err := architecturekit.Replay(state, incremented{By: 4})
		require.NoError(t, err)

		assert.Equal(t, map[int]bool{3: true}, first.Amounts)
		assert.Equal(t, map[int]bool{4: true}, second.Amounts)

		store := requireStore(t)
		subject := subjectFor(t)
		writeRaw(t, subject, incremented{By: 5})
		assert.Equal(t, map[int]bool{5: true}, load(t, store, state, subject).Amounts)
		assert.Nil(t, load(t, store, state, subject+"/empty").Amounts)
	})

	t.Run("with a slice without room for elements needs no Clone function", func(t *testing.T) {
		state := listState([]int{})

		first, err := architecturekit.Replay(state, incremented{By: 3})
		require.NoError(t, err)
		second, err := architecturekit.Replay(state, incremented{By: 4})
		require.NoError(t, err)

		assert.Equal(t, []int{3}, first.Values)
		assert.Equal(t, []int{4}, second.Values)
	})

	t.Run("with a slice with room for elements needs a Clone function", func(t *testing.T) {
		_, err := architecturekit.Replay(listState(make([]int, 0, 4)), incremented{By: 3})
		require.ErrorIs(t, err, architecturekit.ErrPermanent, "an empty slice with room for elements shares its array")
		assert.ErrorContains(t, err, "architecturekit_test.history")

		// Appending to the copy of such a slice writes into the array of the
		// initial value, which a Clone function prevents.
		state := listState(make([]int, 0, 4)).Clone(cloneHistory)

		first, err := architecturekit.Replay(state, incremented{By: 3})
		require.NoError(t, err)
		second, err := architecturekit.Replay(state, incremented{By: 4})
		require.NoError(t, err)

		assert.Equal(t, []int{3}, first.Values, "the second replay changed the first state")
		assert.Equal(t, []int{4}, second.Values)
	})

	t.Run("that consists of values only is copied without a Clone function", func(t *testing.T) {
		state := counterState()

		first, err := architecturekit.Replay(state, incremented{By: 3})
		require.NoError(t, err)
		empty, err := architecturekit.Replay(state)
		require.NoError(t, err)

		assert.Equal(t, 3, first.Total)
		assert.Equal(t, 0, empty.Total)
	})

	t.Run("is copied with the Clone function of the state", func(t *testing.T) {
		calls := 0
		state := listState([]int{1}).Clone(func(current history) history {
			calls++
			return cloneHistory(current)
		})

		_, err := architecturekit.Replay(state, incremented{By: 2})
		require.NoError(t, err)
		current, err := architecturekit.Replay(state, incremented{By: 3})
		require.NoError(t, err)

		assert.Equal(t, []int{1, 3}, current.Values)
		assert.Equal(t, 2, calls, "every replay copies the initial value once")
	})
}
