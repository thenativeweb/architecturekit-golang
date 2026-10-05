package architecturekit_test

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// The kit encodes the data of every event itself before it writes it, so that
// data encoding/json can not encode is a permanent failure, and it has to hand
// the database exactly the JSON the client would have written for the event.

// measured holds a number, which encoding/json can not encode if it is NaN or
// infinite. Its schema is derived.
type measured struct {
	Value float64 `json:"value"`
}

func (measured) EventType() string { return "io.thenativeweb.test.measured" }

// failingToEncode refuses to encode itself.
type failingToEncode struct{}

func (failingToEncode) EventType() string { return "io.thenativeweb.test.failing-to-encode" }

func (failingToEncode) MarshalJSON() ([]byte, error) {
	return nil, errors.New("refusing to be encoded")
}

func (failingToEncode) Schema() map[string]any { return objectSchema(map[string]any{}) }

// selfEncoding writes its JSON itself, with whitespace that encoding/json
// removes and characters that it escapes. Its schema comes from its Schema
// function.
type selfEncoding struct{}

func (selfEncoding) EventType() string { return "io.thenativeweb.test.self-encoding" }

func (selfEncoding) MarshalJSON() ([]byte, error) {
	return []byte("{ \"html\": \"<b>&</b>\",\n  \"separator\": \"\u2028\" }"), nil
}

func (selfEncoding) Schema() map[string]any {
	return objectSchema(map[string]any{
		"html":      map[string]any{"type": "string"},
		"separator": map[string]any{"type": "string"},
	})
}

// sample holds what encoding/json writes in a way of its own: characters that
// it escapes, invalid UTF-8, which it replaces, numbers that it writes with an
// exponent, a map, whose keys it sorts, a []byte, which it writes in base64, a
// nil slice and a nil map, which it writes as null, a time, and a field that
// writes its JSON itself. Its schema is derived.
type sample struct {
	Text    string            `json:"text"`
	Invalid string            `json:"invalid"`
	Large   float64           `json:"large"`
	Small   float64           `json:"small"`
	Counts  map[string]int    `json:"counts"`
	Bytes   []byte            `json:"bytes"`
	Nothing []string          `json:"nothing"`
	Nowhere map[string]string `json:"nowhere"`
	At      time.Time         `json:"at"`
	Inner   selfEncoding      `json:"inner"`
}

func (sample) EventType() string { return "io.thenativeweb.test.sample" }

func newSample() sample {
	return sample{
		Text:    "<b>Tom & Jerry</b>\u2028\u2029",
		Invalid: "\xff",
		Large:   1e21,
		Small:   1e-7,
		Counts:  map[string]int{"zebra": 1, "aardvark": 2, "moose": 3},
		Bytes:   []byte("hello"),
		At:      time.Date(2026, time.October, 2, 12, 0, 0, 123, time.UTC),
	}
}

// encodableEvents are events with a schema of their own, a derived one, one
// that is a pointer, and one that writes its JSON itself.
func encodableEvents() []architecturekit.Event {
	pointed := newSample()

	return []architecturekit.Event{incremented{By: 1}, newSample(), &pointed, selfEncoding{}}
}

// unencodable is an event whose data encoding/json can not encode, together
// with what encoding/json says about it.
type unencodable struct {
	name  string
	event architecturekit.Event
	says  string
}

func unencodableEvents() []unencodable {
	return []unencodable{
		{"a float NaN", measured{Value: math.NaN()}, "json: unsupported value: NaN"},
		{"a failing MarshalJSON", failingToEncode{}, "refusing to be encoded"},
		{"a channel", annotatedUnmarshallable{Channel: make(chan int)}, "json: unsupported type: chan int"},
	}
}

// encodingState counts increments, and ignores all other events of these
// tests, so that Execute gets as far as encoding them.
func encodingState() *architecturekit.State[counter] {
	state := counterState()

	state.Ignore[measured]()
	state.Ignore[failingToEncode]()
	state.Ignore[annotatedUnmarshallable]()
	state.Ignore[sample]()
	state.Ignore[selfEncoding]()

	return state
}

// candidatesOf turns events into what the client writes, with the events
// themselves as their data, for the client to encode.
func candidatesOf(subject string, events []architecturekit.Event) []eventsourcingdb.EventCandidate {
	candidates := make([]eventsourcingdb.EventCandidate, len(events))
	for i, event := range events {
		candidates[i] = eventsourcingdb.EventCandidate{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    event.EventType(),
			Data:    event,
		}
	}

	return candidates
}

// eventsOn puts all events on the same subject, for Write.
func eventsOn(subject string, events []architecturekit.Event) []architecturekit.EventOn {
	on := make([]architecturekit.EventOn, len(events))
	for i, event := range events {
		on[i] = architecturekit.EventOn{Subject: subject, Event: event}
	}

	return on
}

// recordingDatabase answers every read with no events and every write with no
// written events, and records the body of every write. It returns a client for
// it and a function that returns the bodies recorded so far.
func recordingDatabase(t *testing.T) (*eventsourcingdb.Client, func() []string) {
	t.Helper()

	var mutex sync.Mutex
	var writes []string

	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Server", "EventSourcingDB/test")

		if request.URL.Path != "/api/v1/write-events" {
			return
		}

		body, err := io.ReadAll(request.Body)
		if err != nil {
			writer.WriteHeader(http.StatusBadRequest)
			return
		}

		mutex.Lock()
		writes = append(writes, string(body))
		mutex.Unlock()

		_, _ = fmt.Fprint(writer, "[]")
	}))
	t.Cleanup(server.Close)

	return clientFor(t, server), func() []string {
		mutex.Lock()
		defer mutex.Unlock()

		return slices.Clone(writes)
	}
}

func TestEncodingEvents(t *testing.T) {
	t.Run("Execute refuses data that can not be encoded as permanent and writes nothing", func(t *testing.T) {
		for _, test := range unencodableEvents() {
			t.Run(test.name, func(t *testing.T) {
				subject := subjectFor(t)

				written, err := architecturekit.Execute(context.Background(), requireStore(t),
					emittingDecider(encodingState(), test.event), increment{subject: subject})

				require.ErrorIs(t, err, architecturekit.ErrPermanent, "trying again would fail the same way")
				assert.NotErrorIs(t, err, architecturekit.ErrTransient)
				assert.ErrorContains(t, err, fmt.Sprintf("refusing to write an event of type %q to %q, "+
					"since its data can not be encoded as JSON: ", test.event.EventType(), subject),
					"error should name the event type and the subject")
				assert.ErrorContains(t, err, test.says, "error should say why")
				assert.Nil(t, written)
				assert.Empty(t, eventsIn(t, subject), "nothing may have been written")
			})
		}
	})

	t.Run("Execute writes none of the events if the second can not be encoded", func(t *testing.T) {
		for _, test := range unencodableEvents() {
			t.Run(test.name, func(t *testing.T) {
				subject := subjectFor(t)

				_, err := architecturekit.Execute(context.Background(), requireStore(t),
					emittingDecider(encodingState(), incremented{By: 1}, test.event), increment{subject: subject})

				require.ErrorIs(t, err, architecturekit.ErrPermanent)
				assert.ErrorContains(t, err, fmt.Sprintf("%q to %q", test.event.EventType(), subject))
				assert.Empty(t, eventsIn(t, subject), "not even the first event may have been written")
			})
		}
	})

	t.Run("Execute refuses data that can not be encoded also if the context ends while deciding", func(t *testing.T) {
		client, writes := recordingDatabase(t)
		store := architecturekit.NewStore(client, "https://thenativeweb.io")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		decider := emittingDecider(encodingState(), measured{Value: math.NaN()})
		decide := decider.Decide
		decider = architecturekit.NewDecider(decider.State(),
			func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
				cancel()
				return decide(ctx, cmd, current)
			})

		_, err := architecturekit.Execute(ctx, store, decider, increment{subject: subjectFor(t)})

		require.ErrorIs(t, err, architecturekit.ErrPermanent, "a mistake in the code must not hide behind the end of the context")
		assert.NotErrorIs(t, err, context.Canceled)
		assert.Empty(t, writes())
	})

	t.Run("Write refuses data that can not be encoded as permanent and writes none of the events", func(t *testing.T) {
		for _, test := range unencodableEvents() {
			t.Run(test.name, func(t *testing.T) {
				first, second := subjectFor(t)+"/first", subjectFor(t)+"/second"

				for _, events := range [][]architecturekit.EventOn{
					{{Subject: second, Event: test.event}},
					{{Subject: first, Event: incremented{By: 1}}, {Subject: second, Event: test.event}},
				} {
					written, err := architecturekit.Write(context.Background(), requireStore(t), events,
						architecturekit.Unconditionally())

					require.ErrorIs(t, err, architecturekit.ErrPermanent, "trying again would fail the same way")
					assert.NotErrorIs(t, err, architecturekit.ErrTransient)
					assert.ErrorContains(t, err, fmt.Sprintf("refusing to write an event of type %q to %q, "+
						"since its data can not be encoded as JSON: ", test.event.EventType(), second),
						"error should name the event type and the subject")
					assert.ErrorContains(t, err, test.says, "error should say why")
					assert.Nil(t, written)
				}

				assert.Empty(t, eventsIn(t, first), "not even the event that can be encoded may have been written")
				assert.Empty(t, eventsIn(t, second))
			})
		}
	})

	t.Run("Write refuses data that can not be encoded also once the context has ended", func(t *testing.T) {
		client, writes := recordingDatabase(t)
		store := architecturekit.NewStore(client, "https://thenativeweb.io")

		ended, cancel := context.WithCancel(context.Background())
		cancel()

		_, err := architecturekit.Write(ended, store, []architecturekit.EventOn{
			{Subject: subjectFor(t), Event: measured{Value: math.NaN()}},
		}, architecturekit.Unconditionally())

		require.ErrorIs(t, err, architecturekit.ErrPermanent, "a mistake in the code must not hide behind the end of the context")
		assert.NotErrorIs(t, err, context.Canceled)
		assert.Empty(t, writes())
	})

	t.Run("hands the database the same request as the client does for the events themselves", func(t *testing.T) {
		client, writes := recordingDatabase(t)
		store := architecturekit.NewStore(client, "https://thenativeweb.io")
		subject := subjectFor(t)
		events := encodableEvents()

		_, err := client.WriteEvents(candidatesOf(subject, events), nil)
		require.NoError(t, err)

		_, err = architecturekit.Execute(context.Background(), store,
			emittingDecider(encodingState(), events...), increment{subject: subject})
		require.NoError(t, err)

		_, err = architecturekit.Write(context.Background(), store, eventsOn(subject, events),
			architecturekit.Unconditionally())
		require.NoError(t, err)

		recorded := writes()
		require.Len(t, recorded, 3)
		assert.Equal(t, recorded[0], recorded[1], "Execute has to hand over the same JSON as the client")
		assert.Equal(t, recorded[0], recorded[2], "Write has to hand over the same JSON as the client")

		// The events have to make encoding/json do what it does in a way of
		// its own, or the comparison would show little.
		for _, written := range []string{`\u003cb\u003eTom \u0026 Jerry`, `\u2028\u2029`, "\"invalid\":\"\ufffd\"", `1e+21`, `1e-7`,
			`{"aardvark":2,"moose":3,"zebra":1}`, `"aGVsbG8="`, `"nothing":null`, `"nowhere":null`,
			`"2026-10-02T12:00:00.000000123Z"`, `{"html":"\u003cb\u003e\u0026\u003c/b\u003e","separator":"\u2028"}`} {
			assert.Contains(t, recorded[0], written)
		}
	})

	t.Run("stores the same data as the client does for the events themselves", func(t *testing.T) {
		store := requireStore(t)
		require.NoError(t, architecturekit.RegisterSchemas(context.Background(), store, []architecturekit.EventSchema{
			{EventType: (incremented{}).EventType(), Schema: (incremented{}).Schema()},
			{EventType: (sample{}).EventType(), Schema: architecturekit.DeriveSchema[sample]()},
			{EventType: (selfEncoding{}).EventType(), Schema: (selfEncoding{}).Schema()},
		}))

		byClient, byExecute, byWrite := subjectFor(t)+"/client", subjectFor(t)+"/execute", subjectFor(t)+"/write"
		events := encodableEvents()

		_, err := rawClient(t).WriteEvents(candidatesOf(byClient, events), nil)
		require.NoError(t, err)

		_, err = architecturekit.Execute(context.Background(), store,
			emittingDecider(encodingState(), events...), increment{subject: byExecute})
		require.NoError(t, err)

		_, err = architecturekit.Write(context.Background(), store, eventsOn(byWrite, events),
			architecturekit.Unconditionally())
		require.NoError(t, err)

		expected := eventsIn(t, byClient)
		require.Len(t, expected, len(events))

		for _, subject := range []string{byExecute, byWrite} {
			stored := eventsIn(t, subject)
			require.Len(t, stored, len(events))

			for i := range events {
				assert.Equal(t, expected[i].Type, stored[i].Type)
				assert.Equal(t, string(expected[i].Data), string(stored[i].Data), "event %d on %q", i, subject)
			}
		}
	})
}
