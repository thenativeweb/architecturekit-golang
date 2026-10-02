package architecturekit_test

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strconv"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
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

		outcome, err := view.Update(context.Background(), "42", "2", func(item *book) {
			item.Title = "final"
			item.Revision = "made up by the handler"
		})
		require.NoError(t, err)
		assert.Equal(t, architecturekit.Applied, outcome)

		// The view sets the revision, whatever the handler wrote.
		got := mustGet(t, view, "42")
		assert.Equal(t, "final", got.Title)
		assert.Equal(t, "2", got.Revision)
	})

	t.Run("skips an update that is not newer", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "2", book{ID: "42", Title: "current"})

		outcome, err := view.Update(context.Background(), "42", "2", func(item *book) { item.Title = "replayed" })
		require.NoError(t, err)
		assert.Equal(t, architecturekit.AlreadyApplied, outcome)

		got := mustGet(t, view, "42")
		assert.Equal(t, "current", got.Title)
	})

	t.Run("changes nothing for a missing item", func(t *testing.T) {
		view := bookView()

		outcome, err := view.Update(context.Background(), "42", "1", func(item *book) { item.Title = "x" })
		require.NoError(t, err, "updating a missing book")
		assert.Equal(t, architecturekit.Missing, outcome, "updating a missing book")

		outcome, err = view.Delete(context.Background(), "42", "1")
		require.NoError(t, err, "deleting a missing book")
		assert.Equal(t, architecturekit.Missing, outcome, "deleting a missing book")
	})

	t.Run("refuses to change the key", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "42", Title: "kept"})

		outcome, err := view.Update(context.Background(), "42", "2", func(item *book) {
			item.ID = "23"
			item.Title = "moved"
		})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "changing the key is permanent")
		assert.Zero(t, outcome, "an error comes without an outcome")
		got := mustGet(t, view, "42")
		assert.Equal(t, "kept", got.Title, "a refused change must not be kept")
	})

	t.Run("upserts", func(t *testing.T) {
		view := bookView()
		shelve := func(title string) func(item *book) {
			return func(item *book) {
				item.ID = "42"
				item.Title = title
				item.Shelf = item.Shelf + "|" + title
			}
		}

		require.NoError(t, view.Upsert(context.Background(), "42", "1", shelve("new")))
		got := mustGet(t, view, "42")
		assert.Equal(t, "new", got.Title, "a missing book is added")
		assert.Equal(t, "|new", got.Shelf, "a missing book starts from the zero value")
		assert.Equal(t, "1", got.Revision, "a missing book is added with the event")

		require.NoError(t, view.Upsert(context.Background(), "42", "2", shelve("renamed")))
		got = mustGet(t, view, "42")
		assert.Equal(t, "renamed", got.Title, "an existing book is changed")
		assert.Equal(t, "|new|renamed", got.Shelf, "an existing book is changed, not replaced")
		assert.Equal(t, "2", got.Revision, "an existing book is changed with the event")

		require.NoError(t, view.Upsert(context.Background(), "42", "2", shelve("replayed")))
		got = mustGet(t, view, "42")
		assert.Equal(t, "renamed", got.Title, "an event that is not newer is skipped")
	})

	t.Run("refuses to upsert an item under another key", func(t *testing.T) {
		view := bookView()

		err := view.Upsert(context.Background(), "42", "1", func(item *book) { item.Title = "no key set" })

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a missing key is permanent")
		assert.ErrorContains(t, err, "42", "the error has to name the key")
		assert.ErrorContains(t, err, "event 1", "the error has to name the event")
		assert.Empty(t, idsIn(t, view), "a refused item must not be added")

		mustInsert(t, view, "2", book{ID: "42"})
		err = view.Upsert(context.Background(), "42", "3", func(item *book) { item.ID = "23" })
		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "changing the key of an existing item is permanent")
	})

	t.Run("deletes an item", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "5", book{ID: "42"})

		outcome, err := view.Delete(context.Background(), "42", "3")
		require.NoError(t, err)
		assert.Equal(t, architecturekit.AlreadyApplied, outcome, "an older event must not delete")

		outcome, err = view.Delete(context.Background(), "42", "6")
		require.NoError(t, err)
		assert.Equal(t, architecturekit.Applied, outcome)

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

	t.Run("moves the revision with a change that does nothing", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "42", Title: "kept"})

		outcome, err := view.Update(context.Background(), "42", "3", func(*book) {})
		require.NoError(t, err)
		assert.Equal(t, architecturekit.Applied, outcome, "a change that does nothing is still applied")

		got := mustGet(t, view, "42")
		assert.Equal(t, "kept", got.Title)
		assert.Equal(t, "3", got.Revision, "the field has to follow the event")

		outcome, err = view.Update(context.Background(), "42", "2", func(item *book) { item.Title = "older" })
		require.NoError(t, err)
		assert.Equal(t, architecturekit.AlreadyApplied, outcome, "the revision the view keeps has to follow the event as well")
		assert.Equal(t, "kept", mustGet(t, view, "42").Title)
	})

	t.Run("keeps revisions without a field", func(t *testing.T) {
		view := architecturekit.NewInMemoryView(func(item book) string { return item.ID })
		mustInsert(t, view, "5", book{ID: "42", Title: "current"})

		outcome, err := view.Update(context.Background(), "42", "5", func(item *book) { item.Title = "replayed" })
		require.NoError(t, err)
		assert.Equal(t, architecturekit.AlreadyApplied, outcome, "the replayed event has to be skipped")
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
			"Upsert": func() error { return view.Upsert(ctx, "42", "", noChange) },
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

		outcome, err := view.Update(context.Background(), "42", "not a revision", func(*book) {})
		assert.ErrorIs(t, err, architecturekit.ErrNotARevision, "updating")
		assert.Zero(t, outcome, "an error comes without an outcome")

		outcome, err = view.Delete(context.Background(), "42", "not a revision")
		assert.ErrorIs(t, err, architecturekit.ErrNotARevision, "deleting")
		assert.Zero(t, outcome, "an error comes without an outcome")

		err = view.Upsert(context.Background(), "42", "not a revision", func(*book) {})
		assert.ErrorIs(t, err, architecturekit.ErrNotARevision, "upserting an existing item")
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

func TestKeyedView(t *testing.T) {
	t.Run("finds an item by its key through the interface", func(t *testing.T) {
		// A query takes the interface rather than the view, so that a view in a
		// database can stand in for the one in memory later on.
		titleOf := func(view architecturekit.KeyedView[string, book], id string) (string, bool) {
			item, isFound, err := view.Get(context.Background(), id)
			require.NoError(t, err)

			return item.Title, isFound
		}

		view := bookView()
		mustInsert(t, view, "1", book{ID: "42", Title: "Rendezvous with Rama"})

		title, isFound := titleOf(view, "42")
		assert.True(t, isFound)
		assert.Equal(t, "Rendezvous with Rama", title)

		_, isFound = titleOf(view, "23")
		assert.False(t, isFound, "a missing key has to be reported as missing")
	})
}

func TestOutcome(t *testing.T) {
	t.Run("names itself", func(t *testing.T) {
		assert.Equal(t, "applied", architecturekit.Applied.String())
		assert.Equal(t, "missing", architecturekit.Missing.String())
		assert.Equal(t, "already applied", architecturekit.AlreadyApplied.String())
		assert.Equal(t, "Outcome(0)", architecturekit.Outcome(0).String(), "the zero value is no outcome")
	})
}

// shelfItem is an item that holds a slice and a map, which a plain copy shares
// with the original, and a field for its revision.
type shelfItem struct {
	ID       string
	BookIDs  []string
	Labels   map[string]string
	Revision string
}

func newShelfItem(id string) shelfItem {
	return shelfItem{ID: id, BookIDs: []string{"42"}, Labels: map[string]string{"genre": "fiction"}}
}

func cloneShelfItem(item shelfItem) shelfItem {
	item.BookIDs = slices.Clone(item.BookIDs)
	item.Labels = maps.Clone(item.Labels)

	return item
}

func shelfView(options ...architecturekit.InMemoryViewOption[shelfItem]) *architecturekit.InMemoryView[string, shelfItem] {
	return architecturekit.NewInMemoryView(func(item shelfItem) string { return item.ID }, options...)
}

func mustGetShelf(t *testing.T, view *architecturekit.InMemoryView[string, shelfItem], id string) shelfItem {
	t.Helper()

	item, isFound, err := view.Get(context.Background(), id)
	require.NoError(t, err)
	require.True(t, isFound, "shelf %q is missing", id)

	return item
}

// rearrange writes into the slice and the map of a shelf, rather than replacing
// them, which is only safe if the view hands over a clone.
func rearrange(item *shelfItem) {
	item.BookIDs[0] = "23"
	item.Labels["genre"] = "poetry"
}

func TestCloneWith(t *testing.T) {
	t.Run("hands a clone to every change of an existing item", func(t *testing.T) {
		ctx := context.Background()
		changes := map[string]func(view *architecturekit.InMemoryView[string, shelfItem]) error{
			"Update": func(view *architecturekit.InMemoryView[string, shelfItem]) error {
				_, err := view.Update(ctx, "a", "2", rearrange)
				return err
			},
			"Upsert": func(view *architecturekit.InMemoryView[string, shelfItem]) error {
				return view.Upsert(ctx, "a", "2", rearrange)
			},
			"UpdateWhere": func(view *architecturekit.InMemoryView[string, shelfItem]) error {
				_, err := view.UpdateWhere(ctx, func(shelfItem) bool { return true }, "2", rearrange)
				return err
			},
			"Index.Update": func(view *architecturekit.InMemoryView[string, shelfItem]) error {
				byID := view.Index(func(item shelfItem) string { return item.ID })
				_, err := byID.Update(ctx, "a", "2", rearrange)
				return err
			},
		}

		for name, change := range changes {
			t.Run(name, func(t *testing.T) {
				view := shelfView(architecturekit.CloneWith(cloneShelfItem))
				require.NoError(t, view.Insert(ctx, "1", newShelfItem("a")))

				got := mustGetShelf(t, view, "a")
				items, err := view.All(ctx)
				require.NoError(t, err)

				require.NoError(t, change(view))

				// What the readers got before the change must stay as it was.
				assert.Equal(t, []string{"42"}, got.BookIDs, "a change must not reach the slice of an item that was read")
				assert.Equal(t, map[string]string{"genre": "fiction"}, got.Labels, "a change must not reach the map of an item that was read")
				for item := range items {
					assert.Equal(t, []string{"42"}, item.BookIDs, "a change must not reach the slice of an item that was listed")
					assert.Equal(t, map[string]string{"genre": "fiction"}, item.Labels, "a change must not reach the map of an item that was listed")
				}

				changed := mustGetShelf(t, view, "a")
				assert.Equal(t, []string{"23"}, changed.BookIDs)
				assert.Equal(t, map[string]string{"genre": "poetry"}, changed.Labels)
			})
		}
	})

	t.Run("does not clone a new item, or a change that is skipped", func(t *testing.T) {
		ctx := context.Background()
		clones := 0
		view := shelfView(architecturekit.CloneWith(func(item shelfItem) shelfItem {
			clones++
			return cloneShelfItem(item)
		}))

		require.NoError(t, view.Insert(ctx, "1", newShelfItem("a")))
		require.NoError(t, view.Upsert(ctx, "b", "2", func(item *shelfItem) { item.ID = "b" }))
		assert.Zero(t, clones, "a new item has no readers yet")

		_, err := view.Update(ctx, "a", "3", rearrange)
		require.NoError(t, err)
		require.NoError(t, view.Upsert(ctx, "b", "4", func(*shelfItem) {}))
		assert.Equal(t, 2, clones, "every change of an existing item needs a clone")

		outcome, err := view.Update(ctx, "a", "3", rearrange)
		require.NoError(t, err)
		assert.Equal(t, architecturekit.AlreadyApplied, outcome)
		outcome, err = view.Update(ctx, "c", "5", rearrange)
		require.NoError(t, err)
		assert.Equal(t, architecturekit.Missing, outcome)
		assert.Equal(t, 2, clones, "a change that is skipped needs no clone")
	})

	t.Run("sets the revision on the clone it keeps", func(t *testing.T) {
		// The clone leaves out the revision, which the view sets afterwards.
		view := shelfView(
			architecturekit.RevisionIn(func(item *shelfItem) *string { return &item.Revision }),
			architecturekit.CloneWith(func(item shelfItem) shelfItem {
				return shelfItem{ID: item.ID, BookIDs: slices.Clone(item.BookIDs), Labels: maps.Clone(item.Labels)}
			}),
		)
		require.NoError(t, view.Insert(context.Background(), "1", newShelfItem("a")))
		got := mustGetShelf(t, view, "a")

		_, err := view.Update(context.Background(), "a", "2", rearrange)
		require.NoError(t, err)

		assert.Equal(t, "1", got.Revision, "a change must not reach the revision of an item that was read")
		changed := mustGetShelf(t, view, "a")
		assert.Equal(t, "2", changed.Revision)
		assert.Equal(t, []string{"23"}, changed.BookIDs)
	})

	t.Run("keeps an index on data that a change writes into", func(t *testing.T) {
		ctx := context.Background()
		view := shelfView(architecturekit.CloneWith(cloneShelfItem))
		byGenre := view.Index(func(item shelfItem) string { return item.Labels["genre"] })
		require.NoError(t, view.Insert(ctx, "1", newShelfItem("a")))

		_, err := view.Update(ctx, "a", "2", rearrange)
		require.NoError(t, err)

		fiction, err := byGenre.Lookup(ctx, "fiction")
		require.NoError(t, err)
		assert.Empty(t, slices.Collect(fiction), "the index must not find the shelf under its old value")
		poetry, err := byGenre.Lookup(ctx, "poetry")
		require.NoError(t, err)
		assert.Len(t, slices.Collect(poetry), 1, "the index has to find the shelf under its new value")
	})

	t.Run("lets readers read while a change writes into an item", func(t *testing.T) {
		ctx := context.Background()
		view := shelfView(architecturekit.CloneWith(cloneShelfItem))
		require.NoError(t, view.Insert(ctx, "1", newShelfItem("a")))

		const rounds = 1000
		started, done := make(chan struct{}), make(chan struct{})

		var group sync.WaitGroup
		group.Add(1)

		go func() {
			defer group.Done()

			// The reader goes on until the writer is done, and reads the slice and
			// the map of every item it gets, outside of the lock of the view.
			for isFirst := true; ; isFirst = false {
				select {
				case <-done:
					return
				default:
				}

				got, _, _ := view.Get(ctx, "a")
				_ = got.BookIDs[0] + got.Labels["genre"]

				items, _ := view.All(ctx)
				for item := range items {
					for range item.Labels {
					}
				}

				if isFirst {
					close(started)
				}
			}
		}()

		// The writer starts once the reader has read, so that the two overlap.
		<-started

		for round := range rounds {
			_, err := view.Update(ctx, "a", strconv.Itoa(round+2), func(item *shelfItem) {
				item.BookIDs[0] = strconv.Itoa(round)
				item.Labels["genre"] = strconv.Itoa(round)
			})
			require.NoError(t, err)
		}

		close(done)
		group.Wait()

		got := mustGetShelf(t, view, "a")
		assert.Equal(t, []string{strconv.Itoa(rounds - 1)}, got.BookIDs)
	})

	t.Run("leaves a reader alone without a clone if a change replaces what it holds", func(t *testing.T) {
		view := shelfView()
		require.NoError(t, view.Insert(context.Background(), "1", newShelfItem("a")))
		got := mustGetShelf(t, view, "a")

		_, err := view.Update(context.Background(), "a", "2", func(item *shelfItem) {
			item.BookIDs = append(slices.Clone(item.BookIDs), "23")
			item.Labels = maps.Clone(item.Labels)
			item.Labels["genre"] = "poetry"
		})
		require.NoError(t, err)

		assert.Equal(t, []string{"42"}, got.BookIDs)
		assert.Equal(t, map[string]string{"genre": "fiction"}, got.Labels)
		changed := mustGetShelf(t, view, "a")
		assert.Equal(t, []string{"42", "23"}, changed.BookIDs)
		assert.Equal(t, map[string]string{"genre": "poetry"}, changed.Labels)
	})

	t.Run("panics on a nil function", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit: CloneWith needs a function, not nil", func() {
			architecturekit.CloneWith[shelfItem](nil)
		})
	})

	t.Run("panics when it is given twice", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit: CloneWith is given twice", func() {
			shelfView(architecturekit.CloneWith(cloneShelfItem), architecturekit.CloneWith(cloneShelfItem))
		})
	})
}

// counterItem is the item of a view of counters, one per subject, which keeps
// its revision for a precondition on that subject.
type counterItem struct {
	Subject  string
	Total    int
	Revision string
}

// counterItemProjection keeps the total of every counter. With followsResets,
// it also handles resets, which change nothing about the item, with a change
// that does nothing, so that the revision of the item follows the subject.
func counterItemProjection(
	view *architecturekit.InMemoryView[string, counterItem],
	followsResets bool,
) *architecturekit.TypedProjection {
	projection := architecturekit.NewProjection().
		On(func(ctx context.Context, event architecturekit.Envelope[incremented]) error {
			return view.Upsert(ctx, event.Subject, event.ID, func(item *counterItem) {
				item.Subject = event.Subject
				item.Total += event.Data.By
			})
		})

	if followsResets {
		projection.On(func(ctx context.Context, event architecturekit.Envelope[reset]) error {
			_, err := view.Update(ctx, event.Subject, event.ID, func(*counterItem) {})
			return err
		})
	}

	return projection
}

// projectIncrementAndReset writes an increment and a reset, which the state
// ignores, to a subject of the test, and projects them into a view. It returns
// the item of the counter, and the events it wrote.
func projectIncrementAndReset(t *testing.T, followsResets bool) (counterItem, []eventsourcingdb.Event) {
	t.Helper()

	ctx := context.Background()
	subject := subjectFor(t)
	written, err := rawClient(t).WriteEvents([]eventsourcingdb.EventCandidate{
		{Source: "https://thenativeweb.io", Subject: subject, Type: (incremented{}).EventType(), Data: incremented{By: 2}},
		{Source: "https://thenativeweb.io", Subject: subject, Type: (reset{}).EventType(), Data: reset{}},
	}, nil)
	require.NoError(t, err)

	view := architecturekit.NewInMemoryView(
		func(item counterItem) string { return item.Subject },
		architecturekit.RevisionIn(func(item *counterItem) *string { return &item.Revision }),
	)
	projection := counterItemProjection(view, followsResets)
	require.NoError(t, architecturekit.CatchUpProjection(ctx, requireStore(t), architecturekit.ExactSubject(subject), projection))

	item, isFound, err := view.Get(ctx, subject)
	require.NoError(t, err)
	require.True(t, isFound, "the counter is missing")

	return item, written
}

func TestRevisionIn(t *testing.T) {
	// The decider ignores resets, as a state does with events that matter for no
	// decision.
	decider := counterDecider()
	decider.State = resetIgnoringState()

	t.Run("fits a precondition on the subject if the item follows every event of it", func(t *testing.T) {
		item, written := projectIncrementAndReset(t, true)

		assert.Equal(t, 2, item.Total, "the reset must not change the item")
		assert.Equal(t, written[1].ID, item.Revision, "the revision has to follow the reset")

		_, err := architecturekit.Execute(context.Background(), requireStore(t), decider,
			increment{subject: item.Subject, By: 1}.onEventID(item.Revision))
		assert.NoError(t, err, "the revision of the item has to fit the subject")
	})

	t.Run("falls behind the subject if the projection skips an event of it", func(t *testing.T) {
		item, written := projectIncrementAndReset(t, false)

		assert.Equal(t, written[0].ID, item.Revision)

		_, err := architecturekit.Execute(context.Background(), requireStore(t), decider,
			increment{subject: item.Subject, By: 1}.onEventID(item.Revision))
		assert.ErrorIs(t, err, architecturekit.ErrConflict, "a revision behind the subject has to conflict")
	})
}
