package architecturekit_test

import (
	"context"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// A minimal domain shared by all tests in this package: a counter that rejects
// commands once an upper limit is reached.

type incremented struct {
	By int `json:"by"`
}

func (incremented) EventType() string { return "io.thenativeweb.test.incremented" }

func (incremented) Schema() map[string]any {
	return map[string]any{
		"type":                 "object",
		"additionalProperties": false,
		"required":             []string{"by"},
		"properties": map[string]any{
			"by": map[string]any{"type": "number"},
		},
	}
}

// reset deliberately has no schema, to exercise the optional SchemaProvider.
type reset struct{}

func (reset) EventType() string { return "io.thenativeweb.test.reset" }

type counter struct {
	Total int
}

type increment struct {
	subject string
	By      int
	Limit   int

	// preconditions is what the command declares; nil means none, which is the
	// kit's default because it adds none of its own.
	preconditions []eventsourcingdb.Precondition
}

func (c increment) Subject() string { return c.subject }

// pristine returns a copy that only writes if the subject is still empty.
func (c increment) pristine() increment {
	c.preconditions = []eventsourcingdb.Precondition{
		eventsourcingdb.NewIsSubjectPristinePrecondition(c.subject),
	}
	return c
}

// onEventID returns a copy that only writes if the subject is on that event.
func (c increment) onEventID(eventID string) increment {
	c.preconditions = []eventsourcingdb.Precondition{
		eventsourcingdb.NewIsSubjectOnEventIDPrecondition(c.subject, eventID),
	}
	return c
}

// Preconditions is only reached when the command declares some; a command
// without any simply has an empty slice here.
func (c increment) Preconditions() []eventsourcingdb.Precondition {
	return c.preconditions
}

func counterState() *architecturekit.State[counter] {
	state := architecturekit.NewState(counter{})

	state.Evolve(func(current counter, event incremented) counter {
		current.Total += event.By
		return current
	})

	state.Evolve(func(current counter, event reset) counter {
		current.Total = 0
		return current
	})

	return state
}

func counterDecider() architecturekit.Decider[increment, counter] {
	return architecturekit.Decider[increment, counter]{
		State: counterState(),
		Decide: func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
			if cmd.By == 0 {
				return nil, nil
			}
			if cmd.Limit > 0 && current.Total+cmd.By > cmd.Limit {
				return nil, architecturekit.NewDomainError("limit of %d would be exceeded", cmd.Limit)
			}
			return []architecturekit.Event{incremented{By: cmd.By}}, nil
		},
	}
}

// subjectFor gives every test its own subject, so that tests do not interfere
// with each other.
func subjectFor(t *testing.T) string {
	t.Helper()
	return "/test/" + t.Name()
}

// --- types that target specific failure paths ---
//
// All three report the same event type. That makes it possible to confront the
// framework with data that does not match its rule.

// annotated deliberately has no schema, so that the database does not validate
// the payload and mismatching values can end up in the stream.
type annotated struct {
	Note string `json:"note"`
}

func (annotated) EventType() string { return "io.thenativeweb.test.annotated" }

// annotatedBroken produces the same event type with a mismatching field type.
type annotatedBroken struct {
	Note int `json:"note"`
}

func (annotatedBroken) EventType() string { return "io.thenativeweb.test.annotated" }

// annotatedUnmarshallable cannot be marshalled at all.
type annotatedUnmarshallable struct {
	Channel chan int `json:"channel"`
}

func (annotatedUnmarshallable) EventType() string { return "io.thenativeweb.test.annotated" }

// note is a state whose rule expects annotated.
type note struct {
	Text string
}

func noteState() *architecturekit.State[note] {
	state := architecturekit.NewState(note{})

	state.Evolve(func(current note, event annotated) note {
		current.Text = event.Note
		return current
	})

	return state
}

type annotate struct {
	subject string
	event   architecturekit.Event
}

func (c annotate) Subject() string { return c.subject }

// noteDecider emits exactly the event it was handed.
func noteDecider() architecturekit.Decider[annotate, note] {
	return architecturekit.Decider[annotate, note]{
		State: noteState(),
		Decide: func(ctx context.Context, cmd annotate, current note) ([]architecturekit.Event, error) {
			return []architecturekit.Event{cmd.event}, nil
		},
	}
}
