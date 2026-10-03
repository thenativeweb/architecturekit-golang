package architecturekit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// A context ends when the caller goes away or runs out of time, and reading or
// writing has to stop then. What must not happen is that a read which was cut
// short looks complete: a command would be decided on part of its history.

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

		decider := counterDecider()
		decider.State = cancelingCounterState(cancel)

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
		decider.Decide = func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
			cancel()
			return decide(ctx, cmd, current)
		}

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
