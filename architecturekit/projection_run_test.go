package architecturekit_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

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

	run := architecturekit.StartProjection(ctx, store, "/test", false, projection)

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
		t.Fatalf("%s was not closed in time", name)
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

func TestStartProjectionTellsWhenItHasCaughtUp(t *testing.T) {
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
	if ids := target.IDs(); !slices.Equal(ids, []string{"0", "1", "2"}) {
		t.Fatalf("got %v when caught up, want 0, 1, and 2", ids)
	}

	status := run.Status()
	if status.Phase != architecturekit.PhaseLive {
		t.Fatalf("got phase %q, want %q", status.Phase, architecturekit.PhaseLive)
	}
	if status.Revision != "2" || status.Err != nil || status.Attempts != 0 {
		t.Fatalf("got %+v", status)
	}
	if status.Since.Before(started) {
		t.Fatalf("the phase began at %v, before the run started at %v", status.Since, started)
	}

	database.add(3)
	waitFor(t, func() bool { return run.Status().Revision == "3" })

	stop(t)

	// The history is read once, not once for catching up and once again for
	// following the stream.
	if ids := target.IDs(); !slices.Equal(ids, []string{"0", "1", "2", "3"}) {
		t.Fatalf("got %v, want every event exactly once", ids)
	}
	if err := run.Err(); err != nil {
		t.Fatalf("ending through the context is not a failure, got %v", err)
	}

	status = run.Status()
	if status.Phase != architecturekit.PhaseStopped || status.Err != nil {
		t.Fatalf("got %+v, want a stopped run without an error", status)
	}
}

func TestProjectionRunReportsADisruption(t *testing.T) {
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
	if status.Phase != architecturekit.PhaseReconnecting {
		t.Fatalf("got phase %q, want %q", status.Phase, architecturekit.PhaseReconnecting)
	}
	if status.Err != nil {
		t.Fatalf("a stream that ended is reported without an error, got %v", status.Err)
	}
	if status.Attempts != 1 || status.Revision != "0" {
		t.Fatalf("got %+v", status)
	}

	// Having caught up once, the run does not take it back.
	if !isClosed(run.CaughtUp()) {
		t.Fatal("CaughtUp must stay closed while the run reconnects")
	}

	stop(t)
}

func TestProjectionRunIsLiveAgainAfterADisruption(t *testing.T) {
	database := &fakeDatabase{
		events:       []int{0},
		endObserving: func(connection int) bool { return connection == 1 },
	}
	observed := &reconnects{}

	run, stop := startInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), &collector{})

	waitFor(t, func() bool {
		return observed.count() == 1 && run.Status().Phase == architecturekit.PhaseLive
	})

	if status := run.Status(); status.Attempts != 0 || status.Err != nil {
		t.Fatalf("got %+v, want a live run without attempts or an error", status)
	}

	stop(t)
}

func TestProjectionRunKeepsTheBeginningOfADisruption(t *testing.T) {
	observed := &reconnects{}

	run, stop := startInBackground(t, reconnectingStore(deadClient(t), observed), &collector{})

	waitFor(t, func() bool { return run.Status().Attempts >= 1 })
	first := run.Status()

	waitFor(t, func() bool { return run.Status().Attempts >= 3 })
	later := run.Status()

	if later.Phase != architecturekit.PhaseReconnecting {
		t.Fatalf("got phase %q, want %q", later.Phase, architecturekit.PhaseReconnecting)
	}
	if !later.Since.Equal(first.Since) {
		t.Fatalf("the disruption began at %v, but the status says %v", first.Since, later.Since)
	}
	if !errors.Is(later.Err, architecturekit.ErrTransient) {
		t.Fatalf("an unreachable database is transient, got %v", later.Err)
	}
	if isClosed(run.CaughtUp()) {
		t.Fatal("a run that never read anything has not caught up")
	}

	stop(t)

	if err := run.Err(); err != nil {
		t.Fatalf("ending through the context is not a failure, got %v", err)
	}
}

func TestProjectionRunStopsOnAFailureThatRetryingWillNotFix(t *testing.T) {
	database := &fakeDatabase{
		events:       []int{0},
		endObserving: func(int) bool { return true },
	}
	store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io")

	run := architecturekit.StartProjection(context.Background(), store, "/test", false, failingCollector{})

	waitForClosed(t, run.Done(), "Done")

	if err := run.Err(); err == nil || !strings.Contains(err.Error(), "the view is broken") {
		t.Fatalf("expected the failure of Apply, got %v", err)
	}

	status := run.Status()
	if status.Phase != architecturekit.PhaseStopped || !errors.Is(status.Err, run.Err()) {
		t.Fatalf("got %+v, want a stopped run with the failure", status)
	}
	if isClosed(run.CaughtUp()) {
		t.Fatal("a run that failed before catching up has not caught up")
	}
}

func TestStartProjectionRefusesAProjectionThatIsTransactionalAsWell(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a transactional projection")
		}
	}()

	architecturekit.StartProjection(context.Background(), nil, "/test", false, &transactionalWithApply{})
}

func TestStartTransactionalProjectionCommitsBeforeItHasCaughtUp(t *testing.T) {
	database := &fakeDatabase{
		events:       []int{0, 1, 2},
		endObserving: func(int) bool { return false },
	}
	store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io")
	target := &transactionalCollector{}

	ctx, cancel := context.WithCancel(context.Background())
	run := architecturekit.StartTransactionalProjection(ctx, store, "/test", false, target)

	waitForClosed(t, run.CaughtUp(), "CaughtUp")

	if ids := target.IDs(); !slices.Equal(ids, []string{"0", "1", "2"}) {
		t.Fatalf("got %v committed when caught up, want 0, 1, and 2", ids)
	}
	if revision := run.Status().Revision; revision != "2" {
		t.Fatalf("got revision %q, want 2", revision)
	}

	cancel()
	waitForClosed(t, run.Done(), "Done")
}

func TestStartProjectionWithADatabase(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	seed(t, subject, 3)

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	target := &collector{}
	run := architecturekit.StartProjection(ctx, store, subject, false, target)

	waitForClosed(t, run.CaughtUp(), "CaughtUp")
	if len(target.IDs()) != 3 {
		t.Fatalf("got %d events when caught up, want 3", len(target.IDs()))
	}

	seed(t, subject, 1)
	waitFor(t, func() bool { return len(target.IDs()) == 4 })

	cancel()
	waitForClosed(t, run.Done(), "Done")
}
