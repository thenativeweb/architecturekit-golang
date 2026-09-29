package architecturekit_test

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
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

	require.NoError(t, view.Insert(context.Background(), eventID, item), "failed to insert %+v", item)
}

func mustGet(t *testing.T, view *architecturekit.InMemoryView[string, book], id string) book {
	t.Helper()

	item, isFound, err := view.Get(context.Background(), id)
	require.NoError(t, err)
	require.True(t, isFound, "book %q is missing", id)

	return item
}

func idsIn(t *testing.T, view architecturekit.View[book]) []string {
	t.Helper()

	items, err := view.All(context.Background())
	require.NoError(t, err)

	var ids []string
	for item := range items {
		ids = append(ids, item.ID)
	}

	return ids
}

func TestInMemoryView(t *testing.T) {
	t.Run("inserts and gets items", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "42", Title: "2001"})

		got := mustGet(t, view, "42")
		assert.Equal(t, "2001", got.Title)
		assert.Equal(t, "1", got.Revision)

		_, isFound, err := view.Get(context.Background(), "23")
		require.NoError(t, err)
		assert.False(t, isFound)
	})

	t.Run("hands out items in the order of insertion", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "a"})
		mustInsert(t, view, "2", book{ID: "b"})
		mustInsert(t, view, "3", book{ID: "c"})

		_, err := view.Delete(context.Background(), "b", "4")
		require.NoError(t, err)
		mustInsert(t, view, "5", book{ID: "b"})

		// A key that was deleted and inserted again moves to the end.
		assert.Equal(t, []string{"a", "c", "b"}, idsIn(t, view))
	})

	t.Run("hands out a copy", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "a"})

		items, _ := view.All(context.Background())
		mustInsert(t, view, "2", book{ID: "b"})

		// Writing while a query holds its result must not change what it holds.
		assert.Len(t, slices.Collect(items), 1, "a query result must not change underneath")
	})

	t.Run("skips an insert that is not newer", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "5", book{ID: "42", Title: "first"})

		// The same event, applied a second time, and an older one.
		mustInsert(t, view, "5", book{ID: "42", Title: "again"})
		mustInsert(t, view, "3", book{ID: "42", Title: "older"})

		got := mustGet(t, view, "42")
		assert.Equal(t, "first", got.Title)
		assert.Equal(t, "5", got.Revision)
	})

	t.Run("refuses a second item with the same key", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "42"})

		err := view.Insert(context.Background(), "2", book{ID: "42"})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a taken key is permanent")
		assert.ErrorContains(t, err, "42", "the error has to name the key")
		assert.ErrorContains(t, err, "event 2", "the error has to name the event")
	})

	t.Run("updates an item", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "42", Title: "draft"})

		isChanged, err := view.Update(context.Background(), "42", "2", func(item *book) {
			item.Title = "final"
			item.Revision = "made up by the handler"
		})
		require.NoError(t, err)
		assert.True(t, isChanged)

		// The view sets the revision, whatever the handler wrote.
		got := mustGet(t, view, "42")
		assert.Equal(t, "final", got.Title)
		assert.Equal(t, "2", got.Revision)
	})

	t.Run("skips an update that is not newer", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "2", book{ID: "42", Title: "current"})

		isChanged, err := view.Update(context.Background(), "42", "2", func(item *book) { item.Title = "replayed" })
		require.NoError(t, err)
		assert.False(t, isChanged)

		got := mustGet(t, view, "42")
		assert.Equal(t, "current", got.Title)
	})

	t.Run("changes nothing for a missing item", func(t *testing.T) {
		view := bookView()

		isChanged, err := view.Update(context.Background(), "42", "1", func(item *book) { item.Title = "x" })
		require.NoError(t, err, "updating a missing book")
		assert.False(t, isChanged, "updating a missing book")

		isRemoved, err := view.Delete(context.Background(), "42", "1")
		require.NoError(t, err, "deleting a missing book")
		assert.False(t, isRemoved, "deleting a missing book")
	})

	t.Run("refuses to change the key", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "42", Title: "kept"})

		_, err := view.Update(context.Background(), "42", "2", func(item *book) {
			item.ID = "23"
			item.Title = "moved"
		})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "changing the key is permanent")
		got := mustGet(t, view, "42")
		assert.Equal(t, "kept", got.Title, "a refused change must not be kept")
	})

	t.Run("upserts", func(t *testing.T) {
		view := bookView()
		rename := func(item *book) { item.Title = "renamed" }

		require.NoError(t, view.Upsert(context.Background(), "1", book{ID: "42", Title: "new"}, rename))
		got := mustGet(t, view, "42")
		assert.Equal(t, "new", got.Title, "a missing book is inserted as given")
		assert.Equal(t, "1", got.Revision, "a missing book is inserted as given")

		require.NoError(t, view.Upsert(context.Background(), "2", book{ID: "42", Title: "ignored"}, rename))
		got = mustGet(t, view, "42")
		assert.Equal(t, "renamed", got.Title, "an existing book is changed")
		assert.Equal(t, "2", got.Revision, "an existing book is changed")

		require.NoError(t, view.Upsert(context.Background(), "2", book{ID: "42"}, func(item *book) { item.Title = "replayed" }))
		got = mustGet(t, view, "42")
		assert.Equal(t, "renamed", got.Title, "an event that is not newer is skipped")
	})

	t.Run("deletes an item", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "5", book{ID: "42"})

		isRemoved, err := view.Delete(context.Background(), "42", "3")
		require.NoError(t, err)
		assert.False(t, isRemoved, "an older event must not delete")

		isRemoved, err = view.Delete(context.Background(), "42", "6")
		require.NoError(t, err)
		assert.True(t, isRemoved)

		_, isFound, _ := view.Get(context.Background(), "42")
		assert.False(t, isFound, "the book is still there")
	})

	t.Run("updates and deletes where items match", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})
		mustInsert(t, view, "2", book{ID: "b", Shelf: "right"})
		mustInsert(t, view, "9", book{ID: "c", Shelf: "left"})

		onTheLeft := func(item book) bool { return item.Shelf == "left" }

		// c has already seen a newer event, so only a is changed.
		changed, err := view.UpdateWhere(context.Background(), onTheLeft, "5", func(item *book) { item.Title = "left" })
		require.NoError(t, err)
		assert.Equal(t, 1, changed)
		got := mustGet(t, view, "a")
		assert.Equal(t, "left", got.Title)
		assert.Equal(t, "5", got.Revision)

		removed, err := view.DeleteWhere(context.Background(), onTheLeft, "10")
		require.NoError(t, err)
		assert.Equal(t, 2, removed)
		assert.Equal(t, []string{"b"}, idsIn(t, view))
	})

	t.Run("keeps revisions without a field", func(t *testing.T) {
		view := architecturekit.NewInMemoryView(func(item book) string { return item.ID })
		mustInsert(t, view, "5", book{ID: "42", Title: "current"})

		isChanged, err := view.Update(context.Background(), "42", "5", func(item *book) { item.Title = "replayed" })
		require.NoError(t, err)
		assert.False(t, isChanged, "the replayed event has to be skipped")
		got := mustGet(t, view, "42")
		assert.Empty(t, got.Revision, "without a field, the item carries no revision")
	})

	t.Run("refuses an empty event ID", func(t *testing.T) {
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
			t.Run(name, func(t *testing.T) {
				assert.ErrorIs(t, operation(), architecturekit.ErrPermanent)
			})
		}
	})

	t.Run("refuses an event ID that is not a revision", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "42"})

		err := view.Insert(context.Background(), "not a revision", book{ID: "42"})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent)
		assert.ErrorIs(t, err, architecturekit.ErrNotARevision)
	})

	t.Run("reports a failure among several items", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})
		mustInsert(t, view, "2", book{ID: "b", Shelf: "left"})
		onTheLeft := func(item book) bool { return item.Shelf == "left" }

		_, err := view.UpdateWhere(context.Background(), onTheLeft, "3", func(item *book) { item.ID = "moved" })
		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "the change of a key has to be refused")

		_, err = view.DeleteWhere(context.Background(), onTheLeft, "not a revision")
		assert.ErrorIs(t, err, architecturekit.ErrNotARevision)
	})

	t.Run("stays in order when many items are deleted", func(t *testing.T) {
		view := bookView()
		for i := range 40 {
			mustInsert(t, view, strconv.Itoa(i), book{ID: fmt.Sprintf("book-%02d", i)})
		}

		// Deleting most items makes the view drop them from its order.
		for i := range 30 {
			_, err := view.Delete(context.Background(), fmt.Sprintf("book-%02d", i), "100")
			require.NoError(t, err)
		}
		mustInsert(t, view, "101", book{ID: "book-00"})

		want := []string{"book-30", "book-31", "book-32", "book-33", "book-34",
			"book-35", "book-36", "book-37", "book-38", "book-39", "book-00"}
		assert.Equal(t, want, idsIn(t, view))
	})
}

func lookedUp(t *testing.T, index *architecturekit.InMemoryIndex[string, book, string], shelf string) []string {
	t.Helper()

	items, err := index.Lookup(context.Background(), shelf)
	require.NoError(t, err)

	var ids []string
	for item := range items {
		ids = append(ids, item.ID)
	}

	return ids
}

func TestInMemoryIndex(t *testing.T) {
	t.Run("looks up items by value", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})

		// An index that is added later knows the items that are already there.
		byShelf := view.Index(func(item book) string { return item.Shelf })

		mustInsert(t, view, "2", book{ID: "b", Shelf: "right"})
		mustInsert(t, view, "3", book{ID: "c", Shelf: "left"})

		assert.Equal(t, []string{"a", "c"}, lookedUp(t, byShelf, "left"))
		assert.Empty(t, lookedUp(t, byShelf, "nowhere"))
	})

	t.Run("follows a changed value", func(t *testing.T) {
		view := bookView()
		byShelf := view.Index(func(item book) string { return item.Shelf })
		mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})

		_, err := view.Update(context.Background(), "a", "2", func(item *book) { item.Shelf = "right" })
		require.NoError(t, err)

		assert.Empty(t, lookedUp(t, byShelf, "left"))
		assert.Equal(t, []string{"a"}, lookedUp(t, byShelf, "right"))
	})

	t.Run("updates and deletes by value", func(t *testing.T) {
		view := bookView()
		byShelf := view.Index(func(item book) string { return item.Shelf })
		mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})
		mustInsert(t, view, "2", book{ID: "b", Shelf: "right"})
		mustInsert(t, view, "9", book{ID: "c", Shelf: "left"})

		changed, err := byShelf.Update(context.Background(), "left", "5", func(item *book) { item.Title = "left" })
		require.NoError(t, err)
		assert.Equal(t, 1, changed, "only a is changed")

		removed, err := byShelf.Delete(context.Background(), "left", "10")
		require.NoError(t, err)
		assert.Equal(t, 2, removed)

		assert.Empty(t, lookedUp(t, byShelf, "left"))
		assert.Equal(t, []string{"b"}, idsIn(t, view))
	})
}
