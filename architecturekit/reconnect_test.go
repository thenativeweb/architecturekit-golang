package architecturekit_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

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
			t.Fatal("RunProjection did not return after its context ended")
			return nil
		}
	}
}

func TestRunProjectionReconnectsAfterTheStreamEnds(t *testing.T) {
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

	if err := stop(t); err != nil {
		t.Fatalf("ending through the context is not a failure, got %v", err)
	}

	// Catching up again must not apply the events from before the reconnect
	// a second time, although a rebuilt projection has no checkpoint.
	if ids := target.IDs(); !slices.Equal(ids, []string{"0", "1", "2", "3"}) {
		t.Fatalf("got %v, want every event exactly once", ids)
	}

	errs, _ := observed.recorded()
	if errs[0] != nil {
		t.Fatalf("a stream that ended is reported without an error, got %v", errs[0])
	}
}

func TestRunProjectionDoublesTheDelayUpToTheMaximum(t *testing.T) {
	database := &fakeDatabase{
		endObserving: func(int) bool { return true },
	}
	observed := &reconnects{}

	stop := runInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), &collector{})

	waitFor(t, func() bool { return observed.count() >= 4 })
	if err := stop(t); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, delays := observed.recorded()
	want := []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond, 4 * time.Millisecond}
	if !slices.Equal(delays[:4], want) {
		t.Fatalf("got %v, want %v", delays[:4], want)
	}
}

func TestRunProjectionStartsOverWithTheInitialDelayAfterProgress(t *testing.T) {
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

	if err := stop(t); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, delays := observed.recorded()
	if !slices.Contains(delays[3:], time.Millisecond) {
		t.Fatalf("got %v, want the initial delay again after progress", delays)
	}
}

func TestRunProjectionRetriesAnUnreachableDatabase(t *testing.T) {
	observed := &reconnects{}

	stop := runInBackground(t, reconnectingStore(deadClient(t), observed), &collector{})

	waitFor(t, func() bool { return observed.count() >= 2 })
	if err := stop(t); err != nil {
		t.Fatalf("ending through the context is not a failure, got %v", err)
	}

	errs, _ := observed.recorded()
	if !errors.Is(errs[0], architecturekit.ErrTransient) {
		t.Fatalf("an unreachable database is transient, got %v", errs[0])
	}
}

// failingCollector fails on every event, which retrying will not fix.
type failingCollector struct{}

func (failingCollector) Apply(context.Context, eventsourcingdb.Event) error {
	return errors.New("the view is broken")
}

func TestRunProjectionEndsOnAFailureThatRetryingWillNotFix(t *testing.T) {
	database := &fakeDatabase{
		events:       []int{0},
		endObserving: func(int) bool { return true },
	}
	observed := &reconnects{}
	store := reconnectingStore(newFakeDatabase(t, database), observed)

	err := architecturekit.RunProjection(context.Background(), store, "/test", false, failingCollector{})

	if err == nil || !strings.Contains(err.Error(), "the view is broken") {
		t.Fatalf("expected the failure of Apply, got %v", err)
	}
	if observed.count() != 0 {
		t.Fatalf("a failing Apply must not be retried, got %d attempts", observed.count())
	}
}
