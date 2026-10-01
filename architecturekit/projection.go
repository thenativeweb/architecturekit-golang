package architecturekit

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"time"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Projection is what RunProjection drives: it applies events. A projection
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
// Run it with RunTransactionalProjection. That way the compiler checks its
// method set, instead of a mismatch silently turning it into a projection that
// is rebuilt on every start.
type Transactional interface {
	Checkpoint(ctx context.Context) (string, error)
	Begin(ctx context.Context) (Tx, error)
}

// Tx is one unit of work of a transactional projection. Commit has to make the
// applied events and the last event ID durable together, or neither.
type Tx interface {
	Apply(ctx context.Context, event eventsourcingdb.Event) error
	Commit(ctx context.Context, lastEventID string) error
	Rollback(ctx context.Context) error
}

// Batched is optional, for resumable and transactional projections alike. It
// says how many events are applied before the checkpoint is written,
// separately for catching up and for live operation.
//
// Both default to one, which is the safe choice and works for projections that
// are not idempotent. Raising the catch-up size speeds up a rebuild by orders
// of magnitude, at the price of repeating up to that many events after a
// crash. Only the projection knows what its target costs.
type Batched interface {
	BatchSizes() (catchUp, live int)
}

// Mode is how RunProjection drives a projection, derived from the interfaces
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

// ModeOf reports how RunProjection will drive this projection. Log it at
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
		panic(fmt.Sprintf("architecturekit: %T is transactional, run it with RunTransactionalProjection", projection))
	}
}

// batchSizesOf takes any projection, because resumable and transactional ones
// can both be batched, and they share no interface.
func batchSizesOf(projection any) (catchUp, live int) {
	catchUp, live = 1, 1

	if batched, ok := projection.(Batched); ok {
		catchUp, live = batched.BatchSizes()
		if catchUp < 1 {
			catchUp = 1
		}
		if live < 1 {
			live = 1
		}
	}

	return catchUp, live
}

// CatchUpProjection applies everything that is already stored and returns.
// Use it to build a read model once, for a batch job or in a test, instead of
// following the stream.
//
// If the context ends first, it returns the context's error, so that a read
// model that is only partly built does not look complete.
//
// A projection that is transactional as well is a programming error and
// panics; use CatchUpTransactionalProjection for it.
func CatchUpProjection(
	ctx context.Context,
	store *Store,
	subject string,
	recursive bool,
	projection Projection,
) error {
	refuseTransactional(projection)

	catchUpSize, _ := batchSizesOf(projection)
	_, err := catchUp(ctx, store, subject, recursive, writerFor(projection), catchUpSize, nil)

	return err
}

// RunProjection drives a projection until the context ends. It first catches
// up from the checkpoint with a finite read, then follows the stream live.
//
// If reading fails, or if the database ends the stream, for example on a
// restart, it waits and catches up again from where it stopped, with a delay
// that grows with every attempt in a row (see WithReconnectDelays and
// WithReconnectObserver). Only a failure that trying again will not fix, such
// as an error from Apply, ends it with that error.
//
// Ending through the context is how a projection is stopped, so that returns
// no error.
//
// A projection that is transactional as well is a programming error and
// panics; use RunTransactionalProjection for it.
func RunProjection(
	ctx context.Context,
	store *Store,
	subject string,
	recursive bool,
	projection Projection,
) error {
	refuseTransactional(projection)

	return run(ctx, store, subject, recursive, writerFor(projection), projection, nil)
}

// CatchUpTransactionalProjection is CatchUpProjection for a transactional
// projection.
func CatchUpTransactionalProjection(
	ctx context.Context,
	store *Store,
	subject string,
	recursive bool,
	projection Transactional,
) error {
	catchUpSize, _ := batchSizesOf(projection)
	_, err := catchUp(ctx, store, subject, recursive,
		&transactionalWriter{projection: projection}, catchUpSize, nil)

	return err
}

// RunTransactionalProjection is RunProjection for a transactional projection.
// Every batch is applied within one transaction, which is committed together
// with the ID of its last event.
func RunTransactionalProjection(
	ctx context.Context,
	store *Store,
	subject string,
	recursive bool,
	projection Transactional,
) error {
	return run(ctx, store, subject, recursive,
		&transactionalWriter{projection: projection}, projection, nil)
}

// run catches up and then follows the stream. The batch sizes are read from
// the projection behind the writer, which is the one that knows its target.
// Progress is reported to the given run, which is nil for RunProjection.
func run(
	ctx context.Context,
	store *Store,
	subject string,
	recursive bool,
	writer projectionWriter,
	projection any,
	progress *ProjectionRun,
) error {
	catchUpSize, live := batchSizesOf(projection)
	delay := store.reconnectInitialDelay

	for {
		checkpointBefore, _ := writer.checkpoint(ctx)

		err := follow(ctx, store, subject, recursive, writer, catchUpSize, live, progress)
		if ctx.Err() != nil {
			return nil
		}
		if err != nil && !errors.Is(err, ErrTransient) {
			return err
		}

		progress.disrupted(err)

		// A session that got somewhere was healthy until it ended, so the next
		// failure is retried quickly again.
		if checkpointAfter, _ := writer.checkpoint(ctx); checkpointAfter != checkpointBefore {
			delay = store.reconnectInitialDelay
		}

		if store.reconnectObserver != nil {
			store.reconnectObserver(err, delay)
		}

		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}

		delay = min(2*delay, store.reconnectMaxDelay)
	}
}

// follow catches up from the checkpoint and then follows the stream live,
// until reading fails or the stream ends. It returns nil if the database
// ended the stream.
func follow(
	ctx context.Context,
	store *Store,
	subject string,
	recursive bool,
	writer projectionWriter,
	catchUpSize int,
	live int,
	progress *ProjectionRun,
) error {
	lastEventID, err := catchUp(ctx, store, subject, recursive, writer, catchUpSize, progress)
	if err != nil {
		return err
	}

	progress.caughtUpNow()

	_, err = drive(ctx, writer,
		store.client.ObserveEvents(ctx, subject, eventsourcingdb.ObserveEventsOptions{
			Recursive:  recursive,
			LowerBound: boundAfter(lastEventID),
		}), store.verify, progress, live)

	return err
}

// catchUp runs the finite phase and reports where it stopped.
func catchUp(
	ctx context.Context,
	store *Store,
	subject string,
	recursive bool,
	writer projectionWriter,
	catchUpSize int,
	progress *ProjectionRun,
) (string, error) {
	checkpoint, err := writer.checkpoint(ctx)
	if err != nil {
		return "", err
	}

	lastEventID, err := drive(ctx, writer,
		store.client.ReadEvents(ctx, subject, eventsourcingdb.ReadEventsOptions{
			Recursive:  recursive,
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

// writerFor picks the writer for a projection that RunProjection drives. A
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

// resumableWriter writes the checkpoint after the data, not with it.
type resumableWriter struct {
	projection Projection
	resumable  Resumable
}

func (w *resumableWriter) checkpoint(ctx context.Context) (string, error) {
	return w.resumable.Checkpoint(ctx)
}

func (w *resumableWriter) begin(context.Context) error { return nil }

// rollback has nothing to undo: the data is already written, and only the
// checkpoint is still missing. That is what makes this mode at-least-once.
func (w *resumableWriter) rollback(context.Context) error { return nil }

func (w *resumableWriter) apply(ctx context.Context, event eventsourcingdb.Event) error {
	return w.projection.Apply(ctx, event)
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

func (w *transactionalWriter) apply(ctx context.Context, event eventsourcingdb.Event) error {
	return w.tx.Apply(ctx, event)
}

func (w *transactionalWriter) commit(ctx context.Context, lastEventID string) error {
	err := w.tx.Commit(ctx, lastEventID)
	w.tx = nil
	return err
}

func (w *transactionalWriter) rollback(ctx context.Context) error {
	if w.tx == nil {
		return nil
	}

	err := w.tx.Rollback(ctx)
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
// compose without materialising anything in between.
type View[TItem any] interface {
	All(ctx context.Context) (iter.Seq[TItem], error)
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
// has been applied before is to be expected after a restart.
//
// The zero value is no outcome, which is what comes with an error.
type Outcome int

const (
	// Applied means that the event changed or removed the item.
	Applied Outcome = iota + 1

	// Missing means that there is no item with the key.
	Missing

	// AlreadyApplied means that the item has seen the event, or a newer one,
	// so the event changed nothing.
	AlreadyApplied
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
	default:
		return fmt.Sprintf("Outcome(%d)", int(o))
	}
}
