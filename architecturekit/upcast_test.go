package architecturekit_test

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// The current shape of the event, version three.
type credited struct {
	Amount   int    `json:"amount"`
	Currency string `json:"currency"`
}

func (credited) EventType() string { return "io.thenativeweb.test.credited.v3" }

func (credited) Schema() map[string]any {
	return objectSchema(map[string]any{
		"amount":   map[string]any{"type": "number"},
		"currency": map[string]any{"type": "string"},
	})
}

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

// ledgerUpcasters reaches the current shape from the older ones through a
// chain of upcasters, one step per version.
func ledgerUpcasters() *architecturekit.Upcasters {
	return architecturekit.NewUpcasters().
		// v1 had no currency at all.
		Upcast("io.thenativeweb.test.credited.v1",
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
			}).
		// v2 spelled the currency in lower case.
		Upcast("io.thenativeweb.test.credited.v2",
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
}

// ledgerState knows only the current shape and reaches the older ones through
// the upcasters.
func ledgerState() *architecturekit.State[ledger] {
	state := architecturekit.NewState(ledger{})

	state.Evolve(func(current ledger, event credited) ledger {
		current.Total += event.Amount
		current.Currency = event.Currency
		current.Entries++
		return current
	})

	state.UpcastWith(ledgerUpcasters())

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

func TestUpcaster(t *testing.T) {
	t.Run("chain reaches the current shape", func(t *testing.T) {
		current, err := architecturekit.ReplayStored(ledgerState(),
			stored("io.thenativeweb.test.credited.v1", `{"amount":10}`),
			stored("io.thenativeweb.test.credited.v2", `{"amount":5,"currency":"chf"}`),
			stored("io.thenativeweb.test.credited.v3", `{"amount":1,"currency":"USD"}`),
		)
		require.NoError(t, err)

		assert.Equal(t, 16, current.Total)
		assert.Equal(t, 3, current.Entries)
		// The v1 event went through two steps and arrived with an upper case
		// currency, which only the second upcaster produces.
		assert.Equal(t, "USD", current.Currency)
	})

	t.Run("can split one event into two", func(t *testing.T) {
		state := architecturekit.NewState(ledger{})
		state.Evolve(func(current ledger, event credited) ledger {
			current.Total += event.Amount
			current.Entries++
			return current
		})
		state.UpcastWith(architecturekit.NewUpcasters().
			Upcast("io.thenativeweb.test.credited.batch", splitIntoTwo))

		current, err := architecturekit.ReplayStored(state,
			stored("io.thenativeweb.test.credited.batch", `{}`))
		require.NoError(t, err)

		assert.Equal(t, 7, current.Total)
		assert.Equal(t, 2, current.Entries)
	})

	t.Run("error is permanent", func(t *testing.T) {
		_, err := architecturekit.ReplayStored(ledgerState(),
			stored("io.thenativeweb.test.credited.v1", `not json`))

		require.Error(t, err)
		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "an upcaster failure is permanent")
		assert.ErrorContains(t, err, "upcasting")
	})

	t.Run("that keeps its type is stopped", func(t *testing.T) {
		state := architecturekit.NewState(ledger{})
		state.UpcastWith(architecturekit.NewUpcasters().
			Upcast("io.thenativeweb.test.loop", passThrough))

		_, err := architecturekit.ReplayStored(state, stored("io.thenativeweb.test.loop", `{}`))

		require.Error(t, err, "an upcaster that never changes the type must be stopped")
		assert.ErrorContains(t, err, "exceeded")
	})
}

// splitIntoTwo turns one stored event into two credits.
func splitIntoTwo(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
	first, second := event, event
	first.Type = (credited{}).EventType()
	first.Data = json.RawMessage(`{"amount":3,"currency":"EUR"}`)
	second.Type = (credited{}).EventType()
	second.Data = json.RawMessage(`{"amount":4,"currency":"EUR"}`)
	return []eventsourcingdb.Event{first, second}, nil
}

// passThrough keeps an event as it is, including its type.
func passThrough(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
	return []eventsourcingdb.Event{event}, nil
}

func TestUpcast(t *testing.T) {
	t.Run("panics on duplicate registration", func(t *testing.T) {
		assert.Panics(t, func() {
			architecturekit.NewUpcasters().
				Upcast("io.thenativeweb.test.same", passThrough).
				Upcast("io.thenativeweb.test.same", passThrough)
		})
	})
}

func TestStateUpcastWith(t *testing.T) {
	t.Run("panics when called twice", func(t *testing.T) {
		assert.Panics(t, func() {
			architecturekit.NewState(ledger{}).
				UpcastWith(architecturekit.NewUpcasters()).
				UpcastWith(architecturekit.NewUpcasters())
		})
	})

	t.Run("panics without a set", func(t *testing.T) {
		assert.Panics(t, func() {
			architecturekit.NewState(ledger{}).UpcastWith(nil)
		})
	})
}

func TestSharedUpcasters(t *testing.T) {
	t.Run("apply to every state that uses them", func(t *testing.T) {
		upcasters := ledgerUpcasters()

		total := architecturekit.NewState(0).
			Evolve(func(current int, event credited) int { return current + event.Amount }).
			UpcastWith(upcasters)
		currencies := architecturekit.NewState("").
			Evolve(func(current string, event credited) string { return current + event.Currency }).
			UpcastWith(upcasters)

		history := []eventsourcingdb.Event{
			stored("io.thenativeweb.test.credited.v1", `{"amount":10}`),
			stored("io.thenativeweb.test.credited.v2", `{"amount":5,"currency":"chf"}`),
		}

		gotTotal, err := architecturekit.ReplayStored(total, history...)
		require.NoError(t, err)
		gotCurrencies, err := architecturekit.ReplayStored(currencies, history...)
		require.NoError(t, err)

		assert.Equal(t, 15, gotTotal)
		assert.Equal(t, "EURCHF", gotCurrencies)
	})
}

func TestReplayStored(t *testing.T) {
	t.Run("fails on event without rule", func(t *testing.T) {
		_, err := architecturekit.ReplayStored(ledgerState(),
			stored("io.thenativeweb.test.unheard-of", `{}`))

		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
	})

	t.Run("fails on data that does not match", func(t *testing.T) {
		_, err := architecturekit.ReplayStored(ledgerState(),
			stored("io.thenativeweb.test.credited.v3", `{"amount":"not a number"}`))

		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorContains(t, err, "decoding")
	})
}
