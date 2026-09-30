package architecturekit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

func TestDatabaseFailures(t *testing.T) {
	tests := []struct {
		name   string
		status int
		want   error
	}{
		{"a bad request is permanent", http.StatusBadRequest, architecturekit.ErrPermanent},
		{"a rejected API token is permanent", http.StatusUnauthorized, architecturekit.ErrPermanent},
		{"a request that is too large is permanent", http.StatusRequestEntityTooLarge, architecturekit.ErrPermanent},
		{"too many requests are transient", http.StatusTooManyRequests, architecturekit.ErrTransient},
		{"an internal error is transient", http.StatusInternalServerError, architecturekit.ErrTransient},
		{"an unavailable database is transient", http.StatusServiceUnavailable, architecturekit.ErrTransient},
	}

	// Reading and writing follow the same rules, so every status is checked
	// for both, and each time against the other category as well.
	paths := []struct {
		name string
		path string
	}{
		{"when reading", "/api/v1/read-events"},
		{"when writing", "/api/v1/write-events"},
	}

	for _, path := range paths {
		t.Run("are sorted by status "+path.name, func(t *testing.T) {
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					store := architecturekit.NewStore(
						refusingDatabase(t, path.path, test.status, "refused"), "https://thenativeweb.io")

					_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
						increment{subject: "/test", By: 1})

					assert.ErrorIs(t, err, test.want)
					assert.NotErrorIs(t, err, otherCategoryThan(test.want))
					assert.NotErrorIs(t, err, architecturekit.ErrConflict, "only 409 is a conflict")
				})
			}
		})
	}

	t.Run("name a rejected API token", func(t *testing.T) {
		store := architecturekit.NewStore(
			refusingDatabase(t, "/api/v1/read-events", http.StatusUnauthorized, "unauthorized"), "https://thenativeweb.io")

		_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: "/test", By: 1})

		assert.ErrorContains(t, err, "the database rejected the API token")
	})

	t.Run("treat an answer that does not come from an EventSourcingDB as transient", func(t *testing.T) {
		// A proxy in front of the database answers on its own while the
		// database restarts, without saying that it is an EventSourcingDB.
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			writer.WriteHeader(http.StatusBadGateway)
		}))
		t.Cleanup(server.Close)
		store := architecturekit.NewStore(clientFor(t, server), "https://thenativeweb.io")

		_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: "/test", By: 1})

		assert.ErrorIs(t, err, architecturekit.ErrTransient, "a proxy answering on its own is transient")
		assert.NotErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorContains(t, err, "the answer does not come from an EventSourcingDB", "want the reason named")
	})

	t.Run("are sorted by status when reading the registered schemas", func(t *testing.T) {
		store := architecturekit.NewStore(
			refusingDatabase(t, "/api/v1/read-event-types", http.StatusUnauthorized, "unauthorized"), "https://thenativeweb.io")

		err := store.RegisterSchemas(counterState().Schemas())

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a rejected API token is permanent")
		assert.NotErrorIs(t, err, architecturekit.ErrTransient)
	})

	t.Run("are sorted by status when registering a schema", func(t *testing.T) {
		store := architecturekit.NewStore(
			refusingDatabase(t, "/api/v1/register-event-schema", http.StatusServiceUnavailable, "shutting down"),
			"https://thenativeweb.io")

		err := store.RegisterSchemas(counterState().Schemas())

		assert.ErrorIs(t, err, architecturekit.ErrTransient, "an unavailable database is transient")
		assert.NotErrorIs(t, err, architecturekit.ErrPermanent)
	})
}

// otherCategoryThan returns the category an error must not belong to, given
// the one it belongs to.
func otherCategoryThan(category error) error {
	if category == architecturekit.ErrTransient {
		return architecturekit.ErrPermanent
	}

	return architecturekit.ErrTransient
}
