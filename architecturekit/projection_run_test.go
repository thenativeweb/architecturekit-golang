package architecturekit_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
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

// waitingContext ends with the test, and after five seconds at the latest, so
// that a wait that never returns fails the test rather than hang it.
func waitingContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	t.Cleanup(cancel)

	return ctx
}

// endedContext is a context that has ended already, on its deadline, so that
// its error tells it apart from one that was canceled.
func endedContext(t *testing.T) context.Context {
	t.Helper()

	ctx, cancel := context.WithDeadline(context.Background(), time.Now())
	t.Cleanup(cancel)

	return ctx
}

// waitInBackground starts waiting for the run to catch up, and returns a
// function that returns what waiting returned.
func waitInBackground(t *testing.T, ctx context.Context, run *architecturekit.ProjectionRun) func(t *testing.T) error {
	t.Helper()

	waited := make(chan error, 1)
	go func() { waited <- run.WaitCaughtUp(ctx) }()

	return func(t *testing.T) error {
		t.Helper()

		select {
		case err := <-waited:
			return err
		case <-time.After(5 * time.Second):
			require.FailNow(t, "WaitCaughtUp did not return in time")
			return nil
		}
	}
}

// failingOn applies every event but one, on which it fails in a way that
// retrying will not fix.
type failingOn struct {
	collector

	id string
}

func (c *failingOn) Apply(ctx context.Context, event eventsourcingdb.Event) error {
	if event.ID == c.id {
		return errors.New("the view is broken")
	}

	return c.collector.Apply(ctx, event)
}

func TestWaitCaughtUp(t *testing.T) {
	t.Run("returns nil once the run has caught up", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0, 1, 2},
			endObserving: func(int) bool { return false },
		}
		target := &collector{}

		run, stop := startInBackground(t,
			architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io"), target)
		defer stop(t)

		require.NoError(t, run.WaitCaughtUp(waitingContext(t)))

		assert.True(t, isClosed(run.CaughtUp()))
		assert.Equal(t, []string{"0", "1", "2"}, target.IDs(), "everything that was stored at the start has been applied")
	})

	t.Run("returns nil for a run that has caught up, although it has ended since, and so has the context", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0},
			endObserving: func(int) bool { return false },
		}

		run, stop := startInBackground(t,
			architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io"), &collector{})

		waitForClosed(t, run.CaughtUp(), "CaughtUp")
		stop(t)

		// Select picks at random among the cases that are ready, so a single
		// attempt could pass by chance.
		for range 100 {
			require.NoError(t, run.WaitCaughtUp(waitingContext(t)))
			require.NoError(t, run.WaitCaughtUp(endedContext(t)))
		}
	})

	t.Run("returns nil for a run that has caught up, although it has failed since", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0},
			endObserving: func(int) bool { return false },
		}
		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io")

		run := architecturekit.StartProjection(context.Background(), store, architecturekit.ExactSubject("/test"),
			&failingOn{id: "1"})

		waitForClosed(t, run.CaughtUp(), "CaughtUp")
		database.add(1)
		waitForClosed(t, run.Done(), "Done")
		require.Error(t, run.Err())

		for range 100 {
			require.NoError(t, run.WaitCaughtUp(waitingContext(t)))
			require.NoError(t, run.WaitCaughtUp(endedContext(t)))
		}
	})

	t.Run("returns the error of a run that ends before it has caught up", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0},
			endObserving: func(int) bool { return true },
		}
		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io")

		run := architecturekit.StartProjection(context.Background(), store, architecturekit.ExactSubject("/test"), failingCollector{})

		err := run.WaitCaughtUp(waitingContext(t))

		require.Error(t, err)
		assert.ErrorContains(t, err, "the view is broken", "expected the failure of Apply")
		assert.Equal(t, run.Err(), err)
		assert.False(t, isClosed(run.CaughtUp()))
	})

	t.Run("returns the error of a run that has ended, although the context has ended as well", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0},
			endObserving: func(int) bool { return true },
		}
		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io")

		run := architecturekit.StartProjection(context.Background(), store, architecturekit.ExactSubject("/test"), failingCollector{})
		waitForClosed(t, run.Done(), "Done")

		for range 100 {
			err := run.WaitCaughtUp(endedContext(t))

			require.Error(t, err)
			require.ErrorContains(t, err, "the view is broken", "the failure of the run says more than the end of the context")
		}
	})

	t.Run("returns an error that wraps context.Canceled for a run whose context ends before it has caught up", func(t *testing.T) {
		run, stop := startInBackground(t, reconnectingStore(deadClient(t), &reconnects{}), &collector{})

		waited := waitInBackground(t, waitingContext(t), run)
		stop(t)
		err := waited(t)

		require.Error(t, err, "a run that has not caught up must never look like one that has")
		assert.ErrorIs(t, err, context.Canceled)
		assert.EqualError(t, err, "architecturekit: the projection stopped before it caught up: context canceled")
		assert.NoError(t, run.Err(), "ending through the context is not a failure")
		assert.False(t, isClosed(run.CaughtUp()))
	})

	t.Run("names the projection that stopped before it caught up", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		run := architecturekit.StartProjection(ctx, reconnectingStore(deadClient(t), &reconnects{}),
			architecturekit.ExactSubject("/test"), &collector{}, architecturekit.Named("catalog"))

		cancel()
		waitForClosed(t, run.Done(), "Done")

		err := run.WaitCaughtUp(waitingContext(t))

		assert.ErrorIs(t, err, context.Canceled)
		assert.EqualError(t, err, `architecturekit: the projection "catalog" stopped before it caught up: context canceled`)
	})

	t.Run("returns the error of the context if it ends first", func(t *testing.T) {
		run, stop := startInBackground(t, reconnectingStore(deadClient(t), &reconnects{}), &collector{})
		defer stop(t)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		err := waitInBackground(t, ctx, run)(t)

		assert.Equal(t, context.DeadlineExceeded, err)
		assert.False(t, isClosed(run.Done()), "the run goes on, since only waiting for it has ended")
		assert.False(t, isClosed(run.CaughtUp()))
	})

	t.Run("returns the error of the context that has ended already", func(t *testing.T) {
		run, stop := startInBackground(t, reconnectingStore(deadClient(t), &reconnects{}), &collector{})
		defer stop(t)

		assert.Equal(t, context.DeadlineExceeded, waitInBackground(t, endedContext(t), run)(t))
	})

	t.Run("returns the error of the context it shares with the run, as on a timeout while the database can not be reached", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		run := architecturekit.StartProjection(ctx, reconnectingStore(deadClient(t), &reconnects{}),
			architecturekit.ExactSubject("/test"), &collector{})
		waitForClosed(t, run.Done(), "Done")

		// The run has ended without an error, because its context has, and the
		// context of waiting is that same one, so its error says why.
		for range 100 {
			assert.Equal(t, context.DeadlineExceeded, run.WaitCaughtUp(ctx))
		}
	})
}
