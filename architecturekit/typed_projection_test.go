package architecturekit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// credits collects what a typed projection hands over for the credited type.
type credits struct {
	envelopes []architecturekit.Envelope[credited]
}

func (c *credits) projection() *architecturekit.TypedProjection {
	return architecturekit.NewProjection().
		On(func(_ context.Context, event architecturekit.Envelope[credited]) error {
			c.envelopes = append(c.envelopes, event)
			return nil
		})
}

func (c *credits) total() int {
	total := 0
	for _, envelope := range c.envelopes {
		total += envelope.Data.Amount
	}

	return total
}

func apply(t *testing.T, projection architecturekit.Projection, events ...eventsourcingdb.Event) error {
	t.Helper()

	for _, event := range events {
		if err := projection.Apply(context.Background(), event); err != nil {
			return err
		}
	}

	return nil
}

func TestTypedProjection(t *testing.T) {
	t.Run("hands over the metadata and the decoded data", func(t *testing.T) {
		traceParent := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
		traceState := "vendor=value"
		recorded := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

		target := &credits{}
		err := apply(t, target.projection(), eventsourcingdb.Event{
			ID:          "23",
			Time:        recorded,
			Source:      "https://thenativeweb.io",
			Subject:     "/ledger/1",
			Type:        (credited{}).EventType(),
			TraceParent: &traceParent,
			TraceState:  &traceState,
			Data:        json.RawMessage(`{"amount":42,"currency":"EUR"}`),
		})
		require.NoError(t, err)

		require.Len(t, target.envelopes, 1)

		got := target.envelopes[0]
		want := architecturekit.Envelope[credited]{
			ID:          "23",
			Time:        recorded,
			Source:      "https://thenativeweb.io",
			Subject:     "/ledger/1",
			Type:        (credited{}).EventType(),
			TraceParent: &traceParent,
			TraceState:  &traceState,
			Data:        credited{Amount: 42, Currency: "EUR"},
		}
		assert.Equal(t, want, got)
	})

	t.Run("hands each event to the handler of its type", func(t *testing.T) {
		var order []string

		projection := architecturekit.NewProjection().
			On(func(_ context.Context, event architecturekit.Envelope[incremented]) error {
				order = append(order, "incremented "+itoa(event.Data.By))
				return nil
			}).
			On(func(_ context.Context, event architecturekit.Envelope[reset]) error {
				order = append(order, "reset")
				return nil
			})

		err := apply(t, projection,
			stored((incremented{}).EventType(), `{"by":2}`),
			stored((reset{}).EventType(), `{}`),
			stored((incremented{}).EventType(), `{"by":5}`),
		)
		require.NoError(t, err)

		assert.Equal(t, "incremented 2, reset, incremented 5", strings.Join(order, ", "))
	})

	t.Run("skips events without a handler", func(t *testing.T) {
		target := &credits{}

		err := apply(t, target.projection(),
			stored("io.thenativeweb.test.unheard-of", `not even json`),
			stored((credited{}).EventType(), `{"amount":1,"currency":"EUR"}`),
		)
		require.NoError(t, err, "an event without a handler must be skipped")

		assert.Equal(t, 1, target.total())
	})

	t.Run("fails on data that does not match", func(t *testing.T) {
		target := &credits{}

		err := apply(t, target.projection(),
			stored((credited{}).EventType(), `{"amount":"not a number"}`))

		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorContains(t, err, "decoding")
	})

	t.Run("returns the error of a handler unchanged", func(t *testing.T) {
		errFull := errors.New("the view is full")

		projection := architecturekit.NewProjection().
			On(func(context.Context, architecturekit.Envelope[credited]) error {
				return errFull
			})

		err := apply(t, projection, stored((credited{}).EventType(), `{"amount":1}`))

		assert.ErrorIs(t, err, errFull)
		assert.NotErrorIs(t, err, architecturekit.ErrPermanent, "the kit must not categorise the error of a handler")
	})

	t.Run("panics on duplicate handler", func(t *testing.T) {
		ignore := func(context.Context, architecturekit.Envelope[credited]) error { return nil }

		assert.Panics(t, func() {
			architecturekit.NewProjection().On(ignore).On(ignore)
		})
	})

	t.Run("runs the upcasters", func(t *testing.T) {
		target := &credits{}

		err := apply(t, target.projection().UpcastWith(ledgerUpcasters()),
			stored("io.thenativeweb.test.credited.v1", `{"amount":10}`),
			stored("io.thenativeweb.test.credited.v2", `{"amount":5,"currency":"chf"}`),
			stored("io.thenativeweb.test.credited.v3", `{"amount":1,"currency":"USD"}`),
		)
		require.NoError(t, err)

		require.Len(t, target.envelopes, 3)
		assert.Equal(t, 16, target.total())

		first := target.envelopes[0]
		assert.Equal(t, (credited{}).EventType(), first.Type, "the v1 event did not arrive in its current shape")
		assert.Equal(t, "EUR", first.Data.Currency, "the v1 event did not arrive in its current shape")
	})

	t.Run("handles every event an upcaster produces", func(t *testing.T) {
		target := &credits{}
		projection := target.projection().UpcastWith(architecturekit.NewUpcasters().
			Upcast("io.thenativeweb.test.credited.batch", splitIntoTwo))

		require.NoError(t, apply(t, projection, stored("io.thenativeweb.test.credited.batch", `{}`)))

		assert.Len(t, target.envelopes, 2)
		assert.Equal(t, 7, target.total())
	})

	t.Run("reports a failing upcaster", func(t *testing.T) {
		target := &credits{}

		err := apply(t, target.projection().UpcastWith(ledgerUpcasters()),
			stored("io.thenativeweb.test.credited.v1", `not json`))

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "an upcaster failure is permanent")
		assert.ErrorContains(t, err, "upcasting")
	})

	t.Run("sees what the state sees", func(t *testing.T) {
		upcasters := ledgerUpcasters()

		state := architecturekit.NewState(0).
			Evolve(func(current int, event credited) int { return current + event.Amount }).
			UpcastWith(upcasters)
		target := &credits{}
		projection := target.projection().UpcastWith(upcasters)

		history := []eventsourcingdb.Event{
			stored("io.thenativeweb.test.credited.v1", `{"amount":10}`),
			stored("io.thenativeweb.test.credited.v2", `{"amount":5,"currency":"chf"}`),
		}

		decided, err := architecturekit.ReplayStored(state, history...)
		require.NoError(t, err)
		require.NoError(t, apply(t, projection, history...))

		assert.Equal(t, decided, target.total())
	})

	t.Run("UpcastWith panics when called twice", func(t *testing.T) {
		assert.Panics(t, func() {
			architecturekit.NewProjection().
				UpcastWith(architecturekit.NewUpcasters()).
				UpcastWith(architecturekit.NewUpcasters())
		})
	})

	t.Run("UpcastWith panics without a set", func(t *testing.T) {
		assert.Panics(t, func() {
			architecturekit.NewProjection().UpcastWith(nil)
		})
	})

	t.Run("is rebuilt unless embedded in a resumable type", func(t *testing.T) {
		assert.Equal(t, architecturekit.ModeRebuild, architecturekit.ModeOf(architecturekit.NewProjection()))

		target := &credits{}
		resumable := &resumableCredits{TypedProjection: target.projection()}

		assert.Equal(t, architecturekit.ModeResumable, architecturekit.ModeOf(resumable))

		require.NoError(t, apply(t, resumable, stored((credited{}).EventType(), `{"amount":3}`)))
		assert.Equal(t, 3, target.total(), "the embedded projection did not apply the event")
	})

	t.Run("CatchUpProjection with a typed projection", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		seed(t, subject, 3)

		var total int
		var subjects []string

		projection := architecturekit.NewProjection().
			On(func(_ context.Context, event architecturekit.Envelope[incremented]) error {
				total += event.Data.By
				subjects = append(subjects, event.Subject)
				return nil
			})

		require.NoError(t, architecturekit.CatchUpProjection(context.Background(), store, subject, false, projection))

		assert.Equal(t, 3, total)
		for _, got := range subjects {
			assert.Equal(t, subject, got)
		}
	})
}

// resumableCredits makes a typed projection resumable by embedding it.
type resumableCredits struct {
	*architecturekit.TypedProjection
	checkpoint string
}

func (r *resumableCredits) Checkpoint(context.Context) (string, error) {
	return r.checkpoint, nil
}

func (r *resumableCredits) SaveCheckpoint(_ context.Context, eventID string) error {
	r.checkpoint = eventID
	return nil
}
