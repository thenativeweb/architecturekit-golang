package architecturekit_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// The current shape of the event, version three.
type credited struct {
	Amount   int    `json:"amount"`
	Currency string `json:"currency"`
}

func (credited) EventType() string { return "io.thenativeweb.test.credited.v3" }

type ledger struct {
	Total    int
	Currency string
	Entries  int
}

func stored(eventType, data string) eventsourcingdb.Event {
	return eventsourcingdb.Event{
		Subject: "/ledger/1",
		Type:    eventType,
		Data:    json.RawMessage(data),
	}
}

// ledgerState knows only the current shape and reaches the older ones through
// a chain of upcasters, one step per version.
func ledgerState() *architecturekit.State[ledger] {
	state := architecturekit.NewState(ledger{})

	state.Evolve(func(current ledger, event credited) ledger {
		current.Total += event.Amount
		current.Currency = event.Currency
		current.Entries++
		return current
	})

	// v1 had no currency at all.
	state.Upcast("io.thenativeweb.test.credited.v1",
		func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
			var payload struct {
				Amount int `json:"amount"`
			}
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, err
			}

			event.Type = "io.thenativeweb.test.credited.v2"
			event.Data = json.RawMessage(`{"amount":` + itoa(payload.Amount) + `,"currency":"EUR"}`)

			return []eventsourcingdb.Event{event}, nil
		})

	// v2 spelled the currency in lower case.
	state.Upcast("io.thenativeweb.test.credited.v2",
		func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
			var payload credited
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				return nil, err
			}

			payload.Currency = strings.ToUpper(payload.Currency)
			data, err := json.Marshal(payload)
			if err != nil {
				return nil, err
			}

			event.Type = (credited{}).EventType()
			event.Data = data

			return []eventsourcingdb.Event{event}, nil
		})

	return state
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	digits := ""
	for value > 0 {
		digits = string(rune('0'+value%10)) + digits
		value /= 10
	}
	return digits
}

func TestUpcasterChainReachesTheCurrentShape(t *testing.T) {
	current, err := architecturekit.ReplayStored(ledgerState(),
		stored("io.thenativeweb.test.credited.v1", `{"amount":10}`),
		stored("io.thenativeweb.test.credited.v2", `{"amount":5,"currency":"chf"}`),
		stored("io.thenativeweb.test.credited.v3", `{"amount":1,"currency":"USD"}`),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if current.Total != 16 {
		t.Fatalf("got %d, want 16", current.Total)
	}
	if current.Entries != 3 {
		t.Fatalf("got %d entries, want 3", current.Entries)
	}
	// The v1 event went through two steps and arrived with an upper case
	// currency, which only the second upcaster produces.
	if current.Currency != "USD" {
		t.Fatalf("got %q", current.Currency)
	}
}

func TestUpcasterCanSplitOneEventIntoTwo(t *testing.T) {
	state := architecturekit.NewState(ledger{})
	state.Evolve(func(current ledger, event credited) ledger {
		current.Total += event.Amount
		current.Entries++
		return current
	})
	state.Upcast("io.thenativeweb.test.credited.batch",
		func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
			first, second := event, event
			first.Type = (credited{}).EventType()
			first.Data = json.RawMessage(`{"amount":3,"currency":"EUR"}`)
			second.Type = (credited{}).EventType()
			second.Data = json.RawMessage(`{"amount":4,"currency":"EUR"}`)
			return []eventsourcingdb.Event{first, second}, nil
		})

	current, err := architecturekit.ReplayStored(state,
		stored("io.thenativeweb.test.credited.batch", `{}`))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if current.Total != 7 || current.Entries != 2 {
		t.Fatalf("got total %d in %d entries, want 7 in 2", current.Total, current.Entries)
	}
}

func TestUpcasterErrorIsPermanent(t *testing.T) {
	_, err := architecturekit.ReplayStored(ledgerState(),
		stored("io.thenativeweb.test.credited.v1", `not json`))

	if err == nil {
		t.Fatal("expected an error")
	}
	if !errorsIs(err, architecturekit.ErrPermanent) {
		t.Fatalf("an upcaster failure is permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "upcasting") {
		t.Fatalf("got %q", err.Error())
	}
}

func TestUpcasterThatKeepsItsTypeIsStopped(t *testing.T) {
	state := architecturekit.NewState(ledger{})
	state.Upcast("io.thenativeweb.test.loop",
		func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
			return []eventsourcingdb.Event{event}, nil
		})

	_, err := architecturekit.ReplayStored(state, stored("io.thenativeweb.test.loop", `{}`))

	if err == nil {
		t.Fatal("an upcaster that never changes the type must be stopped")
	}
	if !strings.Contains(err.Error(), "exceeded") {
		t.Fatalf("got %q", err.Error())
	}
}

func TestUpcastPanicsOnDuplicateRegistration(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a duplicate upcaster")
		}
	}()

	state := architecturekit.NewState(ledger{})
	passThrough := func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
		return []eventsourcingdb.Event{event}, nil
	}
	state.Upcast("io.thenativeweb.test.same", passThrough)
	state.Upcast("io.thenativeweb.test.same", passThrough)
}

func TestReplayStoredFailsOnEventWithoutRule(t *testing.T) {
	_, err := architecturekit.ReplayStored(ledgerState(),
		stored("io.thenativeweb.test.unheard-of", `{}`))

	if !errorsIs(err, architecturekit.ErrPermanent) {
		t.Fatalf("got %v", err)
	}
}

func TestReplayStoredFailsOnDataThatDoesNotMatch(t *testing.T) {
	_, err := architecturekit.ReplayStored(ledgerState(),
		stored("io.thenativeweb.test.credited.v3", `{"amount":"not a number"}`))

	if !errorsIs(err, architecturekit.ErrPermanent) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "decoding") {
		t.Fatalf("got %q", err.Error())
	}
}

func TestReplayFailsOnAnEventThatCannotBeEncoded(t *testing.T) {
	_, err := architecturekit.Replay(noteState(), annotatedUnmarshallable{Channel: make(chan int)})

	if !errorsIs(err, architecturekit.ErrPermanent) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "encoding") {
		t.Fatalf("got %q", err.Error())
	}
}

func TestReplayFailsOnDataThatDoesNotMatchTheRule(t *testing.T) {
	_, err := architecturekit.Replay(noteState(), annotatedBroken{Note: 42})

	if !errorsIs(err, architecturekit.ErrPermanent) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "decoding") {
		t.Fatalf("got %q", err.Error())
	}
}

func TestReplayFailsOnEventWithoutRule(t *testing.T) {
	_, err := architecturekit.Replay(noteState(), reset{})

	if !errorsIs(err, architecturekit.ErrPermanent) {
		t.Fatalf("got %v", err)
	}
}
