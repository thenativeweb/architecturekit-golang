package architecturekit_test

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// A panic in a projection is a mistake in the code, such as a ProjectionFunc
// that writes into a map it never made. A run has a goroutine of its own, so
// left alone, the panic would end the whole process, including the test that
// provokes it. That every test here finishes is part of what they check.

// panicsIn names the function of a projection that panics, as one with a bug
// does.
type panicsIn string

func (p panicsIn) panicIn(function string) {
	if string(p) == function {
		panic(function + " is broken")
	}
}

// panicky is resumable and batched, so that every function the kit calls on a
// projection that is not transactional can panic.
type panicky struct {
	panicsIn
}

func (p *panicky) Apply(context.Context, eventsourcingdb.Event) error {
	p.panicIn("Apply")
	return nil
}

func (p *panicky) Checkpoint(context.Context) (string, error) {
	p.panicIn("Checkpoint")
	return "", nil
}

func (p *panicky) SaveCheckpoint(context.Context, string) error {
	p.panicIn("SaveCheckpoint")
	return nil
}

func (p *panicky) CatchUpBatchSize() int {
	p.panicIn("CatchUpBatchSize")
	return 1
}

// panickyTransactional is panicky for a transactional projection. Its
// transactions fail with applyFails, if set, and record whether they were
// rolled back.
type panickyTransactional struct {
	panicsIn
	applyFails error

	mutex      sync.Mutex
	rolledBack bool
}

func (p *panickyTransactional) Checkpoint(context.Context) (string, error) {
	p.panicIn("Checkpoint")
	return "", nil
}

func (p *panickyTransactional) CatchUpBatchSize() int {
	p.panicIn("CatchUpBatchSize")
	return 1
}

func (p *panickyTransactional) Begin(context.Context) (architecturekit.Tx, error) {
	p.panicIn("Begin")
	return &panickyTx{owner: p}, nil
}

func (p *panickyTransactional) hasRolledBack() bool {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	return p.rolledBack
}

type panickyTx struct {
	owner *panickyTransactional
}

func (tx *panickyTx) Apply(context.Context, eventsourcingdb.Event) error {
	tx.owner.panicIn("Apply")
	return tx.owner.applyFails
}

func (tx *panickyTx) Commit(context.Context, string) error {
	tx.owner.panicIn("Commit")
	return nil
}

func (tx *panickyTx) Rollback(context.Context) error {
	tx.owner.mutex.Lock()
	tx.owner.rolledBack = true
	tx.owner.mutex.Unlock()

	tx.owner.panicIn("Rollback")
	return nil
}

// unprepared is a projection that writes into a map it never made, which
// panics, an easy mistake to make in a ProjectionFunc.
func unprepared() architecturekit.Projection {
	var catalog struct{ books map[string]string }

	return architecturekit.ProjectionFunc(func(_ context.Context, event eventsourcingdb.Event) error {
		catalog.books[event.ID] = event.Subject
		return nil
	})
}

// storeWithOneEvent is a store on a database that holds a single event, and
// keeps an observed stream open.
func storeWithOneEvent(t *testing.T) *architecturekit.Store {
	t.Helper()

	database := &fakeDatabase{
		events:       []int{0},
		endObserving: func(int) bool { return false },
	}

	return architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io")
}

// assertPanicked asserts that err is what a panic ends a run with: a failure
// of the category ErrPermanent that names the value of the panic, and holds
// the stack it happened on.
func assertPanicked(t *testing.T, err error, value, frame string) {
	t.Helper()

	require.Error(t, err)
	assert.ErrorIs(t, err, architecturekit.ErrPermanent)
	assert.ErrorContains(t, err, "architecturekit: panic while running the projection: "+value)
	assert.Contains(t, err.Error(), "goroutine ", "the error has to hold the stack")
	assert.Contains(t, err.Error(), frame, "the stack has to show where the panic happened")
}

// assertStoppedOnPanic waits for the run to end, and asserts that it has
// stopped on the panic.
func assertStoppedOnPanic(t *testing.T, run *architecturekit.ProjectionRun, value, frame string) {
	t.Helper()

	waitForClosed(t, run.Done(), "Done")
	assertPanicked(t, run.Err(), value, frame)

	status := run.Status()
	assert.Equal(t, architecturekit.PhaseStopped, status.Phase)
	assert.Equal(t, run.Err(), status.Err)
}

func TestPanickingProjection(t *testing.T) {
	t.Run("ends the run rather than the process", func(t *testing.T) {
		run := architecturekit.StartProjection(t.Context(), storeWithOneEvent(t), architecturekit.ExactSubject("/test"), unprepared())

		assertStoppedOnPanic(t, run, "assignment to entry in nil map", "architecturekit_test.unprepared.")
	})

	t.Run("comes back from catching up as an error rather than a panic", func(t *testing.T) {
		var err error
		require.NotPanics(t, func() {
			err = architecturekit.CatchUpProjection(t.Context(), storeWithOneEvent(t), architecturekit.ExactSubject("/test"), unprepared())
		})

		assertPanicked(t, err, "assignment to entry in nil map", "architecturekit_test.unprepared.")
	})

	// Every function of a projection that the kit calls, with the frame the
	// stack has to show for it.
	resumable := map[string]string{
		"Apply":            "architecturekit_test.(*panicky).Apply",
		"Checkpoint":       "architecturekit_test.(*panicky).Checkpoint",
		"SaveCheckpoint":   "architecturekit_test.(*panicky).SaveCheckpoint",
		"CatchUpBatchSize": "architecturekit_test.(*panicky).CatchUpBatchSize",
	}
	transactional := map[string]string{
		"Checkpoint":       "architecturekit_test.(*panickyTransactional).Checkpoint",
		"CatchUpBatchSize": "architecturekit_test.(*panickyTransactional).CatchUpBatchSize",
		"Begin":            "architecturekit_test.(*panickyTransactional).Begin",
		"Apply":            "architecturekit_test.(*panickyTx).Apply",
		"Commit":           "architecturekit_test.(*panickyTx).Commit",
	}

	t.Run("ends the run wherever the projection panics", func(t *testing.T) {
		for function, frame := range resumable {
			t.Run(function, func(t *testing.T) {
				run := architecturekit.StartProjection(t.Context(), storeWithOneEvent(t), architecturekit.ExactSubject("/test"),
					&panicky{panicsIn: panicsIn(function)})

				assertStoppedOnPanic(t, run, function+" is broken", frame)
			})
		}

		for function, frame := range transactional {
			t.Run("transactional "+function, func(t *testing.T) {
				run := architecturekit.StartTransactionalProjection(t.Context(), storeWithOneEvent(t), architecturekit.ExactSubject("/test"),
					&panickyTransactional{panicsIn: panicsIn(function)})

				assertStoppedOnPanic(t, run, function+" is broken", frame)
			})
		}
	})

	t.Run("comes back from catching up wherever the projection panics", func(t *testing.T) {
		for function, frame := range resumable {
			t.Run(function, func(t *testing.T) {
				err := architecturekit.CatchUpProjection(t.Context(), storeWithOneEvent(t), architecturekit.ExactSubject("/test"),
					&panicky{panicsIn: panicsIn(function)})

				assertPanicked(t, err, function+" is broken", frame)
			})
		}

		for function, frame := range transactional {
			t.Run("transactional "+function, func(t *testing.T) {
				err := architecturekit.CatchUpTransactionalProjection(t.Context(), storeWithOneEvent(t), architecturekit.ExactSubject("/test"),
					&panickyTransactional{panicsIn: panicsIn(function)})

				assertPanicked(t, err, function+" is broken", frame)
			})
		}
	})

	t.Run("is permanent, even if it panics with a transient error", func(t *testing.T) {
		transient := architecturekit.ProjectionFunc(func(context.Context, eventsourcingdb.Event) error {
			panic(fmt.Errorf("%w: the index is down", architecturekit.ErrTransient))
		})

		run := architecturekit.StartProjection(t.Context(), storeWithOneEvent(t), architecturekit.ExactSubject("/test"), transient)

		assertStoppedOnPanic(t, run, "transient failure: the index is down", "architecturekit_test.TestPanickingProjection")
		assert.NotErrorIs(t, run.Err(), architecturekit.ErrTransient, "a panic is a mistake in the code, whatever it panicked with")
	})

	t.Run("rolls the transaction back when Apply panics", func(t *testing.T) {
		projection := &panickyTransactional{panicsIn: "Apply"}

		run := architecturekit.StartTransactionalProjection(t.Context(), storeWithOneEvent(t), architecturekit.ExactSubject("/test"), projection)

		assertStoppedOnPanic(t, run, "Apply is broken", "architecturekit_test.(*panickyTx).Apply")
		assert.True(t, projection.hasRolledBack(), "the transaction must not be left open")
	})

	t.Run("reports a panic in Rollback alongside its cause, and ends the run even if the cause may pass", func(t *testing.T) {
		projection := &panickyTransactional{
			panicsIn:   "Rollback",
			applyFails: fmt.Errorf("%w: the row is locked", architecturekit.ErrTransient),
		}
		observed := &reconnects{}

		run := architecturekit.StartTransactionalProjection(t.Context(),
			reconnectingStore(newFakeDatabase(t, &fakeDatabase{events: []int{0}, endObserving: func(int) bool { return false }}), observed),
			architecturekit.ExactSubject("/test"), projection)

		assertStoppedOnPanic(t, run, "Rollback is broken", "architecturekit_test.(*panickyTx).Rollback")
		assert.ErrorContains(t, run.Err(), "the row is locked", "the cause of the rollback must not get lost")
		assert.Zero(t, observed.count(), "trying again would only panic again")
	})

	t.Run("ends the run when the observer of reconnects panics", func(t *testing.T) {
		database := &fakeDatabase{
			events:       []int{0},
			endObserving: func(int) bool { return true },
		}
		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io",
			architecturekit.WithReconnectObserver(func(architecturekit.Reconnect) {
				panic(errors.New("the log is broken"))
			}),
		)

		run := architecturekit.StartProjection(t.Context(), store, architecturekit.ExactSubject("/test"), &collector{})

		assertStoppedOnPanic(t, run, "the log is broken", "architecturekit_test.TestPanickingProjection")
	})
}
