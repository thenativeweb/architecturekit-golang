package architecturekit

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"strconv"
	"time"

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
var ErrNotARevision = errors.New("not a revision")

// Revisioned is optional. A view implements it when it knows how far its
// projection has come, which lets a reader wait for a revision instead of
// polling. InMemoryView does.
type Revisioned interface {
	// Revision is the last event the view has seen, or the empty string while
	// it has seen none. A view that keeps its revision elsewhere, such as in a
	// database, reads it within the context. An error means that it could not
	// be read, so a reader fails rather than taking the empty string for a view
	// that has seen nothing.
	Revision(ctx context.Context) (string, error)

	// WaitFor returns nil once the view has reached the revision, at once if
	// it has already. If the context ends first, it returns the error of the
	// context, and for a revision that is not one, an error that wraps
	// ErrNotARevision.
	WaitFor(ctx context.Context, revision string) error
}

// RevisionSink is what a view implements to record how far its projection has
// come. InMemoryView does.
//
// It is meant for a view that keeps no transaction, since Tracking records
// the revision once the projection has applied the event, apart from its
// data. Where data and revision have to become durable together, as in a view
// in a database, the revision belongs inside the transaction, so the
// projection has to write it itself (see Tracking).
type RevisionSink interface {
	// Seen records an event as processed. Events may arrive more than once and
	// out of order after a restart, so an older ID never moves the revision
	// backwards.
	Seen(eventID string)
}

// Tracking wraps a projection so that the given views record every event that
// reaches it. Hand over every view the projection writes to, so that each of
// them can tell a reader how far it has come.
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
// is transactional as well is a programming error and panics. So does calling
// Tracking without any view, or with a nil projection or a nil view, which
// would otherwise only fail once the first event reaches the projection. A
// nil pointer or function counts as nil, such as a view that was declared but
// never created.
func Tracking(projection Projection, sinks ...RevisionSink) Projection {
	if isNil(projection) {
		panic("architecturekit: Tracking needs a projection, not nil")
	}

	refuseTransactional(projection)

	if len(sinks) == 0 {
		panic("architecturekit: Tracking needs at least one view to record the events in")
	}

	for i, sink := range sinks {
		if isNil(sink) {
			panic(fmt.Sprintf("architecturekit: Tracking needs views to record the events in, but view %d is nil", i))
		}
	}

	tracked := &trackedProjection{sinks: sinks, projection: projection}

	if resumable, ok := projection.(Resumable); ok {
		return &trackedResumable{trackedProjection: tracked, resumable: resumable}
	}

	return tracked
}

// trackedProjection records every event in every view once the projection has
// applied it.
type trackedProjection struct {
	sinks      []RevisionSink
	projection Projection
}

func (p *trackedProjection) Apply(ctx context.Context, event eventsourcingdb.Event) error {
	if err := p.projection.Apply(ctx, event); err != nil {
		return err
	}

	for _, sink := range p.sinks {
		sink.Seen(event.ID)
	}

	return nil
}

// CatchUpBatchSize passes on what the wrapped projection says, so that
// tracking does not change how often a checkpoint is written.
func (p *trackedProjection) CatchUpBatchSize() int {
	return batchSizeOf(p.projection)
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
// as decimal strings, and "10" sorts before "9" as text. A revision is a
// decimal number from 0 to 2^63-1, the range of the database's event IDs;
// anything else is refused with ErrNotARevision, as ParseRevision refuses it.
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

// revisionNumber reads a revision as a number, as ParseRevision does, except
// for the empty revision, which it takes for one that is not set. That has no
// number, which is what the second result says.
func revisionNumber(revision string) (uint64, bool, error) {
	if revision == "" {
		return 0, false, nil
	}

	number, err := ParseRevision(revision)
	if err != nil {
		return 0, false, err
	}

	return number, true, nil
}

// ParseRevision reads a revision as the number it stands for, by the rules
// that CompareRevisions applies: a revision is a decimal number from 0 to
// 2^63-1. Anything else is refused with an error that wraps ErrNotARevision
// and names the value, and so is the empty string, which CompareRevisions
// takes for a view that has seen nothing, but which is the ID of no event.
//
// Use it to check a revision that comes from outside, such as one a caller
// hands over, and to get at its number.
//
// The database numbers its events with signed 64-bit integers, so a revision
// ends at 2^63-1; a larger number is no event ID it could ever hand out, and
// it refuses one as a precondition.
func ParseRevision(revision string) (uint64, error) {
	number, err := strconv.ParseUint(revision, 10, 63)
	if err != nil {
		return 0, fmt.Errorf("%w: %q", ErrNotARevision, revision)
	}

	return number, nil
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

// ErrNotCaughtUp means that a view did not catch up with written events (see
// WaitForWritten), because the time ran out, the context ended, or the view
// could not be asked. The events were written, so the write has succeeded,
// and only the view lags behind.
//
// It belongs to no category. It is not ErrTransient, since trying the write
// again would store its events twice, and not ErrPermanent, since the view may
// still catch up. So it is usually answered as the success that the write is,
// with the revision of the events, for which the caller can wait itself.
var ErrNotCaughtUp = errors.New("not caught up")

// WaitForWritten waits until the view has seen the events that Execute or
// Write returned, for a step on the server that builds on what was just
// written, such as one that reads from the view what a command has changed:
//
//	writtenEvents, err := architecturekit.Execute(ctx, store, borrowBook, cmd)
//	if err != nil {
//	  // ...
//	}
//
//	err = architecturekit.WaitForWritten(ctx, catalog, writtenEvents, 5*time.Second)
//	if errors.Is(err, architecturekit.ErrNotCaughtUp) {
//	  // The command has succeeded, so answer with success all the same, and with
//	  // the revision, for which the caller can wait with Wait-For-Revision.
//	}
//
// It waits with WaitFor for RevisionOf the events, for at most timeout, and
// returns nil once the view has reached it, and at once if no events were
// written, since there is nothing to wait for then.
//
// Unlike Await of httpapi, which answers a caller with what the view holds,
// running out of time is an error, since the step needs what was written.
// Every error it returns wraps ErrNotCaughtUp, since the events were written
// in any case, and says why the view has not caught up, keeping the cause for
// errors.Is and errors.As:
//
//   - if timeout runs out while ctx has not ended, the error says so, as in
//     "not caught up: the events were written, but the view did not catch up
//     within 5s";
//   - if ctx ends first, it wraps the error of ctx, as in "not caught up: the
//     events were written, but waiting for the view ended: context canceled";
//   - if the view fails otherwise, it wraps the error of the view, as in "not
//     caught up: the events were written, but the view could not be asked:
//     ...".
//
// The write has succeeded, and trying it again would store the events twice,
// so check for ErrNotCaughtUp before a category, which the cause may belong
// to, such as ErrTransient of a view in a database, as StatusFor of httpapi
// does. A handler that waits after a command usually answers ErrNotCaughtUp
// with success, and with the revision of the events, for which the caller can
// wait itself.
//
// A nil view, including a nil pointer, is a programming error, and so is a
// timeout that is not positive, since a view that has to catch up with a
// write that has just happened takes some time, and waiting no time at all
// would fail almost always. WaitForWritten panics for both, before it looks at
// the events.
func WaitForWritten(ctx context.Context, view Revisioned, written []eventsourcingdb.Event, timeout time.Duration) error {
	if isNil(view) {
		panic("architecturekit: WaitForWritten needs a view, not nil")
	}
	if timeout <= 0 {
		panic(fmt.Sprintf("architecturekit: WaitForWritten needs a timeout that is positive, not %s", timeout))
	}

	if len(written) == 0 {
		return nil
	}

	waiting, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	err := view.WaitFor(waiting, RevisionOf(written))

	switch {
	case err == nil:
		return nil
	case ctx.Err() != nil:
		return fmt.Errorf("%w: the events were written, but waiting for the view ended: %w", ErrNotCaughtUp, ctx.Err())
	case waiting.Err() != nil && errors.Is(err, context.DeadlineExceeded):
		return fmt.Errorf("%w: the events were written, but the view did not catch up within %s", ErrNotCaughtUp, timeout)
	default:
		return fmt.Errorf("%w: the events were written, but the view could not be asked: %w", ErrNotCaughtUp, err)
	}
}
