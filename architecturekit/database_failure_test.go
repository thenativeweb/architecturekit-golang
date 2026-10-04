package architecturekit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	// Reading and writing follow the same rules for these statuses, so every
	// status is checked for both, and each time against the other category as
	// well. Only 409 is sorted differently (see below).
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

	t.Run("with 409 are sorted as permanent on every read, since reading has no preconditions", func(t *testing.T) {
		reads := []struct {
			name string
			path string
			read func(t *testing.T, store *architecturekit.Store) error
		}{
			{"Read", "/api/v1/read-events", func(t *testing.T, store *architecturekit.Store) error {
				_, errs := readAll(t, store, architecturekit.ExactSubject("/test"))
				require.Len(t, errs, 1)

				return errs[0]
			}},
			{"Load", "/api/v1/read-events", func(_ *testing.T, store *architecturekit.Store) error {
				_, err := architecturekit.Load(context.Background(), store, counterState(), "/test")

				return err
			}},
			{"Execute", "/api/v1/read-events", func(_ *testing.T, store *architecturekit.Store) error {
				_, err := architecturekit.Execute(context.Background(), store, counterDecider(), increment{subject: "/test", By: 1})

				return err
			}},
			{"CatchUpProjection", "/api/v1/read-events", func(_ *testing.T, store *architecturekit.Store) error {
				return architecturekit.CatchUpProjection(context.Background(), store, architecturekit.ExactSubject("/test"), &collector{})
			}},
			{"StartProjection, when observing", "/api/v1/observe-events", func(_ *testing.T, store *architecturekit.Store) error {
				// Retrying would go on until the context ends, which a run
				// reports without an error.
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()

				return runUntilDone(ctx, store, architecturekit.ExactSubject("/test"), &collector{})
			}},
			{"RegisterSchemas, when reading the registered ones", "/api/v1/read-event-types", func(_ *testing.T, store *architecturekit.Store) error {
				return architecturekit.RegisterSchemas(context.Background(), store, counterState().Schemas())
			}},
		}

		reasons := []string{
			"state conflict: fromLatestEvent results in an event ID greater than upperBound ID",
			"schema conflict: the reason does not matter when reading",
		}

		for _, read := range reads {
			t.Run(read.name, func(t *testing.T) {
				for _, reason := range reasons {
					t.Run(reason, func(t *testing.T) {
						store := architecturekit.NewStore(
							refusingDatabase(t, read.path, http.StatusConflict, reason), "https://thenativeweb.io")

						err := read.read(t, store)

						assert.ErrorIs(t, err, architecturekit.ErrPermanent)
						assert.NotErrorIs(t, err, architecturekit.ErrConflict, "a read has no precondition that did not hold")
						assert.NotErrorIs(t, err, architecturekit.ErrTransient, "trying again never helps")
						assert.ErrorContains(t, err, reason, "the message has to keep the reason of the database")
					})
				}
			})
		}
	})

	t.Run("with 409 are sorted by their reason when writing", func(t *testing.T) {
		writes := []struct {
			name  string
			write func(store *architecturekit.Store) error
		}{
			{"Execute", func(store *architecturekit.Store) error {
				_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
					increment{subject: "/test", By: 1})

				return err
			}},
			{"Write", func(store *architecturekit.Store) error {
				_, err := architecturekit.Write(context.Background(), store,
					[]architecturekit.EventOn{{Subject: "/test", Event: incremented{By: 1}}}, architecturekit.Unconditionally())

				return err
			}},
		}

		for _, write := range writes {
			t.Run(write.name, func(t *testing.T) {
				conflicting := architecturekit.NewStore(
					refusingDatabase(t, "/api/v1/write-events", http.StatusConflict, "state conflict: precondition failed"),
					"https://thenativeweb.io")
				violating := architecturekit.NewStore(
					refusingDatabase(t, "/api/v1/write-events", http.StatusConflict, "schema conflict: event does not match"),
					"https://thenativeweb.io")

				conflict := write.write(conflicting)
				violation := write.write(violating)

				assert.ErrorIs(t, conflict, architecturekit.ErrConflict, "a precondition that did not hold may hold later")
				assert.NotErrorIs(t, conflict, architecturekit.ErrPermanent)
				assert.ErrorIs(t, violation, architecturekit.ErrPermanent, "an event that does not match its schema never will")
				assert.NotErrorIs(t, violation, architecturekit.ErrTransient)
			})
		}
	})

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

		err := architecturekit.RegisterSchemas(context.Background(), store, counterState().Schemas())

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a rejected API token is permanent")
		assert.NotErrorIs(t, err, architecturekit.ErrTransient)
	})

	t.Run("are sorted by status when registering a schema", func(t *testing.T) {
		store := architecturekit.NewStore(
			refusingDatabase(t, "/api/v1/register-event-schema", http.StatusServiceUnavailable, "shutting down"),
			"https://thenativeweb.io")

		err := architecturekit.RegisterSchemas(context.Background(), store, counterState().Schemas())

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
