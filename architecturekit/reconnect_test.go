package architecturekit_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
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
	mutex sync.Mutex
	all   []architecturekit.Reconnect
}

func (r *reconnects) observe(reconnect architecturekit.Reconnect) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.all = append(r.all, reconnect)
}

func (r *reconnects) count() int {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return len(r.all)
}

func (r *reconnects) recorded() ([]error, []time.Duration) {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	errs := make([]error, len(r.all))
	delays := make([]time.Duration, len(r.all))
	for i, reconnect := range r.all {
		errs[i] = reconnect.Err
		delays[i] = reconnect.Delay
	}

	return errs, delays
}

func (r *reconnects) reports() []architecturekit.Reconnect {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return slices.Clone(r.all)
}

func reconnectingStore(client *eventsourcingdb.Client, observed *reconnects) *architecturekit.Store {
	return architecturekit.NewStore(client, "https://thenativeweb.io",
		architecturekit.WithReconnectDelays(time.Millisecond, 4*time.Millisecond),
		architecturekit.WithReconnectObserver(observed.observe),
	)
}

// runInBackground runs the projection until the returned function is called,
// which returns what the run ended with. It also ends with the test, so
// that a failed test does not leave it running.
func runInBackground(t *testing.T, store *architecturekit.Store, projection architecturekit.Projection) func(t *testing.T) error {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	done := make(chan error, 1)
	go func() { done <- runUntilDone(ctx, store, architecturekit.ExactSubject("/test"), projection) }()

	return func(t *testing.T) error {
		t.Helper()

		cancel()

		select {
		case err := <-done:
			return err
		case <-time.After(5 * time.Second):
			require.Fail(t, "the run did not end after its context ended")
			return nil
		}
	}
}

// failingCollector fails on every event, which retrying will not fix.
type failingCollector struct{}

func (failingCollector) Apply(context.Context, eventsourcingdb.Event) error {
	return errors.New("the view is broken")
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

func TestStartProjectionWithReconnects(t *testing.T) {
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

		// The attempts start over together with the delay.
		startsOver := false
		for _, report := range observed.reports()[3:] {
			if report.Attempt == 1 && report.Delay == time.Millisecond {
				startsOver = true
			}
		}
		assert.True(t, startsOver, "want the first attempt again after progress")
	})

	t.Run("retries an unreachable database", func(t *testing.T) {
		observed := &reconnects{}

		stop := runInBackground(t, reconnectingStore(deadClient(t), observed), &collector{})

		waitFor(t, func() bool { return observed.count() >= 2 })
		assert.NoError(t, stop(t), "ending through the context is not a failure")

		errs, _ := observed.recorded()
		assert.ErrorIs(t, errs[0], architecturekit.ErrTransient, "an unreachable database is transient")
	})

	t.Run("retries a database that is unable to answer for now", func(t *testing.T) {
		observed := &reconnects{}
		client := refusingDatabase(t, "/api/v1/read-events", http.StatusServiceUnavailable, "shutting down")

		stop := runInBackground(t, reconnectingStore(client, observed), &collector{})

		waitFor(t, func() bool { return observed.count() >= 2 })
		assert.NoError(t, stop(t), "ending through the context is not a failure")

		errs, _ := observed.recorded()
		assert.ErrorIs(t, errs[0], architecturekit.ErrTransient, "an unavailable database is transient")
	})

	t.Run("ends when the database rejects the API token", func(t *testing.T) {
		observed := &reconnects{}
		client := refusingDatabase(t, "/api/v1/read-events", http.StatusUnauthorized, "unauthorized")

		// Retrying would go on until the context ends, which a run
		// reports without an error.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		err := runUntilDone(ctx, reconnectingStore(client, observed), architecturekit.ExactSubject("/test"), &collector{})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a rejected API token is permanent")
		assert.ErrorContains(t, err, "the database rejected the API token")
		assert.Zero(t, observed.count(), "a rejected API token must not be retried")
	})

	t.Run("ends on a failure that retrying will not fix", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0},
			endObserving: func(int) bool { return true },
		}
		observed := &reconnects{}
		store := reconnectingStore(newFakeDatabase(t, database), observed)

		err := runUntilDone(context.Background(), store, architecturekit.ExactSubject("/test"), failingCollector{})

		assert.ErrorContains(t, err, "the view is broken", "expected the failure of Apply")
		assert.Zero(t, observed.count(), "a failing Apply must not be retried")
	})

	t.Run("retries a failure of Apply that is transient", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0, 1, 2},
			endObserving: func(int) bool { return false },
		}
		observed := &reconnects{}
		target := &flakyCollector{flakyID: "1"}
		target.failuresLeft.Store(2)

		stop := runInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), target)

		waitFor(t, func() bool { return len(target.IDs()) == 3 })
		assert.NoError(t, stop(t), "ending through the context is not a failure")

		// The failing event is tried again until it succeeds, and no event is
		// skipped or applied twice on the way.
		assert.Equal(t, []string{"0", "1", "2"}, target.IDs(), "want every event exactly once")

		errs, _ := observed.recorded()
		require.Len(t, errs, 2, "want two retries")
		for _, err := range errs {
			assert.ErrorIs(t, err, architecturekit.ErrTransient, "each retry follows a transient failure")
			assert.ErrorContains(t, err, "the target is unavailable", "each retry follows the failure of Apply")
		}
	})
}

func TestWithReconnectDelays(t *testing.T) {
	t.Run("panics on an initial delay of zero", func(t *testing.T) {
		assert.PanicsWithValue(t,
			"architecturekit: WithReconnectDelays needs an initial delay that is positive, not 0s",
			func() { architecturekit.WithReconnectDelays(0, time.Second) })
	})

	t.Run("panics on a negative initial delay", func(t *testing.T) {
		assert.PanicsWithValue(t,
			"architecturekit: WithReconnectDelays needs an initial delay that is positive, not -1ms",
			func() { architecturekit.WithReconnectDelays(-time.Millisecond, time.Second) })
	})

	t.Run("panics on a maximum delay below the initial delay", func(t *testing.T) {
		assert.PanicsWithValue(t,
			"architecturekit: WithReconnectDelays needs a maximum delay that is not below the initial delay of 2ms, not 1ms",
			func() { architecturekit.WithReconnectDelays(2*time.Millisecond, time.Millisecond) })
	})

	t.Run("names the initial delay if the maximum delay is below it as well", func(t *testing.T) {
		assert.PanicsWithValue(t,
			"architecturekit: WithReconnectDelays needs an initial delay that is positive, not -1ms",
			func() { architecturekit.WithReconnectDelays(-time.Millisecond, -time.Second) })
	})

	t.Run("accepts delays that are equal, and then waits as long on every attempt", func(t *testing.T) {
		database := &fakeDatabase{
			endObserving: func(int) bool { return true },
		}
		observed := &reconnects{}

		var option architecturekit.StoreOption
		require.NotPanics(t, func() { option = architecturekit.WithReconnectDelays(2*time.Millisecond, 2*time.Millisecond) })

		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io",
			option, architecturekit.WithReconnectObserver(observed.observe))
		stop := runInBackground(t, store, &collector{})

		waitFor(t, func() bool { return observed.count() >= 3 })
		require.NoError(t, stop(t))

		_, delays := observed.recorded()
		assert.Equal(t, []time.Duration{2 * time.Millisecond, 2 * time.Millisecond, 2 * time.Millisecond}, delays[:3])
	})

	t.Run("accepts a maximum delay above the initial delay", func(t *testing.T) {
		assert.NotPanics(t, func() { architecturekit.WithReconnectDelays(500*time.Millisecond, 30*time.Second) })
	})
}
