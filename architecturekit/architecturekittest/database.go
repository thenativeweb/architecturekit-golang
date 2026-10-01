package architecturekittest

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"sync"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdbtest"
)

// Database is an EventSourcingDB in a container, for the tests that need a
// real one. Get the one that all tests of a package share with SharedDatabase,
// or one of a test's own with IsolatedDatabase.
type Database struct {
	// URL and APIToken are what a client needs, for a test that connects by
	// itself, such as one that starts a whole server.
	URL      *url.URL
	APIToken string

	client *eventsourcingdb.Client
}

// Client returns a client for the database, for a test that reads or writes
// events past the store, such as one that writes an event no command would.
func (d *Database) Client() *eventsourcingdb.Client {
	return d.client
}

// Store returns a store on the database that writes events with the given
// source, and registers the given schemas first. A schema that the database
// refuses fails the test.
func (d *Database) Store(t testing.TB, source string, schemas ...[]architecturekit.EventSchema) *architecturekit.Store {
	t.Helper()

	store := architecturekit.NewStore(d.client, source)
	if err := store.RegisterSchemas(schemas...); err != nil {
		t.Fatalf("registering the schemas: %v", err)
	}

	return store
}

// isShort tells whether the tests run with -short. It is a variable, so that
// the tests of this package can pretend to.
var isShort = testing.Short

// stopper is what Main and IsolatedDatabase need of a container.
type stopper interface {
	Stop(ctx context.Context) error
}

// sharedDatabase is the database that SharedDatabase starts for all tests of a
// package, and Main stops.
type sharedDatabase struct {
	once      sync.Once
	container stopper
	database  *Database
	err       error
}

var shared = &sharedDatabase{}

// start starts a database. It is a variable, so that the tests of this package
// can make it fail.
var start = startDatabase

// SharedDatabase returns the database that all tests of the package share. The
// first test that asks for it starts it, in a container, and Main stops it once
// all tests have run, so call Main from TestMain. With -short, the test is
// skipped instead, so that the other tests run without Docker.
//
// The tests share the events as well, so a test writes to subjects of its own,
// for example with a random ID in them, and reads only from those. A test that
// reads more than that, such as a projection from "/", needs IsolatedDatabase.
func SharedDatabase(t testing.TB) *Database {
	t.Helper()

	if isShort() {
		t.Skip("skipping a test that needs a database in short mode")
	}

	shared.once.Do(func() {
		shared.container, shared.database, shared.err = start(context.Background())
	})
	if shared.err != nil {
		t.Fatalf("starting the shared database: %v", shared.err)
	}

	return shared.database
}

// IsolatedDatabase starts a database for the test alone, in a container of its
// own, and stops it once the test is over. It takes a few seconds, so use it
// only where the shared one would not do. With -short, the test is skipped
// instead.
func IsolatedDatabase(t testing.TB) *Database {
	t.Helper()

	if isShort() {
		t.Skip("skipping a test that needs a database in short mode")
	}

	container, database, err := start(context.Background())
	if err != nil {
		t.Fatalf("starting a database: %v", err)
	}

	t.Cleanup(func() {
		if err := container.Stop(context.Background()); err != nil {
			t.Errorf("stopping the database: %v", err)
		}
	})

	return database
}

// Store returns a store on the shared database. It is SharedDatabase followed
// by its Store function.
func Store(t testing.TB, source string, schemas ...[]architecturekit.EventSchema) *architecturekit.Store {
	t.Helper()

	return SharedDatabase(t).Store(t, source, schemas...)
}

// IsolatedStore returns a store on a database of the test's own. It is
// IsolatedDatabase followed by its Store function.
func IsolatedStore(t testing.TB, source string, schemas ...[]architecturekit.EventSchema) *architecturekit.Store {
	t.Helper()

	return IsolatedDatabase(t).Store(t, source, schemas...)
}

// Main runs the tests of a package, and stops the shared database afterwards,
// if a test has started it. Call it from TestMain:
//
//	func TestMain(m *testing.M) {
//		architecturekittest.Main(m)
//	}
func Main(m *testing.M) {
	os.Exit(finish(m.Run(), os.Stderr))
}

// finish stops the shared database, and turns a failure to do so into a failed
// run, since a container left behind is a mistake somebody has to look at.
func finish(code int, log io.Writer) int {
	if shared.container == nil {
		return code
	}

	if err := shared.container.Stop(context.Background()); err != nil {
		_, _ = fmt.Fprintf(log, "architecturekittest: stopping the shared database: %v\n", err)
		return 1
	}

	return code
}

// startDatabase starts a database in a container, and stops it again if it
// cannot be reached.
func startDatabase(ctx context.Context) (stopper, *Database, error) {
	container := eventsourcingdbtest.NewContainer()

	if err := container.Start(ctx); err != nil {
		return nil, nil, err
	}

	database, err := databaseIn(ctx, container)
	if err != nil {
		return nil, nil, errors.Join(err, container.Stop(ctx))
	}

	return container, database, nil
}

func databaseIn(ctx context.Context, container *eventsourcingdbtest.Container) (*Database, error) {
	baseURL, err := container.GetBaseURL(ctx)
	if err != nil {
		return nil, err
	}

	client, err := eventsourcingdb.NewClient(baseURL, container.GetAPIToken())
	if err != nil {
		return nil, err
	}

	return &Database{URL: baseURL, APIToken: container.GetAPIToken(), client: client}, nil
}
