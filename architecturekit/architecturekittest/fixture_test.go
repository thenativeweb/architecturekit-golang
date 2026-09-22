package architecturekittest_test

import (
	"context"
	"fmt"
	"strings"
	"testing"

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

	if len(s.failures) == 0 {
		t.Fatalf("expected a failure containing %q, got none", containing)
	}
	if !strings.Contains(s.firstFailure(), containing) {
		t.Fatalf("expected a failure containing %q, got %q", containing, s.firstFailure())
	}
}

func (s *spy) expectNoFailure(t *testing.T) {
	t.Helper()

	if len(s.failures) > 0 {
		t.Fatalf("expected no failure, got %q", s.firstFailure())
	}
}

// --- test domain ---

type opened struct {
	Owner string `json:"owner"`
}

func (opened) EventType() string { return "test.account.opened" }

type closed struct{}

func (closed) EventType() string { return "test.account.closed" }

type unheardOf struct{}

func (unheardOf) EventType() string { return "test.account.unheardOf" }

// unmarshallable reports the same type as opened but cannot be marshalled.
type unmarshallable struct {
	Channel chan int `json:"channel"`
}

func (unmarshallable) EventType() string { return "test.account.opened" }

type account struct {
	IsOpen bool
	Owner  string
}

type open struct {
	Owner string

	// preconditions is what this command declares; nil means none.
	preconditions []eventsourcingdb.Precondition
}

func (open) Subject() string { return "/account/1" }

func (c open) Preconditions() []eventsourcingdb.Precondition { return c.preconditions }

// bare declares no preconditions at all, and does not implement the interface.
type bare struct{}

func (bare) Subject() string { return "/account/1" }

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
	state.Upcast("test.account.opened.v1",
		func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
			event.Type = "test.account.opened"
			event.Data = []byte(`{"owner":"from the old shape"}`)
			return []eventsourcingdb.Event{event}, nil
		})

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

func emitDecider() architecturekit.Decider[emit, account] {
	return architecturekit.Decider[emit, account]{
		State: architecturekit.NewState(account{}),
		Decide: func(ctx context.Context, cmd emit, _ account) ([]architecturekit.Event, error) {
			return cmd.events, nil
		},
	}
}

// --- the happy paths ---

func TestGivenWhenThenEvents(t *testing.T) {
	architecturekittest.Given(t, decider()).
		When(open{Owner: "golo"}).
		ThenEvents(opened{Owner: "golo"})
}

func TestHistoryIsFoldedInOrder(t *testing.T) {
	architecturekittest.Given(t, decider(), opened{Owner: "golo"}, closed{}).
		When(open{Owner: "jane"}).
		ThenEvents(opened{Owner: "jane"})
}

func TestThenNothingWhenThereIsNothingToDo(t *testing.T) {
	architecturekittest.Given(t, decider()).
		When(open{Owner: "nobody"}).
		ThenNothing()
}

func TestThenRejectedAndThenFailedDescribeTheSameRejection(t *testing.T) {
	architecturekittest.Given(t, decider(), opened{Owner: "golo"}).
		When(open{Owner: "jane"}).
		ThenRejected("account is already open").
		ThenFailed(architecturekit.ErrDomain)
}

func TestThenFailedTellsCategoriesApart(t *testing.T) {
	architecturekittest.Given(t, decider()).
		When(open{}).
		ThenFailed(architecturekit.ErrPermanent)
}

func TestThenStateChecksWhatWasDecidedOn(t *testing.T) {
	checked := false

	architecturekittest.Given(t, decider(), opened{Owner: "golo"}).
		When(open{Owner: "jane"}).
		ThenState(func(state account) {
			checked = true
			if !state.IsOpen || state.Owner != "golo" {
				t.Fatalf("got %+v", state)
			}
		})

	if !checked {
		t.Fatal("ThenState did not run its check")
	}
}

func TestGivenStoredRunsTheUpcasters(t *testing.T) {
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
			if state.Owner != "from the old shape" {
				t.Fatalf("the upcaster did not run: %+v", state)
			}
		}).
		ThenRejected("account is already open")
}

func TestEventMatchers(t *testing.T) {
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
}

func TestPreconditionsOfACommand(t *testing.T) {
	architecturekittest.Given(t, decider()).
		When(open{
			Owner: "golo",
			preconditions: []eventsourcingdb.Precondition{
				eventsourcingdb.NewIsSubjectOnEventIDPrecondition("/account/1", "7"),
			},
		}).
		ThenPreconditions(architecturekittest.OnEventID("/account/1", "7"))
}

func TestPreconditionsOfEveryKind(t *testing.T) {
	cmd := open{
		Owner: "golo",
		preconditions: []eventsourcingdb.Precondition{
			eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1"),
			eventsourcingdb.NewIsSubjectPopulatedPrecondition("/account/2"),
			eventsourcingdb.NewIsSubjectOnEventIDPrecondition("/account/3", "9"),
			eventsourcingdb.NewIsEventQLQueryTruePrecondition("FROM e IN events PROJECT INTO true"),
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
		)
}

func TestCommandWithoutPreconditionsDeclaresNone(t *testing.T) {
	if declared := architecturekittest.PreconditionsOf(bare{}); len(declared) != 0 {
		t.Fatalf("got %v", declared)
	}

	// A command that implements the interface but returns nothing counts too.
	if declared := architecturekittest.PreconditionsOf(open{}); len(declared) != 0 {
		t.Fatalf("got %v", declared)
	}
}

func TestThenPreconditionsAcceptsNone(t *testing.T) {
	architecturekittest.Given(t, decider()).
		When(open{Owner: "golo"}).
		ThenPreconditions()
}

// --- the fixture's own failure paths ---

func TestGivenFailsOnEventWithoutRule(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider(), unheardOf{})

	recorder.expectFailure(t, "given:")
}

func TestGivenStoredFailsOnEventWithoutRule(t *testing.T) {
	recorder := &spy{}

	architecturekittest.GivenStored(recorder, decider(), eventsourcingdb.Event{
		Subject: "/account/1",
		Type:    "test.account.unheardOf",
		Data:    []byte(`{}`),
	})

	recorder.expectFailure(t, "given stored:")
}

func TestThenEventsFailsOnUnexpectedError(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider()).
		When(open{}).
		ThenEvents(opened{Owner: "golo"})

	recorder.expectFailure(t, "no owner given")
}

func TestThenEventsFailsOnWrongCount(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider()).
		When(open{Owner: "golo"}).
		ThenEvents(opened{Owner: "golo"}, closed{})

	recorder.expectFailure(t, "expected 2 event(s), got 1")
}

func TestThenEventsFailsOnWrongType(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider()).
		When(open{Owner: "golo"}).
		ThenEvents(closed{})

	recorder.expectFailure(t, "want \"test.account.closed\"")
}

func TestThenEventsFailsOnWrongPayload(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider()).
		When(open{Owner: "golo"}).
		ThenEvents(opened{Owner: "someone-else"})

	recorder.expectFailure(t, "someone-else")
}

func TestThenEventsFailsWhenTheActualEventCannotBeMarshalled(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, emitDecider()).
		When(emit{events: []architecturekit.Event{unmarshallable{Channel: make(chan int)}}}).
		ThenEvents(opened{Owner: "golo"})

	recorder.expectFailure(t, "json")
}

func TestThenEventsFailsWhenTheExpectedEventCannotBeMarshalled(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, emitDecider()).
		When(emit{events: []architecturekit.Event{opened{Owner: "golo"}}}).
		ThenEvents(unmarshallable{Channel: make(chan int)})

	recorder.expectFailure(t, "json")
}

func TestThenNothingFailsWhenSomethingHappened(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider()).
		When(open{Owner: "golo"}).
		ThenNothing()

	recorder.expectFailure(t, "1 event(s)")
}

func TestThenNothingFailsOnAnError(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider()).
		When(open{}).
		ThenNothing()

	recorder.expectFailure(t, "no owner given")
}

func TestThenRejectedFailsWhenEventsWereProduced(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider()).
		When(open{Owner: "golo"}).
		ThenRejected("account is already open")

	recorder.expectFailure(t, "1 event(s)")
}

func TestThenRejectedFailsOnWrongMessage(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider(), opened{Owner: "golo"}).
		When(open{Owner: "jane"}).
		ThenRejected("something else")

	recorder.expectFailure(t, "something else")
}

func TestThenFailedFailsWhenNothingFailed(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider()).
		When(open{Owner: "golo"}).
		ThenFailed(architecturekit.ErrDomain)

	recorder.expectFailure(t, "1 event(s)")
}

func TestThenFailedFailsOnTheWrongCategory(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider(), opened{Owner: "golo"}).
		When(open{Owner: "jane"}).
		ThenFailed(architecturekit.ErrTransient)

	recorder.expectFailure(t, "category")
}

func TestMatchersFailWhenNothingMatches(t *testing.T) {
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
}

func TestMatchersFailOnAnError(t *testing.T) {
	always := func(architecturekit.Event) bool { return true }

	for label, assert := range map[string]func(o *architecturekittest.Outcome[open, account]){
		"ThenSomeEvent":  func(o *architecturekittest.Outcome[open, account]) { o.ThenSomeEvent(always) },
		"ThenEveryEvent": func(o *architecturekittest.Outcome[open, account]) { o.ThenEveryEvent(always) },
		"ThenNoEvent":    func(o *architecturekittest.Outcome[open, account]) { o.ThenNoEvent(always) },
	} {
		recorder := &spy{}
		assert(architecturekittest.Given(recorder, decider()).When(open{}))
		recorder.expectFailure(t, "no owner given")
		if len(recorder.failures) == 0 {
			t.Fatalf("%s did not report the error", label)
		}
	}
}

func TestThenPreconditionsFailsOnWrongCount(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider()).
		When(open{Owner: "golo"}).
		ThenPreconditions(architecturekittest.OnSubject("/account/1"))

	recorder.expectFailure(t, "expected 1 precondition(s), got 0")
}

func TestThenPreconditionsFailsOnWrongContent(t *testing.T) {
	recorder := &spy{}

	architecturekittest.Given(recorder, decider()).
		When(open{
			Owner: "golo",
			preconditions: []eventsourcingdb.Precondition{
				eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1"),
			},
		}).
		ThenPreconditions(architecturekittest.OnSubject("/account/other"))

	recorder.expectFailure(t, "/account/other")
}

func TestSpyHelperIsHarmless(t *testing.T) {
	// Helper only exists to satisfy the interface.
	recorder := &spy{}
	recorder.Helper()
	recorder.expectNoFailure(t)
}

func TestThenEventsSaysSoWhenNothingHappenedAtAll(t *testing.T) {
	recorder := &spy{}

	// The command decides there is nothing to do, so the mismatch has to read
	// as "no events" rather than as an empty list.
	architecturekittest.Given(recorder, decider()).
		When(open{Owner: "nobody"}).
		ThenEvents(opened{Owner: "golo"})

	recorder.expectFailure(t, "no events")
}
