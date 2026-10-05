package architecturekit_test

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

func TestReplay(t *testing.T) {
	t.Run("returns initial state for empty history", func(t *testing.T) {
		current, err := architecturekit.Replay(counterState())
		require.NoError(t, err)
		assert.Equal(t, 0, current.Total)
	})

	t.Run("folds events in order", func(t *testing.T) {
		current, err := architecturekit.Replay(counterState(),
			incremented{By: 3},
			incremented{By: 4},
			reset{},
			incremented{By: 5},
		)
		require.NoError(t, err)
		assert.Equal(t, 5, current.Total)
	})

	t.Run("fails on unknown event type", func(t *testing.T) {
		state := architecturekit.NewState(counter{})
		state.Evolve(func(current counter, event incremented) counter { return current })

		_, err := architecturekit.Replay(state, reset{})
		require.Error(t, err, "expected an error for an event type without a rule")
		assert.Contains(t, err.Error(), reset{}.EventType(), "error should name the event type")
	})

	t.Run("fails on an event that cannot be encoded", func(t *testing.T) {
		_, err := architecturekit.Replay(noteState(), annotatedUnmarshallable{Channel: make(chan int)})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorContains(t, err, "encoding")
	})

	t.Run("fails on data that does not match the rule", func(t *testing.T) {
		_, err := architecturekit.Replay(noteState(), annotatedBroken{Note: 42})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorContains(t, err, "decoding")
	})

	t.Run("fails on event without rule", func(t *testing.T) {
		_, err := architecturekit.Replay(noteState(), reset{})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
	})
}

func TestEvolve(t *testing.T) {
	t.Run("panics on duplicate event type", func(t *testing.T) {
		defer func() {
			recovered := recover()
			require.NotNil(t, recovered, "expected a panic for a duplicate event type")
			message, ok := recovered.(string)
			require.True(t, ok, "panic should name the event type, got %v", recovered)
			assert.Contains(t, message, incremented{}.EventType(), "panic should name the event type")
		}()

		state := architecturekit.NewState(counter{})
		state.Evolve(func(current counter, event incremented) counter { return current })
		state.Evolve(func(current counter, event incremented) counter { return current })
	})

	t.Run("is chainable", func(t *testing.T) {
		state := architecturekit.NewState(counter{}).
			Evolve(func(current counter, event incremented) counter {
				current.Total += event.By
				return current
			})

		current, err := architecturekit.Replay(state, incremented{By: 7})
		require.NoError(t, err)
		assert.Equal(t, 7, current.Total)
	})
}

func TestSchemas(t *testing.T) {
	t.Run("collects the schema of every event", func(t *testing.T) {
		schemas := counterState().Schemas()

		require.Len(t, schemas, 2)
		for i, want := range []architecturekit.Event{incremented{}, reset{}} {
			assert.Equal(t, want.EventType(), schemas[i].EventType)
			assert.NotNil(t, schemas[i].Schema, "schema %d must not be nil", i)
		}
	})
}

func TestHasRule(t *testing.T) {
	t.Run("is true for an event type with an Evolve rule", func(t *testing.T) {
		assert.True(t, resetIgnoringState().HasRule((incremented{}).EventType()))
	})

	t.Run("is true for an event type the state ignores", func(t *testing.T) {
		assert.True(t, resetIgnoringState().HasRule((reset{}).EventType()))
	})

	t.Run("is false for an unknown event type", func(t *testing.T) {
		assert.False(t, resetIgnoringState().HasRule((annotated{}).EventType()))
	})

	t.Run("is false for an older type that only an upcaster knows", func(t *testing.T) {
		state := counterState().UpcastWith(architecturekit.NewUpcasters().
			Upcast("io.thenativeweb.test.outdated",
				func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
					event.Type = (incremented{}).EventType()
					return []eventsourcingdb.Event{event}, nil
				}))

		assert.False(t, state.HasRule("io.thenativeweb.test.outdated"))
	})
}

func TestDomainError(t *testing.T) {
	t.Run("carries its message", func(t *testing.T) {
		err := architecturekit.NewDomainError("limit of %d would be exceeded", 10)

		assert.EqualError(t, err, "limit of 10 would be exceeded")

		var domainError *architecturekit.DomainError
		assert.ErrorAs(t, err, &domainError)
	})
}

// contextKey marks the context that a test hands to a decider, so that the
// test can tell that the decider received it.
type contextKey struct{}

func TestNewDecider(t *testing.T) {
	t.Run("creates a decider that decides on the given state with the given function", func(t *testing.T) {
		state := counterState()

		var (
			receivedContext context.Context
			receivedCommand increment
			receivedState   counter
		)

		// The types of the command and the state are inferred from the
		// arguments, so that none of them has to be given.
		decider := architecturekit.NewDecider(state,
			func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
				receivedContext, receivedCommand, receivedState = ctx, cmd, current
				return []architecturekit.Event{incremented{By: cmd.By}}, errors.New("refused for the test")
			})

		assert.Same(t, state, decider.State())

		ctx := context.WithValue(context.Background(), contextKey{}, "the context of the test")
		events, err := decider.Decide(ctx, increment{subject: "/counter/1", By: 2}, counter{Total: 40})

		assert.Equal(t, []architecturekit.Event{incremented{By: 2}}, events)
		assert.EqualError(t, err, "refused for the test")
		assert.Equal(t, "the context of the test", receivedContext.Value(contextKey{}))
		assert.Equal(t, increment{subject: "/counter/1", By: 2}, receivedCommand)
		assert.Equal(t, counter{Total: 40}, receivedState)
	})

	t.Run("panics for a nil state", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit: NewDecider needs a state, not nil", func() {
			architecturekit.NewDecider(nil,
				func(context.Context, increment, counter) ([]architecturekit.Event, error) { return nil, nil })
		})
	})

	t.Run("panics for a nil function that decides", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit: NewDecider needs a function that decides, not nil", func() {
			architecturekit.NewDecider[increment](counterState(), nil)
		})
	})
}

func TestDecider(t *testing.T) {
	t.Run("has no state as the zero value", func(t *testing.T) {
		var decider architecturekit.Decider[increment, counter]

		assert.Nil(t, decider.State())
	})

	t.Run("panics in Execute as the zero value, naming the mistake", func(t *testing.T) {
		var decider architecturekit.Decider[increment, counter]
		store := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

		assert.PanicsWithValue(t, "architecturekit: Execute needs a decider made with NewDecider, not the zero Decider", func() {
			_, _ = architecturekit.Execute(context.Background(), store, decider, increment{subject: "/counter/1", By: 1})
		})
	})

	t.Run("panics in Execute as the zero value before it checks the command", func(t *testing.T) {
		var decider architecturekit.Decider[increment, counter]
		store := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

		// The command declares no precondition, which Execute refuses.
		assert.PanicsWithValue(t, "architecturekit: Execute needs a decider made with NewDecider, not the zero Decider", func() {
			_, _ = architecturekit.Execute(context.Background(), store, decider, increment{subject: "/counter/1", By: 1}.declaring())
		})
	})
}
