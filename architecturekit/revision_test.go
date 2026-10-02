package architecturekit_test

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/architecturekittest"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

func TestCompareRevisions(t *testing.T) {
	t.Run("revisions are compared as numbers", func(t *testing.T) {
		tests := []struct {
			name        string
			left, right string
			want        int
		}{
			{"empty vs empty", "", "", 0},
			{"0 vs 0", "0", "0", 0},
			{"7 vs 7", "7", "7", 0},
			{"empty vs 0", "", "0", -1},
			{"0 vs empty", "0", "", 1},
			{"1 vs 2", "1", "2", -1},
			{"2 vs 1", "2", "1", 1},
			// The trap this exists for: as text, "10" sorts before "9".
			{"9 vs 10", "9", "10", -1},
			{"10 vs 9", "10", "9", 1},
			{"100 vs 99", "100", "99", 1},
			{"the largest vs the one below", "18446744073709551615", "18446744073709551614", 1},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				got, err := architecturekit.CompareRevisions(test.left, test.right)
				require.NoError(t, err)

				assert.Equal(t, test.want, got)
			})
		}
	})

	t.Run("something that is not a revision is refused", func(t *testing.T) {
		tests := []struct {
			name     string
			revision string
		}{
			{"letters", "abc"},
			{"a fraction", "1.5"},
			{"a negative number", "-1"},
			{"a leading space", " 1"},
			{"a hexadecimal number", "0x10"},
			{"a number beyond 64 bits", "99999999999999999999"},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				_, err := architecturekit.CompareRevisions(test.revision, "1")
				assert.ErrorIs(t, err, architecturekit.ErrNotARevision)

				// Both sides are checked, not only the first.
				_, err = architecturekit.CompareRevisions("1", test.revision)
				assert.ErrorIs(t, err, architecturekit.ErrNotARevision, "on the right")
			})
		}
	})
}

func TestInMemoryViewRevision(t *testing.T) {
	t.Run("a fresh view has seen nothing", func(t *testing.T) {
		assert.Empty(t, intView().Revision())
	})

	t.Run("Seen moves the revision forward only", func(t *testing.T) {
		view := intView()

		view.Seen("5")
		require.Equal(t, "5", view.Revision())

		// An event that arrives twice, or out of order after a restart, must not
		// pull the revision back.
		view.Seen("3")
		view.Seen("5")
		assert.Equal(t, "5", view.Revision())

		view.Seen("12")
		assert.Equal(t, "12", view.Revision())
	})

	t.Run("Seen ignores what is not a revision", func(t *testing.T) {
		view := intView()
		view.Seen("5")
		view.Seen("nonsense")

		assert.Equal(t, "5", view.Revision())
	})

	t.Run("waiting for a revision already reached returns at once", func(t *testing.T) {
		view := intView()
		view.Seen("10")

		tests := []struct {
			name     string
			revision string
		}{
			{"the empty revision", ""},
			{"an older revision", "9"},
			{"the same revision", "10"},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				ctx, cancel := context.WithTimeout(t.Context(), time.Second)

				assert.NoError(t, view.WaitFor(ctx, test.revision))

				cancel()
			})
		}
	})

	t.Run("waiting returns when the revision arrives", func(t *testing.T) {
		view := intView()

		waited := make(chan error, 1)
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()

			waited <- view.WaitFor(ctx, "3")
		}()

		// Everything below the wanted revision leaves the waiter waiting.
		view.Seen("1")
		view.Seen("2")

		select {
		case err := <-waited:
			require.Fail(t, "returned too early", err)
		case <-time.After(50 * time.Millisecond):
		}

		view.Seen("3")

		select {
		case err := <-waited:
			assert.NoError(t, err)
		case <-time.After(10 * time.Second):
			assert.Fail(t, "never returned")
		}
	})

	t.Run("waiting ends with the context", func(t *testing.T) {
		view := intView()

		ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
		defer cancel()

		err := view.WaitFor(ctx, "1")

		assert.ErrorIs(t, err, context.DeadlineExceeded)
	})

	t.Run("waiting for something that is not a revision fails", func(t *testing.T) {
		view := intView()

		assert.ErrorIs(t, view.WaitFor(t.Context(), "soon"), architecturekit.ErrNotARevision)
	})

	t.Run("many waiters are all woken up", func(t *testing.T) {
		view := intView()

		var group sync.WaitGroup
		failures := make(chan error, 10)

		for range 10 {
			group.Add(1)

			go func() {
				defer group.Done()

				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()

				if err := view.WaitFor(ctx, "4"); err != nil {
					failures <- err
				}
			}()
		}

		// Several steps, so that the waiters go around their loop more than once.
		for _, id := range []string{"1", "2", "3", "4"} {
			view.Seen(id)
		}

		group.Wait()
		close(failures)

		for err := range failures {
			assert.NoError(t, err, "a waiter failed")
		}
	})
}

// --- Tracking ---

func TestTracking(t *testing.T) {
	t.Run("records every event", func(t *testing.T) {
		view := intView()

		applied := 0
		projection := architecturekit.Tracking(architecturekit.ProjectionFunc(
			func(context.Context, eventsourcingdb.Event) error {
				applied++
				return nil
			},
		), view)

		architecturekittest.Project(t, projection,
			architecturekittest.StoredEvent("/counter/a", "0", incremented{By: 1}),
			architecturekittest.StoredEvent("/counter/a", "1", incremented{By: 1}),
		)

		assert.Equal(t, 2, applied)

		assert.Equal(t, "1", view.Revision())
	})

	t.Run("also records what the projection ignores", func(t *testing.T) {
		// This is the whole reason Tracking exists: a projection skips what does
		// not concern it, but a reader may be waiting for exactly that event.
		view := intView()

		projection := architecturekit.Tracking(architecturekit.ProjectionFunc(
			func(context.Context, eventsourcingdb.Event) error { return nil },
		), view)

		architecturekittest.Project(t, projection,
			architecturekittest.StoredEvent("/somewhere/else", "42", incremented{By: 1}),
		)

		assert.Equal(t, "42", view.Revision())
	})

	t.Run("records every event in every view", func(t *testing.T) {
		books, shelves, readers := intView(), intView(), intView()

		projection := architecturekit.Tracking(architecturekit.ProjectionFunc(
			func(context.Context, eventsourcingdb.Event) error { return nil },
		), books, shelves, readers)

		architecturekittest.Project(t, projection,
			architecturekittest.StoredEvent("/counter/a", "3", incremented{By: 1}),
			architecturekittest.StoredEvent("/counter/b", "4", incremented{By: 1}),
		)

		for name, view := range map[string]*architecturekit.InMemoryView[int, int]{
			"books": books, "shelves": shelves, "readers": readers,
		} {
			assert.Equal(t, "4", view.Revision(), "the view %s has to record the events", name)
		}
	})

	t.Run("refuses to track without a view", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit: Tracking needs at least one view to record the events in", func() {
			architecturekit.Tracking(&collector{})
		})
	})

	t.Run("does not record a failed event", func(t *testing.T) {
		view := intView()
		failed := errors.New("could not apply")

		projection := architecturekit.Tracking(architecturekit.ProjectionFunc(
			func(context.Context, eventsourcingdb.Event) error { return failed },
		), view)

		err := projection.Apply(t.Context(),
			architecturekittest.StoredEvent("/counter/a", "7", incremented{By: 1}))

		assert.ErrorIs(t, err, failed)

		assert.Empty(t, view.Revision(), "recorded although applying failed")
	})

	t.Run("keeps a projection that is rebuilt as it is", func(t *testing.T) {
		projection := architecturekit.Tracking(&collector{}, intView())

		architecturekittest.ExpectMode(t, projection, architecturekit.ModeRebuild)

		batched, ok := projection.(architecturekit.Batched)
		require.True(t, ok, "a tracked projection passes on its batch sizes")

		catchUp, live := batched.BatchSizes()
		assert.Equal(t, 1, catchUp)
		assert.Equal(t, 1, live)
	})

	t.Run("keeps a resumable projection resumable", func(t *testing.T) {
		// A wrapper that dropped the checkpoint would silently turn this into a
		// projection that is rebuilt on every start.
		target := &batchedResumingCollector{resumingCollector: resumingCollector{checkpoint: "7"}}
		projection := architecturekit.Tracking(target, intView())

		architecturekittest.ExpectMode(t, projection, architecturekit.ModeResumable)

		resumable := projection.(architecturekit.Resumable)

		checkpoint, err := resumable.Checkpoint(t.Context())
		require.NoError(t, err)
		assert.Equal(t, "7", checkpoint, "the checkpoint of the wrapped projection")

		require.NoError(t, resumable.SaveCheckpoint(t.Context(), "8"))
		assert.Equal(t, "8", target.checkpoint, "the checkpoint has to reach the wrapped projection")

		catchUp, live := projection.(architecturekit.Batched).BatchSizes()
		assert.Equal(t, 500, catchUp, "the batch sizes of the wrapped projection")
		assert.Equal(t, 10, live, "the batch sizes of the wrapped projection")
	})

	t.Run("refuses a projection that is transactional as well", func(t *testing.T) {
		assert.Panics(t, func() {
			architecturekit.Tracking(&transactionalWithApply{}, intView())
		}, "tracking would bypass the transactions")
	})

	t.Run("resumes from the checkpoint", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		seed(t, subject, 3)

		view := intView()
		target := &resumingCollector{}

		require.NoError(t, architecturekit.CatchUpProjection(t.Context(), store, architecturekit.ExactSubject(subject),
			architecturekit.Tracking(target, view)))

		require.NotEmpty(t, target.checkpoint, "a tracked resumable projection has to save its checkpoint")
		assert.Equal(t, target.checkpoint, view.Revision())
	})
}

// batchedResumingCollector is resumable and announces its own batch sizes.
type batchedResumingCollector struct {
	resumingCollector
}

func (c *batchedResumingCollector) BatchSizes() (int, int) { return 500, 10 }

// --- RevisionOf ---

func TestRevisionOf(t *testing.T) {
	t.Run("the revision of a write is its highest event ID", func(t *testing.T) {
		tests := []struct {
			name   string
			events []eventsourcingdb.Event
			want   string
		}{
			{"nothing written", nil, ""},
			{"one event", []eventsourcingdb.Event{{ID: "7"}}, "7"},
			{"two events", []eventsourcingdb.Event{{ID: "7"}, {ID: "8"}}, "8"},
			// Order is not assumed, and numbers decide, not text.
			{"out of order", []eventsourcingdb.Event{{ID: "10"}, {ID: "9"}}, "10"},
			{"nonsense is skipped", []eventsourcingdb.Event{{ID: "x"}, {ID: "3"}}, "3"},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				assert.Equal(t, test.want, architecturekit.RevisionOf(test.events))
			})
		}
	})
}

// intView is a view of numbers, each its own key.
func intView() *architecturekit.InMemoryView[int, int] {
	return architecturekit.NewInMemoryView(func(item int) int { return item })
}
