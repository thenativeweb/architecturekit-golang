// Package architecturekittest provides given-when-then for deciders and projections.
//
// Nothing here touches a database: a decider is a pure function of command and
// state, and a projection is driven with events handed to it directly.
package architecturekittest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

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
func (f *Fixture[TCommand, TState]) When(cmd TCommand) *Outcome[TCommand, TState] {
	f.t.Helper()

	events, err := f.decider.Decide(context.Background(), cmd, f.state)

	return &Outcome[TCommand, TState]{
		t:      f.t,
		cmd:    cmd,
		state:  f.state,
		events: events,
		err:    err,
	}
}

// Outcome is what a command did. Every assertion returns the outcome again, so
// they can be chained.
type Outcome[TCommand architecturekit.Command, TState any] struct {
	t      TestingT
	cmd    TCommand
	state  TState
	events []architecturekit.Event
	err    error
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

		gotJSON, err := json.Marshal(got)
		if err != nil {
			o.t.Fatalf("event %d: %v", i, err)
			return o
		}
		wantJSON, err := json.Marshal(want)
		if err != nil {
			o.t.Fatalf("event %d: %v", i, err)
			return o
		}

		if string(gotJSON) != string(wantJSON) {
			o.t.Fatalf("event %d: got %s, want %s", i, gotJSON, wantJSON)
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

// ThenRejected expects the command to have been rejected with exactly this
// message. Prefer ThenFailed where the wording is not the point, because a
// message is easier to reword than a category.
func (o *Outcome[TCommand, TState]) ThenRejected(message string) *Outcome[TCommand, TState] {
	o.t.Helper()

	if o.err == nil {
		o.t.Fatalf("expected rejection %q, got %s", message, describe(o.events))
		return o
	}
	if o.err.Error() != message {
		o.t.Fatalf("expected rejection %q, got %q", message, o.err.Error())
	}

	return o
}

// ThenFailed expects a failure of that category, for instance
// architecturekit.ErrDomain or architecturekit.ErrPermanent.
func (o *Outcome[TCommand, TState]) ThenFailed(category error) *Outcome[TCommand, TState] {
	o.t.Helper()

	if o.err == nil {
		o.t.Fatalf("expected a failure of category %v, got %s", category, describe(o.events))
		return o
	}
	if !errors.Is(o.err, category) {
		o.t.Fatalf("expected a failure of category %v, got %v", category, o.err)
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
// order. A command that declares none is checked with no arguments.
func (o *Outcome[TCommand, TState]) ThenPreconditions(
	expected ...Precondition,
) *Outcome[TCommand, TState] {
	o.t.Helper()

	declared := PreconditionsOf(o.cmd)

	if len(declared) != len(expected) {
		o.t.Fatalf("expected %d precondition(s), got %d: %v",
			len(expected), len(declared), declared)
		return o
	}

	for i, want := range expected {
		if declared[i] != want {
			o.t.Fatalf("precondition %d: got %+v, want %+v", i, declared[i], want)
			return o
		}
	}

	return o
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
