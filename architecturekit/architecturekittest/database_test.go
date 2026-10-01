package architecturekittest_test

import (
	"context"
	"crypto/rand"
	"fmt"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/architecturekittest"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

func TestMain(m *testing.M) {
	architecturekittest.Main(m)
}

// --- a journal, written to subjects of its own in every test ---

type entered struct {
	Text string `json:"text"`
}

func (entered) EventType() string { return "test.journal.entered" }

type enter struct {
	journal string
	Text    string
}

func (c enter) Subject() string { return "/journals/" + c.journal }

func (enter) Preconditions() []architecturekit.Precondition {
	return []architecturekit.Precondition{architecturekit.Unconditionally()}
}

type journal struct {
	Entries []string
}

var journalState = architecturekit.NewState(journal{}).
	Evolve(func(current journal, event entered) journal {
		current.Entries = append(current.Entries, event.Text)
		return current
	})

var enterDecider = architecturekit.Decider[enter, journal]{
	State: journalState,
	Decide: func(_ context.Context, cmd enter, _ journal) ([]architecturekit.Event, error) {
		return []architecturekit.Event{entered{Text: cmd.Text}}, nil
	},
}

const journalSource = "https://architecturekit.test"

// writeAndRead enters a text into a journal of its own, and reads the journal
// back.
func writeAndRead(t *testing.T, store *architecturekit.Store, text string) (string, journal) {
	t.Helper()

	name := rand.Text()
	_, err := architecturekit.Execute(context.Background(), store, enterDecider, enter{journal: name, Text: text})
	require.NoError(t, err)

	current, err := architecturekit.Load(context.Background(), store, journalState, "/journals/"+name)
	require.NoError(t, err)

	return name, current
}

// fatalSpy is a testing.TB that records what would end a test, and ends only
// the goroutine it runs in, as the testing package does.
type fatalSpy struct {
	testing.TB
	failures []string
}

func (s *fatalSpy) Helper() {}

func (s *fatalSpy) Fatalf(format string, args ...any) {
	s.failures = append(s.failures, fmt.Sprintf(format, args...))
	runtime.Goexit()
}

func runWithSpy(t *testing.T, call func(testing.TB)) *fatalSpy {
	spy := &fatalSpy{TB: t}
	done := make(chan struct{})

	go func() {
		defer close(done)
		call(spy)
	}()
	<-done

	return spy
}

func TestSharedDatabase(t *testing.T) {
	t.Run("is the same for every test of the package", func(t *testing.T) {
		first := architecturekittest.SharedDatabase(t)
		second := architecturekittest.SharedDatabase(t)

		assert.Same(t, first, second)
	})

	t.Run("hands out a store that writes and reads", func(t *testing.T) {
		store := architecturekittest.SharedDatabase(t).Store(t, journalSource, journalState.Schemas())

		_, current := writeAndRead(t, store, "first entry")
		assert.Equal(t, []string{"first entry"}, current.Entries)
	})

	t.Run("registers the schemas", func(t *testing.T) {
		database := architecturekittest.SharedDatabase(t)
		database.Store(t, journalSource, journalState.Schemas())

		var schema *map[string]any
		for eventType, err := range database.Client().ReadEventTypes(context.Background()) {
			require.NoError(t, err)
			if eventType.EventType == (entered{}).EventType() {
				schema = eventType.Schema
			}
		}

		require.NotNil(t, schema, "the schema of the journal has to be registered")
		assert.Contains(t, *schema, "properties")
	})

	t.Run("fails the test for a schema the database refuses", func(t *testing.T) {
		database := architecturekittest.SharedDatabase(t)
		eventType := "test.journal.refused." + rand.Text()

		spy := runWithSpy(t, func(tb testing.TB) {
			database.Store(tb, journalSource, []architecturekit.EventSchema{
				{EventType: eventType, Schema: map[string]any{"type": "no such type"}},
			})
		})

		require.Len(t, spy.failures, 1)
		assert.Contains(t, spy.failures[0], "registering the schemas")
		assert.Contains(t, spy.failures[0], eventType, "the failure has to name the event type")
	})

	t.Run("can be reached with its address and API token", func(t *testing.T) {
		database := architecturekittest.SharedDatabase(t)

		client, err := eventsourcingdb.NewClient(database.URL, database.APIToken)
		require.NoError(t, err)
		assert.NoError(t, client.Ping())
	})

	t.Run("is what Store uses", func(t *testing.T) {
		store := architecturekittest.Store(t, journalSource, journalState.Schemas())
		name, _ := writeAndRead(t, store, "through Store")

		viaShared := architecturekittest.SharedDatabase(t).Store(t, journalSource)
		current, err := architecturekit.Load(context.Background(), viaShared, journalState, "/journals/"+name)
		require.NoError(t, err)
		assert.Equal(t, []string{"through Store"}, current.Entries)
	})
}

func TestIsolatedDatabase(t *testing.T) {
	t.Run("is a database of the test's own, which is stopped afterwards", func(t *testing.T) {
		var isolated *architecturekittest.Database

		t.Run("in a test", func(t *testing.T) {
			isolated = architecturekittest.IsolatedDatabase(t)
			require.NotEqual(t, architecturekittest.SharedDatabase(t).URL.String(), isolated.URL.String())

			name, current := writeAndRead(t, isolated.Store(t, journalSource, journalState.Schemas()), "isolated entry")
			assert.Equal(t, []string{"isolated entry"}, current.Entries)

			inShared, err := architecturekit.Load(context.Background(),
				architecturekittest.SharedDatabase(t).Store(t, journalSource), journalState, "/journals/"+name)
			require.NoError(t, err)
			assert.Empty(t, inShared.Entries, "the shared database must not see the events of an isolated one")
		})

		require.NotNil(t, isolated, "the test did not get a database")
		assert.Error(t, isolated.Client().Ping(), "the database has to be stopped once the test is over")
	})

	t.Run("is what IsolatedStore uses", func(t *testing.T) {
		store := architecturekittest.IsolatedStore(t, journalSource, journalState.Schemas())

		_, current := writeAndRead(t, store, "through IsolatedStore")
		assert.Equal(t, []string{"through IsolatedStore"}, current.Entries)
	})
}
