package architecturekit_test

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/http/httptrace"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// A context ends when the caller goes away or runs out of time, and reading has
// to stop then, and so does writing, as long as it has not begun. What must
// not happen is that a read which was cut short looks complete: a command
// would be decided on part of its history.

// thirtyIn writes three increments of ten to the subject of the test.
func thirtyIn(t *testing.T, store *architecturekit.Store) string {
	t.Helper()

	subject := subjectFor(t)
	for range 3 {
		_, err := architecturekit.Execute(context.Background(), store, counterDecider(), increment{subject: subject, By: 10})
		require.NoError(t, err)
	}

	return subject
}

// cancelingCounterState is the state of the counter, which ends the context as
// soon as it has applied its first event, as a caller that goes away halfway
// through reading would.
func cancelingCounterState(cancel context.CancelFunc) *architecturekit.State[counter] {
	state := architecturekit.NewState(counter{})

	state.Evolve(func(current counter, event incremented) counter {
		cancel()
		current.Total += event.By
		return current
	})

	state.Evolve(func(current counter, event reset) counter {
		current.Total = 0
		return current
	})

	return state
}

func TestTheEndOfTheContext(t *testing.T) {
	t.Run("loading with a context that has ended fails with its error", func(t *testing.T) {
		store := requireStore(t)
		subject := thirtyIn(t, store)

		ended, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := architecturekit.Load(ended, store, counterState(), subject)

		assert.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, architecturekit.ErrTransient, "an ended context is no failure of the database")
	})

	t.Run("loading with a deadline that has passed fails with it", func(t *testing.T) {
		store := requireStore(t)
		subject := thirtyIn(t, store)

		expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()

		_, err := architecturekit.Load(expired, store, counterState(), subject)

		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorIs(t, err, architecturekit.ErrTransient, "a deadline that ran out is no failure of the database")
	})

	t.Run("loading that is canceled halfway fails rather than handing out part of the state", func(t *testing.T) {
		store := requireStore(t)
		subject := thirtyIn(t, store)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		_, err := architecturekit.Load(ctx, store, cancelingCounterState(cancel), subject)

		assert.ErrorIs(t, err, context.Canceled)
	})

	t.Run("executing with a context that has ended writes nothing", func(t *testing.T) {
		store := requireStore(t)
		subject := thirtyIn(t, store)

		ended, cancel := context.WithCancel(context.Background())
		cancel()

		written, err := architecturekit.Execute(ended, store, counterDecider(), increment{subject: subject, By: 10})

		assert.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, written)
		assert.Equal(t, 30, totalIn(t, store, subject))
	})

	t.Run("executing that is canceled halfway through reading writes nothing", func(t *testing.T) {
		store := requireStore(t)
		subject := thirtyIn(t, store)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		decider := architecturekit.NewDecider(cancelingCounterState(cancel), counterDecider().Decide)

		// The whole history holds thirty, so ten more would exceed the limit.
		// Its first event alone holds ten, which would let them pass.
		written, err := architecturekit.Execute(ctx, store, decider, increment{subject: subject, By: 10, Limit: 35})

		assert.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, written)
		assert.Equal(t, 30, totalIn(t, store, subject), "the command was decided on part of the history")
	})

	t.Run("executing that is canceled while deciding writes nothing", func(t *testing.T) {
		store := requireStore(t)
		subject := thirtyIn(t, store)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		decider := counterDecider()
		decide := decider.Decide
		decider = architecturekit.NewDecider(decider.State(),
			func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
				cancel()
				return decide(ctx, cmd, current)
			})

		written, err := architecturekit.Execute(ctx, store, decider, increment{subject: subject, By: 10})

		assert.ErrorIs(t, err, context.Canceled)
		assert.Empty(t, written)
		assert.Equal(t, 30, totalIn(t, store, subject))
	})

	t.Run("reading that is canceled halfway ends with the error of the context", func(t *testing.T) {
		store := requireStore(t)
		subject := thirtyIn(t, store)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		eventsRead := 0
		var readErr error

		for _, err := range architecturekit.Read(ctx, store, architecturekit.ExactSubject(subject)) {
			if err != nil {
				readErr = err
				break
			}

			eventsRead++
			cancel()
		}

		assert.Equal(t, 1, eventsRead)
		assert.ErrorIs(t, readErr, context.Canceled)
	})

	t.Run("catching up with a context that has ended reports it", func(t *testing.T) {
		store := requireStore(t)
		subject := thirtyIn(t, store)

		ended, cancel := context.WithCancel(context.Background())
		cancel()

		err := architecturekit.CatchUpProjection(ended, store, architecturekit.ExactSubject(subject), &collector{})

		assert.ErrorIs(t, err, context.Canceled, "a read model that is only partly built looked complete")
	})

	t.Run("catching up with a deadline that has passed reports it", func(t *testing.T) {
		store := requireStore(t)
		subject := thirtyIn(t, store)

		expired, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()

		err := architecturekit.CatchUpProjection(expired, store, architecturekit.ExactSubject(subject), &collector{})

		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorIs(t, err, architecturekit.ErrTransient, "a deadline that ran out is no failure of the database")
	})

	t.Run("catching up that is canceled halfway reports it", func(t *testing.T) {
		store := requireStore(t)
		subject := thirtyIn(t, store)

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		applied := 0
		err := architecturekit.CatchUpProjection(ctx, store, architecturekit.ExactSubject(subject), architecturekit.ProjectionFunc(func(context.Context, eventsourcingdb.Event) error {
			applied++
			cancel()
			return nil
		}))

		assert.ErrorIs(t, err, context.Canceled)
		assert.Equal(t, 1, applied)
	})
}

// silentClient is a client of a database that takes requests but never
// answers them, until the test is over.
func silentClient(t *testing.T) *eventsourcingdb.Client {
	t.Helper()

	released := make(chan struct{})

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-released:
		case <-time.After(5 * time.Second):
		}
	}))
	t.Cleanup(func() {
		close(released)
		server.Close()
	})

	silentURL, err := url.Parse(server.URL)
	require.NoError(t, err)

	client, err := eventsourcingdb.NewClient(silentURL, "secret")
	require.NoError(t, err)

	return client
}

func TestADeadline(t *testing.T) {
	// The client hands the context on to its requests, so a deadline ends a
	// request that the database does not answer, rather than only taking effect
	// between two events that never arrive.
	t.Run("ends loading from a database that does not answer", func(t *testing.T) {
		store := architecturekit.NewStore(silentClient(t), "https://thenativeweb.io")

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		started := time.Now()
		_, err := architecturekit.Load(ctx, store, counterState(), "/counter/1")

		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.NotErrorIs(t, err, architecturekit.ErrTransient, "a deadline that ran out is no failure of the database")
		assert.Less(t, time.Since(started), 2*time.Second, "the deadline did not end the request")
	})

	t.Run("ends executing against a database that does not answer", func(t *testing.T) {
		store := architecturekit.NewStore(silentClient(t), "https://thenativeweb.io")

		ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
		defer cancel()

		started := time.Now()
		_, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: "/counter/1", By: 1})

		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.Less(t, time.Since(started), 2*time.Second, "the deadline did not end the request")
	})
}

// answerAfterEnding answers a write or the registration of a schema with the
// given body, but only after it has ended the context of the caller, and has
// given a client that stops at the end of the context the time to go away.
func answerAfterEnding(cancel context.CancelFunc, body string) http.HandlerFunc {
	return func(writer http.ResponseWriter, request *http.Request) {
		cancel()

		select {
		case <-request.Context().Done():
			return
		case <-time.After(200 * time.Millisecond):
		}

		_, _ = fmt.Fprint(writer, body)
	}
}

func TestAWriteThatHasBegun(t *testing.T) {
	// The database may store the events as soon as the request has left, so the
	// end of the context does not stop a write that has begun, and its result
	// counts. The same goes for the registration of a schema.
	t.Run("is finished by Execute, although the context ends", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store := architecturekit.NewStore(
			writingDatabase(t, "/api/v1/write-events", answerAfterEnding(cancel, writtenAnswer)), "https://thenativeweb.io")

		written, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: "/test", By: 1})

		require.NoError(t, err, "the write had begun, so its result counts")
		assert.Len(t, written, 1)
		assert.ErrorIs(t, ctx.Err(), context.Canceled, "the context has to end while writing")
	})

	t.Run("is finished by Write, although the context ends", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store := architecturekit.NewStore(
			writingDatabase(t, "/api/v1/write-events", answerAfterEnding(cancel, writtenAnswer)), "https://thenativeweb.io")

		written, err := architecturekit.Write(ctx, store,
			[]architecturekit.EventOn{{Subject: "/test", Event: incremented{By: 1}}}, architecturekit.Unconditionally())

		require.NoError(t, err, "the write had begun, so its result counts")
		assert.Len(t, written, 1)
		assert.ErrorIs(t, ctx.Err(), context.Canceled, "the context has to end while writing")
	})

	t.Run("is finished by RegisterSchemas, although the context ends", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		store := architecturekit.NewStore(
			writingDatabase(t, "/api/v1/register-event-schema", answerAfterEnding(cancel, "")), "https://thenativeweb.io")

		err := architecturekit.RegisterSchemas(ctx, store, []architecturekit.EventSchema{architecturekit.SchemaOf[incremented]()})

		require.NoError(t, err, "the registration had begun, so its result counts")
		assert.ErrorIs(t, ctx.Err(), context.Canceled, "the context has to end while registering")
	})

	t.Run("carries the values of the context, such as a trace", func(t *testing.T) {
		// A trace that follows the requests of a call sees the write and the
		// registration as well, not only the reads before them.
		calls := []struct {
			name     string
			path     string
			answer   string
			requests int32
			call     func(ctx context.Context, store *architecturekit.Store) error
		}{
			{"Execute, which reads the state first", "/api/v1/write-events", writtenAnswer, 2,
				func(ctx context.Context, store *architecturekit.Store) error {
					_, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: "/test", By: 1})
					return err
				}},
			{"Write", "/api/v1/write-events", writtenAnswer, 1,
				func(ctx context.Context, store *architecturekit.Store) error {
					_, err := architecturekit.Write(ctx, store,
						[]architecturekit.EventOn{{Subject: "/test", Event: incremented{By: 1}}}, architecturekit.Unconditionally())
					return err
				}},
			{"RegisterSchemas, which reads the registered schemas first", "/api/v1/register-event-schema", "", 2,
				func(ctx context.Context, store *architecturekit.Store) error {
					return architecturekit.RegisterSchemas(ctx, store,
						[]architecturekit.EventSchema{architecturekit.SchemaOf[incremented]()})
				}},
		}

		for _, call := range calls {
			t.Run(call.name, func(t *testing.T) {
				store := architecturekit.NewStore(
					writingDatabase(t, call.path, func(writer http.ResponseWriter, _ *http.Request) {
						_, _ = fmt.Fprint(writer, call.answer)
					}), "https://thenativeweb.io")

				var requests atomic.Int32
				ctx := httptrace.WithClientTrace(context.Background(), &httptrace.ClientTrace{
					WroteRequest: func(httptrace.WroteRequestInfo) { requests.Add(1) },
				})

				require.NoError(t, call.call(ctx, store))
				assert.Equal(t, call.requests, requests.Load(), "the trace of the context has to see every request")
			})
		}
	})
}
