package architecturekittest_test

import (
	"context"
	"fmt"
	"math"
	"strings"
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

// refusalOf returns the error with which the fixture refuses the command, as
// ThenNothing reports it. ThenFailed only tells whether that error matches
// another one, so this is how the tests compare its wording with the one of
// Execute.
func refusalOf[TCommand architecturekit.Command, TState any](
	t *testing.T,
	decider architecturekit.Decider[TCommand, TState],
	cmd TCommand,
) string {
	t.Helper()

	recorder := &spy{}
	architecturekittest.Given(recorder, decider).When(cmd).ThenNothing()

	require.Len(t, recorder.failures, 1, "the fixture has to refuse the command")

	refusal, found := strings.CutPrefix(recorder.firstFailure(), "expected nothing to happen, got error: ")
	require.True(t, found, "the fixture has to refuse the command, but reported: %s", recorder.firstFailure())

	return refusal
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

// measured holds a number, which encoding/json can not encode if it is NaN.
// The account state has no rule for it, so the tests that need one ignore it.
type measured struct {
	Value float64 `json:"value"`
}

func (measured) EventType() string { return "test.account.measured" }

func (measured) Schema() map[string]any {
	return objectSchema(map[string]any{"value": map[string]any{"type": "number"}})
}

type account struct {
	IsOpen bool
	Owner  string
}

type open struct {
	Owner string

	// preconditions is what this command declares; nil means OnStateRead, so
	// that the tests that are not about preconditions need not say so. An
	// empty slice means none, which Execute and When refuse, but which
	// PreconditionsOf still has to describe.
	preconditions []architecturekit.Precondition
}

func (open) Subject() string { return "/account/1" }

func (c open) Preconditions() []architecturekit.Precondition {
	if c.preconditions == nil {
		return []architecturekit.Precondition{architecturekit.OnStateRead()}
	}
	return c.preconditions
}

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

// uncomparableSubjectPrecondition stands for a change to the client that makes
// its subject checks impossible to compare. It satisfies the client's sealed
// interface by embedding it.
type uncomparableSubjectPrecondition struct {
	eventsourcingdb.Precondition

	subject []string
}

func (p uncomparableSubjectPrecondition) Subject() string { return p.subject[0] }

// The rejections of the account. The decider wraps them to name the owner, so
// that only errors.Is finds them, not a comparison of the errors themselves.
var (
	errAccountAlreadyOpen = architecturekit.NewDomainError("account is already open")
	errOwnerBanned        = architecturekit.NewDomainError("owner is banned")
)

func decider() architecturekit.Decider[open, account] {
	return architecturekit.Decider[open, account]{
		State: accountState(),
		Decide: func(ctx context.Context, cmd open, current account) ([]architecturekit.Event, error) {
			if current.IsOpen {
				return nil, fmt.Errorf("%w: owned by %s", errAccountAlreadyOpen, current.Owner)
			}
			if cmd.Owner == "mallory" {
				return nil, fmt.Errorf("%w: %s", errOwnerBanned, cmd.Owner)
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
// the marshalling paths. Its state is the one of the account, which has a rule
// for every event the tests hand it, except unheardOf.
type emit struct {
	events []architecturekit.Event

	// subject is where the events go, "/account/1" unless a test needs a
	// subject of its own.
	subject string
}

func (c emit) Subject() string {
	if c.subject == "" {
		return "/account/1"
	}

	return c.subject
}

func (emit) Preconditions() []architecturekit.Precondition {
	return []architecturekit.Precondition{architecturekit.Unconditionally()}
}

func emitDecider() architecturekit.Decider[emit, account] {
	return emitDeciderOn(accountState())
}

// emitDeciderOn emits exactly what it was handed, on the given state.
func emitDeciderOn(state *architecturekit.State[account]) architecturekit.Decider[emit, account] {
	return architecturekit.Decider[emit, account]{
		State: state,
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

func TestThenFailed(t *testing.T) {
	t.Run("tells categories apart", func(t *testing.T) {
		architecturekittest.Given(t, decider()).
			When(open{}).
			ThenFailed(architecturekit.ErrPermanent)
	})

	t.Run("matches a sentinel error the decider wraps, and its category", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider(), opened{Owner: "golo"}).
			When(open{Owner: "jane"}).
			ThenFailed(errAccountAlreadyOpen).
			ThenFailed(architecturekit.ErrDomain)

		recorder.expectNoFailure(t)
	})

	t.Run("tells the sentinel errors of a decider apart", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{Owner: "mallory"}).
			ThenFailed(errOwnerBanned)

		recorder.expectNoFailure(t)
	})

	t.Run("fails on a different sentinel error", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider(), opened{Owner: "golo"}).
			When(open{Owner: "jane"}).
			ThenFailed(errOwnerBanned)

		require.Len(t, recorder.failures, 1)
		assert.Equal(t,
			"expected an error matching owner is banned, got account is already open: owned by golo",
			recorder.firstFailure())
	})

	t.Run("fails when nothing failed", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{Owner: "golo"}).
			ThenFailed(architecturekit.ErrDomain)

		require.Len(t, recorder.failures, 1)
		assert.Equal(t,
			"expected an error matching domain rule violated, got 1 event(s): [test.account.opened]",
			recorder.firstFailure())
	})

	t.Run("fails on the wrong category", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider(), opened{Owner: "golo"}).
			When(open{Owner: "jane"}).
			ThenFailed(architecturekit.ErrTransient)

		recorder.expectFailure(t,
			"expected an error matching transient failure, got account is already open")
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
			ThenFailed(errAccountAlreadyOpen)
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
				architecturekittest.OnPristineSubject("/account/1"),
				architecturekittest.OnPopulatedSubject("/account/2"),
				architecturekittest.OnEventID("/account/3", "9"),
				architecturekittest.OnQuery("FROM e IN events PROJECT INTO true"),
				architecturekittest.OnStateRead(),
			)
	})

	t.Run("tells a pristine from a populated subject", func(t *testing.T) {
		declared := architecturekittest.PreconditionsOf(open{
			preconditions: []architecturekit.Precondition{
				architecturekit.Require(eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1")),
				architecturekit.Require(eventsourcingdb.NewIsSubjectPopulatedPrecondition("/account/1")),
			},
		})

		assert.Equal(t, []architecturekittest.Precondition{
			{Subject: "/account/1", Pristine: true},
			{Subject: "/account/1", Populated: true},
		}, declared)
	})

	t.Run("a subject check of a type that cannot be compared is neither", func(t *testing.T) {
		// Such a type would take a change to the client. It must not panic,
		// but show up as a subject of no known kind, so that a test fails.
		declared := architecturekittest.PreconditionsOf(open{
			preconditions: []architecturekit.Precondition{
				architecturekit.Require(uncomparableSubjectPrecondition{subject: []string{"/account/1"}}),
			},
		})

		assert.Equal(t, []architecturekittest.Precondition{{Subject: "/account/1"}}, declared)
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
		assert.Empty(t, architecturekittest.PreconditionsOf(open{preconditions: []architecturekit.Precondition{}}))
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

		// When refuses the event already, as Execute does, so ThenEvents
		// reports the refusal.
		architecturekittest.Given(recorder, emitDecider()).
			When(emit{events: []architecturekit.Event{unmarshallable{Channel: make(chan int)}}}).
			ThenEvents(opened{Owner: "golo"})

		recorder.expectFailure(t, "expected events, got error")
		recorder.expectFailure(t, "can not be encoded as JSON: json: unsupported type: chan int")
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

func TestEventWithoutRule(t *testing.T) {
	withoutRule := emit{events: []architecturekit.Event{unheardOf{}}}

	t.Run("fails ThenEvents, naming the event type", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, emitDecider()).
			When(withoutRule).
			ThenEvents(unheardOf{})

		recorder.expectFailure(t, `event of type "test.account.unheardOf" to "/account/1"`)
		recorder.expectFailure(t, "could not read the subject any more")
	})

	t.Run("fails every assertion that expects events or nothing", func(t *testing.T) {
		always := func(architecturekit.Event) bool { return true }

		for label, check := range map[string]func(o *architecturekittest.Outcome[emit, account]){
			"ThenNothing":    func(o *architecturekittest.Outcome[emit, account]) { o.ThenNothing() },
			"ThenSomeEvent":  func(o *architecturekittest.Outcome[emit, account]) { o.ThenSomeEvent(always) },
			"ThenEveryEvent": func(o *architecturekittest.Outcome[emit, account]) { o.ThenEveryEvent(always) },
			"ThenNoEvent":    func(o *architecturekittest.Outcome[emit, account]) { o.ThenNoEvent(always) },
		} {
			t.Run(label, func(t *testing.T) {
				recorder := &spy{}
				check(architecturekittest.Given(recorder, emitDecider()).When(withoutRule))
				recorder.expectFailure(t, `"test.account.unheardOf"`)
			})
		}
	})

	t.Run("is a permanent failure, also as the second event", func(t *testing.T) {
		architecturekittest.Given(t, emitDecider()).
			When(emit{events: []architecturekit.Event{opened{Owner: "golo"}, unheardOf{}}}).
			ThenFailed(architecturekit.ErrPermanent)
	})

	t.Run("lets an event through that the state ignores", func(t *testing.T) {
		ignoring := accountState().Ignore[unheardOf]()

		architecturekittest.Given(t, emitDeciderOn(ignoring)).
			When(withoutRule).
			ThenEvents(unheardOf{})
	})

}

// measuringDecider emits exactly what it was handed, on the account state that
// also ignores measured, so that its rules let every event of the tests
// through except unheardOf.
func measuringDecider() architecturekit.Decider[emit, account] {
	return emitDeciderOn(accountState().Ignore[measured]())
}

// encodingRefusal is the error with which Execute refuses an event whose data
// can not be encoded as JSON.
func encodingRefusal(eventType, subject, reason string) string {
	return fmt.Sprintf("%v: refusing to write an event of type %q to %q, since its data can not be encoded as JSON: %s",
		architecturekit.ErrPermanent, eventType, subject, reason)
}

func TestEventThatCanNotBeEncoded(t *testing.T) {
	withNaN := emit{events: []architecturekit.Event{measured{Value: math.NaN()}}}
	refusal := encodingRefusal("test.account.measured", "/account/1", "json: unsupported value: NaN")

	t.Run("is a permanent failure that names the event type, the subject, and the reason", func(t *testing.T) {
		toAnotherSubject := emit{subject: "/account/2", events: withNaN.events}

		architecturekittest.Given(t, measuringDecider()).
			When(toAnotherSubject).
			ThenFailed(architecturekit.ErrPermanent)

		assert.Equal(t,
			encodingRefusal("test.account.measured", "/account/2", "json: unsupported value: NaN"),
			refusalOf(t, measuringDecider(), toAnotherSubject))
	})

	t.Run("fails every assertion that expects events or nothing, naming the cause", func(t *testing.T) {
		always := func(architecturekit.Event) bool { return true }
		never := func(architecturekit.Event) bool { return false }

		for label, check := range map[string]func(o *architecturekittest.Outcome[emit, account]){
			"ThenEvents":     func(o *architecturekittest.Outcome[emit, account]) { o.ThenEvents(withNaN.events...) },
			"ThenNothing":    func(o *architecturekittest.Outcome[emit, account]) { o.ThenNothing() },
			"ThenSomeEvent":  func(o *architecturekittest.Outcome[emit, account]) { o.ThenSomeEvent(always) },
			"ThenEveryEvent": func(o *architecturekittest.Outcome[emit, account]) { o.ThenEveryEvent(always) },
			"ThenNoEvent":    func(o *architecturekittest.Outcome[emit, account]) { o.ThenNoEvent(never) },
		} {
			t.Run(label, func(t *testing.T) {
				recorder := &spy{}
				check(architecturekittest.Given(recorder, measuringDecider()).When(withNaN))
				recorder.expectFailure(t, "got error: "+refusal)
			})
		}
	})

	t.Run("names the first event that can not be encoded", func(t *testing.T) {
		notANumber := measured{Value: math.NaN()}
		channel := unmarshallable{Channel: make(chan int)}

		assert.Equal(t, refusal, refusalOf(t, measuringDecider(),
			emit{events: []architecturekit.Event{opened{Owner: "golo"}, notANumber, channel}}))

		assert.Equal(t,
			encodingRefusal("test.account.opened", "/account/1", "json: unsupported type: chan int"),
			refusalOf(t, measuringDecider(),
				emit{events: []architecturekit.Event{opened{Owner: "golo"}, channel, notANumber}}))
	})

	t.Run("comes after an event without a rule, as with Execute", func(t *testing.T) {
		recorder := &spy{}

		// The event without a rule comes second, so it is reported only because
		// the rules of all events are checked first.
		architecturekittest.Given(recorder, measuringDecider()).
			When(emit{events: []architecturekit.Event{measured{Value: math.NaN()}, unheardOf{}}}).
			ThenNothing()

		recorder.expectFailure(t, `event of type "test.account.unheardOf" to "/account/1"`)
		recorder.expectFailure(t, "could not read the subject any more")
	})

	t.Run("does not concern events whose data can be encoded", func(t *testing.T) {
		events := []architecturekit.Event{opened{Owner: "golo"}, measured{Value: 1.5}, opened{Owner: "jane"}}

		architecturekittest.Given(t, measuringDecider()).
			When(emit{events: events}).
			ThenEvents(events...)
	})
}

// nilRefusal is the error with which Execute refuses an event that is nil.
func nilRefusal(index int, subject string) string {
	return fmt.Sprintf("%v: refusing to write to %q, since event %d that the decider returned is nil",
		architecturekit.ErrPermanent, subject, index)
}

func TestNilEvent(t *testing.T) {
	withNil := emit{events: []architecturekit.Event{opened{Owner: "golo"}, nil}}
	refusal := nilRefusal(1, "/account/1")

	t.Run("is a permanent failure that names the subject and the index", func(t *testing.T) {
		withOnlyNil := emit{subject: "/account/2", events: []architecturekit.Event{nil}}

		architecturekittest.Given(t, emitDecider()).
			When(withOnlyNil).
			ThenFailed(architecturekit.ErrPermanent)
		assert.Equal(t, nilRefusal(0, "/account/2"), refusalOf(t, emitDecider(), withOnlyNil))
	})

	t.Run("fails every assertion that expects events or nothing, naming the cause", func(t *testing.T) {
		always := func(architecturekit.Event) bool { return true }
		never := func(architecturekit.Event) bool { return false }

		for label, check := range map[string]func(o *architecturekittest.Outcome[emit, account]){
			"ThenEvents":     func(o *architecturekittest.Outcome[emit, account]) { o.ThenEvents(opened{Owner: "golo"}) },
			"ThenNothing":    func(o *architecturekittest.Outcome[emit, account]) { o.ThenNothing() },
			"ThenSomeEvent":  func(o *architecturekittest.Outcome[emit, account]) { o.ThenSomeEvent(always) },
			"ThenEveryEvent": func(o *architecturekittest.Outcome[emit, account]) { o.ThenEveryEvent(always) },
			"ThenNoEvent":    func(o *architecturekittest.Outcome[emit, account]) { o.ThenNoEvent(never) },
		} {
			t.Run(label, func(t *testing.T) {
				recorder := &spy{}
				check(architecturekittest.Given(recorder, emitDecider()).When(withNil))
				recorder.expectFailure(t, "got error: "+refusal)
			})
		}
	})

	t.Run("counts a nil pointer of a concrete type as nil, as with Execute", func(t *testing.T) {
		// The pointer is not equal to nil, since the interface knows its type,
		// but calling EventType on it panics.
		withNilPointer := emit{events: []architecturekit.Event{opened{Owner: "golo"}, (*opened)(nil)}}

		architecturekittest.Given(t, emitDecider()).
			When(withNilPointer).
			ThenFailed(architecturekit.ErrPermanent)
		assert.Equal(t, refusal, refusalOf(t, emitDecider(), withNilPointer))
	})

	t.Run("names the first event that is nil", func(t *testing.T) {
		assert.Equal(t, refusal, refusalOf(t, emitDecider(),
			emit{events: []architecturekit.Event{opened{Owner: "golo"}, nil, opened{Owner: "jane"}, nil}}))
	})

	t.Run("comes before an event without a rule and one that can not be encoded, as with Execute", func(t *testing.T) {
		// The event that is nil comes last, so it is reported only because all
		// events are checked for nil first.
		assert.Equal(t, nilRefusal(2, "/account/1"), refusalOf(t, measuringDecider(),
			emit{events: []architecturekit.Event{measured{Value: math.NaN()}, unheardOf{}, nil}}))
	})
}

func TestWhenChecksPreconditions(t *testing.T) {
	invalid := []struct {
		name          string
		preconditions []architecturekit.Precondition
		cause         string
	}{
		{
			name:          "none",
			preconditions: []architecturekit.Precondition{},
			cause:         "declares no preconditions",
		},
		{
			name: "Unconditionally combined with another one",
			preconditions: []architecturekit.Precondition{
				architecturekit.Unconditionally(),
				architecturekit.OnStateRead(),
			},
			cause: "combines Unconditionally with other preconditions",
		},
		{
			name:          "a zero value",
			preconditions: []architecturekit.Precondition{{}},
			cause:         "not made with Require, OnStateRead, or Unconditionally",
		},
		{
			name:          "a requirement of nothing",
			preconditions: []architecturekit.Precondition{architecturekit.Require(nil)},
			cause:         "requires a precondition that is nil",
		},
	}

	for _, test := range invalid {
		t.Run("refuses "+test.name+" with the error of Execute", func(t *testing.T) {
			cmd := open{Owner: "golo", preconditions: test.preconditions}

			architecturekittest.Given(t, decider()).
				When(cmd).
				ThenFailed(architecturekit.ErrPermanent)

			assert.Equal(t, architecturekit.CheckPreconditions(cmd).Error(), refusalOf(t, decider(), cmd))
		})

		t.Run("fails ThenEvents for "+test.name+", naming the cause", func(t *testing.T) {
			recorder := &spy{}

			architecturekittest.Given(recorder, decider()).
				When(open{Owner: "golo", preconditions: test.preconditions}).
				ThenEvents(opened{Owner: "golo"})

			recorder.expectFailure(t, "expected events, got error")
			recorder.expectFailure(t, test.cause)
		})
	}

	t.Run("fails every assertion that expects events or nothing", func(t *testing.T) {
		always := func(architecturekit.Event) bool { return true }
		combined := open{Owner: "golo", preconditions: []architecturekit.Precondition{
			architecturekit.Unconditionally(),
			architecturekit.OnStateRead(),
		}}

		for label, check := range map[string]func(o *architecturekittest.Outcome[open, account]){
			"ThenNothing":    func(o *architecturekittest.Outcome[open, account]) { o.ThenNothing() },
			"ThenSomeEvent":  func(o *architecturekittest.Outcome[open, account]) { o.ThenSomeEvent(always) },
			"ThenEveryEvent": func(o *architecturekittest.Outcome[open, account]) { o.ThenEveryEvent(always) },
			"ThenNoEvent":    func(o *architecturekittest.Outcome[open, account]) { o.ThenNoEvent(always) },
		} {
			t.Run(label, func(t *testing.T) {
				recorder := &spy{}
				check(architecturekittest.Given(recorder, decider()).When(combined))
				recorder.expectFailure(t, "combines Unconditionally with other preconditions")
			})
		}
	})

	t.Run("does not let the decider decide on a command it refuses", func(t *testing.T) {
		decided := false

		spying := decider()
		decide := spying.Decide
		spying.Decide = func(ctx context.Context, cmd open, current account) ([]architecturekit.Event, error) {
			decided = true
			return decide(ctx, cmd, current)
		}

		architecturekittest.Given(t, spying).
			When(open{Owner: "golo", preconditions: []architecturekit.Precondition{}}).
			ThenFailed(architecturekit.ErrPermanent)

		assert.False(t, decided, "the decider decided on a command that Execute refuses before reading")
	})

	t.Run("lets the decider decide on every valid declaration", func(t *testing.T) {
		for label, preconditions := range map[string][]architecturekit.Precondition{
			"OnStateRead":     {architecturekit.OnStateRead()},
			"Unconditionally": {architecturekit.Unconditionally()},
			"Require":         {architecturekit.Require(eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1"))},
			"Require and OnStateRead": {
				architecturekit.Require(eventsourcingdb.NewIsSubjectPopulatedPrecondition("/account/1")),
				architecturekit.OnStateRead(),
			},
		} {
			t.Run(label, func(t *testing.T) {
				architecturekittest.Given(t, decider()).
					When(open{Owner: "golo", preconditions: preconditions}).
					ThenEvents(opened{Owner: "golo"})
			})
		}
	})
}

func TestThenPreconditions(t *testing.T) {
	t.Run("fails on wrong count", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{Owner: "golo", preconditions: []architecturekit.Precondition{}}).
			ThenPreconditions(architecturekittest.OnPristineSubject("/account/1"))

		recorder.expectFailure(t, "expected 1 precondition(s), got 0: []")
	})

	t.Run("shows the kinds when the count is wrong", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{
				Owner: "golo",
				preconditions: []architecturekit.Precondition{
					architecturekit.Require(eventsourcingdb.NewIsSubjectPopulatedPrecondition("/account/1")),
					architecturekit.OnStateRead(),
				},
			}).
			ThenPreconditions(architecturekittest.OnPristineSubject("/account/1"))

		recorder.expectFailure(t,
			`expected 1 precondition(s), got 2: [{Subject: "/account/1", Populated: true}, {OnStateRead: true}]`)
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
			ThenPreconditions(architecturekittest.OnPristineSubject("/account/other"))

		recorder.expectFailure(t, "/account/other")
	})

	t.Run("fails on a populated subject where a pristine one is expected", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{
				Owner: "golo",
				preconditions: []architecturekit.Precondition{
					architecturekit.Require(eventsourcingdb.NewIsSubjectPopulatedPrecondition("/account/1")),
				},
			}).
			ThenPreconditions(architecturekittest.OnPristineSubject("/account/1"))

		require.Len(t, recorder.failures, 1)
		assert.Equal(t,
			`precondition 0: got {Subject: "/account/1", Populated: true}, want {Subject: "/account/1", Pristine: true}`,
			recorder.firstFailure())
	})

	t.Run("fails on a pristine subject where a populated one is expected", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{
				Owner: "golo",
				preconditions: []architecturekit.Precondition{
					architecturekit.Require(eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1")),
				},
			}).
			ThenPreconditions(architecturekittest.OnPopulatedSubject("/account/1"))

		require.Len(t, recorder.failures, 1)
		assert.Equal(t,
			`precondition 0: got {Subject: "/account/1", Pristine: true}, want {Subject: "/account/1", Populated: true}`,
			recorder.firstFailure())
	})

	t.Run("fails instead of panicking on a subject check that cannot be compared", func(t *testing.T) {
		recorder := &spy{}

		assert.NotPanics(t, func() {
			architecturekittest.Given(recorder, decider()).
				When(open{
					Owner: "golo",
					preconditions: []architecturekit.Precondition{
						architecturekit.Require(uncomparableSubjectPrecondition{subject: []string{"/account/1"}}),
					},
				}).
				ThenPreconditions(architecturekittest.OnPristineSubject("/account/1"))
		})

		recorder.expectFailure(t, `precondition 0: got {Subject: "/account/1"}, want {Subject: "/account/1", Pristine: true}`)
	})

	t.Run("shows an invalid precondition as one without fields", func(t *testing.T) {
		recorder := &spy{}

		architecturekittest.Given(recorder, decider()).
			When(open{Owner: "golo", preconditions: []architecturekit.Precondition{{}}}).
			ThenPreconditions(architecturekittest.Unconditionally())

		recorder.expectFailure(t, "precondition 0: got {}, want {Unconditional: true}")
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
