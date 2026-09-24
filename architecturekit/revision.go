package architecturekit

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// A revision says how far a read model has come: it is the ID of the last
// event its projection has seen. Because the database hands out event IDs as
// one ascending sequence across all streams, a reader can name the revision it
// needs and wait for it.
//
// This is the read side's counterpart to a write precondition. A command says
// "I decided on this revision"; a query says "I want to see at least this
// revision". Together they let a caller read its own writes without guessing
// how long a projection takes.

// ErrNotARevision means a revision could not be read as one.
var ErrNotARevision = errors.New("architecturekit: not a revision")

// Revisioned is optional. A view implements it when it knows how far its
// projection has come, which lets a reader wait for a revision instead of
// polling. ItemView does.
type Revisioned interface {
	// Revision is the last event the view has seen, or the empty string while
	// it has seen none.
	Revision() string

	// WaitFor returns once the view has reached the revision, or when the
	// context ends. Waiting for a revision the view already passed returns
	// immediately.
	WaitFor(ctx context.Context, revision string) error
}

// RevisionSink is what a view implements to record how far its projection has
// come. ItemView does.
type RevisionSink interface {
	// Seen records an event as processed. Events may arrive more than once and
	// out of order after a restart, so an older ID never moves the revision
	// backwards.
	Seen(eventID string)
}

// Tracking wraps a projection so that the view records every event that
// reaches it.
//
// It records the event even when the projection ignores it, and that is the
// point: a projection skips what does not concern it, but a reader may be
// waiting for exactly that event's ID. A view whose revision only counted the
// events it applied would leave such a reader waiting forever.
//
// The wrapped projection keeps its mode: a resumable one stays resumable, and
// its batch sizes are kept too. A wrapper that dropped them would turn it into
// a projection that is rebuilt on every start, and nothing would notice.
//
// Use it with a view that keeps no transaction. Where data and revision have
// to become durable together, the revision belongs inside the transaction, and
// the projection has to write it itself. That is why a transactional
// projection cannot be tracked: it has no Apply to wrap, and a projection that
// is transactional as well is a programming error and panics.
func Tracking(sink RevisionSink, projection Projection) Projection {
	refuseTransactional(projection)

	tracked := &trackedProjection{sink: sink, projection: projection}

	if resumable, ok := projection.(Resumable); ok {
		return &trackedResumable{trackedProjection: tracked, resumable: resumable}
	}

	return tracked
}

// trackedProjection records every event once the projection has applied it.
type trackedProjection struct {
	sink       RevisionSink
	projection Projection
}

func (p *trackedProjection) Apply(ctx context.Context, event eventsourcingdb.Event) error {
	if err := p.projection.Apply(ctx, event); err != nil {
		return err
	}

	p.sink.Seen(event.ID)

	return nil
}

// BatchSizes passes on what the wrapped projection says, so that tracking does
// not change how often a checkpoint is written.
func (p *trackedProjection) BatchSizes() (catchUp, live int) {
	return batchSizesOf(p.projection)
}

// trackedResumable is a tracked projection that keeps its checkpoint.
type trackedResumable struct {
	*trackedProjection
	resumable Resumable
}

func (p *trackedResumable) Checkpoint(ctx context.Context) (string, error) {
	return p.resumable.Checkpoint(ctx)
}

func (p *trackedResumable) SaveCheckpoint(ctx context.Context, eventID string) error {
	return p.resumable.SaveCheckpoint(ctx, eventID)
}

// CompareRevisions orders two revisions the way cmp.Compare orders numbers.
// The empty revision comes before every other one, because a view that has
// seen nothing is behind a view that has seen something.
//
// Revisions are compared as numbers, never as text: the database writes them
// as decimal strings, and "10" sorts before "9" as text.
func CompareRevisions(left, right string) (int, error) {
	// Both sides are read before either is compared, so that a revision that
	// is not one is refused even when the other side is empty.
	leftNumber, leftSet, err := revisionNumber(left)
	if err != nil {
		return 0, err
	}

	rightNumber, rightSet, err := revisionNumber(right)
	if err != nil {
		return 0, err
	}

	// An unset revision comes before every other one. It cannot be folded into
	// the number, because "0" is a real event ID: the database counts from
	// zero.
	switch {
	case !leftSet && !rightSet:
		return 0, nil
	case !leftSet:
		return -1, nil
	case !rightSet:
		return 1, nil
	}

	return cmp.Compare(leftNumber, rightNumber), nil
}

// revisionNumber reads a revision as a number. A revision that is not set has
// no number, which is what the second result says.
func revisionNumber(revision string) (uint64, bool, error) {
	if revision == "" {
		return 0, false, nil
	}

	number, err := strconv.ParseUint(revision, 10, 64)
	if err != nil {
		return 0, false, fmt.Errorf("%w: %q", ErrNotARevision, revision)
	}

	return number, true, nil
}

// RevisionOf is the highest event ID among written events, which is the
// revision a caller has to see before its own write shows up in a read model.
// It is the empty string when nothing was written.
func RevisionOf(events []eventsourcingdb.Event) string {
	highest := ""

	for _, event := range events {
		if newer, err := CompareRevisions(event.ID, highest); err == nil && newer > 0 {
			highest = event.ID
		}
	}

	return highest
}
