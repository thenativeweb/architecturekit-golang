package architecturekit_test

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

func TestSubjects(t *testing.T) {
	t.Run("a subject has to start with a slash", func(t *testing.T) {
		assert.PanicsWithValue(t, `architecturekit: a subject starts with a slash, which "books" does not`, func() {
			architecturekit.SubjectTree("books")
		})
		assert.PanicsWithValue(t, `architecturekit: a subject starts with a slash, which "" does not`, func() {
			architecturekit.ExactSubject("")
		})
	})

	t.Run("a projection needs subjects from SubjectTree or ExactSubject", func(t *testing.T) {
		const message = "architecturekit: a projection reads the subjects of SubjectTree or ExactSubject, not none"

		for name, start := range map[string]func(){
			"CatchUpProjection": func() {
				_ = architecturekit.CatchUpProjection(t.Context(), nil, architecturekit.Subjects{}, &collector{})
			},
			"StartProjection": func() {
				architecturekit.StartProjection(t.Context(), nil, architecturekit.Subjects{}, &collector{})
			},
			"CatchUpTransactionalProjection": func() {
				_ = architecturekit.CatchUpTransactionalProjection(t.Context(), nil, architecturekit.Subjects{}, &transactionalCollector{})
			},
			"StartTransactionalProjection": func() {
				architecturekit.StartTransactionalProjection(t.Context(), nil, architecturekit.Subjects{}, &transactionalCollector{})
			},
		} {
			t.Run(name, func(t *testing.T) {
				assert.PanicsWithValue(t, message, start)
			})
		}
	})

	t.Run("an exact subject leaves out the subjects below it", func(t *testing.T) {
		store := requireStore(t)
		base := subjectFor(t)
		seed(t, base, 1)
		seed(t, base+"/a", 2)

		target := &collector{}
		require.NoError(t, architecturekit.CatchUpProjection(context.Background(), store, architecturekit.ExactSubject(base), target))

		assert.Len(t, target.IDs(), 1, "only the event of the subject itself")
	})
}

func TestNamed(t *testing.T) {
	t.Run("a run returns its name", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		run := architecturekit.StartProjection(ctx, architecturekit.NewStore(deadClient(t), "https://thenativeweb.io"),
			architecturekit.SubjectTree("/"), &collector{}, architecturekit.Named("catalog"))
		defer func() { cancel(); <-run.Done() }()

		assert.Equal(t, "catalog", run.Name())
	})

	t.Run("a run without a name returns none", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		run := architecturekit.StartProjection(ctx, architecturekit.NewStore(deadClient(t), "https://thenativeweb.io"),
			architecturekit.SubjectTree("/"), &collector{})
		defer func() { cancel(); <-run.Done() }()

		assert.Empty(t, run.Name())
	})

	t.Run("an empty name panics", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit: Named needs a name, not an empty one", func() {
			architecturekit.Named("")
		})
	})

	t.Run("naming a projection twice panics, also when catching up", func(t *testing.T) {
		const message = `architecturekit: the projection is named twice, as "catalog" and as "loans"`

		assert.PanicsWithValue(t, message, func() {
			architecturekit.StartProjection(t.Context(), nil, architecturekit.SubjectTree("/"), &collector{},
				architecturekit.Named("catalog"), architecturekit.Named("loans"))
		})
		assert.PanicsWithValue(t, message, func() {
			_ = architecturekit.CatchUpProjection(t.Context(), nil, architecturekit.SubjectTree("/"), &collector{},
				architecturekit.Named("catalog"), architecturekit.Named("loans"))
		})
		assert.PanicsWithValue(t, message, func() {
			_ = architecturekit.CatchUpTransactionalProjection(t.Context(), nil, architecturekit.SubjectTree("/"), &transactionalCollector{},
				architecturekit.Named("catalog"), architecturekit.Named("loans"))
		})
		assert.PanicsWithValue(t, message, func() {
			architecturekit.StartTransactionalProjection(t.Context(), nil, architecturekit.SubjectTree("/"), &transactionalCollector{},
				architecturekit.Named("catalog"), architecturekit.Named("loans"))
		})
	})
}

func TestReconnect(t *testing.T) {
	t.Run("tells the observer which projection reads again, why, how long it waits, and the how-manyth time", func(t *testing.T) {
		database := &fakeDatabase{
			endObserving: func(int) bool { return true },
		}
		observed := &reconnects{}

		store := architecturekit.NewStore(newFakeDatabase(t, database), "https://thenativeweb.io",
			architecturekit.WithReconnectDelays(time.Millisecond, 4*time.Millisecond),
			architecturekit.WithReconnectObserver(observed.observe),
		)

		ctx, cancel := context.WithCancel(context.Background())
		run := architecturekit.StartProjection(ctx, store, architecturekit.ExactSubject("/test"), &collector{},
			architecturekit.Named("catalog"))
		t.Cleanup(func() { cancel(); <-run.Done() })

		waitFor(t, func() bool { return observed.count() >= 3 })

		reports := observed.reports()[:3]
		for i, report := range reports {
			assert.Equal(t, "catalog", report.Projection)
			assert.Equal(t, "/test", report.Subject)
			assert.NoError(t, report.Err, "a stream that ended is reported without an error")
			assert.Equal(t, i+1, report.Attempt, "attempt %d", i)
		}

		assert.Equal(t, []time.Duration{time.Millisecond, 2 * time.Millisecond, 4 * time.Millisecond},
			[]time.Duration{reports[0].Delay, reports[1].Delay, reports[2].Delay})
	})

	t.Run("reports a projection without a name by its subject", func(t *testing.T) {
		database := &fakeDatabase{
			endObserving: func(int) bool { return true },
		}
		observed := &reconnects{}

		run, stop := startInBackground(t, reconnectingStore(newFakeDatabase(t, database), observed), &collector{})
		waitFor(t, func() bool { return observed.count() >= 1 })
		stop(t)

		report := observed.reports()[0]
		assert.Empty(t, report.Projection)
		assert.Equal(t, "/test", report.Subject)
		assert.Empty(t, run.Name())
	})
}
