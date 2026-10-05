package architecturekittest

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// StoredEvent turns a typed event into the shape the database hands back, so
// that a projection can be driven without one.
func StoredEvent(subject, eventID string, event architecturekit.Event) eventsourcingdb.Event {
	data, err := json.Marshal(event)
	if err != nil {
		// A typed event that cannot be marshalled would never reach a
		// database either, so this is a mistake in the test itself.
		panic(fmt.Sprintf("architecturekittest: cannot marshal %q: %v", event.EventType(), err))
	}

	return eventsourcingdb.Event{
		Source:          "https://architecturekit.test",
		Subject:         subject,
		Type:            event.EventType(),
		ID:              eventID,
		SpecVersion:     "1.0",
		DataContentType: "application/json",
		Data:            data,
	}
}

// StoredEvents turns typed events into stored ones for the same subject and
// numbers them from zero, the way the database would.
func StoredEvents(subject string, events ...architecturekit.Event) []eventsourcingdb.Event {
	stored := make([]eventsourcingdb.Event, len(events))
	for i, event := range events {
		stored[i] = StoredEvent(subject, strconv.Itoa(i), event)
	}

	return stored
}

// StoredEventsAt turns typed events into stored ones for the same subject, the
// way the database continues a history: numbered from firstID on, and written
// one minute apart from the given time on. Use it where a projection reads the
// time of an event, or where the events of several subjects have to follow
// one another, as they do in the database.
func StoredEventsAt(subject string, firstID int, at time.Time, events ...architecturekit.Event) []eventsourcingdb.Event {
	stored := make([]eventsourcingdb.Event, len(events))
	for i, event := range events {
		stored[i] = StoredEvent(subject, strconv.Itoa(firstID+i), event)
		stored[i].Time = at.Add(time.Duration(i) * time.Minute)
	}

	return stored
}

// Project hands the events to the projection, one after another, and fails the
// test on the first refusal.
//
// A projection that is transactional as well fails the test, because its Apply
// is not what runs in production; use ProjectTransactional for it.
func Project(t testing.TB, projection architecturekit.Projection, events ...eventsourcingdb.Event) {
	t.Helper()

	if _, ok := projection.(architecturekit.Transactional); ok {
		t.Fatalf("projection %T is transactional, use ProjectTransactional", projection)
		return
	}

	for i, event := range events {
		if err := projection.Apply(context.Background(), event); err != nil {
			t.Fatalf("projecting event %d of type %q: %v", i, event.Type, err)
			return
		}
	}
}

// ProjectTransactional hands the events to a transactional projection within
// one transaction, the way StartTransactionalProjection does for a batch. It
// commits with the ID of the last event, and rolls back and fails the test on
// the first refusal. Without events, no transaction is begun.
func ProjectTransactional(t testing.TB, projection architecturekit.Transactional, events ...eventsourcingdb.Event) {
	t.Helper()

	if len(events) == 0 {
		return
	}

	ctx := context.Background()

	tx, err := projection.Begin(ctx)
	if err != nil {
		t.Fatalf("beginning a transaction: %v", err)
		return
	}

	for i, event := range events {
		if err := tx.Apply(ctx, event); err != nil {
			// A failing rollback is reported alongside, never instead of, the
			// failure that caused it.
			t.Fatalf("projecting event %d of type %q: %v",
				i, event.Type, errors.Join(err, tx.Rollback(ctx)))
			return
		}
	}

	if err := tx.Commit(ctx, events[len(events)-1].ID); err != nil {
		t.Fatalf("committing the transaction: %v", err)
	}
}

// ItemsOf reads a view, which is what a query would do before filtering. A
// view that fails while it is read fails the test, also after some of the
// items.
func ItemsOf[TItem any](t testing.TB, view architecturekit.View[TItem]) []TItem {
	t.Helper()

	var items []TItem
	for item, err := range view.All(context.Background()) {
		if err != nil {
			t.Fatalf("reading the view failed after %d item(s): %v", len(items), err)
			return nil
		}
		items = append(items, item)
	}

	return items
}

// ExpectMode checks which mode the kit will drive this projection in.
//
// Worth asserting: the modes come from optional interfaces, so a typo in a
// method signature leaves one unfulfilled and the projection silently falls
// back to being rebuilt on every start.
func ExpectMode(t testing.TB, projection architecturekit.Projection, want architecturekit.Mode) {
	t.Helper()

	if got := architecturekit.ModeOf(projection); got != want {
		t.Fatalf("projection %T runs in %q mode, want %q", projection, got, want)
	}
}

// ExpectItems expects the view to hold exactly these items, in this order.
//
// It compares the items by value, with reflect.DeepEqual, so that an item may
// hold slices, maps, or pointers, and two items are equal if what they hold
// is. A nil slice or map is not equal to an empty one, though.
func ExpectItems[TItem any](t testing.TB, view architecturekit.View[TItem], expected ...TItem) {
	t.Helper()

	items := ItemsOf(t, view)

	if len(items) != len(expected) {
		t.Fatalf("expected %d item(s), got %d: %v", len(expected), len(items), items)
		return
	}

	for i := range expected {
		if !reflect.DeepEqual(items[i], expected[i]) {
			t.Fatalf("item %d: got %+v, want %+v", i, items[i], expected[i])
			return
		}
	}
}
