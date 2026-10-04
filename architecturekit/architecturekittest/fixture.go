// Package architecturekittest provides given-when-then for deciders and projections.
//
// Nothing here touches a database: a decider is a pure function of command and
// state, and a projection is driven with events handed to it directly. A test
// that needs a real database gets one from the dbtest package, which is kept
// apart so that this one builds without Docker.
package architecturekittest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// TestingT is the part of *testing.T that the fixtures need. It is an
// interface so that the fixtures themselves can be tested.
type TestingT interface {
	Helper()
	Fatalf(format string, args ...any)
}

// Fixture holds the state a command will decide on.
type Fixture[TCommand architecturekit.Command, TState any] struct {
	t       TestingT
	decider architecturekit.Decider[TCommand, TState]
	state   TState
}

// Given builds the state from typed events, which is how a test usually
// spells out a history.
func Given[TCommand architecturekit.Command, TState any](
	t TestingT,
	decider architecturekit.Decider[TCommand, TState],
	history ...architecturekit.Event,
) *Fixture[TCommand, TState] {
	t.Helper()

	state, err := architecturekit.Replay(decider.State, history...)
	if err != nil {
		t.Fatalf("given: %v", err)
	}

	return &Fixture[TCommand, TState]{t: t, decider: decider, state: state}
}

// GivenStored builds the state from events in their stored shape, running the
// upcasters on the way. Use it to test that an older event type still arrives
// correctly, which typed events cannot show.
func GivenStored[TCommand architecturekit.Command, TState any](
	t TestingT,
	decider architecturekit.Decider[TCommand, TState],
	history ...eventsourcingdb.Event,
) *Fixture[TCommand, TState] {
	t.Helper()

	state, err := architecturekit.ReplayStored(decider.State, history...)
	if err != nil {
		t.Fatalf("given stored: %v", err)
	}

	return &Fixture[TCommand, TState]{t: t, decider: decider, state: state}
}

// When runs the command against that state.
//
// Like Execute, it first checks the preconditions the command declares (see
// architecturekit.CheckPreconditions), and the decider does not decide on a
// command they refuse. It also refuses an event that is nil, a nil pointer of
// a concrete type included, an event that the state of the decider has no
// rule for, since the state could not read the subject any more, and an
// event whose data can not be encoded as JSON, for example because it holds
// a float NaN. As Execute does, it checks all events for nil first, then all
// of them for a rule, and encodes them last. In each case, the outcome is
// then the error of the category architecturekit.ErrPermanent that Execute
// returns, so that ThenEvents and the other assertions that expect events
// fail, naming the cause, and ThenFailed(architecturekit.ErrPermanent)
// matches it.
func (f *Fixture[TCommand, TState]) When(cmd TCommand) *Outcome[TCommand, TState] {
	f.t.Helper()

	err := architecturekit.CheckPreconditions(cmd)

	var events []architecturekit.Event
	if err == nil {
		events, err = f.decider.Decide(context.Background(), cmd, f.state)
	}
	if err == nil {
		err = checkNotNil(cmd.Subject(), events)
	}
	if err == nil {
		err = checkRules(f.decider.State, cmd.Subject(), events)
	}

	var encoded [][]byte
	if err == nil {
		encoded, err = encode(cmd.Subject(), events)
	}

	return &Outcome[TCommand, TState]{
		t:       f.t,
		cmd:     cmd,
		state:   f.state,
		events:  events,
		encoded: encoded,
		err:     err,
	}
}

// Outcome is what a command did. Every assertion returns the outcome again, so
// they can be chained.
type Outcome[TCommand architecturekit.Command, TState any] struct {
	t      TestingT
	cmd    TCommand
	state  TState
	events []architecturekit.Event

	// encoded holds the data of every event as JSON, as Execute would write
	// it, which is what ThenEvents compares.
	encoded [][]byte

	err error
}

// ThenEvents expects exactly these events, in this order.
func (o *Outcome[TCommand, TState]) ThenEvents(expected ...architecturekit.Event) *Outcome[TCommand, TState] {
	o.t.Helper()

	if o.err != nil {
		o.t.Fatalf("expected events, got error: %v", o.err)
		return o
	}
	if len(o.events) != len(expected) {
		o.t.Fatalf("expected %d event(s), got %d: %s",
			len(expected), len(o.events), describe(o.events))
		return o
	}

	for i, want := range expected {
		got := o.events[i]

		if got.EventType() != want.EventType() {
			o.t.Fatalf("event %d: got type %q, want %q", i, got.EventType(), want.EventType())
			return o
		}

		wantJSON, err := json.Marshal(want)
		if err != nil {
			o.t.Fatalf("event %d: %v", i, err)
			return o
		}

		if string(o.encoded[i]) != string(wantJSON) {
			o.t.Fatalf("event %d: got %s, want %s", i, o.encoded[i], wantJSON)
			return o
		}
	}

	return o
}

// ThenNothing expects neither events nor a failure, which is what a command
// does when it finds there is nothing left to do.
func (o *Outcome[TCommand, TState]) ThenNothing() *Outcome[TCommand, TState] {
	o.t.Helper()

	if o.err != nil {
		o.t.Fatalf("expected nothing to happen, got error: %v", o.err)
		return o
	}
	if len(o.events) > 0 {
		o.t.Fatalf("expected nothing to happen, got %s", describe(o.events))
	}

	return o
}

// ThenFailed expects the command to have failed with an error that matches
// target, as errors.Is reports it, so target may be the error itself or any
// error it wraps. That covers a category, for instance
// architecturekit.ErrDomain or architecturekit.ErrPermanent, as well as a
// sentinel error of the domain, such as an ErrBookAlreadyAcquired that the
// decider returns as it is, or wraps to add details.
func (o *Outcome[TCommand, TState]) ThenFailed(target error) *Outcome[TCommand, TState] {
	o.t.Helper()

	if o.err == nil {
		o.t.Fatalf("expected an error matching %v, got %s", target, describe(o.events))
		return o
	}
	if !errors.Is(o.err, target) {
		o.t.Fatalf("expected an error matching %v, got %v", target, o.err)
	}

	return o
}

// ThenSomeEvent expects at least one event to match.
func (o *Outcome[TCommand, TState]) ThenSomeEvent(
	match func(architecturekit.Event) bool,
) *Outcome[TCommand, TState] {
	o.t.Helper()

	if o.err != nil {
		o.t.Fatalf("expected events, got error: %v", o.err)
		return o
	}

	for _, event := range o.events {
		if match(event) {
			return o
		}
	}

	o.t.Fatalf("no event matched, got %s", describe(o.events))

	return o
}

// ThenEveryEvent expects all events to match, and at least one to be there.
func (o *Outcome[TCommand, TState]) ThenEveryEvent(
	match func(architecturekit.Event) bool,
) *Outcome[TCommand, TState] {
	o.t.Helper()

	if o.err != nil {
		o.t.Fatalf("expected events, got error: %v", o.err)
		return o
	}
	if len(o.events) == 0 {
		o.t.Fatalf("expected events, got none")
		return o
	}

	for i, event := range o.events {
		if !match(event) {
			o.t.Fatalf("event %d did not match: %s", i, describe(o.events))
			return o
		}
	}

	return o
}

// ThenNoEvent expects nothing to match, which is how a test says that a
// command did not do something in particular.
func (o *Outcome[TCommand, TState]) ThenNoEvent(
	match func(architecturekit.Event) bool,
) *Outcome[TCommand, TState] {
	o.t.Helper()

	if o.err != nil {
		o.t.Fatalf("expected events, got error: %v", o.err)
		return o
	}

	for i, event := range o.events {
		if match(event) {
			o.t.Fatalf("event %d matched although it should not: %s", i, describe(o.events))
			return o
		}
	}

	return o
}

// ThenState checks the state the command decided on. It is the state Given
// built, which is what makes it useful for testing upcasters.
func (o *Outcome[TCommand, TState]) ThenState(
	check func(TState),
) *Outcome[TCommand, TState] {
	o.t.Helper()

	check(o.state)

	return o
}

// ThenPreconditions expects the command to declare exactly these, in this
// order.
func (o *Outcome[TCommand, TState]) ThenPreconditions(
	expected ...Precondition,
) *Outcome[TCommand, TState] {
	o.t.Helper()

	declared := PreconditionsOf(o.cmd)

	if len(declared) != len(expected) {
		o.t.Fatalf("expected %d precondition(s), got %d: %s",
			len(expected), len(declared), describePreconditions(declared))
		return o
	}

	for i, want := range expected {
		if declared[i] != want {
			o.t.Fatalf("precondition %d: got %s, want %s",
				i, describePrecondition(declared[i]), describePrecondition(want))
			return o
		}
	}

	return o
}

// checkNotNil fails if one of the events is nil, also a nil pointer of a
// concrete type (see isNil), with the same error as Execute, which refuses to
// write them then. Execute checks with a function of its own, which is not
// exported, so the wording here has to stay the same as there, and a test
// compares the two.
func checkNotNil(subject string, events []architecturekit.Event) error {
	for i, event := range events {
		if isNil(event) {
			return fmt.Errorf("%w: refusing to write to %q, since event %d that the decider returned is nil",
				architecturekit.ErrPermanent, subject, i)
		}
	}

	return nil
}

// isNil reports whether a value that is handed over as an interface is nil,
// also when it is a nil pointer, map, slice, function, or channel of a
// concrete type. Such a value is not equal to nil, since the interface knows
// its type, but it fails as soon as it is used.
//
// An interface never shows up as the kind, since reflect.ValueOf unpacks it.
// The package architecturekit has the same function, which it does not export,
// so that it does not become part of what the kit offers.
func isNil(value any) bool {
	if value == nil {
		return true
	}

	switch reflected := reflect.ValueOf(value); reflected.Kind() {
	case reflect.Pointer, reflect.Map, reflect.Slice, reflect.Func, reflect.Chan:
		return reflected.IsNil()
	default:
		return false
	}
}

// checkRules fails if the state has no rule for one of the events, with the
// same error as Execute, which refuses to write them then. Execute checks
// with a function of its own, which is not exported, so the wording here has
// to stay the same as there, and a test compares the two.
func checkRules[TState any](
	state *architecturekit.State[TState],
	subject string,
	events []architecturekit.Event,
) error {
	for _, event := range events {
		if !state.HasRule(event.EventType()) {
			return fmt.Errorf("%w: refusing to write an event of type %q to %q, since the state of the "+
				"decider has no rule for it and could not read the subject any more",
				architecturekit.ErrPermanent, event.EventType(), subject)
		}
	}

	return nil
}

// encode encodes the data of every event as JSON, with encoding/json, as
// Execute does. If one of them can not be encoded, it fails with the same
// error as Execute, which then refuses to write any of them. Execute encodes
// with a function of its own, which is not exported, so the wording here has
// to stay the same as there, and a test compares the two.
func encode(subject string, events []architecturekit.Event) ([][]byte, error) {
	encoded := make([][]byte, len(events))

	for i, event := range events {
		data, err := json.Marshal(event)
		if err != nil {
			return nil, fmt.Errorf("%w: refusing to write an event of type %q to %q, "+
				"since its data can not be encoded as JSON: %v",
				architecturekit.ErrPermanent, event.EventType(), subject, err)
		}

		encoded[i] = data
	}

	return encoded, nil
}

func describe(events []architecturekit.Event) string {
	if len(events) == 0 {
		return "no events"
	}

	types := make([]string, len(events))
	for i, event := range events {
		types[i] = event.EventType()
	}

	return fmt.Sprintf("%d event(s): %v", len(events), types)
}
