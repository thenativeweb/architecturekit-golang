package architecturekit_test

import (
	"context"
	"errors"
	"fmt"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/architecturekittest"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

func TestExecute(t *testing.T) {
	t.Run("writes events and returns them", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		written, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 3})
		require.NoError(t, err)

		require.Len(t, written, 1)
		assert.NotEmpty(t, written[0].ID, "a written event must carry its ID")
		assert.Equal(t, (incremented{}).EventType(), written[0].Type)
		assert.Equal(t, 3, totalIn(t, store, subject))
	})

	t.Run("appends to existing subject", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		ctx := context.Background()

		for _, by := range []int{1, 2, 4} {
			_, err := architecturekit.Execute(ctx, store, counterDecider(),
				increment{subject: subject, By: by})
			require.NoError(t, err)
		}

		assert.Equal(t, 7, totalIn(t, store, subject))
	})

	t.Run("returns domain error without writing", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		ctx := context.Background()

		_, err := architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: 5, Limit: 10})
		require.NoError(t, err)

		_, err = architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: 8, Limit: 10})

		var domainError *architecturekit.DomainError
		assert.ErrorAs(t, err, &domainError)
		assert.ErrorIs(t, err, architecturekit.ErrDomain, "a domain error must also be an ErrDomain")
		assert.Equal(t, 5, totalIn(t, store, subject), "a rejected command must not write")
	})

	t.Run("writes nothing when decide returns no events", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		written, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 0})
		require.NoError(t, err)
		assert.Nil(t, written)
		assert.Equal(t, 0, totalIn(t, store, subject))
	})

	t.Run("unconditionally appends blindly", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		ctx := context.Background()

		// Concurrent commands that write unconditionally all succeed. That is what
		// Unconditionally is for, not an accident.
		const concurrent = 4
		var waitGroup sync.WaitGroup
		for range concurrent {
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				_, _ = architecturekit.Execute(ctx, store, counterDecider(),
					increment{subject: subject, By: 1})
			}()
		}
		waitGroup.Wait()

		assert.Equal(t, concurrent, totalIn(t, store, subject))
	})

	t.Run("with pristine precondition admits only one", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		ctx := context.Background()

		const concurrent = 8
		var waitGroup sync.WaitGroup
		results := make([]error, concurrent)

		for i := range concurrent {
			waitGroup.Add(1)
			go func() {
				defer waitGroup.Done()
				_, results[i] = architecturekit.Execute(ctx, store, counterDecider(),
					increment{subject: subject, By: 1}.pristine())
			}()
		}
		waitGroup.Wait()

		succeeded, conflicted := 0, 0
		for i, err := range results {
			switch {
			case err == nil:
				succeeded++
			case errors.Is(err, architecturekit.ErrConflict):
				conflicted++
			default:
				assert.NoError(t, err, "goroutine %d failed unexpectedly", i)
			}
		}

		assert.Equal(t, 1, succeeded, "exactly one command must succeed")
		assert.Equal(t, concurrent-1, conflicted, "the others must conflict")
		assert.Equal(t, 1, totalIn(t, store, subject))
	})

	t.Run("with revision precondition detects stale reads", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		ctx := context.Background()

		written, err := architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: 1})
		require.NoError(t, err)
		stale := written[0].ID

		// Someone else writes in between, so the caller's view is out of date.
		_, err = architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: 1})
		require.NoError(t, err)

		_, err = architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: 1}.onEventID(stale))

		assert.ErrorIs(t, err, architecturekit.ErrConflict)
		assert.ErrorIs(t, err, architecturekit.ErrTransient, "a conflict must also be transient")
	})

	t.Run("fails on event type without rule", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    "io.thenativeweb.test.unexpected",
			Data:    map[string]any{"x": 1},
		}}, nil)
		require.NoError(t, err)

		_, err = architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a missing rule is permanent")
		assert.ErrorContains(t, err, "io.thenativeweb.test.unexpected", "error should name the event type")
	})

	t.Run("refuses an event its state has no rule for and writes nothing", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		_, err := architecturekit.Execute(context.Background(), store,
			emittingDecider(counterState(), annotated{Note: "no rule"}), increment{subject: subject})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "an event without a rule is permanent")
		assert.ErrorContains(t, err, `event of type "io.thenativeweb.test.annotated" to "`+subject+`"`,
			"error should name the event type and the subject")
		assert.ErrorContains(t, err, "could not read the subject any more", "error should say why")
		assert.Empty(t, eventTypesIn(t, subject), "nothing may have been written")

		// The subject is still readable, so the next command succeeds.
		_, err = architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 2})
		require.NoError(t, err)
		assert.Equal(t, 2, totalIn(t, store, subject))
	})

	t.Run("writes none of the events if one has no rule", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		_, err := architecturekit.Execute(context.Background(), store,
			emittingDecider(counterState(), incremented{By: 1}, annotated{Note: "no rule"}),
			increment{subject: subject})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorContains(t, err, `"io.thenativeweb.test.annotated"`, "error should name the event type")
		assert.Empty(t, eventTypesIn(t, subject), "not even the event with a rule may have been written")
	})

	t.Run("refuses with the same error as the test fixture", func(t *testing.T) {
		store := requireStore(t)
		decider := emittingDecider(counterState(), annotated{Note: "no rule"})
		cmd := increment{subject: subjectFor(t)}

		_, err := architecturekit.Execute(context.Background(), store, decider, cmd)
		require.ErrorIs(t, err, architecturekit.ErrPermanent, "Execute has to refuse the event")

		// The fixture of architecturekittest repeats the check of Execute, since
		// it lives in another package, so both have to word the refusal alike.
		assert.Equal(t, err.Error(), fixtureRefusal(t, decider, cmd))
	})

	t.Run("refuses data that can not be encoded with the same error as the test fixture", func(t *testing.T) {
		for _, test := range unencodableEvents() {
			t.Run(test.name, func(t *testing.T) {
				decider := emittingDecider(encodingState(), incremented{By: 1}, test.event)
				cmd := increment{subject: subjectFor(t)}

				_, err := architecturekit.Execute(context.Background(), requireStore(t), decider, cmd)
				require.ErrorIs(t, err, architecturekit.ErrPermanent, "Execute has to refuse the event")
				require.ErrorContains(t, err, "can not be encoded as JSON", "Execute has to refuse the data")

				// The fixture encodes the events itself, as Execute does, so both
				// have to word the refusal alike.
				assert.Equal(t, err.Error(), fixtureRefusal(t, decider, cmd))
			})
		}
	})

	t.Run("checks the rules before the encoding like the test fixture", func(t *testing.T) {
		decider := emittingDecider(encodingState(), measured{Value: math.NaN()}, labelled{Label: "no rule"})
		cmd := increment{subject: subjectFor(t)}

		_, err := architecturekit.Execute(context.Background(), requireStore(t), decider, cmd)
		require.ErrorIs(t, err, architecturekit.ErrPermanent, "Execute has to refuse the events")
		require.ErrorContains(t, err, `event of type "io.thenativeweb.test.labelled"`,
			"Execute has to check the rules of all events first")

		assert.Equal(t, err.Error(), fixtureRefusal(t, decider, cmd))
	})

	t.Run("refuses a nil event and writes nothing", func(t *testing.T) {
		for _, test := range []struct {
			name   string
			events []architecturekit.Event
			index  int
		}{
			{"as the only event", []architecturekit.Event{nil}, 0},
			{"before another event", []architecturekit.Event{nil, incremented{By: 1}}, 0},
			{"after another event", []architecturekit.Event{incremented{By: 1}, nil}, 1},
			{"as the first of several", []architecturekit.Event{incremented{By: 1}, nil, incremented{By: 2}, nil}, 1},
			{"as a nil pointer of a concrete type", []architecturekit.Event{incremented{By: 1}, (*incremented)(nil)}, 1},
		} {
			t.Run(test.name, func(t *testing.T) {
				store := requireStore(t)
				subject := subjectFor(t)

				_, err := architecturekit.Execute(context.Background(), store,
					emittingDecider(counterState(), test.events...), increment{subject: subject})

				require.ErrorIs(t, err, architecturekit.ErrPermanent, "a nil event is permanent")
				assert.EqualError(t, err, fmt.Sprintf("%v: refusing to write to %q, since event %d that the decider "+
					"returned is nil", architecturekit.ErrPermanent, subject, test.index),
					"error should name the subject and the index of the event")
				assert.Empty(t, eventTypesIn(t, subject), "nothing may have been written")
			})
		}
	})

	t.Run("refuses a nil event with the same error as the test fixture", func(t *testing.T) {
		decider := emittingDecider(counterState(), incremented{By: 1}, nil)
		cmd := increment{subject: subjectFor(t)}

		_, err := architecturekit.Execute(context.Background(), requireStore(t), decider, cmd)
		require.ErrorIs(t, err, architecturekit.ErrPermanent, "Execute has to refuse the event")
		require.ErrorContains(t, err, "event 1 that the decider returned is nil", "Execute has to refuse the nil event")

		// The fixture checks for nil events itself, as Execute does, so both
		// have to word the refusal alike.
		assert.Equal(t, err.Error(), fixtureRefusal(t, decider, cmd))
	})

	t.Run("refuses a nil pointer of a concrete type with the same error as the test fixture", func(t *testing.T) {
		// The pointer is not equal to nil, since the interface knows its type,
		// but calling EventType on it panics.
		decider := emittingDecider(counterState(), incremented{By: 1}, (*incremented)(nil))
		cmd := increment{subject: subjectFor(t)}

		_, err := architecturekit.Execute(context.Background(), requireStore(t), decider, cmd)
		require.ErrorIs(t, err, architecturekit.ErrPermanent, "Execute has to refuse the event")
		require.ErrorContains(t, err, "event 1 that the decider returned is nil", "Execute has to refuse the nil pointer")

		assert.Equal(t, err.Error(), fixtureRefusal(t, decider, cmd))
	})

	t.Run("checks for nil events before the rules and the encoding like the test fixture", func(t *testing.T) {
		decider := emittingDecider(encodingState(), measured{Value: math.NaN()}, labelled{Label: "no rule"}, nil)
		cmd := increment{subject: subjectFor(t)}

		_, err := architecturekit.Execute(context.Background(), requireStore(t), decider, cmd)
		require.ErrorIs(t, err, architecturekit.ErrPermanent, "Execute has to refuse the events")
		require.ErrorContains(t, err, "event 2 that the decider returned is nil",
			"Execute has to check all events for nil first")

		assert.Equal(t, err.Error(), fixtureRefusal(t, decider, cmd))
	})

	t.Run("writes an event its state ignores", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		written, err := architecturekit.Execute(context.Background(), store,
			emittingDecider(resetIgnoringState(), incremented{By: 3}, reset{}), increment{subject: subject})
		require.NoError(t, err)

		require.Len(t, written, 2)
		assert.Equal(t, []string{(incremented{}).EventType(), (reset{}).EventType()}, eventTypesIn(t, subject))
	})

	t.Run("reports an unreachable database", func(t *testing.T) {
		brokenStore := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

		_, err := architecturekit.Execute(context.Background(), brokenStore, counterDecider(),
			increment{subject: "/test/unreachable", By: 1})

		assert.ErrorIs(t, err, architecturekit.ErrTransient, "an unreachable database is transient")
		assert.ErrorContains(t, err, "reading")
	})

	t.Run("reports technical write failures", func(t *testing.T) {
		// The source has to be a valid URI, otherwise the database rejects the
		// write. Reading still works, so this reaches the write path.
		brokenStore := architecturekit.NewStore(rawClient(t), "not a valid uri")

		_, err := architecturekit.Execute(context.Background(), brokenStore, counterDecider(),
			increment{subject: subjectFor(t), By: 1})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "an invalid source is permanent")
		assert.NotErrorIs(t, err, architecturekit.ErrConflict, "an invalid source is not a conflict")
	})

	t.Run("fails when stored data does not match the rule", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		// The annotated type has no schema, so the database lets a mismatching
		// field type through, and the rule trips over it when reading.
		_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    (annotated{}).EventType(),
			Data:    map[string]any{"note": 42},
		}}, nil)
		require.NoError(t, err)

		_, err = architecturekit.Execute(context.Background(), store, noteDecider(),
			annotate{subject: subject, event: annotated{Note: "hello"}})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorContains(t, err, "decoding")
	})

	t.Run("reports a schema violation as permanent", func(t *testing.T) {
		store := requireStore(t)

		err := architecturekit.RegisterSchemas(context.Background(), store, []architecturekit.EventSchema{{
			EventType: (labelled{}).EventType(),
			Schema:    (labelled{}).Schema(),
		}})
		require.NoError(t, err)

		_, err = architecturekit.Execute(context.Background(), store, noteDecider(),
			annotate{subject: subjectFor(t), event: labelled{Label: ""}})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a schema violation is permanent")
		assert.NotErrorIs(t, err, architecturekit.ErrConflict, "a schema violation is not a conflict")
		assert.ErrorContains(t, err, "does not match schema", "error should carry the reason from the database")
	})

	t.Run("reports a failing upcaster", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		// An event of the old type is in the stream, and its upcaster refuses.
		_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    "io.thenativeweb.test.outdated",
			Data:    map[string]any{"whatever": true},
		}}, nil)
		require.NoError(t, err)

		state := architecturekit.NewState(counter{})
		state.Evolve(func(current counter, event incremented) counter { return current })
		state.UpcastWith(architecturekit.NewUpcasters().
			Upcast("io.thenativeweb.test.outdated",
				func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
					return nil, errors.New("this one cannot be migrated")
				}))

		decider := architecturekit.Decider[increment, counter]{
			State: state,
			Decide: func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
				return []architecturekit.Event{incremented{By: 1}}, nil
			},
		}

		_, err = architecturekit.Execute(context.Background(), store, decider,
			increment{subject: subject, By: 1})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a failing upcaster is permanent")
		assert.ErrorContains(t, err, "cannot be migrated")
	})
}

// emittingDecider returns exactly the given events, whatever the command and
// the state, on the given state.
func emittingDecider(
	state *architecturekit.State[counter],
	events ...architecturekit.Event,
) architecturekit.Decider[increment, counter] {
	return architecturekit.Decider[increment, counter]{
		State: state,
		Decide: func(context.Context, increment, counter) ([]architecturekit.Event, error) {
			return events, nil
		},
	}
}

// fixtureRefusal returns the error with which the test fixture of
// architecturekittest refuses the command, as it reports the error to an
// assertion that expects nothing to happen. ThenFailed only tells whether
// that error matches another one, so this is how the tests compare its
// wording with the one of Execute.
func fixtureRefusal(t *testing.T, decider architecturekit.Decider[increment, counter], cmd increment) string {
	t.Helper()

	recorder := &failureRecorder{}
	architecturekittest.Given(recorder, decider).When(cmd).ThenNothing()

	require.Len(t, recorder.failures, 1, "the fixture has to refuse the command")

	refusal, found := strings.CutPrefix(recorder.failures[0], "expected nothing to happen, got error: ")
	require.True(t, found, "the fixture has to refuse the command, but reported: %s", recorder.failures[0])

	return refusal
}

// failureRecorder takes the failures of the test fixture in place of a
// *testing.T, so that a test can read them.
type failureRecorder struct {
	failures []string
}

func (r *failureRecorder) Helper() {}

func (r *failureRecorder) Fatalf(format string, args ...any) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// eventTypesIn reads the types of the events in a subject straight from the
// database, bypassing the framework's abstractions.
func eventTypesIn(t *testing.T, subject string) []string {
	t.Helper()

	var eventTypes []string
	for event, err := range rawClient(t).ReadEvents(context.Background(), subject,
		eventsourcingdb.ReadEventsOptions{Recursive: false}) {
		require.NoError(t, err, "failed to read %q", subject)
		eventTypes = append(eventTypes, event.Type)
	}

	return eventTypes
}

func TestConflict(t *testing.T) {
	t.Run("is reported not retried", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		// A pristine precondition on a populated subject can never hold, so this
		// shows that the kit reports instead of trying again.
		_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1})
		require.NoError(t, err)

		_, err = architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1}.pristine())

		assert.ErrorIs(t, err, architecturekit.ErrConflict)
		assert.Equal(t, 1, totalIn(t, store, subject), "nothing more may have been written")
	})
}

// endsBeforeRegistering is a context that counts as ended to whoever asks
// for its error, while its Done channel stays open. The client only watches
// Done while it reads, so reading succeeds, and the first one to ask is the
// check right before a schema would be registered.
type endsBeforeRegistering struct {
	context.Context
}

func (endsBeforeRegistering) Err() error { return context.Canceled }

func registeredSchemaOf(t *testing.T, eventType string) bool {
	t.Helper()

	for registered, err := range rawClient(t).ReadEventTypes(context.Background()) {
		require.NoError(t, err)
		if registered.EventType == eventType && registered.Schema != nil {
			return true
		}
	}

	return false
}

func TestRegisterSchemasWithAContextThatEnds(t *testing.T) {
	t.Run("fails with the error of the context while reading", func(t *testing.T) {
		store := requireStore(t)

		// With every schema registered already, reading the registered ones is
		// all that is left to do, so that is where the context has to count.
		require.NoError(t, architecturekit.RegisterSchemas(context.Background(), store, counterState().Schemas()))

		ended, cancel := context.WithCancel(context.Background())
		cancel()

		err := architecturekit.RegisterSchemas(ended, store, counterState().Schemas())

		assert.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, architecturekit.ErrTransient, "an ended context is no failure of the database")
	})

	t.Run("registers nothing once the context has ended", func(t *testing.T) {
		store := requireStore(t)
		eventType := fmt.Sprintf("io.thenativeweb.test.never-registered-%d", time.Now().UnixNano())

		err := architecturekit.RegisterSchemas(endsBeforeRegistering{context.Background()}, store, []architecturekit.EventSchema{{
			EventType: eventType,
			Schema:    map[string]any{"type": "object"},
		}})

		assert.ErrorIs(t, err, context.Canceled)
		assert.Contains(t, err.Error(), eventType, "the error names the schema it did not register")
		assert.False(t, registeredSchemaOf(t, eventType), "a schema was registered after the context had ended")
	})
}

func TestRegisterSchemas(t *testing.T) {
	t.Run("is idempotent", func(t *testing.T) {
		store := requireStore(t)

		assert.NoError(t, architecturekit.RegisterSchemas(context.Background(), store, counterState().Schemas(), counterState().Schemas()),
			"duplicates within one call")
		assert.NoError(t, architecturekit.RegisterSchemas(context.Background(), store, counterState().Schemas()), "second call")
	})

	t.Run("keeps the database writable", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		// The checked event type must not be the last one: after reading a single
		// event type that further event types follow, EventSourcingDB 1.2.0 stops
		// answering writes. RegisterSchemas must not run into that.
		for _, name := range []string{"a", "b", "c", "d"} {
			_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
				Source:  "https://thenativeweb.io",
				Subject: subject,
				Type:    "io.thenativeweb.test.writable." + name,
				Data:    map[string]any{},
			}}, nil)
			require.NoError(t, err)
		}

		schemas := []architecturekit.EventSchema{{
			EventType: "io.thenativeweb.test.writable.b",
			Schema:    objectSchema(map[string]any{}),
		}}
		for range 2 {
			require.NoError(t, architecturekit.RegisterSchemas(context.Background(), store, schemas))
		}

		written := make(chan error, 1)
		go func() {
			_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
				Source:  "https://thenativeweb.io",
				Subject: subject,
				Type:    "io.thenativeweb.test.writable.e",
				Data:    map[string]any{},
			}}, nil)
			written <- err
		}()

		select {
		case err := <-written:
			assert.NoError(t, err)
		case <-time.After(10 * time.Second):
			assert.Fail(t, "the database stopped answering writes")
		}
	})

	t.Run("reports an unreachable database", func(t *testing.T) {
		store := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

		err := architecturekit.RegisterSchemas(context.Background(), store, counterState().Schemas())

		assert.ErrorIs(t, err, architecturekit.ErrTransient, "an unreachable database is transient")
		assert.ErrorContains(t, err, "reading the registered schemas")
	})

	t.Run("fails on a schema that is not JSON", func(t *testing.T) {
		store := requireStore(t)
		eventType := "io.thenativeweb.test.notjson"
		notJSON := map[string]any{"type": make(chan int)}

		// Twice within one call, where the schemas are compared with each other.
		err := architecturekit.RegisterSchemas(context.Background(), store,
			[]architecturekit.EventSchema{{EventType: eventType, Schema: objectSchema(map[string]any{})}},
			[]architecturekit.EventSchema{{EventType: eventType, Schema: notJSON}},
		)
		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "within one call")
		assert.ErrorContains(t, err, "comparing schemas", "within one call")

		// Once registered, where the schema is compared with the registered one.
		require.NoError(t, architecturekit.RegisterSchemas(context.Background(), store, []architecturekit.EventSchema{{
			EventType: eventType,
			Schema:    objectSchema(map[string]any{}),
		}}))
		err = architecturekit.RegisterSchemas(context.Background(), store, []architecturekit.EventSchema{{EventType: eventType, Schema: notJSON}})
		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "against the registered one")
		assert.ErrorContains(t, err, "comparing schemas", "against the registered one")
	})

	t.Run("reports a failing read after a conflict", func(t *testing.T) {
		// The database knows no schema at first, refuses to register one, and then
		// fails when RegisterSchemas reads the schemas again to tell why.
		var reads atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			writer.Header().Set("Server", "EventSourcingDB/test")

			switch request.URL.Path {
			case "/api/v1/read-event-types":
				if reads.Add(1) > 1 {
					writer.WriteHeader(http.StatusInternalServerError)
					return
				}
				writer.WriteHeader(http.StatusOK)
			case "/api/v1/register-event-schema":
				writer.WriteHeader(http.StatusConflict)
			default:
				writer.WriteHeader(http.StatusNotFound)
			}
		}))
		defer server.Close()

		serverURL, err := url.Parse(server.URL)
		require.NoError(t, err)
		client, err := eventsourcingdb.NewClient(serverURL, "secret")
		require.NoError(t, err)
		store := architecturekit.NewStore(client, "https://thenativeweb.io")

		err = architecturekit.RegisterSchemas(context.Background(), store, counterState().Schemas())

		assert.ErrorIs(t, err, architecturekit.ErrTransient, "a failing read is transient")
		assert.ErrorContains(t, err, "reading the registered schemas")
	})

	t.Run("fails on an invalid schema", func(t *testing.T) {
		store := requireStore(t)

		err := architecturekit.RegisterSchemas(context.Background(), store, []architecturekit.EventSchema{{
			EventType: "io.thenativeweb.test.invalid",
			Schema:    map[string]any{"type": "this-is-not-a-json-schema-type"},
		}})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "an invalid schema is permanent")
		assert.ErrorContains(t, err, "io.thenativeweb.test.invalid", "error should name the event type")
	})

	t.Run("fails on a changed schema", func(t *testing.T) {
		store := requireStore(t)
		eventType := "io.thenativeweb.test.changed"

		err := architecturekit.RegisterSchemas(context.Background(), store, []architecturekit.EventSchema{{
			EventType: eventType,
			Schema:    objectSchema(map[string]any{"text": map[string]any{"type": "string"}}),
		}})
		require.NoError(t, err, "first registration")

		// The same event type, now with an additional field.
		err = architecturekit.RegisterSchemas(context.Background(), store, []architecturekit.EventSchema{{
			EventType: eventType,
			Schema: objectSchema(map[string]any{
				"text": map[string]any{"type": "string"},
				"tags": map[string]any{"type": "array"},
			}),
		}})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a changed schema is permanent")
		assert.ErrorContains(t, err, eventType)
		assert.ErrorContains(t, err, "differs from the registered one")
	})

	t.Run("fails if stored events do not match the schema", func(t *testing.T) {
		store := requireStore(t)
		eventType := "io.thenativeweb.test.unmatched"

		_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
			Source:  "https://thenativeweb.io",
			Subject: subjectFor(t),
			Type:    eventType,
			Data:    map[string]any{"other": 42},
		}}, nil)
		require.NoError(t, err)

		err = architecturekit.RegisterSchemas(context.Background(), store, []architecturekit.EventSchema{{
			EventType: eventType,
			Schema:    objectSchema(map[string]any{"text": map[string]any{"type": "string"}}),
		}})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a refused schema is permanent")
		assert.ErrorContains(t, err, "refused the schema of \""+eventType+"\"")
		// The reason from the database names what does not match.
		assert.ErrorContains(t, err, "additionalProperties 'other' not allowed",
			"error should carry the reason from the database")
	})

	t.Run("fails on a missing schema", func(t *testing.T) {
		// The schema is checked before the database is contacted.
		store := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

		err := architecturekit.RegisterSchemas(context.Background(), store, []architecturekit.EventSchema{{
			EventType: "io.thenativeweb.test.schemaless",
		}})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a missing schema is permanent")
		assert.ErrorContains(t, err, `"io.thenativeweb.test.schemaless" has no schema`)
	})

	t.Run("fails on two schemas for one event type", func(t *testing.T) {
		store := requireStore(t)
		eventType := "io.thenativeweb.test.twofold"

		err := architecturekit.RegisterSchemas(context.Background(), store,
			[]architecturekit.EventSchema{{
				EventType: eventType,
				Schema:    objectSchema(map[string]any{}),
			}},
			[]architecturekit.EventSchema{{
				EventType: eventType,
				Schema:    objectSchema(map[string]any{"text": map[string]any{"type": "string"}}),
			}},
		)

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "two schemas for one event type are permanent")
		assert.ErrorContains(t, err, "two different schemas")
	})
}
