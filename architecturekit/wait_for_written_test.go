package architecturekit_test

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// writtenUpTo stands for the events a write returned, the last of which has
// the given ID.
func writtenUpTo(id string) []eventsourcingdb.Event {
	return []eventsourcingdb.Event{{ID: "1"}, {ID: id}}
}

// viewAt returns a view that has seen the event with the given ID.
func viewAt(revision string) *architecturekit.InMemoryView[string, string] {
	view := architecturekit.NewInMemoryView(func(item string) string { return item })
	view.Seen(revision)

	return view
}

// brokenView is an error of a view that carries more than its text.
type brokenView struct {
	reason string
}

func (e *brokenView) Error() string { return e.reason }

// waitingView is a view whose WaitFor is the given function. It embeds the
// interface for the rest of it, which WaitForWritten does not call.
type waitingView struct {
	architecturekit.Revisioned

	waitFor func(ctx context.Context, revision string) error
}

func (v waitingView) WaitFor(ctx context.Context, revision string) error {
	return v.waitFor(ctx, revision)
}

func TestWaitForWritten(t *testing.T) {
	t.Run("returns nil at once if the view has seen the events already", func(t *testing.T) {
		assert.NoError(t, architecturekit.WaitForWritten(context.Background(), viewAt("5"), writtenUpTo("5"), time.Hour))
		assert.NoError(t, architecturekit.WaitForWritten(context.Background(), viewAt("6"), writtenUpTo("5"), time.Hour))
	})

	t.Run("returns nil although the context has ended, if the view has seen the events already", func(t *testing.T) {
		assert.NoError(t, architecturekit.WaitForWritten(endedContext(t), viewAt("5"), writtenUpTo("5"), time.Hour))
	})

	t.Run("waits for the highest ID among the events", func(t *testing.T) {
		var waitedFor string
		view := waitingView{waitFor: func(_ context.Context, revision string) error {
			waitedFor = revision
			return nil
		}}

		written := []eventsourcingdb.Event{{ID: "9"}, {ID: "10"}, {ID: "2"}}
		require.NoError(t, architecturekit.WaitForWritten(context.Background(), view, written, time.Hour))

		assert.Equal(t, "10", waitedFor)
	})

	t.Run("returns nil once the view has caught up", func(t *testing.T) {
		view := viewAt("4")
		go func() {
			time.Sleep(20 * time.Millisecond)
			view.Seen("5")
		}()

		assert.NoError(t, architecturekit.WaitForWritten(waitingContext(t), view, writtenUpTo("5"), 5*time.Second))
	})

	t.Run("returns nil without waiting if no events were written", func(t *testing.T) {
		view := waitingView{waitFor: func(context.Context, string) error {
			return errors.New("the view must not be asked")
		}}

		assert.NoError(t, architecturekit.WaitForWritten(context.Background(), view, nil, time.Hour))
		assert.NoError(t, architecturekit.WaitForWritten(context.Background(), view, []eventsourcingdb.Event{}, time.Hour))
		assert.NoError(t, architecturekit.WaitForWritten(endedContext(t), view, nil, time.Hour))
	})

	t.Run("fails with ErrNotCaughtUp if the view does not catch up in time", func(t *testing.T) {
		started := time.Now()

		err := architecturekit.WaitForWritten(waitingContext(t), viewAt("4"), writtenUpTo("5"), 50*time.Millisecond)

		require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp)
		assert.EqualError(t, err, "not caught up: the events were written, but the view did not catch up within 50ms")
		assert.GreaterOrEqual(t, time.Since(started), 50*time.Millisecond, "it has to wait for the full time")
	})

	t.Run("belongs to no category if the view does not catch up in time, since the write has succeeded", func(t *testing.T) {
		err := architecturekit.WaitForWritten(waitingContext(t), viewAt("4"), writtenUpTo("5"), 20*time.Millisecond)

		require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp)
		assert.NotErrorIs(t, err, architecturekit.ErrTransient, "trying the write again would store its events twice")
		assert.NotErrorIs(t, err, architecturekit.ErrPermanent, "the view may still catch up")
		assert.NotErrorIs(t, err, architecturekit.ErrDomain)
		assert.NotErrorIs(t, err, architecturekit.ErrOutcomeUnknown, "the write has succeeded")
		assert.NotErrorIs(t, err, context.DeadlineExceeded, "only the end of the context of the caller may look like one")
		assert.Equal(t, architecturekit.ErrNotCaughtUp, errors.Unwrap(err), "it wraps nothing else")
		assert.EqualError(t, architecturekit.ErrNotCaughtUp, "not caught up", "the text leaves out the name of the package")
	})

	t.Run("names the timeout the way a duration prints", func(t *testing.T) {
		err := architecturekit.WaitForWritten(waitingContext(t), viewAt("4"), writtenUpTo("5"), 1500*time.Microsecond)

		assert.EqualError(t, err, "not caught up: the events were written, but the view did not catch up within 1.5ms")
	})

	t.Run("fails with ErrNotCaughtUp that wraps the error of the context if it ends first", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		go func() {
			time.Sleep(20 * time.Millisecond)
			cancel()
		}()

		err := architecturekit.WaitForWritten(ctx, viewAt("4"), writtenUpTo("5"), 5*time.Second)

		require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp, "the events were written all the same")
		assert.ErrorIs(t, err, context.Canceled, "errors.Is has to find the end of the context")
		assert.EqualError(t, err, "not caught up: the events were written, but waiting for the view ended: context canceled")
	})

	t.Run("fails with ErrNotCaughtUp that wraps the error of the context if its deadline comes first", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()

		err := architecturekit.WaitForWritten(ctx, viewAt("4"), writtenUpTo("5"), 5*time.Second)

		require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp, "the events were written all the same")
		assert.ErrorIs(t, err, context.DeadlineExceeded, "errors.Is has to find the end of the context")
		assert.EqualError(t, err, "not caught up: the events were written, but waiting for the view ended: context deadline exceeded",
			"the deadline of the caller is not the timeout")
	})

	t.Run("fails with ErrNotCaughtUp that wraps the error of the context that has ended already", func(t *testing.T) {
		err := architecturekit.WaitForWritten(endedContext(t), viewAt("4"), writtenUpTo("5"), time.Hour)

		require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.EqualError(t, err, "not caught up: the events were written, but waiting for the view ended: context deadline exceeded")
	})

	t.Run("wraps the error of the context rather than what the view made of it, if the context ends first", func(t *testing.T) {
		// A view in a database may report the end of the context in words of
		// its own, but the context is the cause.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		view := waitingView{waitFor: func(ctx context.Context, _ string) error {
			return fmt.Errorf("polling the revision: %w", ctx.Err())
		}}

		err := architecturekit.WaitForWritten(ctx, view, writtenUpTo("5"), time.Hour)

		require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp)
		assert.ErrorIs(t, err, context.Canceled)
		assert.EqualError(t, err, "not caught up: the events were written, but waiting for the view ended: context canceled")
	})

	t.Run("fails with ErrNotCaughtUp that wraps any other error of the view", func(t *testing.T) {
		broken := &brokenView{reason: "the index is gone"}

		for name, waitFor := range map[string]func(ctx context.Context, revision string) error{
			"at once": func(context.Context, string) error {
				return broken
			},
			"once the timeout has run out": func(ctx context.Context, _ string) error {
				<-ctx.Done()
				return broken
			},
		} {
			t.Run(name, func(t *testing.T) {
				err := architecturekit.WaitForWritten(waitingContext(t), waitingView{waitFor: waitFor}, writtenUpTo("5"), 20*time.Millisecond)

				require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp, "the events were written all the same")
				assert.ErrorIs(t, err, broken, "errors.Is has to find the error of the view")
				found, isFound := errors.AsType[*brokenView](err)
				require.True(t, isFound, "errors.As has to find the error of the view")
				assert.Same(t, broken, found)
				assert.EqualError(t, err, "not caught up: the events were written, but the view could not be asked: the index is gone")
			})
		}
	})

	t.Run("keeps the category of an error of the view, behind ErrNotCaughtUp", func(t *testing.T) {
		// A view in a database may fail to read its revision for now.
		view := waitingView{waitFor: func(context.Context, string) error {
			return fmt.Errorf("%w: polling the revision: connection refused", architecturekit.ErrTransient)
		}}

		err := architecturekit.WaitForWritten(waitingContext(t), view, writtenUpTo("5"), time.Hour)

		require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp, "the write has succeeded, so it must not look like a failed one")
		assert.ErrorIs(t, err, architecturekit.ErrTransient, "errors.Is has to find the category of the cause")
		assert.EqualError(t, err,
			"not caught up: the events were written, but the view could not be asked: transient failure: polling the revision: connection refused")
	})

	t.Run("fails with ErrNotCaughtUp that wraps an error of the view that looks like a timeout, if the time has not run out", func(t *testing.T) {
		// A view backed by a database may time out on its own.
		failure := fmt.Errorf("reading the revision: %w", context.DeadlineExceeded)
		view := waitingView{waitFor: func(context.Context, string) error {
			return failure
		}}

		err := architecturekit.WaitForWritten(waitingContext(t), view, writtenUpTo("5"), time.Hour)

		require.ErrorIs(t, err, architecturekit.ErrNotCaughtUp)
		assert.ErrorIs(t, err, failure)
		assert.EqualError(t, err, "not caught up: the events were written, but the view could not be asked: reading the revision: context deadline exceeded",
			"the error of the view is not the timeout")
	})

	t.Run("waits for what Execute wrote in a view whose projection is tracked", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		view := architecturekit.NewInMemoryView(func(item string) string { return item })

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		run := architecturekit.StartProjection(ctx, store, architecturekit.ExactSubject(subject),
			architecturekit.Tracking(architecturekit.ProjectionFunc(func(context.Context, eventsourcingdb.Event) error {
				return nil
			}), view))
		require.NoError(t, architecturekit.WaitCaughtUp(waitingContext(t), run))

		written, err := architecturekit.Execute(context.Background(), store, counterDecider(), increment{subject: subject, By: 1})
		require.NoError(t, err)

		require.NoError(t, architecturekit.WaitForWritten(waitingContext(t), view, written, 5*time.Second))

		assert.NoError(t, view.WaitFor(endedContext(t), written[0].ID), "the view has to have seen the event")
	})

	t.Run("panics for a nil view, before it looks at the events", func(t *testing.T) {
		var neverCreated *architecturekit.InMemoryView[string, string]

		for name, view := range map[string]architecturekit.Revisioned{
			"nil":         nil,
			"nil pointer": neverCreated,
		} {
			t.Run(name, func(t *testing.T) {
				assert.PanicsWithValue(t, "architecturekit: WaitForWritten needs a view, not nil", func() {
					_ = architecturekit.WaitForWritten(context.Background(), view, nil, time.Second)
				})
			})
		}
	})

	t.Run("panics for a timeout that is not positive, before it looks at the events", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit: WaitForWritten needs a timeout that is positive, not -1s", func() {
			_ = architecturekit.WaitForWritten(context.Background(), viewAt("5"), nil, -time.Second)
		})
		assert.PanicsWithValue(t, "architecturekit: WaitForWritten needs a timeout that is positive, not 0s", func() {
			_ = architecturekit.WaitForWritten(context.Background(), viewAt("5"), writtenUpTo("5"), 0)
		})
		assert.NotPanics(t, func() {
			_ = architecturekit.WaitForWritten(context.Background(), viewAt("5"), writtenUpTo("5"), time.Nanosecond)
		})
	})
}
