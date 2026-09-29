package architecturekit_test

import (
	"slices"
	"strconv"
	"strings"
	"testing"

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

func TestStepWalksTheSameStatesAsReplay(t *testing.T) {
	history := []architecturekit.Event{incremented{By: 3}, incremented{By: 4}, reset{}, incremented{By: 2}}
	want := []int{3, 7, 0, 2}

	current, err := architecturekit.Replay(counterState())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	for i, event := range history {
		current, err = architecturekit.Step(counterState(), current, event)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
		if current.Total != want[i] {
			t.Fatalf("step %d: got %d, want %d", i, current.Total, want[i])
		}

		replayed, err := architecturekit.Replay(counterState(), history[:i+1]...)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if current != replayed {
			t.Fatalf("step %d: got %+v, but Replay gives %+v", i, current, replayed)
		}
	}
}

func TestStepStoredRunsTheUpcasters(t *testing.T) {
	history := []eventsourcingdb.Event{
		stored("io.thenativeweb.test.credited.v1", `{"amount":10}`),
		stored("io.thenativeweb.test.credited.v2", `{"amount":5,"currency":"chf"}`),
		stored("io.thenativeweb.test.credited.v3", `{"amount":1,"currency":"USD"}`),
	}

	var current ledger
	for i, event := range history {
		var err error
		current, err = architecturekit.StepStored(ledgerState(), current, event)
		if err != nil {
			t.Fatalf("step %d: %v", i, err)
		}

		replayed, err := architecturekit.ReplayStored(ledgerState(), history[:i+1]...)
		if err != nil {
			t.Fatalf("replay %d: %v", i, err)
		}
		if current != replayed {
			t.Fatalf("step %d: got %+v, but ReplayStored gives %+v", i, current, replayed)
		}
	}

	// The v1 event went through both upcasters, which only the second one
	// turns into an upper case currency.
	if current.Total != 16 || current.Currency != "USD" {
		t.Fatalf("got %+v", current)
	}
}

func TestStepStoredAppliesAllEventsAnUpcasterSplitsOneInto(t *testing.T) {
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
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if current.Total != 8 || current.Entries != 3 {
		t.Fatalf("got total %d in %d entries, want 8 in 3", current.Total, current.Entries)
	}
}

func TestStepLeavesTheGivenStateUnchanged(t *testing.T) {
	state := tallyState().Clone(func(current tally) tally {
		return tally{Entries: slices.Clone(current.Entries)}
	})

	before := tally{Entries: []string{"3", "4"}}

	after, err := architecturekit.Step(state, before, reset{})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if !slices.Equal(after.Entries, []string{"0", "0"}) {
		t.Fatalf("got %v", after.Entries)
	}
	if !slices.Equal(before.Entries, []string{"3", "4"}) {
		t.Fatalf("the given state changed to %v", before.Entries)
	}

	afterStored, err := architecturekit.StepStored(state, before,
		stored((reset{}).EventType(), `{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !slices.Equal(afterStored.Entries, []string{"0", "0"}) {
		t.Fatalf("got %v", afterStored.Entries)
	}
	if !slices.Equal(before.Entries, []string{"3", "4"}) {
		t.Fatalf("the given state changed to %v", before.Entries)
	}
}

func TestStepRefusesAStateThatItCannotCopy(t *testing.T) {
	before := tally{Entries: []string{"3", "4"}}

	_, err := architecturekit.Step(tallyState(), before, reset{})
	if !errorsIs(err, architecturekit.ErrPermanent) {
		t.Fatalf("a state without Clone is refused permanently, got %v", err)
	}
	if !strings.Contains(err.Error(), "Clone") {
		t.Fatalf("error should point to Clone, got %q", err.Error())
	}

	_, err = architecturekit.StepStored(tallyState(), before, stored((reset{}).EventType(), `{}`))
	if !errorsIs(err, architecturekit.ErrPermanent) {
		t.Fatalf("a state without Clone is refused permanently, got %v", err)
	}

	if !slices.Equal(before.Entries, []string{"3", "4"}) {
		t.Fatalf("the given state changed to %v", before.Entries)
	}
}

func TestStepReturnsTheGivenStateOnFailure(t *testing.T) {
	before := counter{Total: 5}

	current, err := architecturekit.Step(counterState(), before, annotated{Note: "unexpected"})
	if !errorsIs(err, architecturekit.ErrPermanent) {
		t.Fatalf("an event without rule is permanent, got %v", err)
	}
	if current != before {
		t.Fatalf("got %+v, want %+v", current, before)
	}

	current, err = architecturekit.StepStored(counterState(), before,
		stored("io.thenativeweb.test.unheard-of", `{}`))
	if !errorsIs(err, architecturekit.ErrPermanent) {
		t.Fatalf("an event without rule is permanent, got %v", err)
	}
	if current != before {
		t.Fatalf("got %+v, want %+v", current, before)
	}

	_, err = architecturekit.StepStored(ledgerState(), ledger{},
		stored("io.thenativeweb.test.credited.v3", `{"amount":"not a number"}`))
	if !errorsIs(err, architecturekit.ErrPermanent) || !strings.Contains(err.Error(), "decoding") {
		t.Fatalf("got %v", err)
	}
}
