package architecturekit_test

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// credits collects what a typed projection hands over for the credited type.
type credits struct {
	envelopes []architecturekit.Envelope[credited]
}

func (c *credits) projection() *architecturekit.TypedProjection {
	return architecturekit.NewProjection().
		On(func(_ context.Context, event architecturekit.Envelope[credited]) error {
			c.envelopes = append(c.envelopes, event)
			return nil
		})
}

func (c *credits) total() int {
	total := 0
	for _, envelope := range c.envelopes {
		total += envelope.Data.Amount
	}

	return total
}

func apply(t *testing.T, projection architecturekit.Projection, events ...eventsourcingdb.Event) error {
	t.Helper()

	for _, event := range events {
		if err := projection.Apply(context.Background(), event); err != nil {
			return err
		}
	}

	return nil
}

func TestTypedProjectionHandsOverTheMetadataAndTheDecodedData(t *testing.T) {
	traceParent := "00-0af7651916cd43dd8448eb211c80319c-b7ad6b7169203331-01"
	traceState := "vendor=value"
	recorded := time.Date(2026, 9, 27, 12, 0, 0, 0, time.UTC)

	target := &credits{}
	err := apply(t, target.projection(), eventsourcingdb.Event{
		ID:          "23",
		Time:        recorded,
		Source:      "https://thenativeweb.io",
		Subject:     "/ledger/1",
		Type:        (credited{}).EventType(),
		TraceParent: &traceParent,
		TraceState:  &traceState,
		Data:        json.RawMessage(`{"amount":42,"currency":"EUR"}`),
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(target.envelopes) != 1 {
		t.Fatalf("got %d events, want 1", len(target.envelopes))
	}

	got := target.envelopes[0]
	want := architecturekit.Envelope[credited]{
		ID:          "23",
		Time:        recorded,
		Source:      "https://thenativeweb.io",
		Subject:     "/ledger/1",
		Type:        (credited{}).EventType(),
		TraceParent: &traceParent,
		TraceState:  &traceState,
		Data:        credited{Amount: 42, Currency: "EUR"},
	}
	if got != want {
		t.Fatalf("got %+v, want %+v", got, want)
	}
}

func TestTypedProjectionHandsEachEventToTheHandlerOfItsType(t *testing.T) {
	var order []string

	projection := architecturekit.NewProjection().
		On(func(_ context.Context, event architecturekit.Envelope[incremented]) error {
			order = append(order, "incremented "+itoa(event.Data.By))
			return nil
		}).
		On(func(_ context.Context, event architecturekit.Envelope[reset]) error {
			order = append(order, "reset")
			return nil
		})

	err := apply(t, projection,
		stored((incremented{}).EventType(), `{"by":2}`),
		stored((reset{}).EventType(), `{}`),
		stored((incremented{}).EventType(), `{"by":5}`),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := strings.Join(order, ", "); got != "incremented 2, reset, incremented 5" {
		t.Fatalf("got %q", got)
	}
}

func TestTypedProjectionSkipsEventsWithoutAHandler(t *testing.T) {
	target := &credits{}

	err := apply(t, target.projection(),
		stored("io.thenativeweb.test.unheard-of", `not even json`),
		stored((credited{}).EventType(), `{"amount":1,"currency":"EUR"}`),
	)
	if err != nil {
		t.Fatalf("an event without a handler must be skipped, got %v", err)
	}

	if target.total() != 1 {
		t.Fatalf("got %d, want 1", target.total())
	}
}

func TestTypedProjectionFailsOnDataThatDoesNotMatch(t *testing.T) {
	target := &credits{}

	err := apply(t, target.projection(),
		stored((credited{}).EventType(), `{"amount":"not a number"}`))

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("got %v", err)
	}
	if !strings.Contains(err.Error(), "decoding") {
		t.Fatalf("got %q", err.Error())
	}
}

func TestTypedProjectionReturnsTheErrorOfAHandlerUnchanged(t *testing.T) {
	errFull := errors.New("the view is full")

	projection := architecturekit.NewProjection().
		On(func(context.Context, architecturekit.Envelope[credited]) error {
			return errFull
		})

	err := apply(t, projection, stored((credited{}).EventType(), `{"amount":1}`))

	if !errors.Is(err, errFull) {
		t.Fatalf("got %v", err)
	}
	if errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("the kit must not categorise the error of a handler, got %v", err)
	}
}

func TestTypedProjectionPanicsOnDuplicateHandler(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a duplicate handler")
		}
	}()

	ignore := func(context.Context, architecturekit.Envelope[credited]) error { return nil }

	architecturekit.NewProjection().On(ignore).On(ignore)
}

func TestTypedProjectionRunsTheUpcasters(t *testing.T) {
	target := &credits{}

	err := apply(t, target.projection().UpcastWith(ledgerUpcasters()),
		stored("io.thenativeweb.test.credited.v1", `{"amount":10}`),
		stored("io.thenativeweb.test.credited.v2", `{"amount":5,"currency":"chf"}`),
		stored("io.thenativeweb.test.credited.v3", `{"amount":1,"currency":"USD"}`),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(target.envelopes) != 3 {
		t.Fatalf("got %d events, want 3", len(target.envelopes))
	}
	if target.total() != 16 {
		t.Fatalf("got %d, want 16", target.total())
	}

	first := target.envelopes[0]
	if first.Type != (credited{}).EventType() || first.Data.Currency != "EUR" {
		t.Fatalf("the v1 event did not arrive in its current shape: %+v", first)
	}
}

func TestTypedProjectionHandlesEveryEventAnUpcasterProduces(t *testing.T) {
	target := &credits{}
	projection := target.projection().UpcastWith(architecturekit.NewUpcasters().
		Upcast("io.thenativeweb.test.credited.batch", splitIntoTwo))

	if err := apply(t, projection, stored("io.thenativeweb.test.credited.batch", `{}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if len(target.envelopes) != 2 || target.total() != 7 {
		t.Fatalf("got %d events with a total of %d, want 2 with 7", len(target.envelopes), target.total())
	}
}

func TestTypedProjectionReportsAFailingUpcaster(t *testing.T) {
	target := &credits{}

	err := apply(t, target.projection().UpcastWith(ledgerUpcasters()),
		stored("io.thenativeweb.test.credited.v1", `not json`))

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("an upcaster failure is permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "upcasting") {
		t.Fatalf("got %q", err.Error())
	}
}

func TestTypedProjectionSeesWhatTheStateSees(t *testing.T) {
	upcasters := ledgerUpcasters()

	state := architecturekit.NewState(0).
		Evolve(func(current int, event credited) int { return current + event.Amount }).
		UpcastWith(upcasters)
	target := &credits{}
	projection := target.projection().UpcastWith(upcasters)

	history := []eventsourcingdb.Event{
		stored("io.thenativeweb.test.credited.v1", `{"amount":10}`),
		stored("io.thenativeweb.test.credited.v2", `{"amount":5,"currency":"chf"}`),
	}

	decided, err := architecturekit.ReplayStored(state, history...)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := apply(t, projection, history...); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if decided != target.total() {
		t.Fatalf("the state sees %d, the projection %d", decided, target.total())
	}
}

func TestTypedProjectionUpcastWithPanicsWhenCalledTwice(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a second set of upcasters")
		}
	}()

	architecturekit.NewProjection().
		UpcastWith(architecturekit.NewUpcasters()).
		UpcastWith(architecturekit.NewUpcasters())
}

func TestTypedProjectionUpcastWithPanicsWithoutASet(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a nil set of upcasters")
		}
	}()

	architecturekit.NewProjection().UpcastWith(nil)
}

// resumableCredits makes a typed projection resumable by embedding it.
type resumableCredits struct {
	*architecturekit.TypedProjection
	checkpoint string
}

func (r *resumableCredits) Checkpoint(context.Context) (string, error) {
	return r.checkpoint, nil
}

func (r *resumableCredits) SaveCheckpoint(_ context.Context, eventID string) error {
	r.checkpoint = eventID
	return nil
}

func TestTypedProjectionIsRebuiltUnlessEmbeddedInAResumableType(t *testing.T) {
	if mode := architecturekit.ModeOf(architecturekit.NewProjection()); mode != architecturekit.ModeRebuild {
		t.Fatalf("got %q, want %q", mode, architecturekit.ModeRebuild)
	}

	target := &credits{}
	resumable := &resumableCredits{TypedProjection: target.projection()}

	if mode := architecturekit.ModeOf(resumable); mode != architecturekit.ModeResumable {
		t.Fatalf("got %q, want %q", mode, architecturekit.ModeResumable)
	}

	if err := apply(t, resumable, stored((credited{}).EventType(), `{"amount":3}`)); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if target.total() != 3 {
		t.Fatalf("the embedded projection did not apply the event, got %d", target.total())
	}
}

func TestCatchUpProjectionWithATypedProjection(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	seed(t, subject, 3)

	var total int
	var subjects []string

	projection := architecturekit.NewProjection().
		On(func(_ context.Context, event architecturekit.Envelope[incremented]) error {
			total += event.Data.By
			subjects = append(subjects, event.Subject)
			return nil
		})

	if err := architecturekit.CatchUpProjection(context.Background(), store, subject, false, projection); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if total != 3 {
		t.Fatalf("got %d, want 3", total)
	}
	for _, got := range subjects {
		if got != subject {
			t.Fatalf("got subject %q, want %q", got, subject)
		}
	}
}
