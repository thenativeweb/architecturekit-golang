package architecturekit_test

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// collector records the IDs it saw. Its other capabilities are switched on by
// embedding, so that one type can stand for all three modes.
type collector struct {
	mutex sync.Mutex
	seen  []string
}

func (c *collector) Apply(_ context.Context, event eventsourcingdb.Event) error {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	c.seen = append(c.seen, event.ID)

	return nil
}

func (c *collector) IDs() []string {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	ids := make([]string, len(c.seen))
	copy(ids, c.seen)

	return ids
}

// resumingCollector remembers a checkpoint, as a projection with a separate
// store would.
type resumingCollector struct {
	collector
	checkpoint string
}

func (c *resumingCollector) Checkpoint(context.Context) (string, error) {
	return c.checkpoint, nil
}

func (c *resumingCollector) SaveCheckpoint(_ context.Context, eventID string) error {
	c.checkpoint = eventID
	return nil
}

func seed(t *testing.T, subject string, count int) {
	t.Helper()

	candidates := make([]eventsourcingdb.EventCandidate, count)
	for i := range count {
		candidates[i] = eventsourcingdb.EventCandidate{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    (incremented{}).EventType(),
			Data:    incremented{By: 1},
		}
	}

	if _, err := rawClient(t).WriteEvents(candidates, nil); err != nil {
		t.Fatalf("failed to seed %q: %v", subject, err)
	}
}

func TestCatchUpProjectionAppliesWhatIsStored(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	seed(t, subject, 3)

	target := &collector{}
	if err := architecturekit.CatchUpProjection(context.Background(), store, subject, false, target); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(target.IDs()); got != 3 {
		t.Fatalf("got %d events, want 3: %v", got, target.IDs())
	}
}

func TestCatchUpProjectionReadsRecursively(t *testing.T) {
	store := requireStore(t)
	base := subjectFor(t)
	seed(t, base+"/a", 2)
	seed(t, base+"/b", 1)

	target := &collector{}
	if err := architecturekit.CatchUpProjection(context.Background(), store, base, true, target); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(target.IDs()); got != 3 {
		t.Fatalf("got %d events across both subjects, want 3", got)
	}
}

func TestCatchUpProjectionResumesFromItsCheckpoint(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	seed(t, subject, 3)

	first := &resumingCollector{}
	if err := architecturekit.CatchUpProjection(context.Background(), store, subject, false, first); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := len(first.IDs()); got != 3 {
		t.Fatalf("got %d, want 3", got)
	}

	// More events arrive, and a second run starts where the first stopped.
	seed(t, subject, 2)

	second := &resumingCollector{checkpoint: first.checkpoint}
	if err := architecturekit.CatchUpProjection(context.Background(), store, subject, false, second); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(second.IDs()); got != 2 {
		t.Fatalf("a resumed run must only see the new events, got %d: %v", got, second.IDs())
	}
}

func TestCatchUpProjectionWithNothingToDo(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)

	target := &resumingCollector{}
	if err := architecturekit.CatchUpProjection(context.Background(), store, subject, false, target); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if got := len(target.IDs()); got != 0 {
		t.Fatalf("got %d, want none", got)
	}
	if target.checkpoint != "" {
		t.Fatalf("nothing was applied, so no checkpoint: %q", target.checkpoint)
	}
}

func TestRunProjectionFollowsTheStreamAndEndsWithItsContext(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	seed(t, subject, 1)

	target := &collector{}
	ctx, stop := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() { done <- architecturekit.RunProjection(ctx, store, subject, false, target) }()

	// Wait until the catch-up phase has arrived, then write again and watch
	// the live phase pick it up.
	waitFor(t, func() bool { return len(target.IDs()) == 1 })
	seed(t, subject, 1)
	waitFor(t, func() bool { return len(target.IDs()) == 2 })

	stop()

	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("ending through the context is not a failure, got %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("RunProjection did not return after its context ended")
	}
}

func TestRunProjectionReportsAnUnreachableDatabase(t *testing.T) {
	brokenStore := architecturekit.NewStore(deadClient(t), "https://thenativeweb.io")

	err := architecturekit.RunProjection(context.Background(), brokenStore, "/nowhere", false, &collector{})

	if !errors.Is(err, architecturekit.ErrTransient) {
		t.Fatalf("an unreachable database is transient, got %v", err)
	}
}

func TestCatchUpProjectionReportsAFailingApply(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	seed(t, subject, 1)

	err := architecturekit.CatchUpProjection(context.Background(), store, subject, false, &refusingCollector{})

	if err == nil {
		t.Fatal("expected the error from Apply")
	}
	if !strings.Contains(err.Error(), "not today") {
		t.Fatalf("got %q", err.Error())
	}
}

type refusingCollector struct{}

func (refusingCollector) Apply(context.Context, eventsourcingdb.Event) error {
	return errors.New("not today")
}

func waitFor(t *testing.T, condition func() bool) {
	t.Helper()

	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	t.Fatal("condition was not met in time")
}

func TestCatchUpProjectionStopsWhenTheCheckpointCannotBeRead(t *testing.T) {
	store := requireStore(t)
	subject := subjectFor(t)
	seed(t, subject, 1)

	err := architecturekit.CatchUpProjection(context.Background(), store, subject, false,
		&unreadableCheckpoint{})

	if err == nil {
		t.Fatal("expected the error from Checkpoint")
	}
	if !strings.Contains(err.Error(), "checkpoint is gone") {
		t.Fatalf("got %q", err.Error())
	}
}

// unreadableCheckpoint cannot tell where it stopped, so nothing may be read.
type unreadableCheckpoint struct{ collector }

func (*unreadableCheckpoint) Checkpoint(context.Context) (string, error) {
	return "", errors.New("checkpoint is gone")
}

func (*unreadableCheckpoint) SaveCheckpoint(context.Context, string) error { return nil }
