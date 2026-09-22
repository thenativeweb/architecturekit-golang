package architecturekit_test

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net/url"
	"os"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// testStore is used by the integration tests and stays nil while they are
// skipped.
var (
	testStore  *architecturekit.Store
	testClient *eventsourcingdb.Client
)

// TestMain starts the EventSourcingDB once for all integration tests. With
// "go test -short" no container is started.
func TestMain(m *testing.M) {
	flag.Parse()

	if testing.Short() {
		os.Exit(m.Run())
	}

	ctx := context.Background()
	container := eventsourcingdb.NewContainer()

	if err := container.Start(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "failed to start eventsourcingdb: %v\n", err)
		os.Exit(1)
	}

	client, err := container.GetClient(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to create client: %v\n", err)
		_ = container.Stop(ctx)
		os.Exit(1)
	}

	testClient = client
	testStore = architecturekit.NewStore(client, "https://thenativeweb.io")

	code := m.Run()

	_ = container.Stop(ctx)
	os.Exit(code)
}

// requireStore skips a test when no database is running.
func requireStore(t *testing.T) *architecturekit.Store {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if testStore == nil {
		t.Fatal("no store available")
	}

	return testStore
}

// rawClient returns the client for tests that work around the framework.
func rawClient(t *testing.T) *eventsourcingdb.Client {
	t.Helper()

	if testing.Short() {
		t.Skip("skipping integration test in short mode")
	}
	if testClient == nil {
		t.Fatal("no client available")
	}

	return testClient
}

// totalIn reads the counter straight from the database, bypassing the
// framework's abstractions.
func totalIn(t *testing.T, store *architecturekit.Store, subject string) int {
	t.Helper()

	total := 0
	for event, err := range rawClient(t).ReadEvents(context.Background(), subject,
		eventsourcingdb.ReadEventsOptions{Recursive: false}) {
		if err != nil {
			t.Fatalf("failed to read %q: %v", subject, err)
		}

		switch event.Type {
		case (incremented{}).EventType():
			var payload incremented
			if err := json.Unmarshal(event.Data, &payload); err != nil {
				t.Fatalf("failed to decode: %v", err)
			}
			total += payload.By
		case (reset{}).EventType():
			total = 0
		}
	}

	return total
}

// deadClient points at a port where nothing listens.
func deadClient(t *testing.T) *eventsourcingdb.Client {
	t.Helper()

	deadURL, err := url.Parse("http://127.0.0.1:1")
	if err != nil {
		t.Fatal(err)
	}

	client, err := eventsourcingdb.NewClient(deadURL, "secret")
	if err != nil {
		t.Fatal(err)
	}

	return client
}
