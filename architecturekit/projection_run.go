package architecturekit

import (
	"context"
	"sync"
	"time"
)

// Phase is where a projection run stands (see ProjectionStatus).
type Phase string

const (
	// PhaseCatchingUp means that the run applies the events that were already
	// stored when it started, and has not caught up yet.
	PhaseCatchingUp Phase = "catching-up"
	// PhaseLive means that the run has caught up and follows the stream.
	PhaseLive Phase = "live"
	// PhaseReconnecting means that reading failed or the stream ended, and that
	// the run has not caught up again since. Waiting between attempts and
	// catching up after them both belong to this phase.
	PhaseReconnecting Phase = "reconnecting"
	// PhaseStopped means that the run has ended.
	PhaseStopped Phase = "stopped"
)

// ProjectionStatus describes a projection run at one moment.
type ProjectionStatus struct {
	// Phase is where the run stands.
	Phase Phase

	// Since is when the phase began. For PhaseReconnecting, that is when the
	// disruption began, not when the latest attempt did, so that it tells how
	// long the projection has not been up to date.
	Since time.Time

	// Err is why the run is reconnecting or has stopped. It is nil if the
	// database ended the stream, or if the run stopped because its context
	// ended, and in every other phase.
	Err error

	// Attempts counts the attempts to read again within the current
	// disruption. It is 0 outside of PhaseReconnecting.
	Attempts int

	// Revision is the ID of the last event the run has applied and committed,
	// or an empty string if there is none yet.
	Revision string

	// HasCaughtUp tells whether the run has caught up at least once. Like
	// CaughtUp, it stays true while the run reconnects later on, which tells a
	// run that is behind for a while from one that has never been up to date.
	HasCaughtUp bool
}

// ProjectionRun is a projection that runs in the background, started with
// StartProjection or StartTransactionalProjection. It tells when the
// projection has caught up, when it has ended, and where it stands.
type ProjectionRun struct {
	caughtUp     chan struct{}
	caughtUpOnce sync.Once
	done         chan struct{}

	mutex  sync.Mutex
	status ProjectionStatus
	err    error
}

// StartProjection runs a projection in the background, the way RunProjection
// does, and returns at once. The returned run tells when the projection has
// caught up, which is when an application can start to answer queries:
//
//	run := architecturekit.StartProjection(ctx, store, "/books", true, catalogProjection)
//
//	select {
//	case <-run.CaughtUp():
//	case <-run.Done():
//	  return run.Err()
//	}
//
// The run ends when the context ends, or on a failure that trying again will
// not fix, exactly as RunProjection does.
//
// A projection that is transactional as well is a programming error and
// panics; use StartTransactionalProjection for it.
func StartProjection(
	ctx context.Context,
	store *Store,
	subject string,
	recursive bool,
	projection Projection,
) *ProjectionRun {
	refuseTransactional(projection)

	return start(func(progress *ProjectionRun) error {
		return run(ctx, store, subject, recursive, writerFor(projection), projection, progress)
	})
}

// StartTransactionalProjection is StartProjection for a transactional
// projection.
func StartTransactionalProjection(
	ctx context.Context,
	store *Store,
	subject string,
	recursive bool,
	projection Transactional,
) *ProjectionRun {
	return start(func(progress *ProjectionRun) error {
		return run(ctx, store, subject, recursive,
			&transactionalWriter{projection: projection}, projection, progress)
	})
}

// start runs a projection in a goroutine of its own, reporting to the returned
// run.
func start(runProjection func(progress *ProjectionRun) error) *ProjectionRun {
	progress := &ProjectionRun{
		caughtUp: make(chan struct{}),
		done:     make(chan struct{}),
		status:   ProjectionStatus{Phase: PhaseCatchingUp, Since: time.Now()},
	}

	go func() {
		progress.stop(runProjection(progress))
	}()

	return progress
}

// CaughtUp is closed once the run has applied the events that were stored
// when it started. It is closed only once, and stays closed while the run
// reconnects later on. If the run ends before it catches up, it is never
// closed, so wait for Done as well.
func (r *ProjectionRun) CaughtUp() <-chan struct{} {
	return r.caughtUp
}

// Done is closed once the run has ended.
func (r *ProjectionRun) Done() <-chan struct{} {
	return r.done
}

// Err returns why the run has ended, once Done is closed, and nil before. It is
// nil as well if the run ended because its context did.
func (r *ProjectionRun) Err() error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return r.err
}

// Status returns where the run stands. It is safe to call from any goroutine.
func (r *ProjectionRun) Status() ProjectionStatus {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	return r.status
}

// The functions below report the progress of a run. RunProjection and the
// catch-up functions run without a ProjectionRun, so they all accept a nil
// receiver and do nothing then.

// caughtUpNow records that the run has caught up, initially or after a
// disruption.
func (r *ProjectionRun) caughtUpNow() {
	if r == nil {
		return
	}

	r.mutex.Lock()
	r.status.Phase = PhaseLive
	r.status.Since = time.Now()
	r.status.Err = nil
	r.status.Attempts = 0
	r.status.HasCaughtUp = true
	r.mutex.Unlock()

	r.caughtUpOnce.Do(func() { close(r.caughtUp) })
}

// disrupted records that reading failed or the stream ended, and that the run
// is about to try again.
func (r *ProjectionRun) disrupted(err error) {
	if r == nil {
		return
	}

	r.mutex.Lock()
	defer r.mutex.Unlock()

	if r.status.Phase != PhaseReconnecting {
		r.status.Phase = PhaseReconnecting
		r.status.Since = time.Now()
		r.status.Attempts = 0
	}

	r.status.Err = err
	r.status.Attempts++
}

// committed records the last event that has been applied and committed.
func (r *ProjectionRun) committed(eventID string) {
	if r == nil {
		return
	}

	r.mutex.Lock()
	defer r.mutex.Unlock()

	r.status.Revision = eventID
}

// stop records that the run has ended, and why.
func (r *ProjectionRun) stop(err error) {
	r.mutex.Lock()
	r.status.Phase = PhaseStopped
	r.status.Since = time.Now()
	r.status.Err = err
	r.status.Attempts = 0
	r.err = err
	r.mutex.Unlock()

	close(r.done)
}
