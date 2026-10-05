package architecturekit_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// eventsIn reads the events of a subject straight from the database.
func eventsIn(t *testing.T, subject string) []eventsourcingdb.Event {
	t.Helper()

	var events []eventsourcingdb.Event
	for event, err := range rawClient(t).ReadEvents(context.Background(), subject, eventsourcingdb.ReadEventsOptions{}) {
		require.NoError(t, err)
		events = append(events, event)
	}

	return events
}

func TestWrite(t *testing.T) {
	t.Run("writes to several subjects at once", func(t *testing.T) {
		first, second := subjectFor(t)+"/first", subjectFor(t)+"/second"

		written, err := architecturekit.Write(context.Background(), requireStore(t), []architecturekit.EventOn{
			{Subject: first, Event: incremented{By: 1}},
			{Subject: second, Event: incremented{By: 2}},
			{Subject: first, Event: reset{}},
		}, architecturekit.Unconditionally())
		require.NoError(t, err)

		require.Len(t, written, 3)
		assert.Equal(t, first, written[0].Subject)
		assert.Equal(t, second, written[1].Subject)
		assert.Equal(t, (reset{}).EventType(), written[2].Type)
		assert.Equal(t, "https://thenativeweb.io", written[0].Source, "the events carry the source of the store")

		assert.Len(t, eventsIn(t, first), 2)
		assert.Len(t, eventsIn(t, second), 1)
	})

	t.Run("writes nothing if a precondition does not hold and calls that a conflict", func(t *testing.T) {
		taken, other := subjectFor(t)+"/taken", subjectFor(t)+"/other"
		seed(t, taken, 1)

		written, err := architecturekit.Write(context.Background(), requireStore(t), []architecturekit.EventOn{
			{Subject: taken, Event: incremented{By: 1}},
			{Subject: other, Event: incremented{By: 1}},
		}, architecturekit.Require(eventsourcingdb.NewIsSubjectPristinePrecondition(taken)))

		require.ErrorIs(t, err, architecturekit.ErrConflict)
		assert.Nil(t, written)
		assert.Len(t, eventsIn(t, taken), 1, "the taken subject must keep only its own event")
		assert.Empty(t, eventsIn(t, other), "no event may be written if one of them can not be")
	})

	t.Run("reports a schema violation as permanent", func(t *testing.T) {
		store := requireStore(t)
		require.NoError(t, architecturekit.RegisterSchemas(context.Background(), store, []architecturekit.EventSchema{{
			EventType: (labelled{}).EventType(),
			Schema:    (labelled{}).Schema(),
		}}))

		_, err := architecturekit.Write(context.Background(), store, []architecturekit.EventOn{
			{Subject: subjectFor(t), Event: labelled{Label: ""}},
		}, architecturekit.Unconditionally())

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a schema violation is permanent")
		assert.NotErrorIs(t, err, architecturekit.ErrConflict, "a schema violation is not a conflict")
	})

	t.Run("writes nothing for no events", func(t *testing.T) {
		written, err := architecturekit.Write(context.Background(), requireStore(t), nil, architecturekit.Unconditionally())

		require.NoError(t, err)
		assert.Nil(t, written)
	})

	t.Run("writes nothing once the context has ended", func(t *testing.T) {
		subject := subjectFor(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := architecturekit.Write(ctx, requireStore(t), []architecturekit.EventOn{
			{Subject: subject, Event: incremented{By: 1}},
		}, architecturekit.Unconditionally())

		require.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, eventsIn(t, subject))
	})

	t.Run("refuses what it can not honor without writing anything", func(t *testing.T) {
		subject := subjectFor(t)
		event := []architecturekit.EventOn{{Subject: subject, Event: incremented{By: 1}}}

		for _, test := range []struct {
			name          string
			events        []architecturekit.EventOn
			preconditions []architecturekit.Precondition
			says          string
		}{
			{"no preconditions", event, nil, "declares no preconditions"},
			{"OnStateRead", event, []architecturekit.Precondition{architecturekit.OnStateRead()},
				"a write reads no state, so OnStateRead has nothing to guard, use OnPristineSubject, OnPopulatedSubject, OnEventID, or Require instead"},
			{"Unconditionally with another precondition", event, []architecturekit.Precondition{
				architecturekit.Unconditionally(),
				architecturekit.Require(eventsourcingdb.NewIsSubjectPristinePrecondition(subject)),
			}, "combines Unconditionally"},
			{"a nil precondition", event, []architecturekit.Precondition{architecturekit.Require(nil)}, "nil"},
			{"a precondition made by hand", event, []architecturekit.Precondition{{}}, "a write declares a zero Precondition, which none of OnPristineSubject, OnPopulatedSubject, OnEventID, OnStateRead, Require, or Unconditionally returns"},
			{"an event without a subject", []architecturekit.EventOn{{Event: incremented{By: 1}}}, []architecturekit.Precondition{architecturekit.Unconditionally()}, "needs a subject and an event"},
			{"a subject without an event", []architecturekit.EventOn{{Subject: subject}}, []architecturekit.Precondition{architecturekit.Unconditionally()}, "needs a subject and an event"},
			{"a nil pointer as the event", []architecturekit.EventOn{{Subject: subject, Event: (*incremented)(nil)}}, []architecturekit.Precondition{architecturekit.Unconditionally()}, "event 0 of a write needs a subject and an event"},
			{"a nil pointer after another event", []architecturekit.EventOn{
				{Subject: subject, Event: incremented{By: 1}},
				{Subject: subject, Event: (*incremented)(nil)},
			}, []architecturekit.Precondition{architecturekit.Unconditionally()}, "event 1 of a write needs a subject and an event"},
		} {
			t.Run(test.name, func(t *testing.T) {
				_, err := architecturekit.Write(context.Background(), requireStore(t), test.events, test.preconditions...)

				require.ErrorIs(t, err, architecturekit.ErrPermanent)
				assert.ErrorContains(t, err, test.says)
			})
		}

		assert.Empty(t, eventsIn(t, subject))
	})
}
