package architecturekit

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"reflect"
	"time"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Projection is what StartProjection drives: it applies events. A projection
// that implements nothing else is rebuilt from the beginning on every start,
// which is right for a view kept in memory. A projection that can only apply
// events within a transaction is a Transactional instead.
type Projection interface {
	Apply(ctx context.Context, event eventsourcingdb.Event) error
}

// Resumable is optional. A projection implements it when its target can hold a
// checkpoint, but not together with the data. Events may then be applied twice
// after a crash, so Apply has to be idempotent.
type Resumable interface {
	Checkpoint(ctx context.Context) (string, error)
	SaveCheckpoint(ctx context.Context, eventID string) error
}

// Transactional is a projection whose target can make data and checkpoint
// durable together, as a relational database can.
//
// It is not an optional addition to Projection, but a kind of its own: it
// applies events only within a transaction, so it has no Apply outside of one.
// Start it with StartTransactionalProjection. That way the compiler checks its
// method set, instead of a mismatch silently turning it into a projection that
// is rebuilt on every start.
type Transactional interface {
	Checkpoint(ctx context.Context) (string, error)
	Begin(ctx context.Context) (Tx, error)
}

// Tx is one unit of work of a transactional projection. Commit has to make the
// applied events and the last event ID durable together, or neither. Rollback
// is not called after Commit fails, so Commit has to roll back itself if it can
// not finish. That holds for a panic in Commit as well, while a panic in Apply
// is rolled back like an error.
type Tx interface {
	Apply(ctx context.Context, event eventsourcingdb.Event) error
	Commit(ctx context.Context, lastEventID string) error
	Rollback(ctx context.Context) error
}

// Batched is optional, for resumable and transactional projections alike. It
// says how many events are applied while catching up before the checkpoint is
// written.
//
// The size defaults to one, which is the safe choice and works for projections
// that are not idempotent. Raising it speeds up a rebuild by orders of
// magnitude. A resumable projection pays for that by applying up to that many
// events a second time after a crash, a transactional one by a larger
// transaction. Only the projection knows what its target costs.
//
// Once a projection has caught up, it writes the checkpoint after every event,
// whatever the size. Events then arrive one at a time, often with pauses in
// between, and a batch that waited to fill up would hold back events that have
// already arrived.
type Batched interface {
	CatchUpBatchSize() int
}

// Mode is how StartProjection drives a projection, derived from the interfaces
// it fulfils. A transactional projection has no mode, because it is run by a
// function of its own.
type Mode string

const (
	// ModeRebuild reads from the beginning on every start.
	ModeRebuild Mode = "rebuild"
	// ModeResumable resumes, without a guarantee that the checkpoint and the
	// data agree after a crash.
	ModeResumable Mode = "resumable"
)

// ModeOf reports how StartProjection will drive this projection. Log it at
// startup: if a projection is driven in rebuild mode against expectations, a
// method signature does not match the interface.
func ModeOf(projection Projection) Mode {
	if _, ok := projection.(Resumable); ok {
		return ModeResumable
	}

	return ModeRebuild
}

// refuseTransactional panics for a projection that fulfils Transactional as
// well. Driving it through Apply would bypass its transactions, and nothing
// would notice, which is why it is refused loudly instead.
func refuseTransactional(projection Projection) {
	if _, ok := projection.(Transactional); ok {
		panic(fmt.Sprintf("architecturekit: %T is transactional, start it with StartTransactionalProjection", projection))
	}
}

// requireProjection panics for a nil projection, also one that is a nil
// pointer or function (see isNil), which would otherwise only fail once it is
// called, for a run in the background.
func requireProjection(function string, projection any) {
	if isNil(projection) {
		panic(fmt.Sprintf("architecturekit: %s needs a projection, not nil", function))
	}
}

// isNil reports whether a value that is handed over as an interface is nil,
// also when it is a nil pointer, map, slice, function, or channel of a
// concrete type, such as a view that was declared but never created. Such a
// value is not equal to nil, since the interface knows its type, but it fails
// as soon as it is used. It uses reflection, so call it only while wiring, or
// where it costs little next to the rest, such as for an event that is about
// to be encoded and written.
//
// An interface never shows up as the kind, since reflect.ValueOf unpacks it.
func isNil(value any) bool {
	if value == nil {
		return true
	}

	switch reflected := reflect.ValueOf(value); reflected.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return reflected.IsNil()
	default:
		return false
	}
}

// batchSizeOf takes any projection, because resumable and transactional ones
// can both be batched, and they share no interface.
func batchSizeOf(projection any) int {
	batched, ok := projection.(Batched)
	if !ok {
		return 1
	}

	return max(batched.CatchUpBatchSize(), 1)
}

// CatchUpProjection applies everything that is already stored and returns.
// Use it to build a read model once, for a batch job or in a test, instead of
// following the stream.
//
// If the context ends first, it returns the context's error, so that a read
// model that is only partly built does not look complete.
//
// If the projection panics, it returns an error of the category ErrPermanent
// that holds the value and the stack of the panic, as a run ends with (see
// StartProjection).
//
// A nil projection, or one that is transactional as well, is a programming
// error and panics; use CatchUpTransactionalProjection for the latter. A nil
// pointer or a nil ProjectionFunc counts as a nil projection. A nil option
// panics as well.
func CatchUpProjection(
	ctx context.Context,
	store *Store,
	subjects Subjects,
	projection Projection,
	options ...ProjectionOption,
) error {
	requireSubjects(subjects)
	requireProjection("CatchUpProjection", projection)
	refuseTransactional(projection)

	// Catching up has no use for a name, but checks the options all the same,
	// so that a mistake in them shows here as well.
	_ = projectionSettingsOf("CatchUpProjection", options)

	return catchUpOnce(ctx, store, subjects, writerFor(projection), projection)
}

// CatchUpTransactionalProjection is CatchUpProjection for a transactional
// projection. A nil projection, including a nil pointer, or a nil option, is a
// programming error and panics.
func CatchUpTransactionalProjection(
	ctx context.Context,
	store *Store,
	subjects Subjects,
	projection Transactional,
	options ...ProjectionOption,
) error {
	requireSubjects(subjects)
	requireProjection("CatchUpTransactionalProjection", projection)

	// Catching up has no use for a name, but checks the options all the same,
	// so that a mistake in them shows here as well.
	_ = projectionSettingsOf("CatchUpTransactionalProjection", options)

	return catchUpOnce(ctx, store, subjects, &transactionalWriter{projection: projection}, projection)
}

// catchUpOnce is what the catch-up functions share. A panic on the way, such
// as one in the projection, comes back as the error a run ends with (see run),
// rather than as a panic that the caller does not expect.
func catchUpOnce(
	ctx context.Context,
	store *Store,
	subjects Subjects,
	writer projectionWriter,
	projection any,
) (err error) {
	defer recoverInto(&err)

	_, err = catchUp(ctx, store, subjects, writer, batchSizeOf(projection), nil)

	return err
}

// run catches up and then follows the stream, until the context ends or a
// failure that trying again will not fix. The batch size is read from the
// projection behind the writer, which is the one that knows its target.
// Progress is reported to the given run.
//
// A run has a goroutine of its own, where a panic on the way, such as one in
// the projection, would end the whole process. So it ends the run instead,
// with an error of the category ErrPermanent (see panicError).
func run(
	ctx context.Context,
	store *Store,
	subjects Subjects,
	writer projectionWriter,
	projection any,
	progress *ProjectionRun,
) (failure error) {
	defer recoverInto(&failure)

	catchUpSize := batchSizeOf(projection)
	delay := store.settings.reconnectInitialDelay
	attempt := 0

	for {
		checkpointBefore, _ := writer.checkpoint(ctx)

		liveFor, err := follow(ctx, store, subjects, writer, catchUpSize, progress)
		if ctx.Err() != nil {
			return nil
		}

		// A panic ends the run even together with a failure that may pass, as
		// when a transaction panics and then fails to roll back because the
		// connection is lost, since trying again would only panic again.
		_, hasPanicked := errors.AsType[*panicError](err)
		if err != nil && (hasPanicked || !errors.Is(err, ErrTransient)) {
			return err
		}

		progress.disrupted(err)

		// A session that got somewhere was healthy until it ended, and so was
		// one that followed the stream for longer than the initial delay, even
		// if no event arrived, as when a load balancer ends long-lived
		// connections regularly. The next failure is then retried quickly
		// again. Comparing with the delay the projection has grown to instead
		// would, once it has reached the maximum, keep a quiet projection
		// waiting that long after every cut that comes sooner. So a database
		// that ends every stream a few seconds after the projection has caught
		// up is tried again after the initial delay each time, which is
		// acceptable, since catching up, the expensive part, succeeded each
		// time. A database that fails before the projection has caught up, or
		// within the initial delay after, is still given ever more time.
		if checkpointAfter, _ := writer.checkpoint(ctx); checkpointAfter != checkpointBefore || liveFor > store.settings.reconnectInitialDelay {
			delay = store.settings.reconnectInitialDelay
			attempt = 0
		}

		attempt++

		if store.settings.reconnectObserver != nil {
			store.settings.reconnectObserver(Reconnect{
				Projection: progress.Name(),
				Subject:    subjects.subject,
				Err:        err,
				Delay:      delay,
				Attempt:    attempt,
			})
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}

		delay = min(2*delay, store.settings.reconnectMaxDelay)
	}
}

// follow catches up from the checkpoint and then follows the stream live,
// until reading fails or the stream ends. It returns how long it followed the
// stream live, which is zero if it did not catch up, and a nil error if the
// database ended the stream.
//
// Live, every event is committed on its own, since a batch would only be
// committed once the next events arrive, which may take a long time.
func follow(
	ctx context.Context,
	store *Store,
	subjects Subjects,
	writer projectionWriter,
	catchUpSize int,
	progress *ProjectionRun,
) (time.Duration, error) {
	lastEventID, err := catchUp(ctx, store, subjects, writer, catchUpSize, progress)
	if err != nil {
		return 0, err
	}

	caughtUpAt := time.Now()
	progress.caughtUpNow()

	_, err = drive(ctx, writer,
		store.client.ObserveEvents(ctx, subjects.subject, eventsourcingdb.ObserveEventsOptions{
			Recursive:  subjects.recursive,
			LowerBound: boundAfter(lastEventID),
		}), store.verify, progress, 1)

	return time.Since(caughtUpAt), err
}

// catchUp runs the finite phase and reports where it stopped.
func catchUp(
	ctx context.Context,
	store *Store,
	subjects Subjects,
	writer projectionWriter,
	catchUpSize int,
	progress *ProjectionRun,
) (string, error) {
	checkpoint, err := writer.checkpoint(ctx)
	if err != nil {
		return "", err
	}

	lastEventID, err := drive(ctx, writer,
		store.client.ReadEvents(ctx, subjects.subject, eventsourcingdb.ReadEventsOptions{
			Recursive:  subjects.recursive,
			LowerBound: boundAfter(checkpoint),
		}), store.verify, progress, catchUpSize)
	if err != nil {
		return lastEventID, err
	}

	if lastEventID == "" {
		lastEventID = checkpoint
	}

	return lastEventID, nil
}

// boundAfter turns a checkpoint into a lower bound that excludes the event the
// checkpoint names, because that one has already been applied.
func boundAfter(eventID string) *eventsourcingdb.Bound {
	if eventID == "" {
		return nil
	}

	return &eventsourcingdb.Bound{ID: eventID, Type: eventsourcingdb.BoundTypeExclusive}
}

// drive applies the events in batches and returns the ID of the last one it
// applied. Every event is verified before it is applied, and one that fails
// ends the batch like an event the projection refuses.
func drive(
	ctx context.Context,
	writer projectionWriter,
	events iter.Seq2[eventsourcingdb.Event, error],
	verify func(eventsourcingdb.Event) error,
	progress *ProjectionRun,
	batchSize int,
) (string, error) {
	var lastEventID string
	inBatch := 0
	open := false

	for event, err := range events {
		if err != nil {
			failure := readFailure(ctx, err, "reading events")
			if open {
				failure = errors.Join(failure, writer.rollback(ctx))
			}

			return lastEventID, failure
		}

		if err := verify(event); err != nil {
			if open {
				err = errors.Join(err, writer.rollback(ctx))
			}

			return lastEventID, err
		}

		if !open {
			if err := writer.begin(ctx); err != nil {
				return lastEventID, err
			}
			open = true
		}

		if err := writer.apply(ctx, event); err != nil {
			// A failing rollback is reported alongside, never instead of, the
			// failure that caused it.
			return lastEventID, errors.Join(err, writer.rollback(ctx))
		}

		lastEventID = event.ID
		inBatch++

		if inBatch < batchSize {
			continue
		}

		if err := writer.commit(ctx, lastEventID); err != nil {
			return lastEventID, err
		}
		progress.committed(lastEventID)
		open = false
		inBatch = 0
	}

	if open {
		if err := writer.commit(ctx, lastEventID); err != nil {
			return lastEventID, err
		}
		progress.committed(lastEventID)
	}

	if ctx.Err() != nil {
		return lastEventID, contextEnded(ctx, "reading events")
	}

	return lastEventID, nil
}

// projectionWriter hides the three kinds of projection behind one shape, so
// that drive does not have to know them apart.
type projectionWriter interface {
	checkpoint(ctx context.Context) (string, error)
	begin(ctx context.Context) error
	apply(ctx context.Context, event eventsourcingdb.Event) error
	commit(ctx context.Context, lastEventID string) error
	rollback(ctx context.Context) error
}

// writerFor picks the writer for a projection that StartProjection drives. A
// transactional projection never gets here, because it is run by functions
// of its own.
func writerFor(projection Projection) projectionWriter {
	if resumable, ok := projection.(Resumable); ok {
		return &resumableWriter{projection: projection, resumable: resumable}
	}

	return &rebuildWriter{projection: projection}
}

// rebuildWriter keeps no checkpoint of its own, so every start reads from the
// beginning. Within a run, it remembers the last event it applied, so that
// catching up again after a lost stream does not apply any event twice.
type rebuildWriter struct {
	projection  Projection
	lastApplied string
}

func (w *rebuildWriter) checkpoint(context.Context) (string, error) { return w.lastApplied, nil }
func (w *rebuildWriter) begin(context.Context) error                { return nil }

// rollback has nothing to undo, because nothing was begun.
func (w *rebuildWriter) rollback(context.Context) error { return nil }

func (w *rebuildWriter) apply(ctx context.Context, event eventsourcingdb.Event) error {
	err := w.projection.Apply(ctx, event)
	if err != nil {
		return err
	}

	w.lastApplied = event.ID

	return nil
}

func (w *rebuildWriter) commit(context.Context, string) error { return nil }

// resumableWriter writes the checkpoint after the data, not with it. Within a
// run, it remembers the last event it applied, as rebuildWriter does, so that
// catching up again after a lost stream goes on from there, rather than from a
// checkpoint that a batch cut short did not get to save.
type resumableWriter struct {
	projection  Projection
	resumable   Resumable
	lastApplied string
}

func (w *resumableWriter) checkpoint(ctx context.Context) (string, error) {
	if w.lastApplied != "" {
		return w.lastApplied, nil
	}

	return w.resumable.Checkpoint(ctx)
}

func (w *resumableWriter) begin(context.Context) error { return nil }

// rollback has nothing to undo: the data is already written, and only the
// checkpoint is still missing. That is what makes this mode at-least-once
// across a crash.
func (w *resumableWriter) rollback(context.Context) error { return nil }

func (w *resumableWriter) apply(ctx context.Context, event eventsourcingdb.Event) error {
	err := w.projection.Apply(ctx, event)
	if err != nil {
		return err
	}

	w.lastApplied = event.ID

	return nil
}

func (w *resumableWriter) commit(ctx context.Context, lastEventID string) error {
	return w.resumable.SaveCheckpoint(ctx, lastEventID)
}

// transactionalWriter keeps data and checkpoint in step.
type transactionalWriter struct {
	projection Transactional
	tx         Tx
}

func (w *transactionalWriter) checkpoint(ctx context.Context) (string, error) {
	return w.projection.Checkpoint(ctx)
}

func (w *transactionalWriter) begin(ctx context.Context) error {
	tx, err := w.projection.Begin(ctx)
	if err != nil {
		return err
	}
	w.tx = tx
	return nil
}

// apply turns a panic into an error right where it happens, so that drive
// rolls the transaction back, as for an error, instead of leaving it open.
func (w *transactionalWriter) apply(ctx context.Context, event eventsourcingdb.Event) (err error) {
	defer recoverInto(&err)

	return w.tx.Apply(ctx, event)
}

func (w *transactionalWriter) commit(ctx context.Context, lastEventID string) error {
	err := w.tx.Commit(ctx, lastEventID)
	w.tx = nil
	return err
}

// rollback turns a panic into an error as well, so that drive reports it
// alongside the failure that made it roll back, never instead of it.
func (w *transactionalWriter) rollback(ctx context.Context) (err error) {
	if w.tx == nil {
		return nil
	}

	defer recoverInto(&err)

	err = w.tx.Rollback(ctx)
	w.tx = nil

	return err
}

// ProjectionFunc turns a plain function into a Projection, in the same way
// http.HandlerFunc does for handlers.
type ProjectionFunc func(ctx context.Context, event eventsourcingdb.Event) error

// Apply satisfies Projection.
func (f ProjectionFunc) Apply(ctx context.Context, event eventsourcingdb.Event) error {
	return f(ctx, event)
}

// View is a collection of items that a query can run over. Where the items
// live, in memory or in a database, is the implementation's business.
//
// All hands out a sequence rather than a slice, so that the steps of a query
// compose without materialising anything in between. Every element is an item
// and an error, since a view in a database fails not only before it reads the
// items, but also while it reads them, for example when its connection breaks
// halfway. Such a view hands out the error as an element of its own, with the
// zero value of the item, and stops. It also stops reading as soon as the
// caller does, which the query package relies on to read no further than it
// must.
type View[TItem any] interface {
	All(ctx context.Context) iter.Seq2[TItem, error]
}

// KeyedView is a view that finds a single item by its key, without running
// over all of them. Get returns false if there is no item with the key.
// InMemoryView is one.
type KeyedView[TKey comparable, TItem any] interface {
	View[TItem]
	Get(ctx context.Context, key TKey) (TItem, bool, error)
}

// Outcome tells what an operation on a single item of a view did with an
// event. A projection needs to tell the cases apart when an event about an
// item that does not exist means that something is wrong, while an event that
// has been applied before is to be expected after a restart. It also needs
// them when it does more than store an item, such as counting the items, which
// it does only for an item that was really added.
//
// Insert reports Added or AlreadyApplied, Upsert reports Added, Applied, or
// AlreadyApplied, and Update and Delete report Applied, Missing, or
// AlreadyApplied.
//
// The zero value is no outcome, which is what comes with an error.
type Outcome int

const (
	// Applied means that the event changed or removed an existing item.
	Applied Outcome = iota + 1

	// Missing means that there is no item with the key.
	Missing

	// AlreadyApplied means that the item has seen the event, or a newer one,
	// so the event changed nothing.
	AlreadyApplied

	// Added means that the event added a new item. It comes last, so that the
	// others keep the values they had before it.
	Added
)

// String names the outcome.
func (o Outcome) String() string {
	switch o {
	case Applied:
		return "applied"
	case Missing:
		return "missing"
	case AlreadyApplied:
		return "already applied"
	case Added:
		return "added"
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}
