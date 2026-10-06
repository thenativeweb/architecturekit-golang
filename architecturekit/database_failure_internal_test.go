package architecturekit

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

// answer is an answer of the database as the client reports it, with the
// error that kept the client from reading the reason, if any.
func answer(status int, reason string, unread error) *eventsourcingdb.DBAPIError {
	return &eventsourcingdb.DBAPIError{Action: "write events", StatusCode: status, Reason: reason, Err: unread}
}

// dialTimeout returns the error with which the standard library reports that
// connecting timed out, as it does after 30 seconds for a database that does
// not answer.
func dialTimeout(t *testing.T) error {
	t.Helper()

	_, err := (&net.Dialer{Timeout: time.Nanosecond}).Dial("tcp", "127.0.0.1:1")
	require.Error(t, err)

	return &url.Error{Op: "Post", URL: "http://localhost:3000/api/v1/write-events", Err: err}
}

func TestDatabaseFailureWrapsTheClientError(t *testing.T) {
	categories := []error{ErrDomain, ErrConflict, ErrTransient, ErrPermanent, ErrUnverified, ErrNotARevision}

	t.Run("after the category, which still decides, and with the same message", func(t *testing.T) {
		refused := errors.New("dial tcp 127.0.0.1:3000: connect: connection refused")

		tests := []struct {
			name     string
			err      error
			category error

			// message is the format the message had when the failure of the
			// client was formatted with %v, applied to the category, what was
			// done, and that failure.
			message string
		}{
			{"without an answer", refused, ErrTransient, "%v: %s: %v"},
			{"from a server that is not an EventSourcingDB", eventsourcingdb.ErrInvalidServerHeader, ErrTransient,
				"%v: %s: the answer does not come from an EventSourcingDB: %v"},
			{"on a stream that stalled", eventsourcingdb.ErrHeartbeatTimeout, ErrTransient, "%v: %s: %v"},
			{"with 429", answer(http.StatusTooManyRequests, "slow down", nil), ErrTransient, "%v: %s: %v"},
			{"with 503", answer(http.StatusServiceUnavailable, "shutting down", nil), ErrTransient, "%v: %s: %v"},
			{"with a failed precondition", answer(http.StatusConflict, "state conflict: precondition failed", nil),
				ErrConflict, "%v: %s: %v"},
			{"with a schema violation", answer(http.StatusConflict, "schema conflict: event does not match", nil),
				ErrPermanent, "%v: %s: %v"},
			{"with a rejected API token", answer(http.StatusUnauthorized, "unauthorized", nil), ErrPermanent,
				"%v: %s: the database rejected the API token: %v"},
			{"with a bad request", answer(http.StatusBadRequest, "bad request", nil), ErrPermanent, "%v: %s: %v"},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				err := databaseFailure(test.err, "writing")

				assert.ErrorIs(t, err, test.err, "errors.Is and errors.As have to reach the failure of the client")
				assert.EqualError(t, err, fmt.Sprintf(test.message, test.category, "writing", test.err))
				for _, category := range categories {
					assert.Equal(t, errors.Is(test.category, category), errors.Is(err, category),
						"the failure of the client must not change the category %v", category)
				}

				wraps, isMultiple := err.(interface{ Unwrap() []error })
				require.True(t, isMultiple, "the failure has to wrap the category and the failure of the client")
				assert.Equal(t, test.category, wraps.Unwrap()[0], "the category comes first")
			})
		}
	})

	t.Run("after the category when the database refuses a schema, with the same message", func(t *testing.T) {
		refusal := answer(http.StatusConflict, "schema conflict: stored events do not match", nil)

		err := schemaRefusal("io.thenativeweb.test.incremented", refusal)

		assert.ErrorIs(t, err, refusal, "errors.As has to reach the answer")
		assert.ErrorIs(t, err, ErrPermanent)
		assert.NotErrorIs(t, err, ErrConflict, "a refused schema is permanent, whatever the reason")
		assert.EqualError(t, err, fmt.Sprintf("%v: the database refused the schema of %q: %v",
			ErrPermanent, "io.thenativeweb.test.incremented", refusal))
	})

	t.Run("lets errors.As reach the answer of the database", func(t *testing.T) {
		err := databaseFailure(answer(http.StatusTooManyRequests, "slow down", nil), "writing")

		refusal, isAnswer := errors.AsType[*eventsourcingdb.DBAPIError](err)
		require.True(t, isAnswer, "errors.As has to reach the answer")
		assert.Equal(t, http.StatusTooManyRequests, refusal.StatusCode)
		assert.Equal(t, "slow down", refusal.Reason)
	})

	t.Run("after the category when reading, also for a 409", func(t *testing.T) {
		refusal := &eventsourcingdb.DBAPIError{
			Action: "read events", StatusCode: http.StatusConflict, Reason: "state conflict: beyond the upper bound",
		}

		err := readFailure(context.Background(), refusal, "reading events")

		assert.ErrorIs(t, err, refusal, "errors.As has to reach the answer")
		assert.ErrorIs(t, err, ErrPermanent)
		assert.EqualError(t, err, fmt.Sprintf("%v: reading events: %v", ErrPermanent, refusal))
	})

	t.Run("keeps a failure of the client that looks like the end of a context as text only", func(t *testing.T) {
		// Only the end of the context the caller handed over is to match
		// context.Canceled or context.DeadlineExceeded. The standard library
		// reports a timeout while connecting as context.DeadlineExceeded as well,
		// and an answer whose reason could not be read keeps the error of the
		// reading, which may be one of a timeout that the application set up.
		timeout := dialTimeout(t)
		require.ErrorIs(t, timeout, context.DeadlineExceeded, "the standard library reports a timeout as a deadline")

		tests := []struct {
			name     string
			err      error
			category error
			message  string
		}{
			{"a timeout while connecting", timeout, ErrTransient, "%v: %s: %v"},
			{"a request that was canceled", &url.Error{Op: "Post", URL: "http://localhost:3000", Err: context.Canceled},
				ErrTransient, "%v: %s: %v"},
			{"an answer from a server that is not an EventSourcingDB", errors.Join(eventsourcingdb.ErrInvalidServerHeader, timeout),
				ErrTransient, "%v: %s: the answer does not come from an EventSourcingDB: %v"},
			{"503", answer(http.StatusServiceUnavailable, "", context.Canceled), ErrTransient, "%v: %s: %v"},
			{"a failed precondition", answer(http.StatusConflict, "state conflict: precondition failed", context.Canceled),
				ErrConflict, "%v: %s: %v"},
			{"a schema violation", answer(http.StatusConflict, "schema conflict: event does not match", context.DeadlineExceeded),
				ErrPermanent, "%v: %s: %v"},
			{"a rejected API token", answer(http.StatusUnauthorized, "", context.DeadlineExceeded), ErrPermanent,
				"%v: %s: the database rejected the API token: %v"},
			{"a bad request whose reason ran into a timeout", answer(http.StatusBadRequest, "", context.DeadlineExceeded),
				ErrPermanent, "%v: %s: %v"},
			{"a bad request whose reading was canceled", answer(http.StatusBadRequest, "", context.Canceled),
				ErrPermanent, "%v: %s: %v"},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				err := databaseFailure(test.err, "writing")

				assert.ErrorIs(t, err, test.category)
				assert.NotErrorIs(t, err, context.Canceled, "only the end of the context is context.Canceled")
				assert.NotErrorIs(t, err, context.DeadlineExceeded, "only the end of the context is context.DeadlineExceeded")
				assert.EqualError(t, err, fmt.Sprintf(test.message, test.category, "writing", test.err),
					"the message stays the same")

				// httpapi.StatusFor asks for these and for the end of the
				// context, so with the same answers, it maps the failure to the
				// same status as before.
				for _, category := range categories {
					assert.Equal(t, errors.Is(test.category, category), errors.Is(err, category),
						"the failure of the client must not change the category %v", category)
				}
			})
		}

		t.Run("when reading a 409", func(t *testing.T) {
			refusal := answer(http.StatusConflict, "", context.DeadlineExceeded)

			err := readFailure(context.Background(), refusal, "reading events")

			assert.ErrorIs(t, err, ErrPermanent)
			assert.NotErrorIs(t, err, context.DeadlineExceeded, "only the end of the context is context.DeadlineExceeded")
			assert.EqualError(t, err, fmt.Sprintf("%v: reading events: %v", ErrPermanent, refusal))
		})

		t.Run("when the database refuses a schema", func(t *testing.T) {
			for _, ended := range []error{context.Canceled, context.DeadlineExceeded} {
				t.Run(ended.Error(), func(t *testing.T) {
					refusal := answer(http.StatusConflict, "", ended)

					err := schemaRefusal("io.thenativeweb.test.incremented", refusal)

					assert.ErrorIs(t, err, ErrPermanent)
					assert.NotErrorIs(t, err, ended, "only the end of the context is %v", ended)
					assert.EqualError(t, err, fmt.Sprintf("%v: the database refused the schema of %q: %v",
						ErrPermanent, "io.thenativeweb.test.incremented", refusal))
				})
			}
		})
	})
}

func TestWriteFailure(t *testing.T) {
	t.Run("is what databaseFailure reports if the request never left completely", func(t *testing.T) {
		// The database reads the whole request before it writes anything, so
		// even an answer that would leave the outcome open, such as 500, comes
		// before writing then.
		for _, failure := range []error{
			worded{errors.New("connection refused")},
			worded{eventsourcingdb.ErrInvalidServerHeader},
			answer(http.StatusInternalServerError, "failed", nil),
			answer(http.StatusConflict, "state conflict: precondition failed", nil),
			dialTimeout(t),
		} {
			t.Run(failure.Error(), func(t *testing.T) {
				err := writeFailure(failure, false, "writing")

				assert.Equal(t, databaseFailure(failure, "writing"), err)
				assert.NotErrorIs(t, err, ErrOutcomeUnknown)
			})
		}
	})

	t.Run("is what databaseFailure reports if the database refused the request before writing", func(t *testing.T) {
		for _, refusal := range []*eventsourcingdb.DBAPIError{
			answer(http.StatusBadRequest, "bad request", nil),
			answer(http.StatusUnauthorized, "unauthorized", nil),
			answer(http.StatusForbidden, "forbidden", nil),
			answer(http.StatusNotFound, "not found", nil),
			answer(http.StatusConflict, "state conflict: precondition failed", nil),
			answer(http.StatusConflict, "schema conflict: event does not match", nil),
			answer(http.StatusRequestEntityTooLarge, "too large", nil),
			answer(http.StatusUnsupportedMediaType, "unsupported", nil),
			answer(http.StatusUnprocessableEntity, "unprocessable", nil),
			answer(http.StatusTooManyRequests, "slow down", nil),
			answer(499, "the last 4xx", nil),
			answer(http.StatusServiceUnavailable, "server is shutting down", nil),
			answer(http.StatusInsufficientStorage, "insufficient storage", nil),
		} {
			t.Run(refusal.Error(), func(t *testing.T) {
				err := writeFailure(refusal, true, "writing")

				assert.Equal(t, databaseFailure(refusal, "writing"), err)
				assert.NotErrorIs(t, err, ErrOutcomeUnknown)
			})
		}
	})

	t.Run("reports every other failure of a request that has left as an unknown outcome", func(t *testing.T) {
		for _, failure := range []error{
			worded{errors.New("read: connection reset by peer")},
			worded{io.ErrUnexpectedEOF},
			worded{eventsourcingdb.ErrInvalidServerHeader},
			worded{eventsourcingdb.ErrHeartbeatTimeout},
			dialTimeout(t),
			answer(http.StatusPermanentRedirect, "the last 3xx", nil),
			answer(http.StatusInternalServerError, "failed", nil),
			answer(http.StatusNotImplemented, "not implemented", nil),
			answer(http.StatusBadGateway, "bad gateway", nil),
			answer(http.StatusGatewayTimeout, "gateway timeout", nil),
			answer(http.StatusLoopDetected, "the status after 507", nil),
		} {
			t.Run(failure.Error(), func(t *testing.T) {
				err := writeFailure(failure, true, "writing")

				assert.ErrorIs(t, err, ErrOutcomeUnknown)
				for _, category := range []error{ErrDomain, ErrTransient, ErrPermanent, ErrNotARevision} {
					assert.NotErrorIs(t, err, category, "an unknown outcome belongs to no category")
				}
			})
		}
	})

	t.Run("words an unknown outcome as databaseFailure words a category", func(t *testing.T) {
		tests := []struct {
			name    string
			err     error
			message string
		}{
			{"without an answer", worded{errors.New("connection reset by peer")}, "%v: %s: %v"},
			{"with 500", answer(http.StatusInternalServerError, "failed", nil), "%v: %s: %v"},
			{"from a server that is not an EventSourcingDB", worded{eventsourcingdb.ErrInvalidServerHeader},
				"%v: %s: the answer does not come from an EventSourcingDB: %v"},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				err := writeFailure(test.err, true, `writing "/books/42"`)

				assert.EqualError(t, err, fmt.Sprintf(test.message, ErrOutcomeUnknown, `writing "/books/42"`, test.err))
				assert.ErrorIs(t, err, test.err, "errors.Is and errors.As have to reach the failure of the client")

				wraps, isMultiple := err.(interface{ Unwrap() []error })
				require.True(t, isMultiple, "the failure has to wrap ErrOutcomeUnknown and the failure of the client")
				assert.Equal(t, ErrOutcomeUnknown, wraps.Unwrap()[0], "ErrOutcomeUnknown comes first")
			})
		}

		assert.EqualError(t, ErrOutcomeUnknown, "outcome unknown")
	})

	t.Run("keeps a failure of the client that looks like the end of a context as text only", func(t *testing.T) {
		// A timeout of the client while it waits for the answer is reported as
		// context.DeadlineExceeded, although the context of the caller has
		// not ended, and httpapi.StatusFor would answer it with 503.
		timeout := dialTimeout(t)

		tests := []struct {
			name    string
			err     error
			message string
		}{
			{"a timeout", timeout, "%v: %s: %v"},
			{"a request that was canceled", &url.Error{Op: "Post", URL: "http://localhost:3000", Err: context.Canceled}, "%v: %s: %v"},
			{"an answer from a server that is not an EventSourcingDB", errors.Join(eventsourcingdb.ErrInvalidServerHeader, timeout),
				"%v: %s: the answer does not come from an EventSourcingDB: %v"},
			{"500 whose reason ran into a timeout", answer(http.StatusInternalServerError, "", context.DeadlineExceeded), "%v: %s: %v"},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				err := writeFailure(test.err, true, "writing")

				assert.ErrorIs(t, err, ErrOutcomeUnknown)
				assert.NotErrorIs(t, err, context.Canceled, "only the end of the context is context.Canceled")
				assert.NotErrorIs(t, err, context.DeadlineExceeded, "only the end of the context is context.DeadlineExceeded")
				assert.EqualError(t, err, fmt.Sprintf(test.message, ErrOutcomeUnknown, "writing", test.err),
					"the message stays the same")
			})
		}
	})
}
