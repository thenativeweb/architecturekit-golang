package architecturekit_test

import (
	"context"
	"errors"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// errAnchorsBroken is what a projection fails with that can not apply any
// event.
var errAnchorsBroken = errors.New("the anchors are broken")

// failingWith fails on every event with the given error.
type failingWith struct {
	err error
}

func (p failingWith) Apply(context.Context, eventsourcingdb.Event) error {
	return p.err
}

// blockingUntil applies every event only once released, so that a run of it
// catches up only then.
type blockingUntil struct {
	collector

	released <-chan struct{}
}

func (p *blockingUntil) Apply(ctx context.Context, event eventsourcingdb.Event) error {
	select {
	case <-p.released:
	case <-ctx.Done():
		return ctx.Err()
	}

	return p.collector.Apply(ctx, event)
}

// storedEvents is a store on a database that holds one event, and keeps an
// observed stream open, so that a run of a projection that applies it catches
// up, and a run of one that fails on it ends before it catches up.
func storedEvents(t *testing.T) *architecturekit.Store {
	t.Helper()

	return architecturekit.NewStore(newFakeDatabase(t, &fakeDatabase{
		events:       []int{0},
		endObserving: func(int) bool { return false },
	}), "https://thenativeweb.io")
}

// runOf starts a run of the projection, which ends with the test at the
// latest, and returns it together with the function that ends it.
func runOf(
	t *testing.T,
	store *architecturekit.Store,
	projection architecturekit.Projection,
	options ...architecturekit.ProjectionOption,
) (*architecturekit.ProjectionRun, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	run := architecturekit.StartProjection(ctx, store, architecturekit.ExactSubject("/test"), projection, options...)
	t.Cleanup(func() {
		cancel()
		<-run.Done()
	})

	return run, cancel
}

// caughtUpRun returns a run that has caught up.
func caughtUpRun(t *testing.T, options ...architecturekit.ProjectionOption) *architecturekit.ProjectionRun {
	t.Helper()

	run, _ := runOf(t, storedEvents(t), &collector{}, options...)
	waitForClosed(t, run.CaughtUp(), "CaughtUp")

	return run
}

// behindRun returns a run that has not caught up, since it can not reach the
// database, and goes on trying.
func behindRun(t *testing.T, options ...architecturekit.ProjectionOption) *architecturekit.ProjectionRun {
	t.Helper()

	run, _ := runOf(t, reconnectingStore(deadClient(t), &reconnects{}), &collector{}, options...)

	return run
}

// failedRun returns a run that has ended with errAnchorsBroken before it
// caught up.
func failedRun(t *testing.T, options ...architecturekit.ProjectionOption) *architecturekit.ProjectionRun {
	t.Helper()

	run, _ := runOf(t, storedEvents(t), failingWith{err: errAnchorsBroken}, options...)
	waitForClosed(t, run.Done(), "Done")

	return run
}

// stoppedRun returns a run whose context ended before it caught up.
func stoppedRun(t *testing.T, options ...architecturekit.ProjectionOption) *architecturekit.ProjectionRun {
	t.Helper()

	run, stop := runOf(t, reconnectingStore(deadClient(t), &reconnects{}), &collector{}, options...)
	stop()
	waitForClosed(t, run.Done(), "Done")

	return run
}

// waitAllInBackground starts waiting for the runs to catch up, and returns a
// channel that receives what waiting returned.
func waitAllInBackground(ctx context.Context, runs ...*architecturekit.ProjectionRun) <-chan error {
	waited := make(chan error, 1)
	go func() { waited <- architecturekit.WaitCaughtUp(ctx, runs...) }()

	return waited
}

// receive returns what waiting returned, and fails the test if it does not
// return in time.
func receive(t *testing.T, waited <-chan error) error {
	t.Helper()

	select {
	case err := <-waited:
		return err
	case <-time.After(5 * time.Second):
		require.FailNow(t, "WaitCaughtUp did not return in time")
		return nil
	}
}

func TestWaitCaughtUpForSeveralRuns(t *testing.T) {
	t.Run("returns nil once every run has caught up", func(t *testing.T) {
		first := caughtUpRun(t, architecturekit.Named("catalog"))

		released := make(chan struct{})
		second, _ := runOf(t, storedEvents(t), &blockingUntil{released: released}, architecturekit.Named("readers"))

		// The context does not end before the test does, so only the runs can
		// end the waiting.
		waited := waitAllInBackground(t.Context(), first, second)

		select {
		case err := <-waited:
			require.FailNow(t, "WaitCaughtUp returned before every run had caught up", "returned %v", err)
		case <-time.After(50 * time.Millisecond):
		}

		close(released)

		require.NoError(t, receive(t, waited))
		assert.True(t, isClosed(first.CaughtUp()))
		assert.True(t, isClosed(second.CaughtUp()))
	})

	t.Run("returns nil without any run at once, also once the context has ended", func(t *testing.T) {
		assert.NoError(t, architecturekit.WaitCaughtUp(waitingContext(t)))
		assert.NoError(t, architecturekit.WaitCaughtUp(endedContext(t)))
	})

	t.Run("waits for every run at once, and returns as soon as one of them ends before it has caught up", func(t *testing.T) {
		// The first run never catches up, so waiting for the runs one after the
		// other would wait until the context ends, rather than notice the second
		// one.
		behind := behindRun(t)
		store := storedEvents(t)

		released := make(chan struct{})
		failing, _ := runOf(t, store, &failingAfter{released: released, err: errAnchorsBroken}, architecturekit.Named("anchors"))

		waited := waitAllInBackground(t.Context(), behind, failing)
		close(released)

		err := receive(t, waited)

		require.ErrorIs(t, err, errAnchorsBroken)
		assert.EqualError(t, err, `projection "anchors": the anchors are broken`)
		assert.False(t, isClosed(behind.Done()), "only waiting has ended, not the other run")
	})

	t.Run("names a run that ends with an error, wrapping its error", func(t *testing.T) {
		run := failedRun(t, architecturekit.Named("anchors"))

		err := architecturekit.WaitCaughtUp(waitingContext(t), caughtUpRun(t), run)

		require.ErrorIs(t, err, errAnchorsBroken, "errors.Is has to find what the error of the run wraps")
		assert.ErrorIs(t, err, run.Err())
		assert.EqualError(t, err, `projection "anchors": the anchors are broken`)
	})

	t.Run("leaves the error of a run without a name as it is", func(t *testing.T) {
		run := failedRun(t)

		assert.Equal(t, run.Err(), architecturekit.WaitCaughtUp(waitingContext(t), caughtUpRun(t), run))
	})

	t.Run("names a run that stopped before it caught up only once", func(t *testing.T) {
		run := stoppedRun(t, architecturekit.Named("readers"))

		err := architecturekit.WaitCaughtUp(waitingContext(t), caughtUpRun(t), run)

		assert.ErrorIs(t, err, context.Canceled)
		assert.EqualError(t, err, `architecturekit: the projection "readers" stopped before it caught up: context canceled`)
	})

	t.Run("returns the error of the first run among the given ones that ended with one", func(t *testing.T) {
		catalog := failedRun(t, architecturekit.Named("catalog"))
		anchors := failedRun(t, architecturekit.Named("anchors"))

		for range 100 {
			assert.EqualError(t, architecturekit.WaitCaughtUp(waitingContext(t), catalog, anchors),
				`projection "catalog": the anchors are broken`)
			assert.EqualError(t, architecturekit.WaitCaughtUp(waitingContext(t), anchors, catalog),
				`projection "anchors": the anchors are broken`)
		}
	})

	t.Run("prefers the error of a run to the end of the context and to a run that stopped", func(t *testing.T) {
		stopped := stoppedRun(t, architecturekit.Named("readers"))
		failed := failedRun(t, architecturekit.Named("anchors"))

		for range 100 {
			err := architecturekit.WaitCaughtUp(endedContext(t), stopped, failed)

			require.ErrorIs(t, err, errAnchorsBroken, "the failure of a run says more than the end of the context")
		}
	})

	t.Run("prefers the end of the context to a run that stopped", func(t *testing.T) {
		stopped := stoppedRun(t, architecturekit.Named("readers"))

		for range 100 {
			assert.Equal(t, context.DeadlineExceeded, architecturekit.WaitCaughtUp(endedContext(t), caughtUpRun(t), stopped))
		}
	})

	t.Run("names the first of the runs that stopped before they caught up", func(t *testing.T) {
		catalog := stoppedRun(t, architecturekit.Named("catalog"))
		readers := stoppedRun(t, architecturekit.Named("readers"))

		assert.EqualError(t, architecturekit.WaitCaughtUp(waitingContext(t), catalog, readers),
			`architecturekit: the projection "catalog" stopped before it caught up: context canceled`)
		assert.EqualError(t, architecturekit.WaitCaughtUp(waitingContext(t), readers, catalog),
			`architecturekit: the projection "readers" stopped before it caught up: context canceled`)
	})

	t.Run("returns nil for runs that have caught up, although they have ended since, and so has the context", func(t *testing.T) {
		first, stopFirst := runOf(t, storedEvents(t), &collector{})
		second, stopSecond := runOf(t, storedEvents(t), &collector{})
		waitForClosed(t, first.CaughtUp(), "CaughtUp")
		waitForClosed(t, second.CaughtUp(), "CaughtUp")
		stopFirst()
		stopSecond()
		waitForClosed(t, first.Done(), "Done")
		waitForClosed(t, second.Done(), "Done")

		for range 100 {
			require.NoError(t, architecturekit.WaitCaughtUp(endedContext(t), first, second))
		}
	})

	t.Run("returns the error of the context if it ends first", func(t *testing.T) {
		first, second := behindRun(t), behindRun(t)

		ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
		defer cancel()

		err := receive(t, waitAllInBackground(ctx, caughtUpRun(t), first, second))

		assert.Equal(t, context.DeadlineExceeded, err)
		assert.False(t, isClosed(first.Done()), "the runs go on, since only waiting for them has ended")
		assert.False(t, isClosed(second.Done()), "the runs go on, since only waiting for them has ended")
	})

	t.Run("leaves nothing behind once it returns", func(t *testing.T) {
		// The runs wait an hour before they read again, so that they start no
		// goroutines of their own while the test counts them.
		store := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io",
			architecturekit.WithReconnectDelays(time.Hour, time.Hour))

		var runs []*architecturekit.ProjectionRun
		for range 3 {
			run, _ := runOf(t, store, &collector{})
			require.Eventually(t, func() bool { return run.Status().Attempts == 1 }, 5*time.Second, time.Millisecond)
			runs = append(runs, run)
		}

		before := runtime.NumGoroutine()

		for range 50 {
			assert.Equal(t, context.DeadlineExceeded, architecturekit.WaitCaughtUp(endedContext(t), runs...))
		}

		// Eventually would count the goroutine it checks the condition in, so
		// this looks again by itself, until the goroutines have ended.
		for deadline := time.Now().Add(5 * time.Second); runtime.NumGoroutine() > before && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}

		assert.LessOrEqual(t, runtime.NumGoroutine(), before, "waiting must not leave goroutines behind")
	})

	t.Run("leaves nothing behind once it returns, also for runs that have caught up", func(t *testing.T) {
		// Runs that have caught up already let it answer before it hears from
		// them, so what it started to hear from them must not go on waiting to
		// tell it.
		runs := []*architecturekit.ProjectionRun{caughtUpRun(t), caughtUpRun(t), caughtUpRun(t)}
		before := listeningToRuns()

		for range 50 {
			require.NoError(t, architecturekit.WaitCaughtUp(context.Background(), runs...))
		}

		// Eventually would count the goroutine it checks the condition in, so
		// this looks again by itself, until the goroutines have ended.
		for deadline := time.Now().Add(5 * time.Second); listeningToRuns() > before && time.Now().Before(deadline); {
			time.Sleep(5 * time.Millisecond)
		}

		assert.LessOrEqual(t, listeningToRuns(), before, "waiting must not leave goroutines behind")
	})

	t.Run("panics for a nil run, naming it, before it waits", func(t *testing.T) {
		// The other run never catches up, so waiting would not return.
		behind := behindRun(t)

		assert.PanicsWithValue(t, "architecturekit: projection 1 has no run", func() {
			_ = architecturekit.WaitCaughtUp(waitingContext(t), behind, nil, behind)
		})
		assert.PanicsWithValue(t, "architecturekit: projection 0 has no run", func() {
			_ = architecturekit.WaitCaughtUp(endedContext(t), (*architecturekit.ProjectionRun)(nil))
		})
	})
}

// failingAfter fails on every event with the given error, but only once
// released, so that a run of it ends only then.
type failingAfter struct {
	released <-chan struct{}
	err      error
}

func (p *failingAfter) Apply(ctx context.Context, _ eventsourcingdb.Event) error {
	select {
	case <-p.released:
	case <-ctx.Done():
		return ctx.Err()
	}

	return p.err
}

// listeningToRuns counts the goroutines that WaitCaughtUp has started to hear
// from the runs. Runs that have caught up follow the stream with goroutines of
// their own, so it tells them apart by their stacks, rather than counting all
// goroutines.
func listeningToRuns() int {
	stacks := make([]byte, 1<<20)
	for {
		length := runtime.Stack(stacks, true)
		if length < len(stacks) {
			return strings.Count(string(stacks[:length]), "architecturekit.WaitCaughtUp.func")
		}

		stacks = make([]byte, 2*len(stacks))
	}
}
