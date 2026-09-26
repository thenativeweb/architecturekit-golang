package architecturekit_test

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"

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

// cachedStore returns a store with a state cache on the test database.
func cachedStore(t *testing.T, maxSubjects int) *architecturekit.Store {
	t.Helper()

	return architecturekit.NewStore(rawClient(t), "https://thenativeweb.io", architecturekit.WithStateCache(maxSubjects))
}

func load[TState any](t *testing.T, store *architecturekit.Store, state *architecturekit.State[TState], subject string) TState {
	t.Helper()

	current, err := architecturekit.Load(context.Background(), store, state, subject)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	return current
}

func TestLoadReadsTheStateOfASubject(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)

	writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})

	current := load(t, store, counterState(), subject)
	if current.Total != 7 {
		t.Fatalf("got %d, want 7", current.Total)
	}
}

func TestLoadWithoutStateCacheReadsAllEventsEveryTime(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)

	var evolved atomic.Int64
	state := countingState(&evolved)

	writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})

	load(t, store, state, subject)
	load(t, store, state, subject)

	if evolved.Load() != 4 {
		t.Fatalf("evolved %d events, want 4", evolved.Load())
	}
}

func TestLoadWithStateCacheReadsOnlyTheEventsWrittenSince(t *testing.T) {
	store := cachedStore(t, 10)
	subject := subjectFor(t)

	var evolved atomic.Int64
	state := countingState(&evolved)

	writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})
	load(t, store, state, subject)

	// These events are written past the store, as another process would.
	writeRaw(t, subject, incremented{By: 5})
	current := load(t, store, state, subject)

	if current.Total != 12 {
		t.Fatalf("got %d, want 12", current.Total)
	}
	if evolved.Load() != 3 {
		t.Fatalf("evolved %d events, want 3", evolved.Load())
	}
}

func TestExecuteWithStateCacheSeesItsOwnWrites(t *testing.T) {
	store := cachedStore(t, 10)
	subject := subjectFor(t)
	ctx := context.Background()

	for range 2 {
		if _, err := architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: 2, Limit: 5}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	// The total is 4, so another 2 exceed the limit. A cache that missed the
	// events it did not read itself would still see 0 or 2.
	_, err := architecturekit.Execute(ctx, store, counterDecider(),
		increment{subject: subject, By: 2, Limit: 5})
	if !errors.Is(err, architecturekit.ErrDomain) {
		t.Fatalf("expected the limit to be exceeded, got %v", err)
	}
}

func TestStateCacheSkipsAStateWithReferencesAndNoCloneFunction(t *testing.T) {
	store := cachedStore(t, 10)
	subject := subjectFor(t)

	var evolved atomic.Int64
	state := historyState(&evolved)

	writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})

	load(t, store, state, subject)
	load(t, store, state, subject)

	if evolved.Load() != 4 {
		t.Fatalf("evolved %d events, want 4, since the state must not be cached", evolved.Load())
	}
}

func TestStateCacheCachesAStateWithACloneFunction(t *testing.T) {
	store := cachedStore(t, 10)
	subject := subjectFor(t)

	var evolved atomic.Int64
	state := historyState(&evolved).Clone(cloneHistory)

	writeRaw(t, subject, incremented{By: 3}, incremented{By: 4})
	load(t, store, state, subject)

	writeRaw(t, subject, incremented{By: 5})
	current := load(t, store, state, subject)

	if !slices.Equal(current.Values, []int{3, 4, 5}) {
		t.Fatalf("got %v, want [3 4 5]", current.Values)
	}
	if evolved.Load() != 3 {
		t.Fatalf("evolved %d events, want 3", evolved.Load())
	}
}

func TestLoadWithStateCacheReturnsACopyOfTheCachedState(t *testing.T) {
	store := cachedStore(t, 10)
	subject := subjectFor(t)

	var evolved atomic.Int64
	state := historyState(&evolved).Clone(cloneHistory)

	writeRaw(t, subject, incremented{By: 3})

	first := load(t, store, state, subject)
	first.Values[0] = 999

	second := load(t, store, state, subject)
	if second.Values[0] != 3 {
		t.Fatalf("got %d, want 3, since changing a loaded state must not change the cache", second.Values[0])
	}
}

func TestStateCacheReadsAnEvictedSubjectAgain(t *testing.T) {
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

	if current.Total != 3 {
		t.Fatalf("got %d, want 3", current.Total)
	}
	if evolved.Load() != 5 {
		t.Fatalf("evolved %d events, want 5, since the first subject was evicted", evolved.Load())
	}
}

func TestStateCacheWorksWithFromLatest(t *testing.T) {
	store := cachedStore(t, 10)
	subject := subjectFor(t)

	var evolved atomic.Int64
	state := countingState(&evolved).FromLatest[reset]()

	writeRaw(t, subject, incremented{By: 10}, reset{}, incremented{By: 2})
	load(t, store, state, subject)

	writeRaw(t, subject, incremented{By: 1})
	current := load(t, store, state, subject)

	if current.Total != 3 {
		t.Fatalf("got %d, want 3", current.Total)
	}
	if evolved.Load() != 3 {
		t.Fatalf("evolved %d events, want 3: the reset and one increment first, then one more", evolved.Load())
	}
}
