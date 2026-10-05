// Package architecturekittest provides given-when-then for deciders and projections.
//
// Nothing here touches a database: a decider is a pure function of command and
// state, and a projection is driven with events handed to it directly. A test
// that needs a real database gets one from the dbtest package, which is kept
// apart so that this one builds without Docker.
//
// Every function here that can fail a test takes the testing.TB of the test,
// such as its *testing.T, or the *testing.B of a benchmark.
package architecturekittest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Fixture holds the state a command will decide on.
type Fixture[TCommand architecturekit.Command, TState any] struct {
	t       testing.TB
	decider architecturekit.Decider[TCommand, TState]
	state   TState
}

// Given builds the state from typed events, which is how a test usually
// spells out a history.
//
// The zero Decider, one that was not made with architecturekit.NewDecider, is
// a programming error, so Given panics, and names the mistake rather than a
// nil pointer.
func Given[TCommand architecturekit.Command, TState any](
	t testing.TB,
	decider architecturekit.Decider[TCommand, TState],
	history ...architecturekit.Event,
) *Fixture[TCommand, TState] {
	t.Helper()

	if decider.State() == nil {
		panic("architecturekittest: Given needs a decider made with NewDecider, not the zero Decider")
	}

	state, err := architecturekit.Replay(decider.State(), history...)
	if err != nil {
		t.Fatalf("given: %v", err)
	}

	return &Fixture[TCommand, TState]{t: t, decider: decider, state: state}
}

// GivenStored builds the state from events in their stored shape, running the
// upcasters on the way. Use it to test that an older event type still arrives
// correctly, which typed events cannot show.
//
// Like Given, it panics for the zero Decider.
func GivenStored[TCommand architecturekit.Command, TState any](
	t testing.TB,
	decider architecturekit.Decider[TCommand, TState],
	history ...eventsourcingdb.Event,
) *Fixture[TCommand, TState] {
	t.Helper()

	if decider.State() == nil {
		panic("architecturekittest: GivenStored needs a decider made with NewDecider, not the zero Decider")
	}

	state, err := architecturekit.ReplayStored(decider.State(), history...)
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
// of them for a rule, and encodes them last. In each case, the decision then
// holds the error of the category architecturekit.ErrPermanent that Execute
// returns, so that ThenEvents and the other assertions that expect events
// fail, naming the cause, and ThenFailed(architecturekit.ErrPermanent)
// matches it. Only an ID of architecturekit.OnEventID that is not a revision
// is refused with an error that wraps architecturekit.ErrNotARevision
// instead, as Execute does, which
// ThenFailed(architecturekit.ErrNotARevision) matches.
func (f *Fixture[TCommand, TState]) When(cmd TCommand) *Decision[TCommand, TState] {
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
		err = checkRules(f.decider.State(), cmd.Subject(), events)
	}

	var encoded [][]byte
	if err == nil {
		encoded, err = encode(cmd.Subject(), events)
	}

	return &Decision[TCommand, TState]{
		t:       f.t,
		cmd:     cmd,
		state:   f.state,
		events:  events,
		encoded: encoded,
		err:     err,
	}
}

// Decision is the decider's decision on the command: events, nothing, or a
// refusal. Every assertion returns the decision again, so they can be chained.
type Decision[TCommand architecturekit.Command, TState any] struct {
	t      testing.TB
	cmd    TCommand
	state  TState
	events []architecturekit.Event

	// encoded holds the data of every event as JSON, as Execute would write
	// it, which is what ThenEvents compares.
	encoded [][]byte

	err error
}

// ThenEvents expects exactly these events, in this order.
func (d *Decision[TCommand, TState]) ThenEvents(expected ...architecturekit.Event) *Decision[TCommand, TState] {
	d.t.Helper()

	if d.err != nil {
		d.t.Fatalf("expected events, got error: %v", d.err)
		return d
	}
	if len(d.events) != len(expected) {
		d.t.Fatalf("expected %d event(s), got %d: %s",
			len(expected), len(d.events), describe(d.events))
		return d
	}

	for i, want := range expected {
		got := d.events[i]

		if got.EventType() != want.EventType() {
			d.t.Fatalf("event %d: got type %q, want %q", i, got.EventType(), want.EventType())
			return d
		}

		wantJSON, err := json.Marshal(want)
		if err != nil {
			d.t.Fatalf("event %d: %v", i, err)
			return d
		}

		if string(d.encoded[i]) != string(wantJSON) {
			d.t.Fatalf("event %d: got %s, want %s", i, d.encoded[i], wantJSON)
			return d
		}
	}

	return d
}

// ThenNothing expects neither events nor a failure, which is what a command
// does when it finds there is nothing left to do.
func (d *Decision[TCommand, TState]) ThenNothing() *Decision[TCommand, TState] {
	d.t.Helper()

	if d.err != nil {
		d.t.Fatalf("expected nothing to happen, got error: %v", d.err)
		return d
	}
	if len(d.events) > 0 {
		d.t.Fatalf("expected nothing to happen, got %s", describe(d.events))
	}

	return d
}

// ThenFailed expects the command to have failed with an error that matches
// target, as errors.Is reports it, so target may be the error itself or any
// error it wraps. That covers a category, for instance
// architecturekit.ErrDomain or architecturekit.ErrPermanent, as well as a
// sentinel error of the domain, such as an ErrBookAlreadyAcquired that the
// decider returns as it is, or wraps to add details.
func (d *Decision[TCommand, TState]) ThenFailed(target error) *Decision[TCommand, TState] {
	d.t.Helper()

	if d.err == nil {
		d.t.Fatalf("expected an error matching %v, got %s", target, describe(d.events))
		return d
	}
	if !errors.Is(d.err, target) {
		d.t.Fatalf("expected an error matching %v, got %v", target, d.err)
	}

	return d
}

// ThenSomeEvent expects at least one event to match.
func (d *Decision[TCommand, TState]) ThenSomeEvent(
	match func(architecturekit.Event) bool,
) *Decision[TCommand, TState] {
	d.t.Helper()

	if d.err != nil {
		d.t.Fatalf("expected events, got error: %v", d.err)
		return d
	}

	for _, event := range d.events {
		if match(event) {
			return d
		}
	}

	d.t.Fatalf("no event matched, got %s", describe(d.events))

	return d
}

// ThenEveryEvent expects all events to match, and at least one to be there.
func (d *Decision[TCommand, TState]) ThenEveryEvent(
	match func(architecturekit.Event) bool,
) *Decision[TCommand, TState] {
	d.t.Helper()

	if d.err != nil {
		d.t.Fatalf("expected events, got error: %v", d.err)
		return d
	}
	if len(d.events) == 0 {
		d.t.Fatalf("expected events, got none")
		return d
	}

	for i, event := range d.events {
		if !match(event) {
			d.t.Fatalf("event %d did not match: %s", i, describe(d.events))
			return d
		}
	}

	return d
}

// ThenNoEvent expects nothing to match, which is how a test says that a
// command did not do something in particular.
func (d *Decision[TCommand, TState]) ThenNoEvent(
	match func(architecturekit.Event) bool,
) *Decision[TCommand, TState] {
	d.t.Helper()

	if d.err != nil {
		d.t.Fatalf("expected events, got error: %v", d.err)
		return d
	}

	for i, event := range d.events {
		if match(event) {
			d.t.Fatalf("event %d matched although it should not: %s", i, describe(d.events))
			return d
		}
	}

	return d
}

// ThenState checks the state the command decided on. It is the state Given
// built, which is what makes it useful for testing upcasters.
func (d *Decision[TCommand, TState]) ThenState(
	check func(TState),
) *Decision[TCommand, TState] {
	d.t.Helper()

	check(d.state)

	return d
}

// ThenPreconditions expects the command to declare exactly these, in this
// order.
func (d *Decision[TCommand, TState]) ThenPreconditions(
	expected ...Precondition,
) *Decision[TCommand, TState] {
	d.t.Helper()

	declared := PreconditionsOf(d.cmd)

	if len(declared) != len(expected) {
		d.t.Fatalf("expected %d precondition(s), got %d: %s",
			len(expected), len(declared), describePreconditions(declared))
		return d
	}

	for i, want := range expected {
		if declared[i] != want {
			d.t.Fatalf("precondition %d: got %s, want %s",
				i, describePrecondition(declared[i]), describePrecondition(want))
			return d
		}
	}

	return d
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
