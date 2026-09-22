package architecturekit_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

func TestExecuteWritesEventsAndReturnsThem(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)

	written, err := architecturekit.Execute(context.Background(), store, counterDecider(),
		increment{subject: subject, By: 3})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(written) != 1 {
		t.Fatalf("expected one written event, got %d", len(written))
	}
	if written[0].ID == "" {
		t.Fatal("a written event must carry its ID")
	}
	if written[0].Type != (incremented{}).EventType() {
		t.Fatalf("got %q", written[0].Type)
	}
	if total := totalIn(t, store, subject); total != 3 {
		t.Fatalf("got %d, want 3", total)
	}
}

func TestExecuteAppendsToExistingSubject(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	ctx := context.Background()

	for _, by := range []int{1, 2, 4} {
		if _, err := architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: by}); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}

	if total := totalIn(t, store, subject); total != 7 {
		t.Fatalf("got %d, want 7", total)
	}
}

func TestExecuteReturnsDomainErrorWithoutWriting(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	ctx := context.Background()

	if _, err := architecturekit.Execute(ctx, store, counterDecider(),
		increment{subject: subject, By: 5, Limit: 10}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err := architecturekit.Execute(ctx, store, counterDecider(),
		increment{subject: subject, By: 8, Limit: 10})

	var domainError *architecturekit.DomainError
	if !errors.As(err, &domainError) {
		t.Fatalf("expected a domain error, got %v", err)
	}
	if !errors.Is(err, architecturekit.ErrDomain) {
		t.Fatal("a domain error must also be an ErrDomain")
	}
	if total := totalIn(t, store, subject); total != 5 {
		t.Fatalf("a rejected command must not write; got %d, want 5", total)
	}
}

func TestExecuteWritesNothingWhenDecideReturnsNoEvents(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)

	written, err := architecturekit.Execute(context.Background(), store, counterDecider(),
		increment{subject: subject, By: 0})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if written != nil {
		t.Fatalf("expected no written events, got %d", len(written))
	}
	if total := totalIn(t, store, subject); total != 0 {
		t.Fatalf("got %d, want 0", total)
	}
}

func TestExecuteWithoutPreconditionsAppendsBlindly(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	ctx := context.Background()

	// The kit adds no preconditions, so concurrent commands that declare none
	// all succeed. That is the documented consequence, not an accident.
	const concurrent = 4
	var waitGroup sync.WaitGroup
	for range concurrent {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, _ = architecturekit.Execute(ctx, store, counterDecider(),
				increment{subject: subject, By: 1})
		}()
	}
	waitGroup.Wait()

	if total := totalIn(t, store, subject); total != concurrent {
		t.Fatalf("got %d, want %d", total, concurrent)
	}
}

func TestExecuteWithPristinePreconditionAdmitsOnlyOne(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	ctx := context.Background()

	const concurrent = 8
	var waitGroup sync.WaitGroup
	results := make([]error, concurrent)

	for i := range concurrent {
		waitGroup.Add(1)
		go func() {
			defer waitGroup.Done()
			_, results[i] = architecturekit.Execute(ctx, store, counterDecider(),
				increment{subject: subject, By: 1}.pristine())
		}()
	}
	waitGroup.Wait()

	succeeded, conflicted := 0, 0
	for i, err := range results {
		switch {
		case err == nil:
			succeeded++
		case errors.Is(err, architecturekit.ErrConflict):
			conflicted++
		default:
			t.Fatalf("goroutine %d failed unexpectedly: %v", i, err)
		}
	}

	if succeeded != 1 {
		t.Fatalf("exactly one command must succeed, got %d", succeeded)
	}
	if conflicted != concurrent-1 {
		t.Fatalf("the others must conflict, got %d", conflicted)
	}
	if total := totalIn(t, store, subject); total != 1 {
		t.Fatalf("got %d, want 1", total)
	}
}

func TestExecuteWithRevisionPreconditionDetectsStaleReads(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	ctx := context.Background()

	written, err := architecturekit.Execute(ctx, store, counterDecider(),
		increment{subject: subject, By: 1})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	stale := written[0].ID

	// Someone else writes in between, so the caller's view is out of date.
	if _, err := architecturekit.Execute(ctx, store, counterDecider(),
		increment{subject: subject, By: 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = architecturekit.Execute(ctx, store, counterDecider(),
		increment{subject: subject, By: 1}.onEventID(stale))

	if !errors.Is(err, architecturekit.ErrConflict) {
		t.Fatalf("expected a conflict, got %v", err)
	}
	if !errors.Is(err, architecturekit.ErrTransient) {
		t.Fatal("a conflict must also be transient")
	}
}

func TestConflictIsReportedNotRetried(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)

	// A pristine precondition on a populated subject can never hold, so this
	// shows that the kit reports instead of trying again.
	if _, err := architecturekit.Execute(context.Background(), store, counterDecider(),
		increment{subject: subject, By: 1}); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
		increment{subject: subject, By: 1}.pristine())

	if !errors.Is(err, architecturekit.ErrConflict) {
		t.Fatalf("expected a conflict, got %v", err)
	}
	if total := totalIn(t, store, subject); total != 1 {
		t.Fatalf("nothing more may have been written; got %d", total)
	}
}

func TestExecuteFailsOnEventTypeWithoutRule(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)

	_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
		Source:  "https://thenativeweb.io",
		Subject: subject,
		Type:    "io.thenativeweb.test.unexpected",
		Data:    map[string]any{"x": 1},
	}}, nil)
	if err != nil {
		t.Fatalf("failed to seed the stream: %v", err)
	}

	_, err = architecturekit.Execute(context.Background(), store, counterDecider(),
		increment{subject: subject, By: 1})

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("a missing rule is permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "io.thenativeweb.test.unexpected") {
		t.Fatalf("error should name the event type, got %q", err.Error())
	}
}

func TestRegisterSchemasIsIdempotent(t *testing.T) {
	store := requireStore(t)

	if err := store.RegisterSchemas(counterState().Schemas(), counterState().Schemas()); err != nil {
		t.Fatalf("duplicates within one call: %v", err)
	}
	if err := store.RegisterSchemas(counterState().Schemas()); err != nil {
		t.Fatalf("second call: %v", err)
	}
}

func TestExecuteReportsAnUnreachableDatabase(t *testing.T) {
	brokenStore := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

	_, err := architecturekit.Execute(context.Background(), brokenStore, counterDecider(),
		increment{subject: "/test/unreachable", By: 1})

	if !errors.Is(err, architecturekit.ErrTransient) {
		t.Fatalf("an unreachable database is transient, got %v", err)
	}
	if !strings.Contains(err.Error(), "reading") {
		t.Fatalf("got %q", err.Error())
	}
}

func TestExecuteReportsTechnicalWriteFailures(t *testing.T) {
	// The source has to be a valid URI, otherwise the database rejects the
	// write. Reading still works, so this reaches the write path.
	brokenStore := architecturekit.NewStore(rawClient(t), "not a valid uri")

	_, err := architecturekit.Execute(context.Background(), brokenStore, counterDecider(),
		increment{subject: subjectFor(t), By: 1})

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("an invalid source is permanent, got %v", err)
	}
	if errors.Is(err, architecturekit.ErrConflict) {
		t.Fatalf("an invalid source is not a conflict: %v", err)
	}
}

func TestExecuteFailsWhenStoredDataDoesNotMatchTheRule(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)

	// The annotated type has no schema, so the database lets a mismatching
	// field type through, and the rule trips over it when reading.
	_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
		Source:  "https://thenativeweb.io",
		Subject: subject,
		Type:    (annotated{}).EventType(),
		Data:    map[string]any{"note": 42},
	}}, nil)
	if err != nil {
		t.Fatalf("failed to seed the stream: %v", err)
	}

	_, err = architecturekit.Execute(context.Background(), store, noteDecider(),
		annotate{subject: subject, event: annotated{Note: "hello"}})

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "decoding") {
		t.Fatalf("got %q", err.Error())
	}
}

func TestRegisterSchemasFailsOnAnInvalidSchema(t *testing.T) {
	store := requireStore(t)

	err := store.RegisterSchemas([]architecturekit.EventSchema{{
		EventType: "io.thenativeweb.test.invalid",
		Schema:    map[string]any{"type": "this-is-not-a-json-schema-type"},
	}})

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("an invalid schema is permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "io.thenativeweb.test.invalid") {
		t.Fatalf("error should name the event type, got %q", err.Error())
	}
}

func TestExecuteReportsAFailingUpcaster(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)

	// An event of the old type is in the stream, and its upcaster refuses.
	_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
		Source:  "https://thenativeweb.io",
		Subject: subject,
		Type:    "io.thenativeweb.test.outdated",
		Data:    map[string]any{"whatever": true},
	}}, nil)
	if err != nil {
		t.Fatalf("failed to seed the stream: %v", err)
	}

	state := architecturekit.NewState(counter{})
	state.Evolve(func(current counter, event incremented) counter { return current })
	state.Upcast("io.thenativeweb.test.outdated",
		func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
			return nil, errors.New("this one cannot be migrated")
		})

	decider := architecturekit.Decider[increment, counter]{
		State: state,
		Decide: func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
			return []architecturekit.Event{incremented{By: 1}}, nil
		},
	}

	_, err = architecturekit.Execute(context.Background(), store, decider,
		increment{subject: subject, By: 1})

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("a failing upcaster is permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "cannot be migrated") {
		t.Fatalf("got %q", err.Error())
	}
}
