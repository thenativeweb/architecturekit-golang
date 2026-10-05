package architecturekit_test

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/architecturekittest"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// resetIgnoringState counts increments, and ignores resets, which in this domain
// only record that somebody pressed the button.
func resetIgnoringState() *architecturekit.State[counter] {
	return architecturekit.NewState(counter{}).
		Evolve(func(current counter, event incremented) counter {
			current.Total += event.By
			return current
		}).
		Ignore[reset]()
}

func TestIgnore(t *testing.T) {
	t.Run("takes an ignored event without changing", func(t *testing.T) {
		current, err := architecturekit.Replay(resetIgnoringState(),
			incremented{By: 3},
			reset{},
			incremented{By: 4},
		)
		require.NoError(t, err)
		assert.Equal(t, 7, current.Total)
	})

	t.Run("does not decode an ignored event", func(t *testing.T) {
		ignored := architecturekittest.StoredEvent("/counter/a", "1", reset{})
		ignored.Data = json.RawMessage(`"no object at all"`)

		current, err := architecturekit.ReplayStored(resetIgnoringState(),
			architecturekittest.StoredEvent("/counter/a", "0", incremented{By: 2}),
			ignored,
		)
		require.NoError(t, err)
		assert.Equal(t, 2, current.Total)
	})

	t.Run("keeps the schema of an ignored event", func(t *testing.T) {
		var eventTypes []string
		for _, schema := range resetIgnoringState().Schemas() {
			eventTypes = append(eventTypes, schema.EventType)
		}

		assert.Equal(t, []string{(incremented{}).EventType(), (reset{}).EventType()}, eventTypes)
	})

	t.Run("loads a subject with an ignored event", func(t *testing.T) {
		subject := subjectFor(t)
		_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{
			{Source: "https://thenativeweb.io", Subject: subject, Type: (incremented{}).EventType(), Data: incremented{By: 5}},
			{Source: "https://thenativeweb.io", Subject: subject, Type: (reset{}).EventType(), Data: reset{}},
			{Source: "https://thenativeweb.io", Subject: subject, Type: (incremented{}).EventType(), Data: incremented{By: 1}},
		}, nil)
		require.NoError(t, err)

		current, err := architecturekit.Load(context.Background(), requireStore(t), resetIgnoringState(), subject)
		require.NoError(t, err)
		assert.Equal(t, 6, current.Total)
	})

	t.Run("panics on an event type that is already registered", func(t *testing.T) {
		for name, build := range map[string]func(){
			"ignored after Evolve": func() {
				architecturekit.NewState(counter{}).
					Evolve(func(current counter, _ reset) counter { return current }).
					Ignore[reset]()
			},
			"Evolve after ignored": func() {
				architecturekit.NewState(counter{}).
					Ignore[reset]().
					Evolve(func(current counter, _ reset) counter { return current })
			},
			"ignored twice": func() {
				architecturekit.NewState(counter{}).Ignore[reset]().Ignore[reset]()
			},
		} {
			t.Run(name, func(t *testing.T) {
				assert.PanicsWithValue(t,
					`architecturekit: event type "io.thenativeweb.test.reset" is already registered on this state`, build)
			})
		}
	})

	t.Run("panics on a pointer as the event type, also if its EventType function has a pointer receiver", func(t *testing.T) {
		assert.PanicsWithValue(t,
			"architecturekit: Ignore needs the event type architecturekit_test.reset, not the pointer *architecturekit_test.reset",
			func() { architecturekit.NewState(counter{}).Ignore[*reset]() })
		assert.PanicsWithValue(t,
			"architecturekit: Ignore needs the event type architecturekit_test.pointed, not the pointer *architecturekit_test.pointed",
			func() { architecturekit.NewState(counter{}).Ignore[*pointed]() })
	})

	t.Run("panics on FromLatest for an ignored event type", func(t *testing.T) {
		assert.PanicsWithValue(t,
			`architecturekit: event type "io.thenativeweb.test.reset" has no Evolve rule on this state`,
			func() { resetIgnoringState().FromLatest[reset]() })
	})
}
