package architecturekit

import (
	"context"
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

	t.Run("sorts a stream on which neither an event nor a heartbeat arrived as transient", func(t *testing.T) {
		err := databaseFailure(worded{eventsourcingdb.ErrHeartbeatTimeout}, "reading events")

		assert.ErrorIs(t, err, ErrTransient)
	})

	t.Run("sorts a 409 when writing by its reason", func(t *testing.T) {
		conflict := databaseFailure(answered(http.StatusConflict, "state conflict: precondition failed"), "writing")
		violation := databaseFailure(answered(http.StatusConflict, "schema conflict: event does not match"), "writing")

		assert.ErrorIs(t, conflict, ErrConflict, "a precondition that did not hold may hold later")
		assert.NotErrorIs(t, conflict, ErrPermanent)
		assert.ErrorIs(t, violation, ErrPermanent, "an event that does not match its schema never will")
		assert.NotErrorIs(t, violation, ErrTransient)
	})

	t.Run("reads no status code from a failure without an answer", func(t *testing.T) {
		assert.Zero(t, statusCodeOf(errors.New("failed to write events, got HTTP status code '409', expected '200'")),
			"a text that looks like an answer is no answer")
		assert.Equal(t, http.StatusConflict, statusCodeOf(answered(http.StatusConflict, "")))
	})
}

func TestReadFailure(t *testing.T) {
	t.Run("sorts a 409 as permanent, whatever the reason, since reading has no preconditions", func(t *testing.T) {
		for _, reason := range []string{
			"state conflict: fromLatestEvent results in an event ID greater than upperBound ID",
			"schema conflict: the reason does not matter when reading",
		} {
			t.Run(reason, func(t *testing.T) {
				refusal := &eventsourcingdb.DBAPIError{Action: "read events", StatusCode: http.StatusConflict, Reason: reason}

				err := readFailure(context.Background(), refusal, `reading "/books/42"`)

				assert.ErrorIs(t, err, ErrPermanent)
				assert.NotErrorIs(t, err, ErrConflict, "a read has no precondition that could hold later")
				assert.NotErrorIs(t, err, ErrTransient)
				assert.ErrorContains(t, err, reason, "the message has to keep the reason of the database")
				assert.ErrorContains(t, err, `reading "/books/42"`)
			})
		}
	})

	t.Run("sorts every other answer as databaseFailure does", func(t *testing.T) {
		tests := []struct {
			name string
			err  error
			want error
		}{
			{"an unavailable database is transient", answered(http.StatusServiceUnavailable, "shutting down"), ErrTransient},
			{"too many requests are transient", answered(http.StatusTooManyRequests, ""), ErrTransient},
			{"a failure without an answer is transient", worded{errors.New("connection refused")}, ErrTransient},
			{"a bad request is permanent", answered(http.StatusBadRequest, "bad request"), ErrPermanent},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				err := readFailure(context.Background(), test.err, "reading events")

				assert.ErrorIs(t, err, test.want)
			})
		}

		err := readFailure(context.Background(), answered(http.StatusUnauthorized, "unauthorized"), "reading events")

		assert.ErrorIs(t, err, ErrPermanent)
		assert.ErrorContains(t, err, "the database rejected the API token")
	})

	t.Run("reports the end of the context rather than a 409", func(t *testing.T) {
		ended, cancel := context.WithCancel(context.Background())
		cancel()

		err := readFailure(ended, answered(http.StatusConflict, "state conflict: precondition failed"), "reading events")

		assert.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, ErrPermanent, "an ended context belongs to no category")
		assert.NotErrorIs(t, err, ErrTransient, "an ended context belongs to no category")
	})
}
