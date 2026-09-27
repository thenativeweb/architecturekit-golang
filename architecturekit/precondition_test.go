package architecturekit_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// interrupted decides like the given decider, but has someone else write to
// the subject after Execute has read the state and before it writes, which is
// exactly the moment another process can get in between.
func interrupted(
	t *testing.T,
	store *architecturekit.Store,
	decider architecturekit.Decider[increment, counter],
) architecturekit.Decider[increment, counter] {
	t.Helper()

	decide := decider.Decide
	decider.Decide = func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
		_, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: cmd.subject, By: 100})
		if err != nil {
			t.Errorf("failed to write in between: %v", err)
		}

		return decide(ctx, cmd, current)
	}

	return decider
}

func TestExecuteOnStateReadWritesIfNothingChanged(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	ctx := context.Background()

	// The first command finds the subject pristine, the second one finds the
	// event of the first.
	for range 2 {
		_, err := architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: 1}.onStateRead())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if total := totalIn(t, store, subject); total != 2 {
		t.Fatalf("got %d, want 2", total)
	}
}

func TestExecuteOnStateReadConflictsIfSomeoneWroteInBetween(t *testing.T) {
	t.Run("on a pristine subject", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		_, err := architecturekit.Execute(context.Background(), store, interrupted(t, store, counterDecider()),
			increment{subject: subject, By: 1}.onStateRead())

		if !errors.Is(err, architecturekit.ErrConflict) {
			t.Fatalf("expected a conflict, got %v", err)
		}
		if total := totalIn(t, store, subject); total != 100 {
			t.Fatalf("only the write in between may have happened; got %d", total)
		}
	})

	t.Run("on a populated subject", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)
		ctx := context.Background()

		if _, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: subject, By: 1}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		_, err := architecturekit.Execute(ctx, store, interrupted(t, store, counterDecider()),
			increment{subject: subject, By: 1}.onStateRead())

		if !errors.Is(err, architecturekit.ErrConflict) {
			t.Fatalf("expected a conflict, got %v", err)
		}
		if total := totalIn(t, store, subject); total != 101 {
			t.Fatalf("only the write in between may have happened; got %d", total)
		}
	})

	t.Run("with a state cache", func(t *testing.T) {
		store := cachedStore(t, 10)
		subject := subjectFor(t)
		ctx := context.Background()

		// The first command leaves the state in the cache.
		if _, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: subject, By: 1}.onStateRead()); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		_, err := architecturekit.Execute(ctx, store, interrupted(t, store, counterDecider()),
			increment{subject: subject, By: 1}.onStateRead())

		if !errors.Is(err, architecturekit.ErrConflict) {
			t.Fatalf("expected a conflict, got %v", err)
		}
		if total := totalIn(t, store, subject); total != 101 {
			t.Fatalf("only the write in between may have happened; got %d", total)
		}
	})

	t.Run("with a state that starts from the latest event of a type", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		writeRaw(t, subject, incremented{By: 5}, reset{}, incremented{By: 1})

		_, err := architecturekit.Execute(context.Background(), store, interrupted(t, store, counterFromLatestDecider()),
			increment{subject: subject, By: 1}.onStateRead())

		if !errors.Is(err, architecturekit.ErrConflict) {
			t.Fatalf("expected a conflict, got %v", err)
		}
		if total := totalIn(t, store, subject); total != 101 {
			t.Fatalf("only the write in between may have happened; got %d", total)
		}
	})
}

func TestExecuteOnStateReadAdmitsOnlyOneOfConcurrentCommands(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	ctx := context.Background()

	if _, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: subject, By: 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Every command waits in Decide until all of them have read the state, so
	// that they all decide on the same one, as concurrent processes may.
	const concurrent = 4
	var haveRead sync.WaitGroup
	haveRead.Add(concurrent)

	decider := counterDecider()
	decide := decider.Decide
	decider.Decide = func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
		haveRead.Done()
		haveRead.Wait()

		return decide(ctx, cmd, current)
	}

	var waitGroup sync.WaitGroup
	results := make([]error, concurrent)
	for i := range concurrent {
		waitGroup.Go(func() {
			_, results[i] = architecturekit.Execute(ctx, store, decider, increment{subject: subject, By: 1}.onStateRead())
		})
	}
	waitGroup.Wait()

	succeeded := 0
	for i, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, architecturekit.ErrConflict):
		default:
			t.Fatalf("command %d failed unexpectedly: %v", i, err)
		}
	}

	if succeeded != 1 {
		t.Fatalf("exactly one command must succeed, got %d", succeeded)
	}
	if total := totalIn(t, store, subject); total != 2 {
		t.Fatalf("got %d, want 2", total)
	}
}

func TestExecuteOnStateReadSeesWhatOtherProcessesWroteThroughTheCache(t *testing.T) {
	cached := cachedStore(t, 10)
	other := requireStore(t)
	subject := subjectFor(t)
	ctx := context.Background()

	if _, err := architecturekit.Execute(ctx, cached, counterDecider(), increment{subject: subject, By: 1}.onStateRead()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// Another process writes, which the cache does not know about.
	if _, err := architecturekit.Execute(ctx, other, counterDecider(), increment{subject: subject, By: 1}.onStateRead()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	// The cached store reads what was written since, and so decides on the
	// current state.
	if _, err := architecturekit.Execute(ctx, cached, counterDecider(), increment{subject: subject, By: 1}.onStateRead()); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if total := totalIn(t, cached, subject); total != 3 {
		t.Fatalf("got %d, want 3", total)
	}
}

func TestExecuteOnStateReadCombinesWithARevisionOfTheCaller(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	ctx := context.Background()

	written, err := architecturekit.Execute(ctx, store, counterDecider(), increment{subject: subject, By: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	seen := written[0].ID

	byCallerAndOnStateRead := func(eventID string) increment {
		return increment{subject: subject, By: 1}.declaring(
			architecturekit.Require(eventsourcingdb.NewIsSubjectOnEventIDPrecondition(subject, eventID)),
			architecturekit.OnStateRead(),
		)
	}

	written, err = architecturekit.Execute(ctx, store, counterDecider(), byCallerAndOnStateRead(seen))
	if err != nil {
		t.Fatalf("a current revision must be accepted, got %v", err)
	}

	_, err = architecturekit.Execute(ctx, store, counterDecider(), byCallerAndOnStateRead(seen))
	if !errors.Is(err, architecturekit.ErrConflict) {
		t.Fatalf("a stale revision must conflict, got %v", err)
	}

	if _, err = architecturekit.Execute(ctx, store, counterDecider(), byCallerAndOnStateRead(written[0].ID)); err != nil {
		t.Fatalf("a current revision must be accepted, got %v", err)
	}
}

func TestExecuteRejectsInvalidPreconditionsBeforeReading(t *testing.T) {
	// The store can not reach a database, so a transient error would show
	// that Execute has tried to read.
	store := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

	tests := []struct {
		name          string
		preconditions []architecturekit.Precondition
	}{
		{name: "none", preconditions: nil},
		{name: "unconditionally combined with another one", preconditions: []architecturekit.Precondition{
			architecturekit.Unconditionally(),
			architecturekit.OnStateRead(),
		}},
		{name: "a zero value", preconditions: []architecturekit.Precondition{{}}},
		{name: "a requirement of nothing", preconditions: []architecturekit.Precondition{architecturekit.Require(nil)}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
				increment{subject: "/test/invalid", By: 1}.declaring(test.preconditions...))

			if !errors.Is(err, architecturekit.ErrPermanent) {
				t.Fatalf("expected a permanent error, got %v", err)
			}
			if errors.Is(err, architecturekit.ErrTransient) {
				t.Fatalf("nothing may have been read, got %v", err)
			}
		})
	}
}

func TestPreconditionTellsHowItWasMade(t *testing.T) {
	pristine := eventsourcingdb.NewIsSubjectPristinePrecondition("/books/42")

	required := architecturekit.Require(pristine)
	if database, ok := required.Database(); !ok || database != pristine {
		t.Fatalf("got %v, %v", database, ok)
	}
	if required.IsOnStateRead() || required.IsUnconditional() {
		t.Fatal("a requirement is neither OnStateRead nor unconditional")
	}

	if _, ok := architecturekit.OnStateRead().Database(); ok || !architecturekit.OnStateRead().IsOnStateRead() {
		t.Fatal("OnStateRead must only be OnStateRead")
	}
	if _, ok := architecturekit.Unconditionally().Database(); ok || !architecturekit.Unconditionally().IsUnconditional() {
		t.Fatal("Unconditionally must only be unconditional")
	}
}
