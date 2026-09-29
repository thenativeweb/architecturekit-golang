package architecturekit_test

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

func TestExecuteUnconditionallyAppendsBlindly(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	ctx := context.Background()

	// Concurrent commands that write unconditionally all succeed. That is what
	// Unconditionally is for, not an accident.
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

func TestRegisterSchemasKeepsTheDatabaseWritable(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)

	// The checked event type must not be the last one: after reading a single
	// event type that further event types follow, EventSourcingDB 1.2.0 stops
	// answering writes. RegisterSchemas must not run into that.
	for _, name := range []string{"a", "b", "c", "d"} {
		_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    "io.thenativeweb.test.writable." + name,
			Data:    map[string]any{},
		}}, nil)
		if err != nil {
			t.Fatalf("failed to seed the stream: %v", err)
		}
	}

	schemas := []architecturekit.EventSchema{{
		EventType: "io.thenativeweb.test.writable.b",
		Schema:    objectSchema(map[string]any{}),
	}}
	for range 2 {
		if err := store.RegisterSchemas(schemas); err != nil {
			t.Fatalf("failed to register the schema: %v", err)
		}
	}

	written := make(chan error, 1)
	go func() {
		_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    "io.thenativeweb.test.writable.e",
			Data:    map[string]any{},
		}}, nil)
		written <- err
	}()

	select {
	case err := <-written:
		if err != nil {
			t.Fatalf("failed to write: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the database stopped answering writes")
	}
}

func TestRegisterSchemasReportsAnUnreachableDatabase(t *testing.T) {
	store := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

	err := store.RegisterSchemas(counterState().Schemas())

	if !errors.Is(err, architecturekit.ErrTransient) {
		t.Fatalf("an unreachable database is transient, got %v", err)
	}
	if !strings.Contains(err.Error(), "reading the registered schemas") {
		t.Fatalf("got %q", err.Error())
	}
}

func TestRegisterSchemasFailsOnASchemaThatIsNotJSON(t *testing.T) {
	store := requireStore(t)
	eventType := "io.thenativeweb.test.notjson"
	notJSON := map[string]any{"type": make(chan int)}

	// Twice within one call, where the schemas are compared with each other.
	err := store.RegisterSchemas(
		[]architecturekit.EventSchema{{EventType: eventType, Schema: objectSchema(map[string]any{})}},
		[]architecturekit.EventSchema{{EventType: eventType, Schema: notJSON}},
	)
	if !errors.Is(err, architecturekit.ErrPermanent) || !strings.Contains(err.Error(), "comparing schemas") {
		t.Fatalf("within one call: got %v", err)
	}

	// Once registered, where the schema is compared with the registered one.
	if err := store.RegisterSchemas([]architecturekit.EventSchema{{
		EventType: eventType,
		Schema:    objectSchema(map[string]any{}),
	}}); err != nil {
		t.Fatalf("failed to register the schema: %v", err)
	}
	err = store.RegisterSchemas([]architecturekit.EventSchema{{EventType: eventType, Schema: notJSON}})
	if !errors.Is(err, architecturekit.ErrPermanent) || !strings.Contains(err.Error(), "comparing schemas") {
		t.Fatalf("against the registered one: got %v", err)
	}
}

func TestRegisterSchemasReportsAFailingReadAfterAConflict(t *testing.T) {
	// The database knows no schema at first, refuses to register one, and then
	// fails when RegisterSchemas reads the schemas again to tell why.
	var reads atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Server", "EventSourcingDB/test")

		switch request.URL.Path {
		case "/api/v1/read-event-types":
			if reads.Add(1) > 1 {
				writer.WriteHeader(http.StatusInternalServerError)
				return
			}
			writer.WriteHeader(http.StatusOK)
		case "/api/v1/register-event-schema":
			writer.WriteHeader(http.StatusConflict)
		default:
			writer.WriteHeader(http.StatusNotFound)
		}
	}))
	defer server.Close()

	serverURL, err := url.Parse(server.URL)
	if err != nil {
		t.Fatal(err)
	}
	client, err := eventsourcingdb.NewClient(serverURL, "secret")
	if err != nil {
		t.Fatal(err)
	}
	store := architecturekit.NewStore(client, "https://thenativeweb.io")

	err = store.RegisterSchemas(counterState().Schemas())

	if !errors.Is(err, architecturekit.ErrTransient) {
		t.Fatalf("a failing read is transient, got %v", err)
	}
	if !strings.Contains(err.Error(), "reading the registered schemas") {
		t.Fatalf("got %q", err.Error())
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

func TestRegisterSchemasFailsOnAChangedSchema(t *testing.T) {
	store := requireStore(t)
	eventType := "io.thenativeweb.test.changed"

	err := store.RegisterSchemas([]architecturekit.EventSchema{{
		EventType: eventType,
		Schema:    objectSchema(map[string]any{"text": map[string]any{"type": "string"}}),
	}})
	if err != nil {
		t.Fatalf("first registration: %v", err)
	}

	// The same event type, now with an additional field.
	err = store.RegisterSchemas([]architecturekit.EventSchema{{
		EventType: eventType,
		Schema: objectSchema(map[string]any{
			"text": map[string]any{"type": "string"},
			"tags": map[string]any{"type": "array"},
		}),
	}})

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("a changed schema is permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), eventType) || !strings.Contains(err.Error(), "differs from the registered one") {
		t.Fatalf("got %q", err.Error())
	}
}

func TestRegisterSchemasFailsIfStoredEventsDoNotMatchTheSchema(t *testing.T) {
	store := requireStore(t)
	eventType := "io.thenativeweb.test.unmatched"

	_, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{{
		Source:  "https://thenativeweb.io",
		Subject: subjectFor(t),
		Type:    eventType,
		Data:    map[string]any{"other": 42},
	}}, nil)
	if err != nil {
		t.Fatalf("failed to seed the stream: %v", err)
	}

	err = store.RegisterSchemas([]architecturekit.EventSchema{{
		EventType: eventType,
		Schema:    objectSchema(map[string]any{"text": map[string]any{"type": "string"}}),
	}})

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("a refused schema is permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "refused the schema of \""+eventType+"\"") {
		t.Fatalf("got %q", err.Error())
	}
	// The reason from the database names what does not match.
	if !strings.Contains(err.Error(), "additionalProperties 'other' not allowed") {
		t.Fatalf("error should carry the reason from the database, got %q", err.Error())
	}
}

func TestRegisterSchemasFailsOnAMissingSchema(t *testing.T) {
	// The schema is checked before the database is contacted.
	store := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

	err := store.RegisterSchemas([]architecturekit.EventSchema{{
		EventType: "io.thenativeweb.test.schemaless",
	}})

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("a missing schema is permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), `"io.thenativeweb.test.schemaless" has no schema`) {
		t.Fatalf("got %q", err.Error())
	}
}

func TestRegisterSchemasFailsOnTwoSchemasForOneEventType(t *testing.T) {
	store := requireStore(t)
	eventType := "io.thenativeweb.test.twofold"

	err := store.RegisterSchemas(
		[]architecturekit.EventSchema{{
			EventType: eventType,
			Schema:    objectSchema(map[string]any{}),
		}},
		[]architecturekit.EventSchema{{
			EventType: eventType,
			Schema:    objectSchema(map[string]any{"text": map[string]any{"type": "string"}}),
		}},
	)

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("two schemas for one event type are permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "two different schemas") {
		t.Fatalf("got %q", err.Error())
	}
}

func TestExecuteReportsASchemaViolationAsPermanent(t *testing.T) {
	store := requireStore(t)

	err := store.RegisterSchemas([]architecturekit.EventSchema{{
		EventType: (labelled{}).EventType(),
		Schema:    (labelled{}).Schema(),
	}})
	if err != nil {
		t.Fatalf("failed to register the schema: %v", err)
	}

	_, err = architecturekit.Execute(context.Background(), store, noteDecider(),
		annotate{subject: subjectFor(t), event: labelled{Label: ""}})

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("a schema violation is permanent, got %v", err)
	}
	if errors.Is(err, architecturekit.ErrConflict) {
		t.Fatalf("a schema violation is not a conflict: %v", err)
	}
	if !strings.Contains(err.Error(), "does not match schema") {
		t.Fatalf("error should carry the reason from the database, got %q", err.Error())
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
	state.UpcastWith(architecturekit.NewUpcasters().
		Upcast("io.thenativeweb.test.outdated",
			func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
				return nil, errors.New("this one cannot be migrated")
			}))

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
