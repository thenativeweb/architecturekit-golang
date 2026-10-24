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
	"github.com/thenativeweb/eventsourcingdb-client-golang/v2/eventsourcingdb"
)

// worded hides the text of the error it wraps behind a wording of its own,
// as a client with a different wording would, so that only the type of the
// wrapped error tells what the database answered.
type worded struct{ err error }

func (w worded) Error() string { return "the client words this differently" }
func (w worded) Unwrap() error { return w.err }

// answered is an answer without a code, as a database that offers just the v1
// API sends it for an error that the client can not match to a code.
func answered(status int, description string) error {
	return worded{&eventsourcingdb.DBAPIError{Action: "write events", Status: status, Description: description}}
}

// coded is an error that the database reports with a code, as a database that
// offers the v2 API does in the body of an answer with the status 200.
func coded(code string) error {
	return worded{&eventsourcingdb.DBAPIError{
		Action: "write events", Status: http.StatusOK, Code: code, Description: "the description does not matter",
	}}
}

// stalled is a connection that stalled, as the client reports it: errors.Is
// takes it for eventsourcingdb.ErrConnectionStalled, and for
// context.DeadlineExceeded as well, as for a timeout of net/http.
type stalled struct{}

func (stalled) Error() string {
	return "connection stalled, nothing arrived from the server for 30 seconds"
}

func (stalled) Is(target error) bool {
	return target == eventsourcingdb.ErrConnectionStalled || target == context.DeadlineExceeded
}

// mayHaveBeenWritten is a failure of a write that the client reports as
// leaving open whether the database stored it, wrapping its cause, as the
// client does.
type mayHaveBeenWritten struct{ cause error }

func (m mayHaveBeenWritten) Error() string {
	return "outcome unknown, the events may have been written nevertheless: " + m.cause.Error()
}
func (m mayHaveBeenWritten) Is(target error) bool { return target == eventsourcingdb.ErrOutcomeUnknown }
func (m mayHaveBeenWritten) Unwrap() error        { return m.cause }

func TestDatabaseFailureByType(t *testing.T) {
	t.Run("sorts an answer without a code by its status, not by the error text", func(t *testing.T) {
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
			{"an error within a stream of the v1 API is transient", answered(http.StatusOK, "store is shutting down"), ErrTransient},
			{"a request that did not arrive in time is transient", answered(http.StatusRequestTimeout, ""), ErrTransient},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				err := databaseFailure(test.err, "writing")

				assert.ErrorIs(t, err, test.want)
			})
		}
	})

	t.Run("sorts an error with a code by its code alone", func(t *testing.T) {
		tests := []struct {
			name string
			code string
			want error
		}{
			{"a precondition that did not hold is a conflict", eventsourcingdb.CodePreconditionViolated, ErrConflict},
			{"a database that shuts down is transient", eventsourcingdb.CodeServerShuttingDown, ErrTransient},
			{"a store that is read-only is transient", eventsourcingdb.CodeStoreReadOnly, ErrTransient},
			{"indexes that are rebuilt are transient", eventsourcingdb.CodeIndexesRebuilding, ErrTransient},
			{"a restore in progress is transient", eventsourcingdb.CodeRestoreInProgress, ErrTransient},
			{"a full disk is transient", eventsourcingdb.CodeDiskFull, ErrTransient},
			{"a store that failed is transient", eventsourcingdb.CodeStoreFailed, ErrTransient},
			{"an invalid license is transient", eventsourcingdb.CodeLicenseInvalid, ErrTransient},
			{"an exceeded capacity is transient", eventsourcingdb.CodeCapacityExceeded, ErrTransient},
			{"an event that does not match its schema is permanent", eventsourcingdb.CodeEventSchemaViolated, ErrPermanent},
			{"a malformed subject is permanent", eventsourcingdb.CodeSubjectMalformed, ErrPermanent},
			{"an event that is too large is permanent", eventsourcingdb.CodeEventTooLarge, ErrPermanent},
			{"a query that can not be evaluated is permanent", eventsourcingdb.CodeQueryUnevaluable, ErrPermanent},
			{"a range that no event can satisfy is permanent", eventsourcingdb.CodeRangeUnsatisfiable, ErrPermanent},
			{"a code the kit does not know is permanent", "https://docs.eventsourcingdb.io/errors/something-new", ErrPermanent},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				err := databaseFailure(coded(test.code), "writing")

				assert.ErrorIs(t, err, test.want)
				if test.want != ErrConflict {
					assert.NotErrorIs(t, err, ErrConflict, "ErrConflict is transient as well, so only this tells them apart")
				}
			})
		}
	})

	t.Run("keeps a conflict apart from the other transient failures", func(t *testing.T) {
		conflict := databaseFailure(coded(eventsourcingdb.CodePreconditionViolated), "writing")
		capacity := databaseFailure(coded(eventsourcingdb.CodeCapacityExceeded), "writing")

		assert.ErrorIs(t, conflict, ErrConflict, "a precondition that did not hold may hold once the state is read again")
		assert.NotErrorIs(t, capacity, ErrConflict, "reading the state again never makes room in the store")
		assert.NotErrorIs(t, conflict, ErrPermanent)
	})

	t.Run("sorts by the code rather than by the status of the answer", func(t *testing.T) {
		refusal := worded{&eventsourcingdb.DBAPIError{
			Action: "write events", Status: http.StatusConflict, Code: eventsourcingdb.CodeEventSchemaViolated,
			Description: "state conflict: the description does not decide either",
		}}

		err := databaseFailure(refusal, "writing")

		assert.ErrorIs(t, err, ErrPermanent)
		assert.NotErrorIs(t, err, ErrConflict)
	})

	t.Run("tells a conflict from a schema violation only by the start of the description", func(t *testing.T) {
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

	t.Run("sorts a connection that stalled as transient, and keeps the stall reachable", func(t *testing.T) {
		err := databaseFailure(stalled{}, "reading events")

		assert.ErrorIs(t, err, ErrTransient)
		assert.ErrorIs(t, err, eventsourcingdb.ErrConnectionStalled)
		assert.NotErrorIs(t, err, context.DeadlineExceeded, "only the end of the context is context.DeadlineExceeded")
	})

	t.Run("sorts a 409 without a code when writing by its description", func(t *testing.T) {
		conflict := databaseFailure(answered(http.StatusConflict, "state conflict: precondition failed"), "writing")
		violation := databaseFailure(answered(http.StatusConflict, "schema conflict: event does not match"), "writing")

		assert.ErrorIs(t, conflict, ErrConflict, "a precondition that did not hold may hold later")
		assert.NotErrorIs(t, conflict, ErrPermanent)
		assert.ErrorIs(t, violation, ErrPermanent, "an event that does not match its schema never will")
		assert.NotErrorIs(t, violation, ErrTransient)
	})

	t.Run("reads no status and no code from a failure without an answer", func(t *testing.T) {
		assert.Zero(t, statusOf(errors.New("failed to write events, got HTTP status code '409', expected '200'")),
			"a text that looks like an answer is no answer")
		assert.Empty(t, codeOf(errors.New("failed to write events: https://docs.eventsourcingdb.io/errors/precondition-violated")),
			"a text that looks like a code is no code")
		assert.Equal(t, http.StatusConflict, statusOf(answered(http.StatusConflict, "")))
		assert.Equal(t, eventsourcingdb.CodePreconditionViolated, codeOf(coded(eventsourcingdb.CodePreconditionViolated)))
	})
}

func TestReadFailure(t *testing.T) {
	t.Run("sorts a 409 without a code as permanent, whatever the description, since reading has no preconditions", func(t *testing.T) {
		for _, reason := range []string{
			"state conflict: the description does not matter when reading",
			"schema conflict: the description does not matter when reading",
		} {
			t.Run(reason, func(t *testing.T) {
				refusal := &eventsourcingdb.DBAPIError{Action: "read events", Status: http.StatusConflict, Description: reason}

				err := readFailure(context.Background(), refusal, `reading "/books/42"`)

				assert.ErrorIs(t, err, ErrPermanent)
				assert.NotErrorIs(t, err, ErrConflict, "a read has no precondition that could hold later")
				assert.NotErrorIs(t, err, ErrTransient)
				assert.ErrorContains(t, err, reason, "the message has to keep the reason of the database")
				assert.ErrorContains(t, err, `reading "/books/42"`)
			})
		}
	})

	t.Run("sorts a FromLatestEvent beyond the upper bound as permanent by its code", func(t *testing.T) {
		refusal := &eventsourcingdb.DBAPIError{
			Action: "read events", Status: http.StatusConflict, Code: eventsourcingdb.CodeRangeUnsatisfiable,
			Description: "state conflict: fromLatestEvent results in an event ID greater than upperBound ID",
		}

		err := readFailure(context.Background(), refusal, "reading events")

		assert.ErrorIs(t, err, ErrPermanent)
		assert.NotErrorIs(t, err, ErrConflict)
	})

	t.Run("sorts every other answer as databaseFailure does", func(t *testing.T) {
		tests := []struct {
			name string
			err  error
			want error
		}{
			{"a database that rebuilds its indexes is transient", coded(eventsourcingdb.CodeIndexesRebuilding), ErrTransient},
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

// answer is an answer of the database without a code as the client reports
// it, with the error that kept the client from reading the description, if
// any.
func answer(status int, description string, unread error) *eventsourcingdb.DBAPIError {
	return &eventsourcingdb.DBAPIError{Action: "write events", Status: status, Description: description, Err: unread}
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
			{"on a connection that stalled", stalled{}, ErrTransient, "%v: %s: %v"},
			{"with a code of a conflict", &eventsourcingdb.DBAPIError{
				Action: "write events", Status: http.StatusOK, Code: eventsourcingdb.CodePreconditionViolated,
				Description: "/preconditions/0: precondition isSubjectPristine does not hold",
			}, ErrConflict, "%v: %s: %v"},
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
		assert.Equal(t, http.StatusTooManyRequests, refusal.Status)
		assert.Equal(t, "slow down", refusal.Description)
	})

	t.Run("after the category when reading, also for a 409", func(t *testing.T) {
		refusal := &eventsourcingdb.DBAPIError{
			Action: "read events", Status: http.StatusConflict, Description: "state conflict: beyond the upper bound",
		}

		err := readFailure(context.Background(), refusal, "reading events")

		assert.ErrorIs(t, err, refusal, "errors.As has to reach the answer")
		assert.ErrorIs(t, err, ErrPermanent)
		assert.EqualError(t, err, fmt.Sprintf("%v: reading events: %v", ErrPermanent, refusal))
	})

	t.Run("keeps a failure of the client that looks like the end of a context, but not as the end of the context", func(t *testing.T) {
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
				assert.ErrorIs(t, err, test.err, "errors.Is has to reach the failure of the client all the same")
				if answer, isAnswer := test.err.(*eventsourcingdb.DBAPIError); isAnswer {
					reached, isReached := errors.AsType[*eventsourcingdb.DBAPIError](err)
					require.True(t, isReached, "errors.As has to reach the answer of the database all the same")
					assert.Same(t, answer, reached)
				}
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
	t.Run("is what databaseFailure reports unless the client reports an unknown outcome", func(t *testing.T) {
		// The client reports a write as certainly not stored only if it could
		// not connect, or if the database refused it, by the same rule in
		// every SDK, so whatever it does not mark is sorted into a category.
		for _, failure := range []error{
			worded{errors.New("connection refused")},
			worded{eventsourcingdb.ErrInvalidServerHeader},
			dialTimeout(t),
			answer(http.StatusBadRequest, "bad request", nil),
			answer(http.StatusUnauthorized, "unauthorized", nil),
			answer(http.StatusConflict, "state conflict: precondition failed", nil),
			answer(http.StatusConflict, "schema conflict: event does not match", nil),
			answer(http.StatusTooManyRequests, "slow down", nil),
			answer(http.StatusServiceUnavailable, "server is shutting down", nil),
			coded(eventsourcingdb.CodePreconditionViolated),
			coded(eventsourcingdb.CodeEventSchemaViolated),
			coded(eventsourcingdb.CodeServerShuttingDown),
			coded("https://docs.eventsourcingdb.io/errors/something-new"),
		} {
			t.Run(failure.Error(), func(t *testing.T) {
				err := writeFailure(failure, "writing")

				assert.Equal(t, databaseFailure(failure, "writing"), err)
				assert.NotErrorIs(t, err, ErrOutcomeUnknown)
			})
		}
	})

	t.Run("reports a failure that the client reports as an unknown outcome as one", func(t *testing.T) {
		for _, failure := range []error{
			mayHaveBeenWritten{errors.New("read: connection reset by peer")},
			mayHaveBeenWritten{io.ErrUnexpectedEOF},
			mayHaveBeenWritten{eventsourcingdb.ErrInvalidServerHeader},
			mayHaveBeenWritten{stalled{}},
			mayHaveBeenWritten{answer(http.StatusInternalServerError, "failed", nil)},
			mayHaveBeenWritten{answer(http.StatusBadGateway, "bad gateway", nil)},
			mayHaveBeenWritten{answer(http.StatusConflict, "state conflict: precondition failed", nil)},
		} {
			t.Run(failure.Error(), func(t *testing.T) {
				err := writeFailure(failure, "writing")

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
			{"without an answer", mayHaveBeenWritten{errors.New("connection reset by peer")}, "%v: %s: %v"},
			{"with 500", mayHaveBeenWritten{answer(http.StatusInternalServerError, "failed", nil)}, "%v: %s: %v"},
			{"from a server that is not an EventSourcingDB", mayHaveBeenWritten{eventsourcingdb.ErrInvalidServerHeader},
				"%v: %s: the answer does not come from an EventSourcingDB: %v"},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				err := writeFailure(test.err, `writing "/books/42"`)

				assert.EqualError(t, err, fmt.Sprintf(test.message, ErrOutcomeUnknown, `writing "/books/42"`, test.err))
				assert.ErrorIs(t, err, test.err, "errors.Is and errors.As have to reach the failure of the client")
				assert.ErrorIs(t, err, eventsourcingdb.ErrOutcomeUnknown, "errors.Is has to reach the unknown outcome of the client")

				wraps, isMultiple := err.(interface{ Unwrap() []error })
				require.True(t, isMultiple, "the failure has to wrap ErrOutcomeUnknown and the failure of the client")
				assert.Equal(t, ErrOutcomeUnknown, wraps.Unwrap()[0], "ErrOutcomeUnknown comes first")
			})
		}

		assert.EqualError(t, ErrOutcomeUnknown, "outcome unknown")
	})

	t.Run("keeps the unknown outcome and the stall reachable after a write that stalled", func(t *testing.T) {
		err := writeFailure(mayHaveBeenWritten{stalled{}}, "writing")

		assert.ErrorIs(t, err, ErrOutcomeUnknown)
		assert.ErrorIs(t, err, eventsourcingdb.ErrOutcomeUnknown)
		assert.ErrorIs(t, err, eventsourcingdb.ErrConnectionStalled)
		assert.NotErrorIs(t, err, context.DeadlineExceeded, "only the end of the context is context.DeadlineExceeded")
	})

	t.Run("keeps a failure of the client that looks like the end of a context, but not as the end of the context", func(t *testing.T) {
		// A timeout while connecting is reported as context.DeadlineExceeded,
		// although the context of the caller has not ended, and
		// httpapi.StatusFor would answer it with 503.
		timeout := dialTimeout(t)

		tests := []struct {
			name    string
			err     error
			message string
		}{
			{"a timeout", mayHaveBeenWritten{timeout}, "%v: %s: %v"},
			{"a request that was canceled", mayHaveBeenWritten{&url.Error{Op: "Post", URL: "http://localhost:3000", Err: context.Canceled}}, "%v: %s: %v"},
			{"an answer from a server that is not an EventSourcingDB", mayHaveBeenWritten{errors.Join(eventsourcingdb.ErrInvalidServerHeader, timeout)},
				"%v: %s: the answer does not come from an EventSourcingDB: %v"},
			{"500 whose description ran into a timeout", mayHaveBeenWritten{answer(http.StatusInternalServerError, "", context.DeadlineExceeded)}, "%v: %s: %v"},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				err := writeFailure(test.err, "writing")

				assert.ErrorIs(t, err, ErrOutcomeUnknown)
				assert.NotErrorIs(t, err, context.Canceled, "only the end of the context is context.Canceled")
				assert.NotErrorIs(t, err, context.DeadlineExceeded, "only the end of the context is context.DeadlineExceeded")
				assert.EqualError(t, err, fmt.Sprintf(test.message, ErrOutcomeUnknown, "writing", test.err),
					"the message stays the same")
			})
		}
	})
}

func TestIsSchemaConflict(t *testing.T) {
	tests := []struct {
		name    string
		refusal error
		want    bool
	}{
		{"a schema that is registered already", coded(eventsourcingdb.CodeEventSchemaAlreadyRegistered), true},
		{"stored events that do not match the schema", coded(eventsourcingdb.CodeEventSchemaViolated), true},
		{"a 409 without a code, from a database that offers just the v1 API", answered(http.StatusConflict, "schema conflict"), true},
		{"a malformed schema", coded(eventsourcingdb.CodeEventSchemaMalformed), false},
		{"a database that shuts down", coded(eventsourcingdb.CodeServerShuttingDown), false},
		{"a 409 with a code of something else", worded{&eventsourcingdb.DBAPIError{
			Action: "register event schema", Status: http.StatusConflict, Code: eventsourcingdb.CodeEventSchemaMalformed,
		}}, false},
		{"a 503 without a code", answered(http.StatusServiceUnavailable, "shutting down"), false},
		{"a failure without an answer", worded{errors.New("connection refused")}, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, isSchemaConflict(test.refusal))
		})
	}
}
