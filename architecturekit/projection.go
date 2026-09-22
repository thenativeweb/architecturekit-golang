package architecturekit

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"sync"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Projection is the minimum every projection fulfils: it applies events. A
// projection that implements nothing else is rebuilt from the beginning on
// every start, which is right for a view kept in memory.
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

// Transactional is optional. A projection implements it when its target can
// make data and checkpoint durable together, as a relational database can.
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

// Batched is optional. It says how many events are applied before the
// checkpoint is written, separately for catching up and for live operation.
//
// Both default to one, which is the safe choice and works for projections that
// are not idempotent. Raising the catch-up size speeds up a rebuild by orders
// of magnitude, at the price of repeating up to that many events after a
// crash. Only the projection knows what its target costs.
type Batched interface {
	BatchSizes() (catchUp, live int)
}

// Mode is how a projection is driven, derived from the interfaces it fulfils.
type Mode string

const (
	// ModeRebuild reads from the beginning on every start.
	ModeRebuild Mode = "rebuild"
	// ModeResumable resumes, without a guarantee that the checkpoint and the
	// data agree after a crash.
	ModeResumable Mode = "resumable"
	// ModeTransactional resumes, with data and checkpoint always in step.
	ModeTransactional Mode = "transactional"
)

// ModeOf reports how RunProjection will drive this projection. Log it at
// startup: if a projection is driven in rebuild mode against expectations, a
// method signature does not match the interface.
func ModeOf(projection Projection) Mode {
	switch projection.(type) {
	case Transactional:
		return ModeTransactional
	case Resumable:
		return ModeResumable
	default:
		return ModeRebuild
	}
}

func batchSizesOf(projection Projection) (catchUp, live int) {
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
func CatchUpProjection(
	ctx context.Context,
	store *Store,
	subject string,
	recursive bool,
	projection Projection,
) error {
	_, err := catchUp(ctx, store, subject, recursive, projection)

	return ignoreContextEnd(err)
}

// RunProjection drives a projection until the context ends. It first catches
// up from the checkpoint with a finite read, then follows the stream live.
//
// Ending through the context is how a projection is stopped, so that returns
// no error.
func RunProjection(
	ctx context.Context,
	store *Store,
	subject string,
	recursive bool,
	projection Projection,
) error {
	lastEventID, err := catchUp(ctx, store, subject, recursive, projection)
	if err != nil {
		return ignoreContextEnd(err)
	}

	_, live := batchSizesOf(projection)

	_, err = drive(ctx, writerFor(projection),
		store.client.ObserveEvents(ctx, subject, eventsourcingdb.ObserveEventsOptions{
			Recursive:  recursive,
			LowerBound: boundAfter(lastEventID),
		}), live)

	return ignoreContextEnd(err)
}

// catchUp runs the finite phase and reports where it stopped.
func catchUp(
	ctx context.Context,
	store *Store,
	subject string,
	recursive bool,
	projection Projection,
) (string, error) {
	catchUpSize, _ := batchSizesOf(projection)
	writer := writerFor(projection)

	checkpoint, err := writer.checkpoint(ctx)
	if err != nil {
		return "", err
	}

	lastEventID, err := drive(ctx, writer,
		store.client.ReadEvents(ctx, subject, eventsourcingdb.ReadEventsOptions{
			Recursive:  recursive,
			LowerBound: boundAfter(checkpoint),
		}), catchUpSize)
	if err != nil {
		return lastEventID, err
	}

	if lastEventID == "" {
		lastEventID = checkpoint
	}

	return lastEventID, nil
}

// ignoreContextEnd turns the expected end of a run into a clean return.
func ignoreContextEnd(err error) error {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}

	return err
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
// applied.
func drive(
	ctx context.Context,
	writer projectionWriter,
	events iter.Seq2[eventsourcingdb.Event, error],
	batchSize int,
) (string, error) {
	var lastEventID string
	inBatch := 0
	open := false

	for event, err := range events {
		if err != nil {
			failure := fmt.Errorf("%w: reading %v", ErrTransient, err)
			if open {
				failure = errors.Join(failure, writer.rollback(ctx))
			}

			return lastEventID, failure
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
		open = false
		inBatch = 0
	}

	if open {
		if err := writer.commit(ctx, lastEventID); err != nil {
			return lastEventID, err
		}
	}

	return lastEventID, ctx.Err()
}

// projectionWriter hides the three modes behind one shape, so that drive does
// not have to know them apart.
type projectionWriter interface {
	checkpoint(ctx context.Context) (string, error)
	begin(ctx context.Context) error
	apply(ctx context.Context, event eventsourcingdb.Event) error
	commit(ctx context.Context, lastEventID string) error
	rollback(ctx context.Context) error
}

func writerFor(projection Projection) projectionWriter {
	switch typed := projection.(type) {
	case Transactional:
		return &transactionalWriter{projection: typed}
	case Resumable:
		return &resumableWriter{projection: projection, resumable: typed}
	default:
		return &rebuildWriter{projection: projection}
	}
}

// rebuildWriter keeps no checkpoint, so every start reads from the beginning.
type rebuildWriter struct{ projection Projection }

func (w *rebuildWriter) checkpoint(context.Context) (string, error) { return "", nil }
func (w *rebuildWriter) begin(context.Context) error                { return nil }

// rollback has nothing to undo, because nothing was begun.
func (w *rebuildWriter) rollback(context.Context) error { return nil }

func (w *rebuildWriter) apply(ctx context.Context, event eventsourcingdb.Event) error {
	return w.projection.Apply(ctx, event)
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

// ItemView holds the items of a read model in memory. It is the one view the
// kit ships, and because it keeps no checkpoint, its projection is rebuilt on
// every start.
//
// Note that this is the stored shape, not the answer to a query. Use the
// query package to filter, order and project it into whatever an answer needs.
type ItemView[TItem any] struct {
	mutex sync.RWMutex
	items []TItem

	// revision is how far the projection has come, and changed is closed
	// whenever it moves, so that waiting readers wake up.
	revision string
	changed  chan struct{}
}

// NewItemView creates an empty view.
func NewItemView[TItem any]() *ItemView[TItem] {
	return &ItemView[TItem]{changed: make(chan struct{})}
}

// Revision is the last event the view has seen.
func (v *ItemView[TItem]) Revision() string {
	v.mutex.RLock()
	defer v.mutex.RUnlock()

	return v.revision
}

// Seen records an event as processed. An event the view has already passed
// does not move the revision backwards, which is what makes it safe to apply
// the same event twice after a restart.
func (v *ItemView[TItem]) Seen(eventID string) {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	if newer, err := CompareRevisions(eventID, v.revision); err != nil || newer <= 0 {
		return
	}

	v.revision = eventID

	// Everyone waiting is woken by closing the channel; the next waiter gets a
	// fresh one.
	close(v.changed)
	v.changed = make(chan struct{})
}

// WaitFor returns once the view has reached the revision, or when the context
// ends.
func (v *ItemView[TItem]) WaitFor(ctx context.Context, revision string) error {
	for {
		v.mutex.RLock()
		current, changed := v.revision, v.changed
		v.mutex.RUnlock()

		reached, err := CompareRevisions(current, revision)
		if err != nil {
			return err
		}
		if reached >= 0 {
			return nil
		}

		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Insert adds an item.
func (v *ItemView[TItem]) Insert(item TItem) {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	v.items = append(v.items, item)
}

// Update changes every matching item and reports how many it changed.
func (v *ItemView[TItem]) Update(match func(TItem) bool, update func(*TItem)) int {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	changed := 0
	for i := range v.items {
		if !match(v.items[i]) {
			continue
		}
		update(&v.items[i])
		changed++
	}

	return changed
}

// Upsert changes every matching item, or inserts the given one when nothing
// matched. It reports how many items it changed.
func (v *ItemView[TItem]) Upsert(match func(TItem) bool, update func(*TItem), item TItem) int {
	if changed := v.Update(match, update); changed > 0 {
		return changed
	}

	v.mutex.Lock()
	defer v.mutex.Unlock()

	v.items = append(v.items, item)

	return 0
}

// Delete removes every matching item and reports how many it removed.
func (v *ItemView[TItem]) Delete(match func(TItem) bool) int {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	kept := v.items[:0]
	removed := 0

	for _, item := range v.items {
		if match(item) {
			removed++
			continue
		}
		kept = append(kept, item)
	}
	v.items = kept

	return removed
}

// All hands out every item. The items are copied under the lock, so a query
// can take its time without blocking the projection that feeds the view.
func (v *ItemView[TItem]) All(context.Context) (iter.Seq[TItem], error) {
	v.mutex.RLock()
	items := slices.Clone(v.items)
	v.mutex.RUnlock()

	return slices.Values(items), nil
}
