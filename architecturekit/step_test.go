package architecturekit_test

import (
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// tally holds a slice, so that it can only be stepped with a Clone function.
type tally struct {
	Entries []string
}

// tallyState counts in place: reset overwrites the entries it already holds,
// which is exactly what would change the given state without a copy.
func tallyState() *architecturekit.State[tally] {
	return architecturekit.NewState(tally{}).
		Evolve(func(current tally, event incremented) tally {
			current.Entries = append(current.Entries, strconv.Itoa(event.By))
			return current
		}).
		Evolve(func(current tally, _ reset) tally {
			for i := range current.Entries {
				current.Entries[i] = "0"
			}
			return current
		})
}

func TestStep(t *testing.T) {
	t.Run("walks the same states as Replay", func(t *testing.T) {
		history := []architecturekit.Event{incremented{By: 3}, incremented{By: 4}, reset{}, incremented{By: 2}}
		want := []int{3, 7, 0, 2}

		current, err := architecturekit.Replay(counterState())
		require.NoError(t, err)

		for i, event := range history {
			current, err = architecturekit.Step(counterState(), current, event)
			require.NoError(t, err, "step %d", i)
			assert.Equal(t, want[i], current.Total, "step %d", i)

			replayed, err := architecturekit.Replay(counterState(), history[:i+1]...)
			require.NoError(t, err, "replay %d", i)
			assert.Equal(t, replayed, current, "step %d walks another state than Replay", i)
		}
	})

	t.Run("leaves the given state unchanged", func(t *testing.T) {
		state := tallyState().Clone(func(current tally) tally {
			return tally{Entries: slices.Clone(current.Entries)}
		})

		before := tally{Entries: []string{"3", "4"}}

		after, err := architecturekit.Step(state, before, reset{})
		require.NoError(t, err)

		assert.Equal(t, []string{"0", "0"}, after.Entries)
		assert.Equal(t, []string{"3", "4"}, before.Entries, "the given state changed")

		afterStored, err := architecturekit.StepStored(state, before,
			stored((reset{}).EventType(), `{}`))
		require.NoError(t, err)
		assert.Equal(t, []string{"0", "0"}, afterStored.Entries)
		assert.Equal(t, []string{"3", "4"}, before.Entries, "the given state changed")
	})

	t.Run("refuses a state that it cannot copy", func(t *testing.T) {
		before := tally{Entries: []string{"3", "4"}}

		_, err := architecturekit.Step(tallyState(), before, reset{})
		require.ErrorIs(t, err, architecturekit.ErrPermanent, "a state without Clone is refused permanently")
		assert.ErrorContains(t, err, "Clone", "the error should point to Clone")

		_, err = architecturekit.StepStored(tallyState(), before, stored((reset{}).EventType(), `{}`))
		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a state without Clone is refused permanently")

		assert.Equal(t, []string{"3", "4"}, before.Entries, "the given state changed")
	})

	t.Run("returns the given state on failure", func(t *testing.T) {
		before := counter{Total: 5}

		current, err := architecturekit.Step(counterState(), before, annotated{Note: "unexpected"})
		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "an event without rule is permanent")
		assert.Equal(t, before, current)

		current, err = architecturekit.StepStored(counterState(), before,
			stored("io.thenativeweb.test.unheard-of", `{}`))
		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "an event without rule is permanent")
		assert.Equal(t, before, current)

		_, err = architecturekit.StepStored(ledgerState(), ledger{},
			stored("io.thenativeweb.test.credited.v3", `{"amount":"not a number"}`))
		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorContains(t, err, "decoding")
	})
}

func TestStepStored(t *testing.T) {
	t.Run("runs the upcasters", func(t *testing.T) {
		history := []eventsourcingdb.Event{
			stored("io.thenativeweb.test.credited.v1", `{"amount":10}`),
			stored("io.thenativeweb.test.credited.v2", `{"amount":5,"currency":"chf"}`),
			stored("io.thenativeweb.test.credited.v3", `{"amount":1,"currency":"USD"}`),
		}

		var current ledger
		for i, event := range history {
			var err error
			current, err = architecturekit.StepStored(ledgerState(), current, event)
			require.NoError(t, err, "step %d", i)

			replayed, err := architecturekit.ReplayStored(ledgerState(), history[:i+1]...)
			require.NoError(t, err, "replay %d", i)
			assert.Equal(t, replayed, current, "step %d walks another state than ReplayStored", i)
		}

		// The v1 event went through both upcasters, which only the second one
		// turns into an upper case currency.
		assert.Equal(t, 16, current.Total)
		assert.Equal(t, "USD", current.Currency)
	})

	t.Run("applies all events an upcaster splits one into", func(t *testing.T) {
		state := architecturekit.NewState(ledger{})
		state.Evolve(func(current ledger, event credited) ledger {
			current.Total += event.Amount
			current.Entries++
			return current
		})
		state.UpcastWith(architecturekit.NewUpcasters().
			Upcast("io.thenativeweb.test.credited.batch", splitIntoTwo))

		current, err := architecturekit.StepStored(state, ledger{Total: 1, Entries: 1},
			stored("io.thenativeweb.test.credited.batch", `{}`))
		require.NoError(t, err)

		assert.Equal(t, 8, current.Total)
		assert.Equal(t, 3, current.Entries)
	})
}
