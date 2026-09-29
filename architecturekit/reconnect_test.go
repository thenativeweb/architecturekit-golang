package architecturekit_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// reconnects records what a store reports before it reads again.
type reconnects struct {
	mutex  sync.Mutex
	errs   []error
	delays []time.Duration
}

func (r *reconnects) observe(err error, delay time.Duration) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.errs = append(r.errs, err)
	r.delays = append(r.delays, delay)
}

func (r *reconnects) count() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return len(r.delays)
}

func (r *reconnects) recorded() ([]error, []time.Duration) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return slices.Clone(r.errs), slices.Clone(r.delays)
}

func reconnectingStore(client *eventsourcingdb.Client, observed *reconnects) *architecturekit.Store {
	return architecturekit.NewStore(client, "https://thenativeweb.io",
		architecturekit.WithReconnectDelays(time.Millisecond, 4*time.Millisecond),
		architecturekit.WithReconnectObserver(observed.observe),
	)
}

// runInBackground runs the projection until the returned function is called,
// which returns what RunProjection returned. It also ends with the test, so
// that a failed test does not leave it running.
func runInBackground(t *testing.T, store *architecturekit.Store, projection architecturekit.Projection) func(t *testing.T) error {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- architecturekit.RunProjection(ctx, store, "/test", false, projection) }()

	return func(t *testing.T) error {
		t.Helper()

		cancel()

		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			require.Fail(t, "RunProjection did not return after its context ended")
			return nil
		}
	}
}

// failingCollector fails on every event, which retrying will not fix.
type failingCollector struct{}

func (failingCollector) Apply(context.Context, eventsourcingdb.Event) error {
	return errors.New("the view is broken")
}

func TestRunProjectionWithReconnects(t *testing.T) {
	t.Run("reconnects after the stream ends", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0, 1, 2},
			endObserving: func(connection int) bool { return connection == 1 },
		}
		observed := &reconnects{}
		target := &collector{}

		stop := runInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), target)

		waitFor(t, func() bool { return observed.count() == 1 })
		database.add(3)
		waitFor(t, func() bool { return len(target.IDs()) == 4 })

		assert.NoError(t, stop(t), "ending through the context is not a failure")

		// Catching up again must not apply the events from before the reconnect
		// a second time, although a rebuilt projection has no checkpoint.
		assert.Equal(t, []string{"0", "1", "2", "3"}, target.IDs(), "want every event exactly once")

		errs, _ := observed.recorded()
		assert.NoError(t, errs[0], "a stream that ended is reported without an error")
	})

	t.Run("doubles the delay up to the maximum", func(t *testing.T) {
		database := &fakeDatabase{
			endObserving: func(int) bool { return true },
		}
		observed := &reconnects{}

		stop := runInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), &collector{})

		waitFor(t, func() bool { return observed.count() >= 4 })
		require.NoError(t, stop(t))

		_, delays := observed.recorded()
		want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond}
		assert.Equal(t, want, delays[:4])
	})

	t.Run("starts over with the initial delay after progress", func(t *testing.T) {
		database := &fakeDatabase{
			endObserving: func(int) bool { return true },
		}
		observed := &reconnects{}
		target := &collector{}

		stop := runInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), target)

		waitFor(t, func() bool { return observed.count() >= 3 })
		database.add(0)
		waitFor(t, func() bool { return len(target.IDs()) == 1 })

		// The attempt after the one that applied the event starts over.
		countAfterProgress := observed.count()
		waitFor(t, func() bool { return observed.count() > countAfterProgress })

		require.NoError(t, stop(t))

		_, delays := observed.recorded()
		assert.Contains(t, delays[3:], time.Millisecond, "want the initial delay again after progress")
	})

	t.Run("retries an unreachable database", func(t *testing.T) {
		observed := &reconnects{}

		stop := runInBackground(t, reconnectingStore(deadClient(t), observed), &collector{})

		waitFor(t, func() bool { return observed.count() >= 2 })
		assert.NoError(t, stop(t), "ending through the context is not a failure")

		errs, _ := observed.recorded()
		assert.ErrorIs(t, errs[0], architecturekit.ErrTransient, "an unreachable database is transient")
	})

	t.Run("ends on a failure that retrying will not fix", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0},
			endObserving: func(int) bool { return true },
		}
		observed := &reconnects{}
		store := reconnectingStore(newFakeDatabase(t, database), observed)

		err := architecturekit.RunProjection(context.Background(), store, "/test", false, failingCollector{})

		assert.ErrorContains(t, err, "the view is broken", "expected the failure of Apply")
		assert.Zero(t, observed.count(), "a failing Apply must not be retried")
	})
}

// flakyCollector fails on the first attempts at one event with a transient
// failure, as a publisher does whose target is unavailable for a moment.
type flakyCollector struct {
	collector

	flakyID      string
	failuresLeft atomic.Int32
}

func (c *flakyCollector) Apply(ctx context.Context, event eventsourcingdb.Event) error {
	if event.ID == c.flakyID && c.failuresLeft.Add(-1) >= 0 {
		return fmt.Errorf("%w: the target is unavailable", architecturekit.ErrTransient)
	}

	return c.collector.Apply(ctx, event)
}

func TestRunProjectionRetriesAFailureOfApplyThatIsTransient(t *testing.T) {
	database := &fakeDatabase{
		events:       []int{0, 1, 2},
		endObserving: func(int) bool { return false },
	}
	observed := &reconnects{}
	target := &flakyCollector{flakyID: "1"}
	target.failuresLeft.Store(2)

	stop := runInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), target)

	waitFor(t, func() bool { return len(target.IDs()) == 3 })
	if err := stop(t); err != nil {
		t.Fatalf("ending through the context is not a failure, got %v", err)
	}

	// The failing event is tried again until it succeeds, and no event is
	// skipped or applied twice on the way.
	if ids := target.IDs(); !slices.Equal(ids, []string{"0", "1", "2"}) {
		t.Fatalf("got %v, want every event exactly once", ids)
	}

	errs, _ := observed.recorded()
	if len(errs) != 2 {
		t.Fatalf("got %d retries, want 2", len(errs))
	}
	for _, err := range errs {
		if !errors.Is(err, architecturekit.ErrTransient) || !strings.Contains(err.Error(), "the target is unavailable") {
			t.Fatalf("each retry follows the failure of Apply, got %v", err)
		}
	}
}
