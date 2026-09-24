package architecturekit

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// These tests reach into the package, because the batching logic of drive is
// the part that is hard to hit from the outside and easy to get wrong.

// journal writes down what happened, and refuses the event it was told to.
type journal struct {
	log    []string
	failOn string
}

func (j *journal) apply(event eventsourcingdb.Event) error {
	if event.ID == j.failOn {
		return fmt.Errorf("%w: refusing %s", ErrPermanent, event.ID)
	}
	j.log = append(j.log, "apply "+event.ID)
	return nil
}

// recorder stands in for a projection target.
type recorder struct{ journal }

func (r *recorder) Apply(_ context.Context, event eventsourcingdb.Event) error {
	return r.apply(event)
}

// resumableRecorder keeps a checkpoint, but not together with the data.
type resumableRecorder struct {
	recorder
	saved string
	start string
}

func (r *resumableRecorder) Checkpoint(context.Context) (string, error) { return r.start, nil }

func (r *resumableRecorder) SaveCheckpoint(_ context.Context, eventID string) error {
	r.saved = eventID
	r.log = append(r.log, "checkpoint "+eventID)
	return nil
}

// transactionalRecorder keeps both in step. It has no Apply of its own,
// because a transactional projection applies events only within a
// transaction.
type transactionalRecorder struct {
	journal
	start       string
	committed   []string
	beginErr    error
	rollbackErr error
}

func (r *transactionalRecorder) Checkpoint(context.Context) (string, error) { return r.start, nil }

func (r *transactionalRecorder) Begin(context.Context) (Tx, error) {
	if r.beginErr != nil {
		return nil, r.beginErr
	}
	r.log = append(r.log, "begin")
	return &recordingTx{owner: r}, nil
}

type recordingTx struct{ owner *transactionalRecorder }

func (t *recordingTx) Apply(_ context.Context, event eventsourcingdb.Event) error {
	return t.owner.apply(event)
}

func (t *recordingTx) Commit(_ context.Context, lastEventID string) error {
	t.owner.committed = append(t.owner.committed, lastEventID)
	t.owner.log = append(t.owner.log, "commit "+lastEventID)
	return nil
}

func (t *recordingTx) Rollback(context.Context) error {
	t.owner.log = append(t.owner.log, "rollback")
	return t.owner.rollbackErr
}

// batchedRecorder announces its own batch sizes.
type batchedRecorder struct {
	resumableRecorder
	catchUp, live int
}

func (r *batchedRecorder) BatchSizes() (int, int) { return r.catchUp, r.live }

// inTransactions is the writer RunTransactionalProjection uses.
func inTransactions(projection Transactional) projectionWriter {
	return &transactionalWriter{projection: projection}
}

func events(ids ...string) iter.Seq2[eventsourcingdb.Event, error] {
	return func(yield func(eventsourcingdb.Event, error) bool) {
		for _, id := range ids {
			if !yield(eventsourcingdb.Event{ID: id, Type: "test.happened"}, nil) {
				return
			}
		}
	}
}

func failingEvents(after int, err error) iter.Seq2[eventsourcingdb.Event, error] {
	return func(yield func(eventsourcingdb.Event, error) bool) {
		for i := range after {
			if !yield(eventsourcingdb.Event{ID: fmt.Sprint(i), Type: "test.happened"}, nil) {
				return
			}
		}
		yield(eventsourcingdb.Event{}, err)
	}
}

func TestModeOfDerivesFromTheInterfaces(t *testing.T) {
	cases := []struct {
		projection Projection
		want       Mode
	}{
		{&recorder{}, ModeRebuild},
		{&resumableRecorder{}, ModeResumable},
		{&batchedRecorder{}, ModeResumable},
	}

	for _, c := range cases {
		if got := ModeOf(c.projection); got != c.want {
			t.Fatalf("%T: got %q, want %q", c.projection, got, c.want)
		}
	}
}

func TestBatchSizesDefaultToOneAndAreClamped(t *testing.T) {
	if catchUp, live := batchSizesOf(&recorder{}); catchUp != 1 || live != 1 {
		t.Fatalf("got %d and %d, want 1 and 1", catchUp, live)
	}

	// A projection that announces nonsense is corrected rather than trusted.
	if catchUp, live := batchSizesOf(&batchedRecorder{catchUp: 0, live: -5}); catchUp != 1 || live != 1 {
		t.Fatalf("got %d and %d, want 1 and 1", catchUp, live)
	}

	if catchUp, live := batchSizesOf(&batchedRecorder{catchUp: 500, live: 10}); catchUp != 500 || live != 10 {
		t.Fatalf("got %d and %d, want 500 and 10", catchUp, live)
	}
}

func TestDriveInRebuildModeKeepsNoCheckpoint(t *testing.T) {
	target := &recorder{}

	last, err := drive(context.Background(), writerFor(target), events("0", "1", "2"), 1)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if last != "2" {
		t.Fatalf("got %q, want 2", last)
	}

	want := []string{"apply 0", "apply 1", "apply 2"}
	assertLog(t, target.log, want)
}

func TestDriveInResumableModeWritesCheckpointAfterEveryEvent(t *testing.T) {
	target := &resumableRecorder{}

	if _, err := drive(context.Background(), writerFor(target), events("0", "1"), 1); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertLog(t, target.log, []string{"apply 0", "checkpoint 0", "apply 1", "checkpoint 1"})
	if target.saved != "1" {
		t.Fatalf("got %q, want 1", target.saved)
	}
}

func TestDriveHonoursTheBatchSize(t *testing.T) {
	target := &resumableRecorder{}

	if _, err := drive(context.Background(), writerFor(target), events("0", "1", "2", "3"), 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertLog(t, target.log, []string{
		"apply 0", "apply 1", "checkpoint 1",
		"apply 2", "apply 3", "checkpoint 3",
	})
}

func TestDriveCommitsAnIncompleteFinalBatch(t *testing.T) {
	target := &resumableRecorder{}

	if _, err := drive(context.Background(), writerFor(target), events("0", "1", "2"), 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertLog(t, target.log, []string{
		"apply 0", "apply 1", "checkpoint 1",
		"apply 2", "checkpoint 2",
	})
}

func TestDriveInTransactionalModeBracketsEachBatch(t *testing.T) {
	target := &transactionalRecorder{}

	if _, err := drive(context.Background(), inTransactions(target), events("0", "1", "2"), 2); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	assertLog(t, target.log, []string{
		"begin", "apply 0", "apply 1", "commit 1",
		"begin", "apply 2", "commit 2",
	})
	if len(target.committed) != 2 {
		t.Fatalf("got %v", target.committed)
	}
}

func TestDriveRollsBackWhenApplyFails(t *testing.T) {
	target := &transactionalRecorder{}
	target.failOn = "1"

	_, err := drive(context.Background(), inTransactions(target), events("0", "1", "2"), 10)

	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("got %v", err)
	}
	assertLog(t, target.log, []string{"begin", "apply 0", "rollback"})
}

func TestDriveRollsBackWhenTheStreamFails(t *testing.T) {
	target := &transactionalRecorder{}

	_, err := drive(context.Background(), inTransactions(target),
		failingEvents(2, errors.New("connection lost")), 10)

	if !errors.Is(err, ErrTransient) {
		t.Fatalf("a broken stream is transient, got %v", err)
	}
	assertLog(t, target.log, []string{"begin", "apply 0", "apply 1", "rollback"})
}

func TestDriveReportsAFailingBegin(t *testing.T) {
	target := &transactionalRecorder{}
	target.beginErr = errors.New("no connection")

	if _, err := drive(context.Background(), inTransactions(target), events("0"), 1); err == nil {
		t.Fatal("expected the error from begin")
	}
}

func TestDriveReportsACancelledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := drive(ctx, writerFor(&recorder{}), events(), 1)

	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v", err)
	}
}

func TestBoundAfterExcludesTheCheckpoint(t *testing.T) {
	if bound := boundAfter(""); bound != nil {
		t.Fatalf("an empty checkpoint means no bound, got %v", bound)
	}

	bound := boundAfter("7")
	if bound == nil || bound.ID != "7" || bound.Type != eventsourcingdb.BoundTypeExclusive {
		t.Fatalf("got %v", bound)
	}
}

func assertLog(t *testing.T, got, want []string) {
	t.Helper()

	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("step %d: got %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestWritersWithoutTransactionsAreHarmless(t *testing.T) {
	ctx := context.Background()

	// A rebuild writer keeps no checkpoint and has nothing to roll back.
	rebuild := writerFor(&recorder{})
	if checkpoint, err := rebuild.checkpoint(ctx); checkpoint != "" || err != nil {
		t.Fatalf("got %q, %v", checkpoint, err)
	}
	if err := rebuild.rollback(ctx); err != nil {
		t.Fatalf("nothing was begun, so nothing can fail: %v", err)
	}

	// A resumable writer reports where it stopped, but also cannot roll back.
	resumable := writerFor(&resumableRecorder{start: "7"})
	if checkpoint, err := resumable.checkpoint(ctx); checkpoint != "7" || err != nil {
		t.Fatalf("got %q, %v", checkpoint, err)
	}
	if err := resumable.rollback(ctx); err != nil {
		t.Fatalf("the data is already written, so nothing can fail: %v", err)
	}

	transactional := inTransactions(&transactionalRecorder{start: "9"})
	if checkpoint, err := transactional.checkpoint(ctx); checkpoint != "9" || err != nil {
		t.Fatalf("got %q, %v", checkpoint, err)
	}
	// Rolling back without an open transaction does nothing.
	if err := transactional.rollback(ctx); err != nil {
		t.Fatalf("got %v", err)
	}
}

func TestIgnoreContextEndKeepsRealFailures(t *testing.T) {
	if err := ignoreContextEnd(nil); err != nil {
		t.Fatalf("got %v", err)
	}
	if err := ignoreContextEnd(context.Canceled); err != nil {
		t.Fatalf("a cancelled run is not a failure, got %v", err)
	}
	if err := ignoreContextEnd(context.DeadlineExceeded); err != nil {
		t.Fatalf("a deadline is not a failure, got %v", err)
	}

	real := errors.New("disk on fire")
	if err := ignoreContextEnd(real); !errors.Is(err, real) {
		t.Fatalf("a real failure has to survive, got %v", err)
	}
}

func TestDriveStopsWhenCommitFails(t *testing.T) {
	target := &failingCommitRecorder{}

	_, err := drive(context.Background(), writerFor(target), events("0", "1"), 1)

	if err == nil {
		t.Fatal("expected the error from commit")
	}
}

// failingCommitRecorder refuses to save its checkpoint.
type failingCommitRecorder struct{ recorder }

func (r *failingCommitRecorder) Checkpoint(context.Context) (string, error) { return "", nil }

func (r *failingCommitRecorder) SaveCheckpoint(context.Context, string) error {
	return errors.New("checkpoint storage is full")
}

func TestCheckpointFailureStopsTheRun(t *testing.T) {
	// catchUp asks for the checkpoint first; if that fails, nothing is read.
	writer := writerFor(&brokenCheckpointRecorder{})

	if _, err := writer.checkpoint(context.Background()); err == nil {
		t.Fatal("expected the error from checkpoint")
	}
}

type brokenCheckpointRecorder struct{ recorder }

func (r *brokenCheckpointRecorder) Checkpoint(context.Context) (string, error) {
	return "", errors.New("cannot read the checkpoint")
}

func (r *brokenCheckpointRecorder) SaveCheckpoint(context.Context, string) error { return nil }

func TestItemViewInsertsAndHandsOutItems(t *testing.T) {
	view := NewItemView[string]()

	items, err := view.All(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(slices.Collect(items)) != 0 {
		t.Fatal("a fresh view is empty")
	}

	view.Insert("alpha")
	view.Insert("bravo")

	items, err = view.All(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := slices.Collect(items); len(got) != 2 || got[0] != "alpha" || got[1] != "bravo" {
		t.Fatalf("got %v", got)
	}
}

func TestItemViewUpdatesMatchingItems(t *testing.T) {
	view := NewItemView[int]()
	for _, value := range []int{1, 2, 3, 4} {
		view.Insert(value)
	}

	changed := view.Update(func(item int) bool { return item%2 == 0 },
		func(item *int) { *item *= 10 })

	if changed != 2 {
		t.Fatalf("got %d, want 2", changed)
	}

	items, _ := view.All(context.Background())
	if got := slices.Collect(items); !slices.Equal(got, []int{1, 20, 3, 40}) {
		t.Fatalf("got %v", got)
	}
}

func TestItemViewUpsertsWhenNothingMatches(t *testing.T) {
	view := NewItemView[string]()

	// Nothing matches yet, so the item is inserted and no change is reported.
	if changed := view.Upsert(func(item string) bool { return item == "alpha" },
		func(item *string) { *item = "changed" }, "alpha"); changed != 0 {
		t.Fatalf("got %d, want 0", changed)
	}

	// Now it matches, so it is changed instead of inserted again.
	if changed := view.Upsert(func(item string) bool { return item == "alpha" },
		func(item *string) { *item = "changed" }, "alpha"); changed != 1 {
		t.Fatalf("got %d, want 1", changed)
	}

	items, _ := view.All(context.Background())
	if got := slices.Collect(items); !slices.Equal(got, []string{"changed"}) {
		t.Fatalf("got %v", got)
	}
}

func TestItemViewDeletesMatchingItems(t *testing.T) {
	view := NewItemView[int]()
	for _, value := range []int{1, 2, 3, 4, 5} {
		view.Insert(value)
	}

	if removed := view.Delete(func(item int) bool { return item > 3 }); removed != 2 {
		t.Fatalf("got %d, want 2", removed)
	}

	items, _ := view.All(context.Background())
	if got := slices.Collect(items); !slices.Equal(got, []int{1, 2, 3}) {
		t.Fatalf("got %v", got)
	}
}

func TestItemViewHandsOutACopy(t *testing.T) {
	view := NewItemView[int]()
	view.Insert(1)

	items, _ := view.All(context.Background())
	collected := slices.Collect(items)

	// Writing while a query holds its result must not change what it holds.
	view.Insert(2)

	if len(collected) != 1 {
		t.Fatalf("a query result must not change underneath: %v", collected)
	}
}

func TestProjectionFuncSatisfiesProjection(t *testing.T) {
	seen := 0
	projection := ProjectionFunc(func(context.Context, eventsourcingdb.Event) error {
		seen++
		return nil
	})

	if err := projection.Apply(context.Background(), eventsourcingdb.Event{ID: "0"}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if seen != 1 {
		t.Fatalf("got %d, want 1", seen)
	}
	if ModeOf(projection) != ModeRebuild {
		t.Fatalf("a plain function is rebuilt, got %q", ModeOf(projection))
	}
}

func TestProjectionFuncReportsItsFailure(t *testing.T) {
	projection := ProjectionFunc(func(context.Context, eventsourcingdb.Event) error {
		return errors.New("cannot apply this")
	})

	if err := projection.Apply(context.Background(), eventsourcingdb.Event{}); err == nil {
		t.Fatal("expected the error from the function")
	}
}

func TestDriveStopsWhenTheFinalCommitFails(t *testing.T) {
	target := &failingCommitRecorder{}

	// The batch is larger than the stream, so the only commit is the one after
	// the loop.
	_, err := drive(context.Background(), writerFor(target), events("0", "1"), 10)

	if err == nil {
		t.Fatal("expected the error from the final commit")
	}
}

func TestDriveReportsAFailingRollbackAlongsideTheCause(t *testing.T) {
	target := &transactionalRecorder{}
	target.failOn = "1"
	target.rollbackErr = errors.New("rollback did not work either")

	_, err := drive(context.Background(), inTransactions(target), events("0", "1"), 10)

	// The failure that caused the rollback has to survive, and the rollback
	// failure comes with it rather than replacing it.
	if !errors.Is(err, ErrPermanent) {
		t.Fatalf("the cause must survive, got %v", err)
	}
	if !strings.Contains(err.Error(), "rollback did not work either") {
		t.Fatalf("the rollback failure has to be reported too, got %q", err.Error())
	}
}

func TestDriveReportsAFailingRollbackAfterABrokenStream(t *testing.T) {
	target := &transactionalRecorder{}
	target.rollbackErr = errors.New("rollback did not work either")

	_, err := drive(context.Background(), inTransactions(target),
		failingEvents(1, errors.New("connection lost")), 10)

	if !errors.Is(err, ErrTransient) {
		t.Fatalf("the cause must survive, got %v", err)
	}
	if !strings.Contains(err.Error(), "rollback did not work either") {
		t.Fatalf("got %q", err.Error())
	}
}
