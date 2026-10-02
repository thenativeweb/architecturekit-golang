package dbtest

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdbtest"
)

// tbSpy is a testing.TB that records what would skip or end a test, and ends
// only the goroutine it runs in, as the testing package does. It keeps the
// functions handed to Cleanup, so that a test can run them when it wants to.
type tbSpy struct {
	testing.TB
	skipped  []string
	failures []string
	errors   []string
	cleanups []func()
}

func (s *tbSpy) Helper() {}

func (s *tbSpy) Skip(args ...any) {
	s.skipped = append(s.skipped, fmt.Sprint(args...))
	runtime.Goexit()
}

func (s *tbSpy) Fatalf(format string, args ...any) {
	s.failures = append(s.failures, fmt.Sprintf(format, args...))
	runtime.Goexit()
}

func (s *tbSpy) Errorf(format string, args ...any) {
	s.errors = append(s.errors, fmt.Sprintf(format, args...))
}

func (s *tbSpy) Cleanup(cleanup func()) {
	s.cleanups = append(s.cleanups, cleanup)
}

func spyOn(t *testing.T, call func(testing.TB)) *tbSpy {
	spy := &tbSpy{TB: t}
	done := make(chan struct{})

	go func() {
		defer close(done)
		call(spy)
	}()
	<-done

	return spy
}

// replace swaps a package variable for the duration of a test.
func replace[T any](t *testing.T, variable *T, value T) {
	previous := *variable
	*variable = value
	t.Cleanup(func() { *variable = previous })
}

type stopperFunc func(context.Context) error

func (f stopperFunc) Stop(ctx context.Context) error { return f(ctx) }

func TestShortMode(t *testing.T) {
	t.Run("skips instead of starting a database", func(t *testing.T) {
		replace(t, &isShort, func() bool { return true })
		replace(t, &shared, &sharedDatabase{})
		replace(t, &start, func(context.Context) (stopper, *Database, error) {
			t.Error("no database may be started in short mode")
			return nil, nil, errors.New("started")
		})

		for name, call := range map[string]func(testing.TB){
			"SharedDatabase":   func(tb testing.TB) { SharedDatabase(tb) },
			"IsolatedDatabase": func(tb testing.TB) { IsolatedDatabase(tb) },
		} {
			t.Run(name, func(t *testing.T) {
				spy := spyOn(t, call)

				assert.Len(t, spy.skipped, 1, "the test has to be skipped")
				assert.Empty(t, spy.failures)
			})
		}
	})
}

func TestStartFailure(t *testing.T) {
	failing := func(context.Context) (stopper, *Database, error) {
		return nil, nil, errors.New("docker is not running")
	}

	t.Run("fails every test that asks for the shared database", func(t *testing.T) {
		replace(t, &isShort, func() bool { return false })
		replace(t, &shared, &sharedDatabase{})
		replace(t, &start, failing)

		for range 2 {
			spy := spyOn(t, func(tb testing.TB) { SharedDatabase(tb) })

			require.Len(t, spy.failures, 1)
			assert.Contains(t, spy.failures[0], "starting the shared database: docker is not running")
		}
	})

	t.Run("fails the test that asks for an isolated database", func(t *testing.T) {
		replace(t, &isShort, func() bool { return false })
		replace(t, &start, failing)

		spy := spyOn(t, func(tb testing.TB) { IsolatedDatabase(tb) })

		require.Len(t, spy.failures, 1)
		assert.Contains(t, spy.failures[0], "starting a database: docker is not running")
	})

	t.Run("reports a container without an address", func(t *testing.T) {
		_, err := databaseIn(context.Background(), eventsourcingdbtest.NewContainer())

		assert.Error(t, err, "a container that is not running has no address")
	})

	t.Run("reports a container that could not be started", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		_, _, err := startDatabase(ctx)

		assert.ErrorIs(t, err, context.Canceled)
	})
}

func TestIsolatedDatabaseStop(t *testing.T) {
	t.Run("reports a database that could not be stopped", func(t *testing.T) {
		replace(t, &isShort, func() bool { return false })
		replace(t, &start, func(context.Context) (stopper, *Database, error) {
			return stopperFunc(func(context.Context) error { return errors.New("container is stuck") }), &Database{}, nil
		})

		spy := spyOn(t, func(tb testing.TB) { IsolatedDatabase(tb) })
		require.Len(t, spy.cleanups, 1, "stopping has to be left to the end of the test")

		spy.cleanups[0]()

		require.Len(t, spy.errors, 1)
		assert.Contains(t, spy.errors[0], "stopping the database: container is stuck")
	})
}

func TestFinish(t *testing.T) {
	t.Run("passes the result on without a shared database", func(t *testing.T) {
		replace(t, &shared, &sharedDatabase{})

		var log bytes.Buffer
		assert.Equal(t, 3, finish(3, &log))
		assert.Empty(t, log.String())
	})

	t.Run("stops the shared database", func(t *testing.T) {
		stopped := false
		replace(t, &shared, &sharedDatabase{container: stopperFunc(func(context.Context) error {
			stopped = true
			return nil
		})})

		var log bytes.Buffer
		assert.Equal(t, 0, finish(0, &log))
		assert.True(t, stopped)
		assert.Empty(t, log.String())
	})

	t.Run("fails a run whose shared database could not be stopped", func(t *testing.T) {
		replace(t, &shared, &sharedDatabase{container: stopperFunc(func(context.Context) error {
			return errors.New("container is stuck")
		})})

		var log bytes.Buffer
		assert.Equal(t, 1, finish(0, &log), "a container left behind has to fail the run")
		assert.Equal(t, "architecturekittest/dbtest: stopping the shared database: container is stuck\n", log.String())
	})
}

func TestStopSharedDatabase(t *testing.T) {
	t.Run("does nothing without a shared database", func(t *testing.T) {
		replace(t, &shared, &sharedDatabase{})

		assert.NoError(t, StopSharedDatabase())
	})

	t.Run("stops the shared database", func(t *testing.T) {
		stopped := false
		replace(t, &shared, &sharedDatabase{container: stopperFunc(func(context.Context) error {
			stopped = true
			return nil
		})})

		require.NoError(t, StopSharedDatabase())
		assert.True(t, stopped)
	})

	t.Run("reports a shared database that could not be stopped", func(t *testing.T) {
		replace(t, &shared, &sharedDatabase{container: stopperFunc(func(context.Context) error {
			return errors.New("container is stuck")
		})})

		assert.EqualError(t, StopSharedDatabase(), "container is stuck")
	})
}
