package architecturekit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// readAll collects what Read hands out, and the errors separately.
func readAll(
	t *testing.T,
	store *architecturekit.Store,
	subject string,
	options eventsourcingdb.ReadEventsOptions,
) ([]eventsourcingdb.Event, []error) {
	t.Helper()

	var events []eventsourcingdb.Event
	var errs []error
	for event, err := range architecturekit.Read(context.Background(), store, subject, options) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		events = append(events, event)
	}

	return events, errs
}

func idsOf(events []eventsourcingdb.Event) []string {
	ids := make([]string, len(events))
	for i, event := range events {
		ids[i] = event.ID
	}

	return ids
}

func TestRead(t *testing.T) {
	t.Run("reads the events of a subject in order", func(t *testing.T) {
		subject := subjectFor(t)
		seed(t, subject, 3)

		events, errs := readAll(t, requireStore(t), subject, eventsourcingdb.ReadEventsOptions{})
		require.Empty(t, errs)
		require.Len(t, events, 3)
		assert.Equal(t, subject, events[0].Subject)
		order, err := architecturekit.CompareRevisions(events[0].ID, events[2].ID)
		require.NoError(t, err)
		assert.Negative(t, order, "the events come in the order they were written")
	})

	t.Run("passes the options on", func(t *testing.T) {
		subject := subjectFor(t)
		seed(t, subject, 3)
		seed(t, subject+"/nested", 1)

		all, errs := readAll(t, requireStore(t), subject, eventsourcingdb.ReadEventsOptions{Recursive: true})
		require.Empty(t, errs)
		require.Len(t, all, 4, "a recursive read includes the nested subject")

		after, errs := readAll(t, requireStore(t), subject, eventsourcingdb.ReadEventsOptions{
			LowerBound: &eventsourcingdb.Bound{ID: all[0].ID, Type: eventsourcingdb.BoundTypeExclusive},
		})
		require.Empty(t, errs)
		assert.Equal(t, idsOf(all[1:3]), idsOf(after), "a lower bound skips what comes before it")
	})

	t.Run("stops when the caller stops", func(t *testing.T) {
		subject := subjectFor(t)
		seed(t, subject, 3)

		var first []eventsourcingdb.Event
		for event, err := range architecturekit.Read(context.Background(), requireStore(t), subject, eventsourcingdb.ReadEventsOptions{}) {
			require.NoError(t, err)
			first = append(first, event)
			break
		}

		assert.Len(t, first, 1)
	})

	t.Run("refuses an event whose hash does not match", func(t *testing.T) {
		store := architecturekit.NewStore(newFakeDatabase(t, &fakeDatabase{events: []int{0, 1}, tampered: true}), "https://thenativeweb.io")

		events, errs := readAll(t, store, "/test", eventsourcingdb.ReadEventsOptions{})

		assert.Empty(t, events, "an unverified event must not be handed out")
		require.Len(t, errs, 1, "the iteration ends with the first error")
		expectUnverified(t, errs[0])
	})

	t.Run("hands out events whose hashes match", func(t *testing.T) {
		store := architecturekit.NewStore(newFakeDatabase(t, &fakeDatabase{events: []int{0, 1}}), "https://thenativeweb.io")

		events, errs := readAll(t, store, "/test", eventsourcingdb.ReadEventsOptions{})

		require.Empty(t, errs)
		assert.Equal(t, []string{"0", "1"}, idsOf(events))
	})

	t.Run("sorts a rejected API token as permanent", func(t *testing.T) {
		store := architecturekit.NewStore(
			refusingDatabase(t, "/api/v1/read-events", http.StatusUnauthorized, "unauthorized"), "https://thenativeweb.io")

		_, errs := readAll(t, store, "/test", eventsourcingdb.ReadEventsOptions{})

		require.Len(t, errs, 1)
		assert.ErrorIs(t, errs[0], architecturekit.ErrPermanent)
		assert.ErrorContains(t, errs[0], "the database rejected the API token")
		assert.ErrorContains(t, errs[0], `reading "/test"`, "the error has to name the subject")
	})

	t.Run("sorts an unreachable database as transient", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close()
		store := architecturekit.NewStore(clientFor(t, server), "https://thenativeweb.io")

		_, errs := readAll(t, store, "/test", eventsourcingdb.ReadEventsOptions{})

		require.Len(t, errs, 1)
		assert.ErrorIs(t, errs[0], architecturekit.ErrTransient)
		assert.NotErrorIs(t, errs[0], architecturekit.ErrPermanent)
	})
}
