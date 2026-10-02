package architecturekit_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// startInBackground starts the projection and returns the run, and a function
// that ends it and waits for it to end.
func startInBackground(
	t *testing.T,
	store *architecturekit.Store,
	projection architecturekit.Projection,
) (*architecturekit.ProjectionRun, func(t *testing.T)) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	run := architecturekit.StartProjection(ctx, store, architecturekit.ExactSubject("/test"), projection)

	return run, func(t *testing.T) {
		t.Helper()

		cancel()
		waitForClosed(t, run.Done(), "Done")
	}
}

func waitForClosed(t *testing.T, channel <-chan struct{}, name string) {
	t.Helper()

	select {
	case <-channel:
	case <-time.After(5 * time.Second):
		require.FailNow(t, name+" was not closed in time")
	}
}

func isClosed(channel <-chan struct{}) bool {
	select {
	case <-channel:
		return true
	default:
		return false
	}
}

func TestStartProjection(t *testing.T) {
	t.Run("tells when it has caught up", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0, 1, 2},
			endObserving: func(int) bool { return false },
		}
		target := &collector{}
		started := time.Now()

		run, stop := startInBackground(t,
			architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io"), target)

		waitForClosed(t, run.CaughtUp(), "CaughtUp")

		// Everything that was stored at the start has been applied by then.
		assert.Equal(t, []string{"0", "1", "2"}, target.IDs())

		status := run.Status()
		assert.Equal(t, architecturekit.PhaseLive, status.Phase)
		assert.True(t, status.HasCaughtUp)
		assert.Equal(t, "2", status.Revision)
		assert.NoError(t, status.Err)
		assert.Equal(t, 0, status.Attempts)
		assert.False(t, status.Since.Before(started),
			"the phase began at %v, before the run started at %v", status.Since, started)

		database.add(3)
		waitFor(t, func() bool { return run.Status().Revision == "3" })

		stop(t)

		// The history is read once, not once for catching up and once again for
		// following the stream.
		assert.Equal(t, []string{"0", "1", "2", "3"}, target.IDs(), "every event exactly once")
		assert.NoError(t, run.Err(), "ending through the context is not a failure")

		status = run.Status()
		assert.Equal(t, architecturekit.PhaseStopped, status.Phase)
		assert.NoError(t, status.Err)
		assert.True(t, status.HasCaughtUp, "a run that has caught up once keeps saying so after it stopped")
	})

	t.Run("refuses a projection that is transactional as well", func(t *testing.T) {
		assert.Panics(t, func() {
			architecturekit.StartProjection(context.Background(), nil, architecturekit.ExactSubject("/test"), &transactionalWithApply{})
		})
	})

	t.Run("with a database", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		seed(t, subject, 3)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		target := &collector{}
		run := architecturekit.StartProjection(ctx, store, architecturekit.ExactSubject(subject), target)

		waitForClosed(t, run.CaughtUp(), "CaughtUp")
		assert.Len(t, target.IDs(), 3)

		seed(t, subject, 1)
		waitFor(t, func() bool { return len(target.IDs()) == 4 })

		cancel()
		waitForClosed(t, run.Done(), "Done")
	})
}

func TestProjectionRun(t *testing.T) {
	t.Run("reports a disruption", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0},
			endObserving: func(connection int) bool { return connection == 1 },
		}
		observed := &reconnects{}

		// The delay is long enough for the run to stay in the disruption until
		// the test ends it.
		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io",
			architecturekit.WithReconnectDelays(time.Hour, time.Hour),
			architecturekit.WithReconnectObserver(observed.observe),
		)

		run, stop := startInBackground(t, store, &collector{})

		waitForClosed(t, run.CaughtUp(), "CaughtUp")
		waitFor(t, func() bool { return observed.count() == 1 })

		status := run.Status()
		assert.Equal(t, architecturekit.PhaseReconnecting, status.Phase)
		assert.NoError(t, status.Err, "a stream that ended is reported without an error")
		assert.Equal(t, 1, status.Attempts)
		assert.Equal(t, "0", status.Revision)

		// Having caught up once, the run does not take it back.
		assert.True(t, isClosed(run.CaughtUp()), "CaughtUp must stay closed while the run reconnects")
		assert.True(t, status.HasCaughtUp, "HasCaughtUp must stay true while the run reconnects")

		stop(t)
	})

	t.Run("is live again after a disruption", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0},
			endObserving: func(connection int) bool { return connection == 1 },
		}
		observed := &reconnects{}

		run, stop := startInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), &collector{})

		waitFor(t, func() bool {
			return observed.count() == 1 && run.Status().Phase == architecturekit.PhaseLive
		})

		status := run.Status()
		assert.Equal(t, 0, status.Attempts)
		assert.NoError(t, status.Err)

		stop(t)
	})

	t.Run("starts the attempts over whenever the run has caught up, unlike the observer", func(t *testing.T) {
		// The stream ends right after every catch-up, so every attempt is the
		// first of a disruption of its own, while the observer counts on.
		database := &fakeDatabase{
			endObserving: func(int) bool { return true },
		}
		observed := &reconnects{}

		run, stop := startInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), &collector{})

		mostAttempts := 0
		waitFor(t, func() bool {
			mostAttempts = max(mostAttempts, run.Status().Attempts)
			return observed.count() >= 4
		})
		stop(t)

		assert.Equal(t, 4, observed.reports()[3].Attempt)
		assert.Equal(t, 1, mostAttempts, "want the attempts of the status to start over whenever the run has caught up")
	})

	t.Run("keeps the beginning of a disruption", func(t *testing.T) {
		observed := &reconnects{}

		run, stop := startInBackground(t, reconnectingStore(deadClient(t), observed), &collector{})

		waitFor(t, func() bool { return run.Status().Attempts >= 1 })
		first := run.Status()

		waitFor(t, func() bool { return run.Status().Attempts >= 3 })
		later := run.Status()

		assert.Equal(t, architecturekit.PhaseReconnecting, later.Phase)
		assert.True(t, later.Since.Equal(first.Since),
			"the disruption began at %v, but the status says %v", first.Since, later.Since)
		assert.ErrorIs(t, later.Err, architecturekit.ErrTransient, "an unreachable database is transient")
		assert.False(t, isClosed(run.CaughtUp()), "a run that never read anything has not caught up")
		assert.False(t, later.HasCaughtUp, "a run that never read anything has not caught up")

		stop(t)

		assert.NoError(t, run.Err(), "ending through the context is not a failure")
	})

	t.Run("stops on a failure that retrying will not fix", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0},
			endObserving: func(int) bool { return true },
		}
		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io")

		run := architecturekit.StartProjection(context.Background(), store, architecturekit.ExactSubject("/test"), failingCollector{})

		waitForClosed(t, run.Done(), "Done")

		assert.ErrorContains(t, run.Err(), "the view is broken", "expected the failure of Apply")

		status := run.Status()
		assert.Equal(t, architecturekit.PhaseStopped, status.Phase)
		assert.ErrorIs(t, status.Err, run.Err())
		assert.False(t, isClosed(run.CaughtUp()), "a run that failed before catching up has not caught up")
		assert.False(t, status.HasCaughtUp, "a run that failed before catching up has not caught up")
	})
}

func TestStartTransactionalProjection(t *testing.T) {
	t.Run("commits before it has caught up", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0, 1, 2},
			endObserving: func(int) bool { return false },
		}
		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io")
		target := &transactionalCollector{}

		ctx, cancel := context.WithCancel(context.Background())
		run := architecturekit.StartTransactionalProjection(ctx, store, architecturekit.ExactSubject("/test"), target)

		waitForClosed(t, run.CaughtUp(), "CaughtUp")

		assert.Equal(t, []string{"0", "1", "2"}, target.IDs(), "committed when caught up")
		assert.Equal(t, "2", run.Status().Revision)

		cancel()
		waitForClosed(t, run.Done(), "Done")
	})
}
