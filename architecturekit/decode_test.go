package architecturekit_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

func TestDecode(t *testing.T) {
	traceParent := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	stored := eventsourcingdb.Event{
		ID:          "42",
		Time:        time.Date(2026, time.October, 1, 12, 0, 0, 0, time.UTC),
		Source:      "https://thenativeweb.io",
		Subject:     "/counter/a",
		Type:        "io.thenativeweb.test.incremented",
		TraceParent: &traceParent,
		Data:        json.RawMessage(`{"by":7}`),
	}

	t.Run("hands out the data and the metadata of an event", func(t *testing.T) {
		envelope, err := architecturekit.Decode[incremented](stored)
		require.NoError(t, err)

		assert.Equal(t, incremented{By: 7}, envelope.Data)
		assert.Equal(t, "42", envelope.ID)
		assert.Equal(t, stored.Time, envelope.Time)
		assert.Equal(t, "https://thenativeweb.io", envelope.Source)
		assert.Equal(t, "/counter/a", envelope.Subject)
		assert.Equal(t, "io.thenativeweb.test.incremented", envelope.Type)
		assert.Equal(t, &traceParent, envelope.TraceParent)
	})

	t.Run("refuses an event of another type", func(t *testing.T) {
		// Decoding into the wrong struct would otherwise leave its fields empty
		// without a word.
		envelope, err := architecturekit.Decode[reset](stored)

		require.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorContains(t, err, `event 42 is of type "io.thenativeweb.test.incremented", not "io.thenativeweb.test.reset"`)
		assert.Zero(t, envelope)
	})

	t.Run("refuses data that does not fit", func(t *testing.T) {
		broken := stored
		broken.Data = json.RawMessage(`{"by":"seven"}`)

		envelope, err := architecturekit.Decode[incremented](broken)

		require.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorContains(t, err, `decoding event 42 of type "io.thenativeweb.test.incremented"`)
		assert.Zero(t, envelope)
	})

	t.Run("decodes the events Execute returns", func(t *testing.T) {
		written, err := architecturekit.Execute(context.Background(), requireStore(t), counterDecider(),
			increment{subject: subjectFor(t), By: 3})
		require.NoError(t, err)
		require.Len(t, written, 1)

		envelope, err := architecturekit.Decode[incremented](written[0])
		require.NoError(t, err)
		assert.Equal(t, incremented{By: 3}, envelope.Data)
		assert.Equal(t, written[0].ID, envelope.ID)
	})
}
