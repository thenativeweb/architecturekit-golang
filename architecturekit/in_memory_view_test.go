package architecturekit_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// book is an item with a key, a value to index, and a field for its revision.
type book struct {
	ID       string
	Shelf    string
	Title    string
	Revision string
}

func bookView() *architecturekit.InMemoryView[string, book] {
	return architecturekit.NewInMemoryView(
		func(item book) string { return item.ID },
		architecturekit.RevisionIn(func(item *book) *string { return &item.Revision }),
	)
}

func mustInsert(t *testing.T, view *architecturekit.InMemoryView[string, book], eventID string, item book) {
	t.Helper()

	if err := view.Insert(context.Background(), eventID, item); err != nil {
		t.Fatalf("failed to insert %+v: %v", item, err)
	}
}

func mustGet(t *testing.T, view *architecturekit.InMemoryView[string, book], id string) book {
	t.Helper()

	item, isFound, err := view.Get(context.Background(), id)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !isFound {
		t.Fatalf("book %q is missing", id)
	}

	return item
}

func idsIn(t *testing.T, view architecturekit.View[book]) []string {
	t.Helper()

	items, err := view.All(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var ids []string
	for item := range items {
		ids = append(ids, item.ID)
	}

	return ids
}

func TestInMemoryViewInsertsAndGetsItems(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "1", book{ID: "42", Title: "2001"})

	got := mustGet(t, view, "42")
	if got.Title != "2001" || got.Revision != "1" {
		t.Fatalf("got %+v, want the title and the revision of the event", got)
	}

	_, isFound, err := view.Get(context.Background(), "23")
	if err != nil || isFound {
		t.Fatalf("got %v and %v for a missing book", isFound, err)
	}
}

func TestInMemoryViewHandsOutItemsInTheOrderOfInsertion(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "1", book{ID: "a"})
	mustInsert(t, view, "2", book{ID: "b"})
	mustInsert(t, view, "3", book{ID: "c"})

	if _, err := view.Delete(context.Background(), "b", "4"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	mustInsert(t, view, "5", book{ID: "b"})

	// A key that was deleted and inserted again moves to the end.
	if ids := idsIn(t, view); !slices.Equal(ids, []string{"a", "c", "b"}) {
		t.Fatalf("got %v, want a, c, and b", ids)
	}
}

func TestInMemoryViewHandsOutACopy(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "1", book{ID: "a"})

	items, _ := view.All(context.Background())
	mustInsert(t, view, "2", book{ID: "b"})

	// Writing while a query holds its result must not change what it holds.
	if collected := slices.Collect(items); len(collected) != 1 {
		t.Fatalf("a query result must not change underneath: %v", collected)
	}
}

func TestInMemoryViewSkipsAnInsertThatIsNotNewer(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "5", book{ID: "42", Title: "first"})

	// The same event, applied a second time, and an older one.
	mustInsert(t, view, "5", book{ID: "42", Title: "again"})
	mustInsert(t, view, "3", book{ID: "42", Title: "older"})

	if got := mustGet(t, view, "42"); got.Title != "first" || got.Revision != "5" {
		t.Fatalf("got %+v, want the first book unchanged", got)
	}
}

func TestInMemoryViewRefusesASecondItemWithTheSameKey(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "1", book{ID: "42"})

	err := view.Insert(context.Background(), "2", book{ID: "42"})

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("a taken key is permanent, got %v", err)
	}
	if !strings.Contains(err.Error(), "42") || !strings.Contains(err.Error(), "event 2") {
		t.Fatalf("the error has to name the key and the event, got %q", err.Error())
	}
}

func TestInMemoryViewUpdatesAnItem(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "1", book{ID: "42", Title: "draft"})

	isChanged, err := view.Update(context.Background(), "42", "2", func(item *book) {
		item.Title = "final"
		item.Revision = "made up by the handler"
	})
	if err != nil || !isChanged {
		t.Fatalf("got %v and %v, want the book changed", isChanged, err)
	}

	// The view sets the revision, whatever the handler wrote.
	if got := mustGet(t, view, "42"); got.Title != "final" || got.Revision != "2" {
		t.Fatalf("got %+v", got)
	}
}

func TestInMemoryViewSkipsAnUpdateThatIsNotNewer(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "2", book{ID: "42", Title: "current"})

	isChanged, err := view.Update(context.Background(), "42", "2", func(item *book) { item.Title = "replayed" })
	if err != nil || isChanged {
		t.Fatalf("got %v and %v, want nothing changed", isChanged, err)
	}

	if got := mustGet(t, view, "42"); got.Title != "current" {
		t.Fatalf("got %+v", got)
	}
}

func TestInMemoryViewChangesNothingForAMissingItem(t *testing.T) {
	view := bookView()

	isChanged, err := view.Update(context.Background(), "42", "1", func(item *book) { item.Title = "x" })
	if err != nil || isChanged {
		t.Fatalf("updating a missing book: got %v and %v", isChanged, err)
	}

	isRemoved, err := view.Delete(context.Background(), "42", "1")
	if err != nil || isRemoved {
		t.Fatalf("deleting a missing book: got %v and %v", isRemoved, err)
	}
}

func TestInMemoryViewRefusesToChangeTheKey(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "1", book{ID: "42", Title: "kept"})

	_, err := view.Update(context.Background(), "42", "2", func(item *book) {
		item.ID = "23"
		item.Title = "moved"
	})

	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("changing the key is permanent, got %v", err)
	}
	if got := mustGet(t, view, "42"); got.Title != "kept" {
		t.Fatalf("a refused change must not be kept, got %+v", got)
	}
}

func TestInMemoryViewUpserts(t *testing.T) {
	view := bookView()
	rename := func(item *book) { item.Title = "renamed" }

	if err := view.Upsert(context.Background(), "1", book{ID: "42", Title: "new"}, rename); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mustGet(t, view, "42"); got.Title != "new" || got.Revision != "1" {
		t.Fatalf("a missing book is inserted as given, got %+v", got)
	}

	if err := view.Upsert(context.Background(), "2", book{ID: "42", Title: "ignored"}, rename); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mustGet(t, view, "42"); got.Title != "renamed" || got.Revision != "2" {
		t.Fatalf("an existing book is changed, got %+v", got)
	}

	if err := view.Upsert(context.Background(), "2", book{ID: "42"}, func(item *book) { item.Title = "replayed" }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := mustGet(t, view, "42"); got.Title != "renamed" {
		t.Fatalf("an event that is not newer is skipped, got %+v", got)
	}
}

func TestInMemoryViewDeletesAnItem(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "5", book{ID: "42"})

	isRemoved, err := view.Delete(context.Background(), "42", "3")
	if err != nil || isRemoved {
		t.Fatalf("an older event must not delete, got %v and %v", isRemoved, err)
	}

	isRemoved, err = view.Delete(context.Background(), "42", "6")
	if err != nil || !isRemoved {
		t.Fatalf("got %v and %v, want the book removed", isRemoved, err)
	}

	if _, isFound, _ := view.Get(context.Background(), "42"); isFound {
		t.Fatal("the book is still there")
	}
}

func TestInMemoryViewUpdatesAndDeletesWhereItemsMatch(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})
	mustInsert(t, view, "2", book{ID: "b", Shelf: "right"})
	mustInsert(t, view, "9", book{ID: "c", Shelf: "left"})

	onTheLeft := func(item book) bool { return item.Shelf == "left" }

	// c has already seen a newer event, so only a is changed.
	changed, err := view.UpdateWhere(context.Background(), onTheLeft, "5", func(item *book) { item.Title = "left" })
	if err != nil || changed != 1 {
		t.Fatalf("got %d and %v, want 1 changed", changed, err)
	}
	if got := mustGet(t, view, "a"); got.Title != "left" || got.Revision != "5" {
		t.Fatalf("got %+v", got)
	}

	removed, err := view.DeleteWhere(context.Background(), onTheLeft, "10")
	if err != nil || removed != 2 {
		t.Fatalf("got %d and %v, want 2 removed", removed, err)
	}
	if ids := idsIn(t, view); !slices.Equal(ids, []string{"b"}) {
		t.Fatalf("got %v, want b only", ids)
	}
}

func TestInMemoryViewKeepsRevisionsWithoutAField(t *testing.T) {
	view := architecturekit.NewInMemoryView(func(item book) string { return item.ID })
	mustInsert(t, view, "5", book{ID: "42", Title: "current"})

	isChanged, err := view.Update(context.Background(), "42", "5", func(item *book) { item.Title = "replayed" })
	if err != nil || isChanged {
		t.Fatalf("got %v and %v, want the replayed event skipped", isChanged, err)
	}
	if got := mustGet(t, view, "42"); got.Revision != "" {
		t.Fatalf("without a field, the item carries no revision, got %+v", got)
	}
}

func TestInMemoryViewRefusesAnEmptyEventID(t *testing.T) {
	view := bookView()
	byShelf := view.Index(func(item book) string { return item.Shelf })
	ctx := context.Background()
	anything := func(book) bool { return true }
	noChange := func(*book) {}

	operations := map[string]func() error{
		"Insert": func() error { return view.Insert(ctx, "", book{ID: "42"}) },
		"Upsert": func() error { return view.Upsert(ctx, "", book{ID: "42"}, noChange) },
		"Update": func() error { _, err := view.Update(ctx, "42", "", noChange); return err },
		"Delete": func() error { _, err := view.Delete(ctx, "42", ""); return err },
		"UpdateWhere": func() error {
			_, err := view.UpdateWhere(ctx, anything, "", noChange)
			return err
		},
		"DeleteWhere":  func() error { _, err := view.DeleteWhere(ctx, anything, ""); return err },
		"Index.Update": func() error { _, err := byShelf.Update(ctx, "left", "", noChange); return err },
		"Index.Delete": func() error { _, err := byShelf.Delete(ctx, "left", ""); return err },
	}

	for name, operation := range operations {
		if err := operation(); !errors.Is(err, architecturekit.ErrPermanent) {
			t.Errorf("%s: got %v, want ErrPermanent", name, err)
		}
	}
}

func TestInMemoryViewRefusesAnEventIDThatIsNotARevision(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "1", book{ID: "42"})

	err := view.Insert(context.Background(), "not a revision", book{ID: "42"})

	if !errors.Is(err, architecturekit.ErrPermanent) || !errors.Is(err, architecturekit.ErrNotARevision) {
		t.Fatalf("got %v, want ErrPermanent and ErrNotARevision", err)
	}
}

func TestInMemoryViewReportsAFailureAmongSeveralItems(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})
	mustInsert(t, view, "2", book{ID: "b", Shelf: "left"})
	onTheLeft := func(item book) bool { return item.Shelf == "left" }

	_, err := view.UpdateWhere(context.Background(), onTheLeft, "3", func(item *book) { item.ID = "moved" })
	if !errors.Is(err, architecturekit.ErrPermanent) {
		t.Fatalf("got %v, want the refused change of a key", err)
	}

	_, err = view.DeleteWhere(context.Background(), onTheLeft, "not a revision")
	if !errors.Is(err, architecturekit.ErrNotARevision) {
		t.Fatalf("got %v, want ErrNotARevision", err)
	}
}

func TestInMemoryViewStaysInOrderWhenManyItemsAreDeleted(t *testing.T) {
	view := bookView()
	for i := range 40 {
		mustInsert(t, view, strconv.Itoa(i), book{ID: fmt.Sprintf("book-%02d", i)})
	}

	// Deleting most items makes the view drop them from its order.
	for i := range 30 {
		if _, err := view.Delete(context.Background(), fmt.Sprintf("book-%02d", i), "100"); err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	mustInsert(t, view, "101", book{ID: "book-00"})

	want := []string{"book-30", "book-31", "book-32", "book-33", "book-34",
		"book-35", "book-36", "book-37", "book-38", "book-39", "book-00"}
	if ids := idsIn(t, view); !slices.Equal(ids, want) {
		t.Fatalf("got %v, want %v", ids, want)
	}
}

func TestInMemoryIndexLooksUpItemsByValue(t *testing.T) {
	view := bookView()
	mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})

	// An index that is added later knows the items that are already there.
	byShelf := view.Index(func(item book) string { return item.Shelf })

	mustInsert(t, view, "2", book{ID: "b", Shelf: "right"})
	mustInsert(t, view, "3", book{ID: "c", Shelf: "left"})

	if ids := lookedUp(t, byShelf, "left"); !slices.Equal(ids, []string{"a", "c"}) {
		t.Fatalf("got %v, want a and c", ids)
	}
	if ids := lookedUp(t, byShelf, "nowhere"); len(ids) != 0 {
		t.Fatalf("got %v, want nothing", ids)
	}
}

func lookedUp(t *testing.T, index *architecturekit.InMemoryIndex[string, book, string], shelf string) []string {
	t.Helper()

	items, err := index.Lookup(context.Background(), shelf)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var ids []string
	for item := range items {
		ids = append(ids, item.ID)
	}

	return ids
}

func TestInMemoryIndexFollowsAChangedValue(t *testing.T) {
	view := bookView()
	byShelf := view.Index(func(item book) string { return item.Shelf })
	mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})

	if _, err := view.Update(context.Background(), "a", "2", func(item *book) { item.Shelf = "right" }); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if ids := lookedUp(t, byShelf, "left"); len(ids) != 0 {
		t.Fatalf("got %v on the left, want nothing", ids)
	}
	if ids := lookedUp(t, byShelf, "right"); !slices.Equal(ids, []string{"a"}) {
		t.Fatalf("got %v on the right, want a", ids)
	}
}

func TestInMemoryIndexUpdatesAndDeletesByValue(t *testing.T) {
	view := bookView()
	byShelf := view.Index(func(item book) string { return item.Shelf })
	mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})
	mustInsert(t, view, "2", book{ID: "b", Shelf: "right"})
	mustInsert(t, view, "9", book{ID: "c", Shelf: "left"})

	changed, err := byShelf.Update(context.Background(), "left", "5", func(item *book) { item.Title = "left" })
	if err != nil || changed != 1 {
		t.Fatalf("got %d and %v, want only a changed", changed, err)
	}

	removed, err := byShelf.Delete(context.Background(), "left", "10")
	if err != nil || removed != 2 {
		t.Fatalf("got %d and %v, want 2 removed", removed, err)
	}

	if ids := lookedUp(t, byShelf, "left"); len(ids) != 0 {
		t.Fatalf("got %v on the left, want nothing", ids)
	}
	if ids := idsIn(t, view); !slices.Equal(ids, []string{"b"}) {
		t.Fatalf("got %v, want b only", ids)
	}
}
