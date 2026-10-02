package architecturekit

import (
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// worded hides the text of the error it wraps behind a wording of its own,
// as a client with a different wording would, so that only the type of the
// wrapped error tells what the database answered.
type worded struct{ err error }

func (w worded) Error() string { return "the client words this differently" }
func (w worded) Unwrap() error { return w.err }

func answered(status int, reason string) error {
	return worded{&eventsourcingdb.DBAPIError{Action: "write events", StatusCode: status, Reason: reason}}
}

func TestDatabaseFailureByType(t *testing.T) {
	t.Run("sorts by the status code of the answer, not by the error text", func(t *testing.T) {
		tests := []struct {
			name string
			err  error
			want error
		}{
			{"a failed precondition is a conflict", answered(http.StatusConflict, "state conflict: precondition failed"), ErrConflict},
			{"a schema violation is permanent", answered(http.StatusConflict, "schema conflict: event does not match"), ErrPermanent},
			{"too many requests are transient", answered(http.StatusTooManyRequests, ""), ErrTransient},
			{"an unavailable database is transient", answered(http.StatusServiceUnavailable, "shutting down"), ErrTransient},
			{"a rejected API token is permanent", answered(http.StatusUnauthorized, "unauthorized"), ErrPermanent},
			{"a bad request is permanent", answered(http.StatusBadRequest, "bad request"), ErrPermanent},
			{"a failure without an answer is transient", worded{errors.New("connection refused")}, ErrTransient},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				err := databaseFailure(test.err, "writing")

				assert.ErrorIs(t, err, test.want)
			})
		}
	})

	t.Run("tells a conflict from a schema violation only by the start of the reason", func(t *testing.T) {
		err := databaseFailure(answered(http.StatusConflict, "state conflict: the schema conflict is elsewhere"), "writing")

		assert.ErrorIs(t, err, ErrConflict)
		assert.NotErrorIs(t, err, ErrPermanent)
	})

	t.Run("names a rejected API token", func(t *testing.T) {
		err := databaseFailure(answered(http.StatusUnauthorized, "unauthorized"), "writing")

		assert.ErrorContains(t, err, "the database rejected the API token")
	})

	t.Run("names an answer that does not come from an EventSourcingDB", func(t *testing.T) {
		err := databaseFailure(worded{eventsourcingdb.ErrInvalidServerHeader}, "writing")

		assert.ErrorIs(t, err, ErrTransient)
		assert.ErrorContains(t, err, "the answer does not come from an EventSourcingDB")
	})

	t.Run("reads no status code from a failure without an answer", func(t *testing.T) {
		assert.Zero(t, statusCodeOf(errors.New("failed to write events, got HTTP status code '409', expected '200'")),
			"a text that looks like an answer is no answer")
		assert.Equal(t, http.StatusConflict, statusCodeOf(answered(http.StatusConflict, "")))
	})
}
