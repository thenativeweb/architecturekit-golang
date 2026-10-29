package architecturekit_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/v2/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/v2/eventsourcingdb"
)

// A projection that fails at an event, whether it returns an error or panics,
// names the event in what the run ends with, so that a log tells which event
// to look at. These tests drive every kind of projection the kit runs over a
// database that holds a single event, 42.

// storeWithEvent42 is a store on a database that holds the single event 42,
// and keeps an observed stream open.
func storeWithEvent42(t *testing.T) *architecturekit.Store {
	t.Helper()

	return architecturekit.NewStore(newFakeDatabase(t, &fakeDatabase{
		events:       []int{42},
		endObserving: func(int) bool { return false },
	}), "https://thenativeweb.io")
}

// incrementedByWord has the type of the event of storeWithEvent42, but a text
// where the event has a number, so that a typed projection can not decode the
// event into it.
type incrementedByWord struct {
	By string `json:"by"`
}

func (incrementedByWord) EventType() string { return "io.thenativeweb.test.incremented" }

// shelfFull is an error of a type of its own, which errors.As has to find.
type shelfFull struct {
	shelf string
}

func (e *shelfFull) Error() string { return "shelf " + e.shelf + " is full" }

// plainFailing is a projection that implements Apply alone, and fails at
// every event as fail does.
type plainFailing struct {
	fail func() error
}

func (p plainFailing) Apply(context.Context, eventsourcingdb.Event) error { return p.fail() }

// resumableFailing keeps a checkpoint as well, and fails to save it with
// saveErr, if set.
type resumableFailing struct {
	plainFailing
	saveErr error
}

func (resumableFailing) Checkpoint(context.Context) (string, error) { return "", nil }

func (p resumableFailing) SaveCheckpoint(context.Context, string) error { return p.saveErr }

// transactionalFailing applies every event within a transaction, which fails
// as fail does, and fails around applying it where its other errors say.
type transactionalFailing struct {
	fail          func() error
	checkpointErr error
	beginErr      error
	commitErr     error
	rollbackErr   error
}

func (p transactionalFailing) Checkpoint(context.Context) (string, error) {
	return "", p.checkpointErr
}

func (p transactionalFailing) Begin(context.Context) (architecturekit.Tx, error) {
	if p.beginErr != nil {
		return nil, p.beginErr
	}

	return failingTx{projection: p}, nil
}

type failingTx struct {
	projection transactionalFailing
}

func (tx failingTx) Apply(context.Context, eventsourcingdb.Event) error {
	if tx.projection.fail == nil {
		return nil
	}

	return tx.projection.fail()
}

func (tx failingTx) Commit(context.Context, string) error { return tx.projection.commitErr }

func (tx failingTx) Rollback(context.Context) error { return tx.projection.rollbackErr }

// failingProjections are the kinds of projection the kit runs, by name, each
// failing at every event as fail does. All but the transactional one are a
// Projection.
func failingProjections(fail func() error) map[string]any {
	return map[string]any{
		"plain":         plainFailing{fail: fail},
		"resumable":     resumableFailing{plainFailing: plainFailing{fail: fail}},
		"transactional": transactionalFailing{fail: fail},
		"typed": architecturekit.NewTypedProjection().
			On(func(context.Context, architecturekit.Envelope[incremented]) error { return fail() }),
		"ProjectionFunc": architecturekit.ProjectionFunc(func(context.Context, eventsourcingdb.Event) error { return fail() }),
	}
}

// endedRun runs a projection of any kind over storeWithEvent42, and returns
// the run once it has ended.
func endedRun(t *testing.T, projection any, options ...architecturekit.ProjectionOption) *architecturekit.ProjectionRun {
	t.Helper()

	var run *architecturekit.ProjectionRun

	switch projection := projection.(type) {
	case architecturekit.Transactional:
		run = architecturekit.StartTransactionalProjection(t.Context(), storeWithEvent42(t),
			architecturekit.ExactSubject("/test"), projection, options...)
	case architecturekit.Projection:
		run = architecturekit.StartProjection(t.Context(), storeWithEvent42(t),
			architecturekit.ExactSubject("/test"), projection, options...)
	default:
		require.FailNow(t, "not a projection", "%T", projection)
	}

	waitForClosed(t, run.Done(), "Done")

	return run
}

// caughtUpOnce catches a projection of any kind up with storeWithEvent42, and
// returns what that returned.
func caughtUpOnce(t *testing.T, projection any) error {
	t.Helper()

	switch projection := projection.(type) {
	case architecturekit.Transactional:
		return architecturekit.CatchUpTransactionalProjection(t.Context(), storeWithEvent42(t),
			architecturekit.ExactSubject("/test"), projection)
	case architecturekit.Projection:
		return architecturekit.CatchUpProjection(t.Context(), storeWithEvent42(t),
			architecturekit.ExactSubject("/test"), projection)
	default:
		require.FailNow(t, "not a projection", "%T", projection)
		return nil
	}
}

func TestApplyFailure(t *testing.T) {
	errShelfFull := errors.New("the shelf is full")
	shelfA := &shelfFull{shelf: "A"}

	// Every way in which a projection fails at an event, with the message the
	// run ends with, which a panic only starts with, since its stack follows,
	// and what errors.Is and errors.As have to find in it all the same.
	failures := []struct {
		name    string
		fail    func() error
		message string
		panics  bool
		finds   func(t *testing.T, err error)
	}{
		{
			name:    "returns an error without a category",
			fail:    func() error { return errShelfFull },
			message: "applying event 42: the shelf is full",
			finds: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, errShelfFull)
				assert.NotErrorIs(t, err, architecturekit.ErrPermanent, "naming the event adds no category")
			},
		},
		{
			name:    "returns an error of a category",
			fail:    func() error { return fmt.Errorf("%w: %w", architecturekit.ErrPermanent, shelfA) },
			message: "permanent failure: applying event 42: shelf A is full",
			finds: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, architecturekit.ErrPermanent)

				found, isFound := errors.AsType[*shelfFull](err)
				require.True(t, isFound, "errors.As has to find the error of the projection")
				assert.Same(t, shelfA, found)
			},
		},
		{
			name:    "returns a domain error, whose text does not start with the one of its category",
			fail:    func() error { return architecturekit.NewDomainError("shelf %s is full", "A") },
			message: "applying event 42: shelf A is full",
			finds: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, architecturekit.ErrDomain)

				found, isFound := errors.AsType[*architecturekit.DomainError](err)
				require.True(t, isFound, "errors.As has to find the error of the projection")
				assert.Equal(t, "shelf A is full", found.Error())
			},
		},
		{
			name:    "panics",
			fail:    func() error { panic("the shelf is on fire") },
			message: "architecturekit: applying event 42: panic while running the projection: the shelf is on fire\n\n",
			panics:  true,
			finds: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, architecturekit.ErrPermanent)
				assert.Contains(t, err.Error(), "goroutine ", "the error has to hold the stack")
			},
		},
		{
			name:    "panics with an error of a category that may pass",
			fail:    func() error { panic(fmt.Errorf("%w: the index is down", architecturekit.ErrTransient)) },
			message: "architecturekit: applying event 42: panic while running the projection: transient failure: the index is down\n\n",
			panics:  true,
			finds: func(t *testing.T, err error) {
				assert.ErrorIs(t, err, architecturekit.ErrPermanent)
				assert.NotErrorIs(t, err, architecturekit.ErrTransient, "a panic is a mistake in the code, whatever it panicked with")
			},
		},
	}

	// assertNamed asserts that err names the event as the failure says, and
	// that errors.Is and errors.As find what the failure says.
	assertNamed := func(t *testing.T, err error, message string, panics bool, finds func(*testing.T, error)) {
		t.Helper()

		require.Error(t, err)
		if panics {
			assert.True(t, strings.HasPrefix(err.Error(), message), "got %q, want it to start with %q", err.Error(), message)
		} else {
			assert.EqualError(t, err, message)
		}

		finds(t, err)
	}

	for _, failure := range failures {
		t.Run("names the event at which the projection "+failure.name, func(t *testing.T) {
			for kind, projection := range failingProjections(failure.fail) {
				t.Run(kind, func(t *testing.T) {
					run := endedRun(t, projection)

					assertNamed(t, run.Err(), failure.message, failure.panics, failure.finds)
					assert.Equal(t, run.Err(), run.Status().Err)
				})

				t.Run(kind+" caught up", func(t *testing.T) {
					assertNamed(t, caughtUpOnce(t, projection), failure.message, failure.panics, failure.finds)
				})
			}
		})

		t.Run("names the event, and the run by its name or by its subject, at which the projection "+failure.name, func(t *testing.T) {
			for kind, projection := range failingProjections(failure.fail) {
				t.Run(kind, func(t *testing.T) {
					named := endedRun(t, projection, architecturekit.Named("shelves"))
					unnamed := endedRun(t, projection)

					err := architecturekit.WaitCaughtUp(waitingContext(t), named)
					assertNamed(t, err, `projection "shelves": `+failure.message, failure.panics, failure.finds)
					assert.ErrorIs(t, err, named.Err())

					err = architecturekit.WaitCaughtUp(waitingContext(t), unnamed)
					assertNamed(t, err, `projection of "/test": `+failure.message, failure.panics, failure.finds)
					assert.ErrorIs(t, err, unnamed.Err())
				})
			}
		})
	}

	t.Run("names the event in a failure of the kit, after the text of its category", func(t *testing.T) {
		projection := architecturekit.NewTypedProjection().
			On(func(context.Context, architecturekit.Envelope[incrementedByWord]) error { return nil })

		run := endedRun(t, projection)

		require.ErrorIs(t, run.Err(), architecturekit.ErrPermanent)
		assert.True(t,
			strings.HasPrefix(run.Err().Error(),
				`permanent failure: applying event 42: decoding event 42 of type "io.thenativeweb.test.incremented": `),
			"got %q", run.Err().Error())
	})

	t.Run("names the event in a failure that may pass, which the run tries again", func(t *testing.T) {
		observed := &reconnects{}
		target := &flakyCollector{flakyID: "42"}
		target.failuresLeft.Store(1)

		database := &fakeDatabase{events: []int{42}, endObserving: func(int) bool { return false }}
		run := architecturekit.StartProjection(t.Context(), reconnectingStore(newFakeDatabase(t, database), observed),
			architecturekit.ExactSubject("/test"), target)

		waitForClosed(t, run.CaughtUp(), "CaughtUp")

		errs, _ := observed.recorded()
		require.Len(t, errs, 1)
		assert.ErrorIs(t, errs[0], architecturekit.ErrTransient, "the run has to try the event again")
		assert.EqualError(t, errs[0], "transient failure: applying event 42: the target is unavailable")
		assert.Equal(t, []string{"42"}, target.IDs())
	})

	t.Run("names no event in a failure outside of applying one", func(t *testing.T) {
		checkpointErr := errors.New("the checkpoint is gone")
		saveErr := errors.New("the disk is full")
		beginErr := errors.New("the connection is gone")
		commitErr := errors.New("the transaction was aborted")

		// Every function around Apply, with a projection that fails in it, and
		// the error it fails with.
		around := map[string]struct {
			projection any
			err        error
		}{
			"Checkpoint": {transactionalFailing{checkpointErr: checkpointErr}, checkpointErr},
			"SaveCheckpoint": {
				resumableFailing{plainFailing: plainFailing{fail: func() error { return nil }}, saveErr: saveErr},
				saveErr,
			},
			"Begin":  {transactionalFailing{beginErr: beginErr}, beginErr},
			"Commit": {transactionalFailing{commitErr: commitErr}, commitErr},
		}

		for function, failing := range around {
			t.Run(function, func(t *testing.T) {
				run := endedRun(t, failing.projection)

				assert.Equal(t, failing.err, run.Err(), "a failure outside of Apply stays as it is")
			})
		}
	})

	t.Run("names the event in the failure of Apply, but not in the one of the rollback that follows", func(t *testing.T) {
		rollbackErr := errors.New("the rollback failed")

		run := endedRun(t, transactionalFailing{fail: func() error { return errShelfFull }, rollbackErr: rollbackErr})

		assert.EqualError(t, run.Err(), "applying event 42: the shelf is full\nthe rollback failed")
		assert.ErrorIs(t, run.Err(), errShelfFull)
		assert.ErrorIs(t, run.Err(), rollbackErr)
	})
}
