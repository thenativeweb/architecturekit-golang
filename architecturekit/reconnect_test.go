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

// reconnectingStore waits with these delays. They are well above how long a
// stream that the database ends right away stays live, since a stream that
// stays live for longer than the initial delay starts the delay over.
const (
	testInitialDelay = 20 * time.Millisecond
	testMaxDelay     = 4 * testInitialDelay
)

func reconnectingStore(client *eventsourcingdb.Client, observed *reconnects) *architecturekit.Store {
	return architecturekit.NewStore(client, "https://thenativeweb.io",
		architecturekit.WithReconnectDelays(testInitialDelay, testMaxDelay),
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

	t.Run("doubles the delay up to the maximum if the stream ends right after the projection has caught up", func(t *testing.T) {
		database := &fakeDatabase{
			endObserving: func(int) bool { return true },
		}
		observed := &reconnects{}

		stop := runInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), &collector{})

		waitFor(t, func() bool { return observed.count() >= 4 })
		require.NoError(t, stop(t))

		_, delays := observed.recorded()
		want := []time.Duration{testInitialDelay, 2 * testInitialDelay, testMaxDelay, testMaxDelay}
		assert.Equal(t, want, delays[:4])

		for i, report := range observed.reports()[:4] {
			assert.Equal(t, i+1, report.Attempt, "attempt %d", i)
		}
	})

	t.Run("doubles the delay if the database fails before the projection has caught up", func(t *testing.T) {
		observed := &reconnects{}
		client := refusingDatabase(t, "/api/v1/read-events", http.StatusServiceUnavailable, "starting up")

		stop := runInBackground(t, reconnectingStore(client, observed), &collector{})

		waitFor(t, func() bool { return observed.count() >= 4 })
		require.NoError(t, stop(t))

		_, delays := observed.recorded()
		want := []time.Duration{testInitialDelay, 2 * testInitialDelay, testMaxDelay, testMaxDelay}
		assert.Equal(t, want, delays[:4])

		for i, report := range observed.reports()[:4] {
			assert.Equal(t, i+1, report.Attempt, "attempt %d", i)
		}
	})

	t.Run("doubles the delay if the stream ends right after a catch-up that took longer than the delay", func(t *testing.T) {
		// What counts is how long the projection followed the stream, not how
		// long the whole attempt took.
		database := &fakeDatabase{
			endObserving: func(int) bool { return true },
			readDelay:    2 * testMaxDelay,
		}
		observed := &reconnects{}

		stop := runInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), &collector{})

		waitFor(t, func() bool { return observed.count() >= 3 })
		require.NoError(t, stop(t))

		_, delays := observed.recorded()
		assert.Equal(t, []time.Duration{testInitialDelay, 2 * testInitialDelay, testMaxDelay}, delays[:3])
	})

	t.Run("starts over with the initial delay after following the stream for longer than the initial delay, even if no event arrived", func(t *testing.T) {
		// A load balancer that limits how long a connection may last cuts the
		// stream of a quiet projection regularly, each time after it has been
		// live for longer than the initial delay.
		database := &fakeDatabase{
			endObserving: func(int) bool { return false },
			cutAfter:     3 * testInitialDelay,
		}
		observed := &reconnects{}
		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io",
			architecturekit.WithReconnectDelays(testInitialDelay, time.Second),
			architecturekit.WithReconnectObserver(observed.observe),
		)

		stop := runInBackground(t, store, &collector{})

		waitFor(t, func() bool { return observed.count() >= 4 })
		require.NoError(t, stop(t))

		for i, report := range observed.reports()[:4] {
			assert.Equal(t, testInitialDelay, report.Delay, "delay %d", i)
			assert.Equal(t, 1, report.Attempt, "attempt %d", i)
		}
	})

	t.Run("starts over with the initial delay after following the stream for longer than the initial delay, although the delay has grown to the maximum", func(t *testing.T) {
		// After an outage, the first streams end right away, so the delay grows
		// to the maximum. Then a load balancer cuts every stream after it has
		// been live for longer than the initial delay, but not as long as the
		// delay has grown to, and no event arrives in between.
		database := &fakeDatabase{
			endObserving: func(connection int) bool { return connection <= 4 },
			cutAfter:     3 * testInitialDelay,
		}
		observed := &reconnects{}
		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io",
			architecturekit.WithReconnectDelays(testInitialDelay, 8*testInitialDelay),
			architecturekit.WithReconnectObserver(observed.observe),
		)

		stop := runInBackground(t, store, &collector{})

		waitFor(t, func() bool { return observed.count() >= 6 })
		require.NoError(t, stop(t))

		reports := observed.reports()
		require.Equal(t, 8*testInitialDelay, reports[3].Delay, "want the delay to have grown to the maximum")
		require.Equal(t, 4, reports[3].Attempt)

		for i, report := range reports[4:6] {
			assert.Equal(t, testInitialDelay, report.Delay, "delay %d", i+4)
			assert.Equal(t, 1, report.Attempt, "attempt %d", i+4)
		}
	})

	t.Run("doubles the delay if the stream ends after the projection has followed it for less than the initial delay", func(t *testing.T) {
		// The stream stays live for a moment, as when a database fails shortly
		// after the projection has caught up, but not as long as the initial
		// delay.
		database := &fakeDatabase{
			endObserving: func(int) bool { return false },
			cutAfter:     testInitialDelay,
		}
		observed := &reconnects{}
		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io",
			architecturekit.WithReconnectDelays(5*testInitialDelay, 10*testInitialDelay),
			architecturekit.WithReconnectObserver(observed.observe),
		)

		stop := runInBackground(t, store, &collector{})

		waitFor(t, func() bool { return observed.count() >= 3 })
		require.NoError(t, stop(t))

		_, delays := observed.recorded()
		assert.Equal(t, []time.Duration{5 * testInitialDelay, 10 * testInitialDelay, 10 * testInitialDelay}, delays[:3])

		for i, report := range observed.reports()[:3] {
			assert.Equal(t, i+1, report.Attempt, "attempt %d", i)
		}
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
		assert.Contains(t, delays[3:], testInitialDelay, "want the initial delay again after progress")

		// The attempts start over together with the delay.
		startsOver := false
		for _, report := range observed.reports()[3:] {
			if report.Attempt == 1 && report.Delay == testInitialDelay {
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
