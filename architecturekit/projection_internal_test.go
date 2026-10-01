package architecturekit

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

func TestModeOf(t *testing.T) {
	t.Run("derives from the interfaces", func(t *testing.T) {
		cases := []struct {
			label      string
			projection Projection
			want       Mode
		}{
			{"recorder", &recorder{}, ModeRebuild},
			{"resumableRecorder", &resumableRecorder{}, ModeResumable},
			{"batchedRecorder", &batchedRecorder{}, ModeResumable},
		}

		for _, c := range cases {
			t.Run(c.label, func(t *testing.T) {
				assert.Equal(t, c.want, ModeOf(c.projection))
			})
		}
	})
}

func TestBatchSizes(t *testing.T) {
	t.Run("default to one and are clamped", func(t *testing.T) {
		catchUp, live := batchSizesOf(&recorder{})
		assert.Equal(t, 1, catchUp)
		assert.Equal(t, 1, live)

		// A projection that announces nonsense is corrected rather than trusted.
		catchUp, live = batchSizesOf(&batchedRecorder{catchUp: 0, live: -5})
		assert.Equal(t, 1, catchUp)
		assert.Equal(t, 1, live)

		catchUp, live = batchSizesOf(&batchedRecorder{catchUp: 500, live: 10})
		assert.Equal(t, 500, catchUp)
		assert.Equal(t, 10, live)
	})
}

// unverified is the verification of a store created with
// WithoutHashVerification, which lets every event pass.
var unverified = (&Store{skipsHashes: true}).verify

func TestDrive(t *testing.T) {
	t.Run("in rebuild mode keeps no checkpoint", func(t *testing.T) {
		target := &recorder{}

		last, err := drive(context.Background(), writerFor(target), events("0", "1", "2"), unverified, nil, 1)
		require.NoError(t, err)
		assert.Equal(t, "2", last)

		want := []string{"apply 0", "apply 1", "apply 2"}
		assertLog(t, target.log, want)
	})

	t.Run("in resumable mode writes checkpoint after every event", func(t *testing.T) {
		target := &resumableRecorder{}

		_, err := drive(context.Background(), writerFor(target), events("0", "1"), unverified, nil, 1)
		require.NoError(t, err)

		assertLog(t, target.log, []string{"apply 0", "checkpoint 0", "apply 1", "checkpoint 1"})
		assert.Equal(t, "1", target.saved)
	})

	t.Run("honours the batch size", func(t *testing.T) {
		target := &resumableRecorder{}

		_, err := drive(context.Background(), writerFor(target), events("0", "1", "2", "3"), unverified, nil, 2)
		require.NoError(t, err)

		assertLog(t, target.log, []string{
			"apply 0", "apply 1", "checkpoint 1",
			"apply 2", "apply 3", "checkpoint 3",
		})
	})

	t.Run("commits an incomplete final batch", func(t *testing.T) {
		target := &resumableRecorder{}

		_, err := drive(context.Background(), writerFor(target), events("0", "1", "2"), unverified, nil, 2)
		require.NoError(t, err)

		assertLog(t, target.log, []string{
			"apply 0", "apply 1", "checkpoint 1",
			"apply 2", "checkpoint 2",
		})
	})

	t.Run("in transactional mode brackets each batch", func(t *testing.T) {
		target := &transactionalRecorder{}

		_, err := drive(context.Background(), inTransactions(target), events("0", "1", "2"), unverified, nil, 2)
		require.NoError(t, err)

		assertLog(t, target.log, []string{
			"begin", "apply 0", "apply 1", "commit 1",
			"begin", "apply 2", "commit 2",
		})
		assert.Len(t, target.committed, 2)
	})

	t.Run("rolls back when apply fails", func(t *testing.T) {
		target := &transactionalRecorder{}
		target.failOn = "1"

		_, err := drive(context.Background(), inTransactions(target), events("0", "1", "2"), unverified, nil, 10)

		assert.ErrorIs(t, err, ErrPermanent)
		assertLog(t, target.log, []string{"begin", "apply 0", "rollback"})
	})

	t.Run("rolls back when an event fails verification", func(t *testing.T) {
		target := &transactionalRecorder{}
		failOnSecond := func(event eventsourcingdb.Event) error {
			if event.ID == "1" {
				return ErrUnverified
			}
			return nil
		}

		_, err := drive(context.Background(), inTransactions(target), events("0", "1", "2"), failOnSecond, nil, 10)

		assert.ErrorIs(t, err, ErrUnverified)
		// The event that failed is never applied, and neither is anything after it.
		assertLog(t, target.log, []string{"begin", "apply 0", "rollback"})
	})

	t.Run("rolls back when the stream fails", func(t *testing.T) {
		target := &transactionalRecorder{}

		_, err := drive(context.Background(), inTransactions(target),
			failingEvents(2, errors.New("connection lost")), unverified, nil, 10)

		assert.ErrorIs(t, err, ErrTransient, "a broken stream is transient")
		assertLog(t, target.log, []string{"begin", "apply 0", "apply 1", "rollback"})
	})

	t.Run("reports a failing begin", func(t *testing.T) {
		target := &transactionalRecorder{}
		target.beginErr = errors.New("no connection")

		_, err := drive(context.Background(), inTransactions(target), events("0"), unverified, nil, 1)
		assert.Error(t, err, "expected the error from begin")
	})

	t.Run("reports a cancelled context", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := drive(ctx, writerFor(&recorder{}), events(), unverified, nil, 1)

		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("stops when commit fails", func(t *testing.T) {
		target := &failingCommitRecorder{}

		_, err := drive(context.Background(), writerFor(target), events("0", "1"), unverified, nil, 1)

		assert.Error(t, err, "expected the error from commit")
	})

	t.Run("stops when the final commit fails", func(t *testing.T) {
		target := &failingCommitRecorder{}

		// The batch is larger than the stream, so the only commit is the one after
		// the loop.
		_, err := drive(context.Background(), writerFor(target), events("0", "1"), unverified, nil, 10)

		assert.Error(t, err, "expected the error from the final commit")
	})

	t.Run("reports a failing rollback alongside the cause", func(t *testing.T) {
		target := &transactionalRecorder{}
		target.failOn = "1"
		target.rollbackErr = errors.New("rollback did not work either")

		_, err := drive(context.Background(), inTransactions(target), events("0", "1"), unverified, nil, 10)

		// The failure that caused the rollback has to survive, and the rollback
		// failure comes with it rather than replacing it.
		assert.ErrorIs(t, err, ErrPermanent, "the cause must survive")
		assert.ErrorContains(t, err, "rollback did not work either", "the rollback failure has to be reported too")
	})

	t.Run("reports a failing rollback after a broken stream", func(t *testing.T) {
		target := &transactionalRecorder{}
		target.rollbackErr = errors.New("rollback did not work either")

		_, err := drive(context.Background(), inTransactions(target),
			failingEvents(1, errors.New("connection lost")), unverified, nil, 10)

		assert.ErrorIs(t, err, ErrTransient, "the cause must survive")
		assert.ErrorContains(t, err, "rollback did not work either")
	})
}

func TestBoundAfter(t *testing.T) {
	t.Run("excludes the checkpoint", func(t *testing.T) {
		assert.Nil(t, boundAfter(""), "an empty checkpoint means no bound")

		bound := boundAfter("7")
		require.NotNil(t, bound)
		assert.Equal(t, "7", bound.ID)
		assert.Equal(t, eventsourcingdb.BoundTypeExclusive, bound.Type)
	})
}

func assertLog(t *testing.T, got, want []string) {
	t.Helper()

	require.Equal(t, want, got)
}

func TestWriters(t *testing.T) {
	t.Run("without transactions are harmless", func(t *testing.T) {
		ctx := context.Background()

		// A rebuild writer keeps no checkpoint and has nothing to roll back.
		rebuild := writerFor(&recorder{})
		checkpoint, err := rebuild.checkpoint(ctx)
		require.NoError(t, err)
		assert.Empty(t, checkpoint)
		assert.NoError(t, rebuild.rollback(ctx), "nothing was begun, so nothing can fail")

		// A resumable writer reports where it stopped, but also cannot roll back.
		resumable := writerFor(&resumableRecorder{start: "7"})
		checkpoint, err = resumable.checkpoint(ctx)
		require.NoError(t, err)
		assert.Equal(t, "7", checkpoint)
		assert.NoError(t, resumable.rollback(ctx), "the data is already written, so nothing can fail")

		transactional := inTransactions(&transactionalRecorder{start: "9"})
		checkpoint, err = transactional.checkpoint(ctx)
		require.NoError(t, err)
		assert.Equal(t, "9", checkpoint)
		// Rolling back without an open transaction does nothing.
		assert.NoError(t, transactional.rollback(ctx))
	})
}

// failingCommitRecorder refuses to save its checkpoint.
type failingCommitRecorder struct{ recorder }

func (r *failingCommitRecorder) Checkpoint(context.Context) (string, error) { return "", nil }

func (r *failingCommitRecorder) SaveCheckpoint(context.Context, string) error {
	return errors.New("checkpoint storage is full")
}

func TestCheckpointFailure(t *testing.T) {
	t.Run("stops the run", func(t *testing.T) {
		// catchUp asks for the checkpoint first; if that fails, nothing is read.
		writer := writerFor(&brokenCheckpointRecorder{})

		_, err := writer.checkpoint(context.Background())
		assert.Error(t, err, "expected the error from checkpoint")
	})
}

type brokenCheckpointRecorder struct{ recorder }

func (r *brokenCheckpointRecorder) Checkpoint(context.Context) (string, error) {
	return "", errors.New("cannot read the checkpoint")
}

func (r *brokenCheckpointRecorder) SaveCheckpoint(context.Context, string) error { return nil }

func TestProjectionFunc(t *testing.T) {
	t.Run("satisfies Projection", func(t *testing.T) {
		seen := 0
		projection := ProjectionFunc(func(context.Context, eventsourcingdb.Event) error {
			seen++
			return nil
		})

		require.NoError(t, projection.Apply(context.Background(), eventsourcingdb.Event{ID: "0"}))
		assert.Equal(t, 1, seen)
		assert.Equal(t, ModeRebuild, ModeOf(projection), "a plain function is rebuilt")
	})

	t.Run("reports its failure", func(t *testing.T) {
		projection := ProjectionFunc(func(context.Context, eventsourcingdb.Event) error {
			return errors.New("cannot apply this")
		})

		assert.Error(t, projection.Apply(context.Background(), eventsourcingdb.Event{}), "expected the error from the function")
	})
}
