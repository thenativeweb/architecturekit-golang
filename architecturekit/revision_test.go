package architecturekit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/architecturekittest"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

func TestRevisionsAreComparedAsNumbers(t *testing.T) {
	tests := []struct {
		left, right string
		want        int
	}{
		{"", "", 0},
		{"0", "0", 0},
		{"7", "7", 0},
		{"", "0", -1},
		{"0", "", 1},
		{"1", "2", -1},
		{"2", "1", 1},
		// The trap this exists for: as text, "10" sorts before "9".
		{"9", "10", -1},
		{"10", "9", 1},
		{"100", "99", 1},
		{"18446744073709551615", "18446744073709551614", 1},
	}

	for _, test := range tests {
		got, err := architecturekit.CompareRevisions(test.left, test.right)
		if err != nil {
			t.Errorf("%q vs %q: %v", test.left, test.right, err)
			continue
		}

		if got != test.want {
			t.Errorf("%q vs %q: got %d, want %d", test.left, test.right, got, test.want)
		}
	}
}

func TestSomethingThatIsNotARevisionIsRefused(t *testing.T) {
	for _, revision := range []string{"abc", "1.5", "-1", " 1", "0x10", "99999999999999999999"} {
		if _, err := architecturekit.CompareRevisions(revision, "1"); !errors.Is(err, architecturekit.ErrNotARevision) {
			t.Errorf("%q: got %v, want %v", revision, err, architecturekit.ErrNotARevision)
		}

		// Both sides are checked, not only the first.
		if _, err := architecturekit.CompareRevisions("1", revision); !errors.Is(err, architecturekit.ErrNotARevision) {
			t.Errorf("%q on the right: got %v", revision, err)
		}
	}
}

func TestAFreshViewHasSeenNothing(t *testing.T) {
	if got := architecturekit.NewItemView[int]().Revision(); got != "" {
		t.Errorf("got %q, want the empty revision", got)
	}
}

func TestSeenMovesTheRevisionForwardOnly(t *testing.T) {
	view := architecturekit.NewItemView[int]()

	view.Seen("5")
	if got := view.Revision(); got != "5" {
		t.Fatalf("got %q, want %q", got, "5")
	}

	// An event that arrives twice, or out of order after a restart, must not
	// pull the revision back.
	view.Seen("3")
	view.Seen("5")
	if got := view.Revision(); got != "5" {
		t.Errorf("got %q, want %q", got, "5")
	}

	view.Seen("12")
	if got := view.Revision(); got != "12" {
		t.Errorf("got %q, want %q", got, "12")
	}
}

func TestSeenIgnoresWhatIsNotARevision(t *testing.T) {
	view := architecturekit.NewItemView[int]()
	view.Seen("5")
	view.Seen("nonsense")

	if got := view.Revision(); got != "5" {
		t.Errorf("got %q, want %q", got, "5")
	}
}

func TestWaitingForARevisionAlreadyReachedReturnsAtOnce(t *testing.T) {
	view := architecturekit.NewItemView[int]()
	view.Seen("10")

	for _, revision := range []string{"", "9", "10"} {
		ctx, cancel := context.WithTimeout(t.Context(), time.Second)

		if err := view.WaitFor(ctx, revision); err != nil {
			t.Errorf("waiting for %q: %v", revision, err)
		}

		cancel()
	}
}

func TestWaitingReturnsWhenTheRevisionArrives(t *testing.T) {
	view := architecturekit.NewItemView[int]()

	waited := make(chan error, 1)
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()

		waited <- view.WaitFor(ctx, "3")
	}()

	// Everything below the wanted revision leaves the waiter waiting.
	view.Seen("1")
	view.Seen("2")

	select {
	case err := <-waited:
		t.Fatalf("returned too early: %v", err)
	case <-time.After(50 * time.Millisecond):
	}

	view.Seen("3")

	select {
	case err := <-waited:
		if err != nil {
			t.Errorf("got %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Error("never returned")
	}
}

func TestWaitingEndsWithTheContext(t *testing.T) {
	view := architecturekit.NewItemView[int]()

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	err := view.WaitFor(ctx, "1")

	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("got %v, want %v", err, context.DeadlineExceeded)
	}
}

func TestWaitingForSomethingThatIsNotARevisionFails(t *testing.T) {
	view := architecturekit.NewItemView[int]()

	if err := view.WaitFor(t.Context(), "soon"); !errors.Is(err, architecturekit.ErrNotARevision) {
		t.Errorf("got %v, want %v", err, architecturekit.ErrNotARevision)
	}
}

func TestManyWaitersAreAllWokenUp(t *testing.T) {
	view := architecturekit.NewItemView[int]()

	var group sync.WaitGroup
	failures := make(chan error, 10)

	for range 10 {
		group.Add(1)

		go func() {
			defer group.Done()

			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			if err := view.WaitFor(ctx, "4"); err != nil {
				failures <- err
			}
		}()
	}

	// Several steps, so that the waiters go around their loop more than once.
	for _, id := range []string{"1", "2", "3", "4"} {
		view.Seen(id)
	}

	group.Wait()
	close(failures)

	for err := range failures {
		t.Errorf("a waiter failed: %v", err)
	}
}

// --- Tracking ---

func TestTrackingRecordsEveryEvent(t *testing.T) {
	view := architecturekit.NewItemView[int]()

	applied := 0
	projection := architecturekit.Tracking(view, architecturekit.ProjectionFunc(
		func(context.Context, eventsourcingdb.Event) error {
			applied++
			return nil
		},
	))

	architecturekittest.Project(t, projection,
		architecturekittest.StoredEvent("/counter/a", "0", incremented{By: 1}),
		architecturekittest.StoredEvent("/counter/a", "1", incremented{By: 1}),
	)

	if applied != 2 {
		t.Errorf("applied %d event(s), want 2", applied)
	}

	if got := view.Revision(); got != "1" {
		t.Errorf("got revision %q, want %q", got, "1")
	}
}

func TestTrackingAlsoRecordsWhatTheProjectionIgnores(t *testing.T) {
	// This is the whole reason Tracking exists: a projection skips what does
	// not concern it, but a reader may be waiting for exactly that event.
	view := architecturekit.NewItemView[int]()

	projection := architecturekit.Tracking(view, architecturekit.ProjectionFunc(
		func(context.Context, eventsourcingdb.Event) error { return nil },
	))

	architecturekittest.Project(t, projection,
		architecturekittest.StoredEvent("/somewhere/else", "42", incremented{By: 1}),
	)

	if got := view.Revision(); got != "42" {
		t.Errorf("got %q, want %q", got, "42")
	}
}

func TestTrackingDoesNotRecordAFailedEvent(t *testing.T) {
	view := architecturekit.NewItemView[int]()
	failed := errors.New("could not apply")

	projection := architecturekit.Tracking(view, architecturekit.ProjectionFunc(
		func(context.Context, eventsourcingdb.Event) error { return failed },
	))

	err := projection.Apply(t.Context(),
		architecturekittest.StoredEvent("/counter/a", "7", incremented{By: 1}))

	if !errors.Is(err, failed) {
		t.Errorf("got %v, want %v", err, failed)
	}

	if got := view.Revision(); got != "" {
		t.Errorf("recorded %q although applying failed", got)
	}
}

func TestTrackingKeepsAProjectionThatIsRebuiltAsItIs(t *testing.T) {
	projection := architecturekit.Tracking(architecturekit.NewItemView[int](), &collector{})

	architecturekittest.ExpectMode(t, projection, architecturekit.ModeRebuild)

	batched, ok := projection.(architecturekit.Batched)
	if !ok {
		t.Fatal("a tracked projection passes on its batch sizes")
	}
	if catchUp, live := batched.BatchSizes(); catchUp != 1 || live != 1 {
		t.Errorf("got %d and %d, want the defaults 1 and 1", catchUp, live)
	}
}

func TestTrackingKeepsAResumableProjectionResumable(t *testing.T) {
	// A wrapper that dropped the checkpoint would silently turn this into a
	// projection that is rebuilt on every start.
	target := &batchedResumingCollector{resumingCollector: resumingCollector{checkpoint: "7"}}
	projection := architecturekit.Tracking(architecturekit.NewItemView[int](), target)

	architecturekittest.ExpectMode(t, projection, architecturekit.ModeResumable)

	resumable := projection.(architecturekit.Resumable)

	checkpoint, err := resumable.Checkpoint(t.Context())
	if err != nil || checkpoint != "7" {
		t.Fatalf("got %q, %v, want the checkpoint of the wrapped projection", checkpoint, err)
	}

	if err := resumable.SaveCheckpoint(t.Context(), "8"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target.checkpoint != "8" {
		t.Errorf("got %q, the checkpoint has to reach the wrapped projection", target.checkpoint)
	}

	if catchUp, live := projection.(architecturekit.Batched).BatchSizes(); catchUp != 500 || live != 10 {
		t.Errorf("got %d and %d, want the batch sizes of the wrapped projection", catchUp, live)
	}
}

func TestTrackingRefusesAProjectionThatIsTransactionalAsWell(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic, because tracking would bypass the transactions")
		}
	}()

	architecturekit.Tracking(architecturekit.NewItemView[int](), &transactionalWithApply{})
}

func TestTrackingResumesFromTheCheckpoint(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	seed(t, subject, 3)

	view := architecturekit.NewItemView[int]()
	target := &resumingCollector{}

	if err := architecturekit.CatchUpProjection(t.Context(), store, subject, false,
		architecturekit.Tracking(view, target)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if target.checkpoint == "" {
		t.Fatal("a tracked resumable projection has to save its checkpoint")
	}
	if got := view.Revision(); got != target.checkpoint {
		t.Errorf("got revision %q, want %q", got, target.checkpoint)
	}
}

// batchedResumingCollector is resumable and announces its own batch sizes.
type batchedResumingCollector struct {
	resumingCollector
}

func (c *batchedResumingCollector) BatchSizes() (int, int) { return 500, 10 }

// --- RevisionOf ---

func TestTheRevisionOfAWriteIsItsHighestEventID(t *testing.T) {
	tests := []struct {
		name   string
		events []eventsourcingdb.Event
		want   string
	}{
		{"nothing written", nil, ""},
		{"one event", []eventsourcingdb.Event{{ID: "7"}}, "7"},
		{"two events", []eventsourcingdb.Event{{ID: "7"}, {ID: "8"}}, "8"},
		// Order is not assumed, and numbers decide, not text.
		{"out of order", []eventsourcingdb.Event{{ID: "10"}, {ID: "9"}}, "10"},
		{"nonsense is skipped", []eventsourcingdb.Event{{ID: "x"}, {ID: "3"}}, "3"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := architecturekit.RevisionOf(test.events); got != test.want {
				t.Errorf("got %q, want %q", got, test.want)
			}
		})
	}
}
