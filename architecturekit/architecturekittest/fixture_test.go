package architecturekittest_test

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/architecturekittest"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// spy captures the fixture's failure messages instead of ending a test.
type spy struct {
	failures []string
}

func (s *spy) Helper() {}

func (s *spy) Fatalf(format string, args ...any) {
	s.failures = append(s.failures, fmt.Sprintf(format, args...))
}

func (s *spy) firstFailure() string {
	if len(s.failures) == 0 {
		return ""
	}

	return s.failures[0]
}

func (s *spy) expectFailure(t *testing.T, containing string) {
	t.Helper()

	require.NotEmpty(t, s.failures, "expected a failure containing %q", containing)
	require.Contains(t, s.firstFailure(), containing)
}

func (s *spy) expectNoFailure(t *testing.T) {
	t.Helper()

	require.Empty(t, s.failures)
}

// --- test domain ---

type opened struct {
	Owner string `json:"owner"`
}

func (opened) EventType() string { return "test.account.opened" }

func (opened) Schema() map[string]any { return openedSchema() }

func openedSchema() map[string]any {
	return objectSchema(map[string]any{"owner": map[string]any{"type": "string"}})
}

type closed struct{}

func (closed) EventType() string { return "test.account.closed" }

func (closed) Schema() map[string]any { return objectSchema(map[string]any{}) }

type unheardOf struct{}

func (unheardOf) EventType() string { return "test.account.unheardOf" }

func (unheardOf) Schema() map[string]any { return objectSchema(map[string]any{}) }

// objectSchema describes an object with exactly the given properties.
func objectSchema(properties map[string]any) map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"properties":           properties,
	}
}

// unmarshallable reports the same type as opened but cannot be marshalled.
type unmarshallable struct {
	Channel chan int `json:"channel"`
}

func (unmarshallable) EventType() string { return "test.account.opened" }

func (unmarshallable) Schema() map[string]any { return openedSchema() }

type account struct {
	IsOpen bool
	Owner  string
}

type open struct {
	Owner string

	// preconditions is what this command declares; nil means none, which
	// Execute would reject, but which the fixture still has to describe.
	preconditions []architecturekit.Precondition
}

func (open) Subject() string { return "/account/1" }

func (c open) Preconditions() []architecturekit.Precondition { return c.preconditions }

func accountState() *architecturekit.State[account] {
	state := architecturekit.NewState(account{})

	state.Evolve(func(current account, event opened) account {
		current.IsOpen = true
		current.Owner = event.Owner
		return current
	})
	state.Evolve(func(current account, event closed) account {
		current.IsOpen = false
		return current
	})

	// An older type, reachable only through its upcaster.
	state.UpcastWith(architecturekit.NewUpcasters().
		Upcast("test.account.opened.v1",
			func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
				event.Type = "test.account.opened"
				event.Data = []byte(`{"owner":"from the old shape"}`)
				return []eventsourcingdb.Event{event}, nil
			}))

	return state
}

func decider() architecturekit.Decider[open, account] {
	return architecturekit.Decider[open, account]{
		State: accountState(),
		Decide: func(ctx context.Context, cmd open, current account) ([]architecturekit.Event, error) {
			if current.IsOpen {
				return nil, architecturekit.NewDomainError("account is already open")
			}
			if cmd.Owner == "" {
				return nil, fmt.Errorf("%w: no owner given", architecturekit.ErrPermanent)
			}
			if cmd.Owner == "nobody" {
				// Nothing to do, and nothing wrong either.
				return nil, nil
			}

			return []architecturekit.Event{opened{Owner: cmd.Owner}}, nil
		},
	}
}

// emitDecider emits exactly what it was handed, which is how the tests reach
// the marshalling paths.
type emit struct {
	events []architecturekit.Event
}

func (emit) Subject() string { return "/account/1" }

func (emit) Preconditions() []architecturekit.Precondition {
	return []architecturekit.Precondition{architecturekit.Unconditionally()}
}

func emitDecider() architecturekit.Decider[emit, account] {
	return architecturekit.Decider[emit, account]{
		State: architecturekit.NewState(account{}),
		Decide: func(ctx context.Context, cmd emit, _ account) ([]architecturekit.Event, error) {
			return cmd.events, nil
		},
	}
}

// --- the happy paths and the fixture's own failure paths ---

func TestGiven(t *testing.T) {
	t.Run("when then events", func(t *testing.T) {
		architecturekittest.Given(t, decider()).
			When(open{Owner: "golo"}).
			ThenEvents(opened{Owner: "golo"})
	})

	t.Run("fails on event without rule", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider(), unheardOf{})

		recorder.expectFailure(t, "given:")
	})
}

func TestHistory(t *testing.T) {
	t.Run("is folded in order", func(t *testing.T) {
		architecturekittest.Given(t, decider(), opened{Owner: "golo"}, closed{}).
			When(open{Owner: "jane"}).
			ThenEvents(opened{Owner: "jane"})
	})
}

func TestThenNothing(t *testing.T) {
	t.Run("when there is nothing to do", func(t *testing.T) {
		architecturekittest.Given(t, decider()).
			When(open{Owner: "nobody"}).
			ThenNothing()
	})

	t.Run("fails when something happened", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{Owner: "golo"}).
			ThenNothing()

		recorder.expectFailure(t, "1 event(s)")
	})

	t.Run("fails on an error", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{}).
			ThenNothing()

		recorder.expectFailure(t, "no owner given")
	})
}

func TestThenRejected(t *testing.T) {
	t.Run("and ThenFailed describe the same rejection", func(t *testing.T) {
		architecturekittest.Given(t, decider(), opened{Owner: "golo"}).
			When(open{Owner: "jane"}).
			ThenRejected("account is already open").
			ThenFailed(architecturekit.ErrDomain)
	})

	t.Run("fails when events were produced", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{Owner: "golo"}).
			ThenRejected("account is already open")

		recorder.expectFailure(t, "1 event(s)")
	})

	t.Run("fails on wrong message", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider(), opened{Owner: "golo"}).
			When(open{Owner: "jane"}).
			ThenRejected("something else")

		recorder.expectFailure(t, "something else")
	})
}

func TestThenFailed(t *testing.T) {
	t.Run("tells categories apart", func(t *testing.T) {
		architecturekittest.Given(t, decider()).
			When(open{}).
			ThenFailed(architecturekit.ErrPermanent)
	})

	t.Run("fails when nothing failed", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{Owner: "golo"}).
			ThenFailed(architecturekit.ErrDomain)

		recorder.expectFailure(t, "1 event(s)")
	})

	t.Run("fails on the wrong category", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider(), opened{Owner: "golo"}).
			When(open{Owner: "jane"}).
			ThenFailed(architecturekit.ErrTransient)

		recorder.expectFailure(t, "category")
	})
}

func TestThenState(t *testing.T) {
	t.Run("checks what was decided on", func(t *testing.T) {
		checked := false

		architecturekittest.Given(t, decider(), opened{Owner: "golo"}).
			When(open{Owner: "jane"}).
			ThenState(func(state account) {
				checked = true
				assert.Equal(t, account{IsOpen: true, Owner: "golo"}, state)
			})

		assert.True(t, checked, "ThenState did not run its check")
	})
}

func TestGivenStored(t *testing.T) {
	t.Run("runs the upcasters", func(t *testing.T) {
		// The old type has no Go struct any more, so only the stored shape can
		// carry it. This is exactly what typed events cannot express.
		old := eventsourcingdb.Event{
			Subject: "/account/1",
			Type:    "test.account.opened.v1",
			ID:      "0",
			Data:    []byte(`{"whatever":true}`),
		}

		architecturekittest.GivenStored(t, decider(), old).
			When(open{Owner: "jane"}).
			ThenState(func(state account) {
				assert.Equal(t, "from the old shape", state.Owner, "the upcaster did not run")
			}).
			ThenRejected("account is already open")
	})

	t.Run("fails on event without rule", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.GivenStored(recorder, decider(), eventsourcingdb.Event{
			Subject: "/account/1",
			Type:    "test.account.unheardOf",
			Data:    []byte(`{}`),
		})

		recorder.expectFailure(t, "given stored:")
	})
}

func TestEventMatchers(t *testing.T) {
	t.Run("pass when the events match", func(t *testing.T) {
		isOpened := func(event architecturekit.Event) bool {
			return event.EventType() == (opened{}).EventType()
		}
		isClosed := func(event architecturekit.Event) bool {
			return event.EventType() == (closed{}).EventType()
		}

		architecturekittest.Given(t, emitDecider()).
			When(emit{events: []architecturekit.Event{opened{Owner: "golo"}, opened{Owner: "jane"}}}).
			ThenSomeEvent(isOpened).
			ThenEveryEvent(isOpened).
			ThenNoEvent(isClosed)
	})

	t.Run("fail when nothing matches", func(t *testing.T) {
		isClosed := func(event architecturekit.Event) bool {
			return event.EventType() == (closed{}).EventType()
		}
		isOpened := func(event architecturekit.Event) bool {
			return event.EventType() == (opened{}).EventType()
		}

		some := &spy{}
		architecturekittest.Given(some, decider()).When(open{Owner: "golo"}).ThenSomeEvent(isClosed)
		some.expectFailure(t, "no event matched")

		every := &spy{}
		architecturekittest.Given(every, emitDecider()).
			When(emit{events: []architecturekit.Event{opened{Owner: "golo"}, closed{}}}).
			ThenEveryEvent(isOpened)
		every.expectFailure(t, "did not match")

		none := &spy{}
		architecturekittest.Given(none, decider()).When(open{Owner: "golo"}).ThenNoEvent(isOpened)
		none.expectFailure(t, "matched although it should not")

		empty := &spy{}
		architecturekittest.Given(empty, decider()).When(open{Owner: "nobody"}).ThenEveryEvent(isOpened)
		empty.expectFailure(t, "got none")
	})

	t.Run("fail on an error", func(t *testing.T) {
		always := func(architecturekit.Event) bool { return true }

		for label, check := range map[string]func(o *architecturekittest.Outcome[open, account]){
			"ThenSomeEvent":  func(o *architecturekittest.Outcome[open, account]) { o.ThenSomeEvent(always) },
			"ThenEveryEvent": func(o *architecturekittest.Outcome[open, account]) { o.ThenEveryEvent(always) },
			"ThenNoEvent":    func(o *architecturekittest.Outcome[open, account]) { o.ThenNoEvent(always) },
		} {
			t.Run(label, func(t *testing.T) {
				recorder := &spy{}
				check(architecturekittest.Given(recorder, decider()).When(open{}))
				recorder.expectFailure(t, "no owner given")
				assert.NotEmpty(t, recorder.failures, "%s did not report the error", label)
			})
		}
	})
}

func TestPreconditionsOf(t *testing.T) {
	t.Run("a command", func(t *testing.T) {
		architecturekittest.Given(t, decider()).
			When(open{
				Owner: "golo",
				preconditions: []architecturekit.Precondition{
					architecturekit.Require(eventsourcingdb.NewIsSubjectOnEventIDPrecondition("/account/1", "7")),
				},
			}).
			ThenPreconditions(architecturekittest.OnEventID("/account/1", "7"))
	})

	t.Run("every kind", func(t *testing.T) {
		cmd := open{
			Owner: "golo",
			preconditions: []architecturekit.Precondition{
				architecturekit.Require(eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1")),
				architecturekit.Require(eventsourcingdb.NewIsSubjectPopulatedPrecondition("/account/2")),
				architecturekit.Require(eventsourcingdb.NewIsSubjectOnEventIDPrecondition("/account/3", "9")),
				architecturekit.Require(eventsourcingdb.NewIsEventQLQueryTruePrecondition("FROM e IN events PROJECT INTO true")),
				architecturekit.OnStateRead(),
			},
		}

		architecturekittest.Given(t, decider()).
			When(cmd).
			ThenPreconditions(
				// A pristine and a populated check look alike from outside,
				// because the client exposes only the subject for both.
				architecturekittest.OnSubject("/account/1"),
				architecturekittest.OnSubject("/account/2"),
				architecturekittest.OnEventID("/account/3", "9"),
				architecturekittest.OnQuery("FROM e IN events PROJECT INTO true"),
				architecturekittest.OnStateRead(),
			)
	})

	t.Run("an unconditional command", func(t *testing.T) {
		architecturekittest.Given(t, decider()).
			When(open{
				Owner:         "golo",
				preconditions: []architecturekit.Precondition{architecturekit.Unconditionally()},
			}).
			ThenPreconditions(architecturekittest.Unconditionally())
	})

	t.Run("shows an invalid precondition", func(t *testing.T) {
		// A zero value or a nil requirement is rejected by Execute, so the fixture
		// shows it as an empty description instead of dropping it.
		declared := architecturekittest.PreconditionsOf(open{
			preconditions: []architecturekit.Precondition{{}, architecturekit.Require(nil)},
		})

		assert.Equal(t, []architecturekittest.Precondition{{}, {}}, declared)
	})
}

func TestCommandWithoutPreconditions(t *testing.T) {
	t.Run("declares none", func(t *testing.T) {
		// Execute rejects such a command, but the fixture reports what it sees.
		assert.Empty(t, architecturekittest.PreconditionsOf(open{}))
	})
}

func TestThenEvents(t *testing.T) {
	t.Run("fails on unexpected error", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{}).
			ThenEvents(opened{Owner: "golo"})

		recorder.expectFailure(t, "no owner given")
	})

	t.Run("fails on wrong count", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{Owner: "golo"}).
			ThenEvents(opened{Owner: "golo"}, closed{})

		recorder.expectFailure(t, "expected 2 event(s), got 1")
	})

	t.Run("fails on wrong type", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{Owner: "golo"}).
			ThenEvents(closed{})

		recorder.expectFailure(t, "want \"test.account.closed\"")
	})

	t.Run("fails on wrong payload", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{Owner: "golo"}).
			ThenEvents(opened{Owner: "someone-else"})

		recorder.expectFailure(t, "someone-else")
	})

	t.Run("fails when the actual event cannot be marshalled", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, emitDecider()).
			When(emit{events: []architecturekit.Event{unmarshallable{Channel: make(chan int)}}}).
			ThenEvents(opened{Owner: "golo"})

		recorder.expectFailure(t, "json")
	})

	t.Run("fails when the expected event cannot be marshalled", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, emitDecider()).
			When(emit{events: []architecturekit.Event{opened{Owner: "golo"}}}).
			ThenEvents(unmarshallable{Channel: make(chan int)})

		recorder.expectFailure(t, "json")
	})

	t.Run("says so when nothing happened at all", func(t *testing.T) {
		recorder := &spy{}

		// The command decides there is nothing to do, so the mismatch has to read
		// as "no events" rather than as an empty list.
		architecturekittest.Given(recorder, decider()).
			When(open{Owner: "nobody"}).
			ThenEvents(opened{Owner: "golo"})

		recorder.expectFailure(t, "no events")
	})
}

func TestThenPreconditions(t *testing.T) {
	t.Run("fails on wrong count", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{Owner: "golo"}).
			ThenPreconditions(architecturekittest.OnSubject("/account/1"))

		recorder.expectFailure(t, "expected 1 precondition(s), got 0")
	})

	t.Run("fails on wrong content", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{
				Owner: "golo",
				preconditions: []architecturekit.Precondition{
					architecturekit.Require(eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1")),
				},
			}).
			ThenPreconditions(architecturekittest.OnSubject("/account/other"))

		recorder.expectFailure(t, "/account/other")
	})
}

func TestSpy(t *testing.T) {
	t.Run("Helper is harmless", func(t *testing.T) {
		// Helper only exists to satisfy the interface.
		recorder := &spy{}
		recorder.Helper()
		recorder.expectNoFailure(t)
	})
}
