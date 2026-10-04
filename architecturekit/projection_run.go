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
	name         string
	caughtUp     chan struct{}
	caughtUpOnce sync.Once
	done         chan struct{}

	mutex  sync.Mutex
	status ProjectionStatus
	err    error
}

// StartProjection runs a projection in the background and returns at once. It
// first catches up from the checkpoint with a finite read, then follows the
// stream live. The returned run tells when the projection has caught up, which
// is when an application can start to answer queries:
//
//	run := architecturekit.StartProjection(ctx, store, architecturekit.SubjectTree("/books"), catalogProjection,
//	  architecturekit.Named("catalog"))
//
//	select {
//	case <-run.CaughtUp():
//	case <-run.Done():
//	  if err := run.Err(); err != nil {
//	    return err
//	  }
//	  return ctx.Err()
//	}
//
// If reading fails, or if the database ends the stream, for example on a
// restart, it waits and catches up again from where it stopped, with a delay
// that grows with every attempt in a row (see WithReconnectDelays and
// WithReconnectObserver). An error of Apply of the category ErrTransient is
// handled the same way, so the event is tried again. Only a failure that
// trying again will not fix, such as any other error of Apply, ends the run,
// and Err returns it. Ending the context is how a projection is stopped, so
// Err returns nil then. That is why the example returns the error of the
// context if Err returns nil: a run whose context ends before it has caught
// up, for example on a timeout while the database can not be reached, has not
// caught up, and returning Err alone would look like success.
//
// A panic in the projection, such as one in Apply that writes into a nil map,
// ends the run as well, rather than the whole process. Err then returns an
// error of the category ErrPermanent, since a panic is a mistake in the code
// that trying again will not fix. Its message holds the value and the stack of
// the panic.
//
// To wait for the run to end, as a process does that does nothing else:
//
//	<-run.Done()
//	return run.Err()
//
// A nil projection, or one that is transactional as well, is a programming
// error and panics; use StartTransactionalProjection for the latter. A nil
// pointer or a nil ProjectionFunc counts as a nil projection.
func StartProjection(
	ctx context.Context,
	store *Store,
	subjects Subjects,
	projection Projection,
	options ...ProjectionOption,
) *ProjectionRun {
	requireSubjects(subjects)
	requireProjection("StartProjection", projection)
	refuseTransactional(projection)

	return start(projectionSettingsOf(options), func(progress *ProjectionRun) error {
		return run(ctx, store, subjects, writerFor(projection), projection, progress)
	})
}

// StartTransactionalProjection is StartProjection for a transactional
// projection. Every batch is applied within one transaction, which is
// committed together with the ID of its last event. A panic in the Apply of a
// transaction rolls the transaction back before the run ends.
//
// A nil projection, including a nil pointer, is a programming error and
// panics.
func StartTransactionalProjection(
	ctx context.Context,
	store *Store,
	subjects Subjects,
	projection Transactional,
	options ...ProjectionOption,
) *ProjectionRun {
	requireSubjects(subjects)
	requireProjection("StartTransactionalProjection", projection)

	return start(projectionSettingsOf(options), func(progress *ProjectionRun) error {
		return run(ctx, store, subjects,
			&transactionalWriter{projection: projection}, projection, progress)
	})
}

// start runs a projection in a goroutine of its own, reporting to the returned
// run.
func start(settings projectionSettings, runProjection func(progress *ProjectionRun) error) *ProjectionRun {
	progress := &ProjectionRun{
		name:     settings.name,
		caughtUp: make(chan struct{}),
		done:     make(chan struct{}),
		status:   ProjectionStatus{Phase: PhaseCatchingUp, Since: time.Now()},
	}

	go func() {
		progress.stop(runProjection(progress))
	}()

	return progress
}

// Name returns the name the projection was given with Named, or an empty
// string if it has none.
func (r *ProjectionRun) Name() string {
	return r.name
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
// nil as well if the run ended because its context did, so a caller that waits
// for CaughtUp takes the error of the context then (see StartProjection).
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

// The functions below report the progress of a run.

// caughtUpNow records that the run has caught up, initially or after a
// disruption.
func (r *ProjectionRun) caughtUpNow() {
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

// committed records the last event that has been applied and committed. The
// catch-up functions run without a ProjectionRun, so it accepts a nil receiver
// and does nothing then.
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
