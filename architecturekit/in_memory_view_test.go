package architecturekit_test

import (
	"context"
	"fmt"
	"iter"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/query"
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

func mustInsert[TItem any](t *testing.T, view *architecturekit.InMemoryView[string, TItem], eventID string, item TItem) {
	t.Helper()

	outcome, err := view.Insert(context.Background(), eventID, item)
	require.NoError(t, err, "failed to insert %+v", item)
	require.Equal(t, architecturekit.Added, outcome, "failed to add %+v", item)
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

	return bookIDsOf(t, view.All(context.Background()))
}

func bookIDsOf(t *testing.T, items iter.Seq2[book, error]) []string {
	t.Helper()

	var ids []string
	for item, err := range items {
		require.NoError(t, err)
		ids = append(ids, item.ID)
	}

	return ids
}

func TestInMemoryView(t *testing.T) {
	t.Run("panics on a nil function for the key, also one that was declared but never set", func(t *testing.T) {
		var declared func(book) string

		for _, keyOf := range []func(book) string{nil, declared} {
			assert.PanicsWithValue(t,
				"architecturekit: NewInMemoryView needs a function that returns the key of an item, not nil",
				func() { architecturekit.NewInMemoryView(keyOf) })
		}
	})

	t.Run("inserts and gets items", func(t *testing.T) {
		view := bookView()

		outcome, err := view.Insert(context.Background(), "1", book{ID: "42", Title: "2001"})
		require.NoError(t, err)
		assert.Equal(t, architecturekit.Added, outcome)

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

		items := view.All(context.Background())
		mustInsert(t, view, "2", book{ID: "b"})

		// Writing while a query holds its result must not change what it holds.
		assert.Equal(t, []string{"a"}, bookIDsOf(t, items), "a query result must not change underneath")
		assert.Equal(t, []string{"a"}, bookIDsOf(t, items), "reading the result again must hand out the same items")
	})

	t.Run("stops handing out items when the caller does", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "a"})
		mustInsert(t, view, "2", book{ID: "b"})

		// A view that went on after the caller stopped would make the loop panic.
		var ids []string
		for item, err := range view.All(context.Background()) {
			require.NoError(t, err)
			ids = append(ids, item.ID)
			if len(ids) == 1 {
				break
			}
		}

		assert.Equal(t, []string{"a"}, ids)
	})

	t.Run("skips an insert that is not newer", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "5", book{ID: "42", Title: "first"})

		// The same event, applied a second time, and an older one.
		outcome, err := view.Insert(context.Background(), "5", book{ID: "42", Title: "again"})
		require.NoError(t, err)
		assert.Equal(t, architecturekit.AlreadyApplied, outcome, "the same event has to be skipped")

		outcome, err = view.Insert(context.Background(), "3", book{ID: "42", Title: "older"})
		require.NoError(t, err)
		assert.Equal(t, architecturekit.AlreadyApplied, outcome, "an older event has to be skipped")

		got := mustGet(t, view, "42")
		assert.Equal(t, "first", got.Title)
		assert.Equal(t, "5", got.Revision)
	})

	t.Run("refuses a second item with the same key", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "42"})

		outcome, err := view.Insert(context.Background(), "2", book{ID: "42"})

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a taken key is permanent")
		assert.ErrorContains(t, err, "42", "the error has to name the key")
		assert.ErrorContains(t, err, "event 2", "the error has to name the event")
		assert.Zero(t, outcome, "an error comes without an outcome")
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

		outcome, err := view.Upsert(context.Background(), "42", "1", shelve("new"))
		require.NoError(t, err)
		assert.Equal(t, architecturekit.Added, outcome, "a missing book is added")
		got := mustGet(t, view, "42")
		assert.Equal(t, "new", got.Title, "a missing book is added")
		assert.Equal(t, "|new", got.Shelf, "a missing book starts from the zero value")
		assert.Equal(t, "1", got.Revision, "a missing book is added with the event")

		outcome, err = view.Upsert(context.Background(), "42", "2", shelve("renamed"))
		require.NoError(t, err)
		assert.Equal(t, architecturekit.Applied, outcome, "an existing book is changed")
		got = mustGet(t, view, "42")
		assert.Equal(t, "renamed", got.Title, "an existing book is changed")
		assert.Equal(t, "|new|renamed", got.Shelf, "an existing book is changed, not replaced")
		assert.Equal(t, "2", got.Revision, "an existing book is changed with the event")

		outcome, err = view.Upsert(context.Background(), "42", "2", shelve("replayed"))
		require.NoError(t, err)
		assert.Equal(t, architecturekit.AlreadyApplied, outcome, "an event that is not newer is skipped")
		got = mustGet(t, view, "42")
		assert.Equal(t, "renamed", got.Title, "an event that is not newer is skipped")
	})

	t.Run("refuses to upsert an item under another key", func(t *testing.T) {
		view := bookView()

		outcome, err := view.Upsert(context.Background(), "42", "1", func(item *book) { item.Title = "no key set" })

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a missing key is permanent")
		assert.ErrorContains(t, err, "42", "the error has to name the key")
		assert.ErrorContains(t, err, "event 1", "the error has to name the event")
		assert.Zero(t, outcome, "an error comes without an outcome")
		assert.Empty(t, idsIn(t, view), "a refused item must not be added")

		mustInsert(t, view, "2", book{ID: "42"})
		outcome, err = view.Upsert(context.Background(), "42", "3", func(item *book) { item.ID = "23" })
		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "changing the key of an existing item is permanent")
		assert.Zero(t, outcome, "an error comes without an outcome")
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

	t.Run("refuses an event ID that is empty or not a revision, and changes nothing", func(t *testing.T) {
		for _, eventID := range []string{"", "abc", "-1", "1.5", " 1", "9223372036854775808"} {
			for targetName, target := range viewTargets {
				for name, change := range viewChanges {
					t.Run(fmt.Sprintf("%s with %q %s", name, eventID, targetName), func(t *testing.T) {
						fixture := newViewFixture(t)
						before := fixture.state(t)

						result, err := change(context.Background(), fixture, target, eventID)

						assert.ErrorIs(t, err, architecturekit.ErrPermanent)
						assert.ErrorIs(t, err, architecturekit.ErrNotARevision)
						assert.EqualError(t, err, "architecturekit: permanent failure: architecturekit: not a revision: "+
							"an operation on a view needs the ID of the event it applies, not "+strconv.Quote(eventID))
						assert.Zero(t, result, "an error comes without an outcome or a count")
						assert.False(t, fixture.ran, "a function that was handed over must not run")
						assert.Equal(t, before, fixture.state(t), "the view has to stay as it is")
					})
				}
			}
		}
	})

	t.Run("accepts every revision as an event ID", func(t *testing.T) {
		// What each function hands out on a free key, and on a taken key for an
		// event that is older than the item, or newer. An error stands for the
		// category the function fails with.
		wants := map[string]struct{ onFree, olderOnTaken, newerOnTaken any }{
			"Insert":       {architecturekit.Added, architecturekit.AlreadyApplied, architecturekit.ErrPermanent},
			"Upsert":       {architecturekit.Added, architecturekit.AlreadyApplied, architecturekit.Applied},
			"Update":       {architecturekit.Missing, architecturekit.AlreadyApplied, architecturekit.Applied},
			"Delete":       {architecturekit.Missing, architecturekit.AlreadyApplied, architecturekit.Applied},
			"UpdateWhere":  {0, 0, 1},
			"DeleteWhere":  {0, 0, 1},
			"Index.Update": {0, 0, 1},
			"Index.Delete": {0, 0, 1},
		}

		// The smallest event ID is older than every item of the fixture, the
		// largest one newer.
		const smallest, largest = "0", "9223372036854775807"

		for name, change := range viewChanges {
			want := wants[name]
			cases := []struct {
				name    string
				target  viewTarget
				eventID string
				want    any
			}{
				{"the smallest one on a free key", viewTargets["on a free key"], smallest, want.onFree},
				{"the largest one on a free key", viewTargets["on a free key"], largest, want.onFree},
				{"the smallest one on a taken key", viewTargets["on a taken key"], smallest, want.olderOnTaken},
				{"the largest one on a taken key", viewTargets["on a taken key"], largest, want.newerOnTaken},
			}

			for _, test := range cases {
				t.Run(name+" with "+test.name, func(t *testing.T) {
					result, err := change(context.Background(), newViewFixture(t), test.target, test.eventID)

					if category, isError := test.want.(error); isError {
						assert.ErrorIs(t, err, category)
						assert.NotErrorIs(t, err, architecturekit.ErrNotARevision, "the event ID itself is fine")
						return
					}

					require.NoError(t, err)
					assert.Equal(t, test.want, result)
				})
			}
		}
	})

	t.Run("refuses an event ID that is not a revision where it is given, not at the next change", func(t *testing.T) {
		view := bookView()
		ctx := context.Background()

		outcome, err := view.Insert(ctx, "abc", book{ID: "42", Title: "draft"})
		assert.ErrorIs(t, err, architecturekit.ErrNotARevision, "the insert has to be refused")
		assert.Zero(t, outcome, "an error comes without an outcome")

		// The book never got in, so the next event about it finds nothing to
		// compare with, rather than an ID that is not a revision.
		outcome, err = view.Update(ctx, "42", "7", func(item *book) { item.Title = "final" })
		require.NoError(t, err, "the next change must not fail")
		assert.Equal(t, architecturekit.Missing, outcome)

		mustInsert(t, view, "5", book{ID: "42", Title: "draft"})

		outcome, err = view.Update(ctx, "42", "7", func(item *book) { item.Title = "final" })
		require.NoError(t, err)
		assert.Equal(t, architecturekit.Applied, outcome)
		assert.Equal(t, book{ID: "42", Title: "final", Revision: "7"}, mustGet(t, view, "42"))
	})

	t.Run("reports a failure among several items", func(t *testing.T) {
		view := bookView()
		mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})
		mustInsert(t, view, "2", book{ID: "b", Shelf: "left"})
		onTheLeft := func(item book) bool { return item.Shelf == "left" }

		_, err := view.UpdateWhere(context.Background(), onTheLeft, "3", func(item *book) { item.ID = "moved" })
		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "the change of a key has to be refused")
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

	return bookIDsOf(t, index.Lookup(context.Background(), shelf))
}

// viewFixture is a view with an index and two books on different shelves,
// which records whether a function it handed over ran.
type viewFixture struct {
	view    *architecturekit.InMemoryView[string, book]
	byShelf *architecturekit.InMemoryIndex[string, book, string]
	ran     bool
}

func newViewFixture(t *testing.T) *viewFixture {
	t.Helper()

	view := bookView()
	fixture := &viewFixture{view: view, byShelf: view.Index(func(item book) string { return item.Shelf })}

	mustInsert(t, view, "1", book{ID: "a", Shelf: "left", Title: "kept"})
	mustInsert(t, view, "2", book{ID: "b", Shelf: "right", Title: "kept"})
	view.Seen("2")

	return fixture
}

// change returns a change that sets the given key, so that Upsert can add an
// item with it, and records that it ran.
func (f *viewFixture) change(key string) func(item *book) {
	return func(item *book) {
		f.ran = true
		item.ID = key
		item.Title = "changed"
	}
}

// isOn returns a match for the books on the given shelf, which records that
// it ran.
func (f *viewFixture) isOn(shelf string) func(item book) bool {
	return func(item book) bool {
		f.ran = true
		return item.Shelf == shelf
	}
}

// viewState is everything a change may touch: the items with their revisions,
// the revision of the view, and what the index finds on every shelf.
type viewState struct {
	Items    []book
	Revision string
	Shelves  map[string][]string
}

func (f *viewFixture) state(t *testing.T) viewState {
	t.Helper()

	var items []book
	for item, err := range f.view.All(context.Background()) {
		require.NoError(t, err)
		items = append(items, item)
	}

	shelves := map[string][]string{}
	for _, shelf := range []string{"left", "right", "nowhere"} {
		shelves[shelf] = lookedUp(t, f.byShelf, shelf)
	}

	return viewState{Items: items, Revision: f.view.Revision(), Shelves: shelves}
}

// viewTarget is the item a change is about, by its key and its shelf. The
// fixture has a book with the key on the shelf, or none.
type viewTarget struct{ key, shelf string }

var viewTargets = map[string]viewTarget{
	"on a free key":  {key: "c", shelf: "nowhere"},
	"on a taken key": {key: "a", shelf: "left"},
}

// viewChanges are all functions that change a view, applied to the target with
// the given event ID. Each hands out its outcome, or its count.
var viewChanges = map[string]func(ctx context.Context, f *viewFixture, target viewTarget, eventID string) (any, error){
	"Insert": func(ctx context.Context, f *viewFixture, target viewTarget, eventID string) (any, error) {
		return f.view.Insert(ctx, eventID, book{ID: target.key, Shelf: target.shelf})
	},
	"Upsert": func(ctx context.Context, f *viewFixture, target viewTarget, eventID string) (any, error) {
		return f.view.Upsert(ctx, target.key, eventID, f.change(target.key))
	},
	"Update": func(ctx context.Context, f *viewFixture, target viewTarget, eventID string) (any, error) {
		return f.view.Update(ctx, target.key, eventID, f.change(target.key))
	},
	"Delete": func(ctx context.Context, f *viewFixture, target viewTarget, eventID string) (any, error) {
		return f.view.Delete(ctx, target.key, eventID)
	},
	"UpdateWhere": func(ctx context.Context, f *viewFixture, target viewTarget, eventID string) (any, error) {
		return f.view.UpdateWhere(ctx, f.isOn(target.shelf), eventID, f.change(target.key))
	},
	"DeleteWhere": func(ctx context.Context, f *viewFixture, target viewTarget, eventID string) (any, error) {
		return f.view.DeleteWhere(ctx, f.isOn(target.shelf), eventID)
	},
	"Index.Update": func(ctx context.Context, f *viewFixture, target viewTarget, eventID string) (any, error) {
		return f.byShelf.Update(ctx, target.shelf, eventID, f.change(target.key))
	},
	"Index.Delete": func(ctx context.Context, f *viewFixture, target viewTarget, eventID string) (any, error) {
		return f.byShelf.Delete(ctx, target.shelf, eventID)
	},
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

	t.Run("hands out a copy", func(t *testing.T) {
		view := bookView()
		byShelf := view.Index(func(item book) string { return item.Shelf })
		mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})

		items := byShelf.Lookup(context.Background(), "left")
		mustInsert(t, view, "2", book{ID: "b", Shelf: "left"})

		assert.Equal(t, []string{"a"}, bookIDsOf(t, items), "a query result must not change underneath")
		assert.Equal(t, []string{"a"}, bookIDsOf(t, items), "reading the result again must hand out the same items")
	})

	t.Run("stops handing out items when the caller does", func(t *testing.T) {
		view := bookView()
		byShelf := view.Index(func(item book) string { return item.Shelf })
		mustInsert(t, view, "1", book{ID: "a", Shelf: "left"})
		mustInsert(t, view, "2", book{ID: "b", Shelf: "left"})

		// An index that went on after the caller stopped would make the loop panic.
		var ids []string
		for item, err := range byShelf.Lookup(context.Background(), "left") {
			require.NoError(t, err)
			ids = append(ids, item.ID)
			if len(ids) == 1 {
				break
			}
		}

		assert.Equal(t, []string{"a"}, ids)
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
		assert.Equal(t, "added", architecturekit.Added.String())
		assert.Equal(t, "Outcome(0)", architecturekit.Outcome(0).String(), "the zero value is no outcome")
	})

	t.Run("keeps the values the outcomes had before Added", func(t *testing.T) {
		assert.Equal(t, 1, int(architecturekit.Applied))
		assert.Equal(t, 2, int(architecturekit.Missing))
		assert.Equal(t, 3, int(architecturekit.AlreadyApplied))
		assert.Equal(t, 4, int(architecturekit.Added))
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
				_, err := view.Upsert(ctx, "a", "2", rearrange)
				return err
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
				mustInsert(t, view, "1", newShelfItem("a"))

				got := mustGetShelf(t, view, "a")
				items := view.All(ctx)

				require.NoError(t, change(view))

				// What the readers got before the change must stay as it was.
				assert.Equal(t, []string{"42"}, got.BookIDs, "a change must not reach the slice of an item that was read")
				assert.Equal(t, map[string]string{"genre": "fiction"}, got.Labels, "a change must not reach the map of an item that was read")
				for item, err := range items {
					require.NoError(t, err)
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

		mustInsert(t, view, "1", newShelfItem("a"))
		outcome, err := view.Upsert(ctx, "b", "2", func(item *shelfItem) { item.ID = "b" })
		require.NoError(t, err)
		assert.Equal(t, architecturekit.Added, outcome)
		assert.Zero(t, clones, "a new item has no readers yet")

		_, err = view.Update(ctx, "a", "3", rearrange)
		require.NoError(t, err)
		outcome, err = view.Upsert(ctx, "b", "4", func(*shelfItem) {})
		require.NoError(t, err)
		assert.Equal(t, architecturekit.Applied, outcome)
		assert.Equal(t, 2, clones, "every change of an existing item needs a clone")

		outcome, err = view.Update(ctx, "a", "3", rearrange)
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
		mustInsert(t, view, "1", newShelfItem("a"))
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
		mustInsert(t, view, "1", newShelfItem("a"))

		_, err := view.Update(ctx, "a", "2", rearrange)
		require.NoError(t, err)

		fiction, err := query.Count(byGenre.Lookup(ctx, "fiction"))
		require.NoError(t, err)
		assert.Zero(t, fiction, "the index must not find the shelf under its old value")
		poetry, err := query.Count(byGenre.Lookup(ctx, "poetry"))
		require.NoError(t, err)
		assert.Equal(t, 1, poetry, "the index has to find the shelf under its new value")
	})

	t.Run("lets readers read while a change writes into an item", func(t *testing.T) {
		ctx := context.Background()
		view := shelfView(architecturekit.CloneWith(cloneShelfItem))
		mustInsert(t, view, "1", newShelfItem("a"))

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

				for item := range view.All(ctx) {
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
		mustInsert(t, view, "1", newShelfItem("a"))
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
			_, err := view.Upsert(ctx, event.Subject, event.ID, func(item *counterItem) {
				item.Subject = event.Subject
				item.Total += event.Data.By
			})
			return err
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

	t.Run("panics on a nil function", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit: RevisionIn needs a function, not nil", func() {
			architecturekit.RevisionIn[counterItem](nil)
		})
	})

	t.Run("panics when it is given twice", func(t *testing.T) {
		revisionOf := func(item *counterItem) *string { return &item.Revision }

		assert.PanicsWithValue(t, "architecturekit: RevisionIn is given twice", func() {
			architecturekit.NewInMemoryView(func(item counterItem) string { return item.Subject },
				architecturekit.RevisionIn(revisionOf), architecturekit.RevisionIn(revisionOf))
		})
	})

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

// bookAcquired, bookShelved, and bookDiscarded are the events of a book, as a
// newer version of an application writes them.
type bookAcquired struct {
	Title string `json:"title"`
}

func (bookAcquired) EventType() string { return "io.thenativeweb.test.book-acquired" }

type bookShelved struct {
	Shelf string `json:"shelf"`
}

func (bookShelved) EventType() string { return "io.thenativeweb.test.book-shelved" }

type bookDiscarded struct{}

func (bookDiscarded) EventType() string { return "io.thenativeweb.test.book-discarded" }

// An older version of the application wrote a single event for what are two
// events now, which bookUpcasters split. The last one is a mistake, since a
// book is acquired only once.
const (
	bookAcquiredAndShelved  = "io.thenativeweb.test.book-acquired-and-shelved"
	bookShelvedAndDiscarded = "io.thenativeweb.test.book-shelved-and-discarded"
	bookReplaced            = "io.thenativeweb.test.book-replaced"
	bookAcquiredTwice       = "io.thenativeweb.test.book-acquired-twice"
)

func bookUpcasters() *architecturekit.Upcasters {
	return architecturekit.NewUpcasters().
		Upcast(bookAcquiredAndShelved, splitInto(bookAcquired{}.EventType(), bookShelved{}.EventType())).
		Upcast(bookShelvedAndDiscarded, splitInto(bookShelved{}.EventType(), bookDiscarded{}.EventType())).
		Upcast(bookReplaced, splitInto(bookDiscarded{}.EventType(), bookAcquired{}.EventType())).
		Upcast(bookAcquiredTwice, splitInto(bookAcquired{}.EventType(), bookAcquired{}.EventType()))
}

// splitInto is an upcaster that splits a stored event into one event per given
// type. Each is a copy of the stored event, with its ID and its data, of which
// every type decodes the fields it knows.
func splitInto(eventTypes ...string) architecturekit.Upcaster {
	return func(event eventsourcingdb.Event) ([]eventsourcingdb.Event, error) {
		parts := make([]eventsourcingdb.Event, 0, len(eventTypes))
		for _, eventType := range eventTypes {
			part := event
			part.Type = eventType
			parts = append(parts, part)
		}

		return parts, nil
	}
}

// storedBook is a stored event about the book 42.
func storedBook(eventID, eventType, data string) eventsourcingdb.Event {
	event := stored(eventType, data)
	event.ID = eventID
	event.Subject = "/books/42"

	return event
}

// bookCatalog is a view of books with an index by ID, so that the index can
// stand in for the view. It counts how often a change of a book runs, and how
// many books are removed, and it keeps what every acquisition of a book
// reported, so that a test sees whether a part was applied again.
type bookCatalog struct {
	view     *architecturekit.InMemoryView[string, book]
	byID     *architecturekit.InMemoryIndex[string, book, string]
	changes  int
	removed  int
	acquired []architecturekit.Outcome
}

// The ways to add, change, and remove a book, through the view or its index.
type (
	bookAcquisition func(ctx context.Context, catalog *bookCatalog, id, eventID, title string) (architecturekit.Outcome, error)
	bookChange      func(ctx context.Context, catalog *bookCatalog, id, eventID string, change func(item *book)) error
	bookRemoval     func(ctx context.Context, catalog *bookCatalog, id, eventID string) (int, error)
)

func insertBook(ctx context.Context, catalog *bookCatalog, id, eventID, title string) (architecturekit.Outcome, error) {
	return catalog.view.Insert(ctx, eventID, book{ID: id, Title: title})
}

func upsertBook(ctx context.Context, catalog *bookCatalog, id, eventID, title string) (architecturekit.Outcome, error) {
	return catalog.view.Upsert(ctx, id, eventID, func(item *book) {
		catalog.changes++
		item.ID = id
		item.Title = title
	})
}

func updateBook(ctx context.Context, catalog *bookCatalog, id, eventID string, change func(item *book)) error {
	_, err := catalog.view.Update(ctx, id, eventID, change)
	return err
}

func deleteBook(ctx context.Context, catalog *bookCatalog, id, eventID string) (int, error) {
	outcome, err := catalog.view.Delete(ctx, id, eventID)
	if outcome != architecturekit.Applied {
		return 0, err
	}

	return 1, err
}

var bookAcquisitions = map[string]bookAcquisition{
	"Insert": insertBook,
	"Upsert": upsertBook,
}

var bookChanges = map[string]bookChange{
	"Update": updateBook,
	"Upsert": func(ctx context.Context, catalog *bookCatalog, id, eventID string, change func(item *book)) error {
		_, err := catalog.view.Upsert(ctx, id, eventID, func(item *book) {
			item.ID = id
			change(item)
		})
		return err
	},
	"UpdateWhere": func(ctx context.Context, catalog *bookCatalog, id, eventID string, change func(item *book)) error {
		_, err := catalog.view.UpdateWhere(ctx, func(item book) bool { return item.ID == id }, eventID, change)
		return err
	},
	"Index.Update": func(ctx context.Context, catalog *bookCatalog, id, eventID string, change func(item *book)) error {
		_, err := catalog.byID.Update(ctx, id, eventID, change)
		return err
	},
}

var bookRemovals = map[string]bookRemoval{
	"Delete": deleteBook,
	"DeleteWhere": func(ctx context.Context, catalog *bookCatalog, id, eventID string) (int, error) {
		return catalog.view.DeleteWhere(ctx, func(item book) bool { return item.ID == id }, eventID)
	},
	"Index.Delete": func(ctx context.Context, catalog *bookCatalog, id, eventID string) (int, error) {
		return catalog.byID.Delete(ctx, id, eventID)
	},
}

// newBookCatalog creates a catalog, and a projection without upcasters that
// applies the events of books to it in the given ways.
func newBookCatalog(
	acquire bookAcquisition,
	shelve bookChange,
	discard bookRemoval,
) (*bookCatalog, *architecturekit.TypedProjection) {
	view := bookView()
	catalog := &bookCatalog{view: view, byID: view.Index(func(item book) string { return item.ID })}

	projection := architecturekit.NewProjection().
		On(func(ctx context.Context, event architecturekit.Envelope[bookAcquired]) error {
			outcome, err := acquire(ctx, catalog, strings.TrimPrefix(event.Subject, "/books/"), event.ID, event.Data.Title)
			catalog.acquired = append(catalog.acquired, outcome)
			return err
		}).
		On(func(ctx context.Context, event architecturekit.Envelope[bookShelved]) error {
			return shelve(ctx, catalog, strings.TrimPrefix(event.Subject, "/books/"), event.ID, func(item *book) {
				catalog.changes++
				item.Shelf = event.Data.Shelf
			})
		}).
		On(func(ctx context.Context, event architecturekit.Envelope[bookDiscarded]) error {
			removed, err := discard(ctx, catalog, strings.TrimPrefix(event.Subject, "/books/"), event.ID)
			catalog.removed += removed
			return err
		})

	return catalog, projection
}

func TestSplitEvents(t *testing.T) {
	acquiredAndShelved := storedBook("0", bookAcquiredAndShelved, `{"title":"Dune","shelf":"A3"}`)

	t.Run("applies a later part that changes the item an earlier part inserted", func(t *testing.T) {
		for name, change := range bookChanges {
			t.Run(name, func(t *testing.T) {
				catalog, projection := newBookCatalog(insertBook, change, deleteBook)

				require.NoError(t, apply(t, projection.UpcastWith(bookUpcasters()), acquiredAndShelved))

				want := book{ID: "42", Title: "Dune", Shelf: "A3", Revision: "0"}
				assert.Equal(t, want, mustGet(t, catalog.view, "42"), "the second part has to change the item")
				assert.Equal(t, 1, catalog.changes)
				assert.Equal(t, []architecturekit.Outcome{architecturekit.Added}, catalog.acquired, "the first part has to add the item")
			})
		}
	})

	t.Run("changes nothing when the stored event is applied again", func(t *testing.T) {
		for name, change := range bookChanges {
			t.Run(name, func(t *testing.T) {
				catalog, projection := newBookCatalog(insertBook, change, deleteBook)

				require.NoError(t, apply(t, projection.UpcastWith(bookUpcasters()),
					acquiredAndShelved, acquiredAndShelved))

				want := book{ID: "42", Title: "Dune", Shelf: "A3", Revision: "0"}
				assert.Equal(t, want, mustGet(t, catalog.view, "42"))
				assert.Equal(t, 1, catalog.changes, "no part may change the item a second time")
				assert.Equal(t, []architecturekit.Outcome{architecturekit.Added, architecturekit.AlreadyApplied}, catalog.acquired,
					"no part may add the item a second time")
			})
		}
	})

	t.Run("applies a later stored event after a split one", func(t *testing.T) {
		for name, change := range bookChanges {
			t.Run(name, func(t *testing.T) {
				catalog, projection := newBookCatalog(insertBook, change, deleteBook)

				require.NoError(t, apply(t, projection.UpcastWith(bookUpcasters()),
					acquiredAndShelved,
					storedBook("1", bookShelved{}.EventType(), `{"shelf":"B7"}`)))

				want := book{ID: "42", Title: "Dune", Shelf: "B7", Revision: "1"}
				assert.Equal(t, want, mustGet(t, catalog.view, "42"), "a later event has to be newer than every part of an earlier one")
				assert.Equal(t, 2, catalog.changes)
			})
		}
	})

	t.Run("skips a change of the same event without a part, which counts as part 0", func(t *testing.T) {
		catalog, projection := newBookCatalog(insertBook, updateBook, deleteBook)
		require.NoError(t, apply(t, projection.UpcastWith(bookUpcasters()), acquiredAndShelved))

		outcome, err := catalog.view.Update(context.Background(), "42", "0", func(item *book) { item.Shelf = "C1" })
		require.NoError(t, err)
		assert.Equal(t, architecturekit.AlreadyApplied, outcome)

		outcome, err = catalog.view.Update(context.Background(), "42", "1", func(item *book) { item.Shelf = "C1" })
		require.NoError(t, err)
		assert.Equal(t, architecturekit.Applied, outcome, "a later event without a part has to be applied")
	})

	t.Run("removes an item in a later part", func(t *testing.T) {
		for name, discard := range bookRemovals {
			t.Run(name, func(t *testing.T) {
				catalog, projection := newBookCatalog(insertBook, updateBook, discard)
				shelvedAndDiscarded := storedBook("1", bookShelvedAndDiscarded, `{"shelf":"B7"}`)

				require.NoError(t, apply(t, projection.UpcastWith(bookUpcasters()),
					acquiredAndShelved, shelvedAndDiscarded))

				assert.Empty(t, idsIn(t, catalog.view), "the second part has to remove the item the first one changed")
				assert.Equal(t, 1, catalog.removed)
				assert.Equal(t, 2, catalog.changes)

				require.NoError(t, apply(t, projection, shelvedAndDiscarded))

				assert.Empty(t, idsIn(t, catalog.view))
				assert.Equal(t, 1, catalog.removed, "applying the stored event again must not remove anything")
				assert.Equal(t, 2, catalog.changes, "applying the stored event again must not change anything")
			})
		}
	})

	t.Run("adds an item in a later part, and keeps it when the stored event is applied again", func(t *testing.T) {
		for name, acquire := range bookAcquisitions {
			t.Run(name, func(t *testing.T) {
				catalog, projection := newBookCatalog(acquire, updateBook, deleteBook)
				replaced := storedBook("1", bookReplaced, `{"title":"Dune, second copy"}`)

				require.NoError(t, apply(t, projection.UpcastWith(bookUpcasters()),
					storedBook("0", bookAcquired{}.EventType(), `{"title":"Dune"}`), replaced))

				want := book{ID: "42", Title: "Dune, second copy", Revision: "1"}
				assert.Equal(t, want, mustGet(t, catalog.view, "42"), "the first part has to remove the old item, and the second one has to add the new one")
				assert.Equal(t, 1, catalog.removed)
				assert.Equal(t, []architecturekit.Outcome{architecturekit.Added, architecturekit.Added}, catalog.acquired,
					"the second part has to add the item anew")
				changes := catalog.changes

				require.NoError(t, apply(t, projection, replaced), "applying the stored event again has to skip both parts")

				assert.Equal(t, want, mustGet(t, catalog.view, "42"))
				assert.Equal(t, 1, catalog.removed)
				assert.Equal(t, changes, catalog.changes, "applying the stored event again must not change anything")
				assert.Equal(t, []architecturekit.Outcome{architecturekit.Added, architecturekit.Added, architecturekit.AlreadyApplied}, catalog.acquired,
					"applying the stored event again must not add anything")
			})
		}
	})

	t.Run("refuses a later part that inserts an item whose key an earlier part took", func(t *testing.T) {
		catalog, projection := newBookCatalog(insertBook, updateBook, deleteBook)

		err := apply(t, projection.UpcastWith(bookUpcasters()), storedBook("0", bookAcquiredTwice, `{"title":"Dune"}`))

		assert.ErrorIs(t, err, architecturekit.ErrPermanent, "a second item with the same key is a mistake, also within one stored event")
		assert.ErrorContains(t, err, "already taken")
		assert.Equal(t, []architecturekit.Outcome{architecturekit.Added, 0}, catalog.acquired, "the second part has to fail without an outcome")
	})

	t.Run("upserts in a later part the item an earlier part added, and skips both when the stored event is applied again", func(t *testing.T) {
		catalog, projection := newBookCatalog(upsertBook, updateBook, deleteBook)
		acquiredTwice := storedBook("0", bookAcquiredTwice, `{"title":"Dune"}`)

		require.NoError(t, apply(t, projection.UpcastWith(bookUpcasters()), acquiredTwice, acquiredTwice))

		want := []architecturekit.Outcome{
			architecturekit.Added, architecturekit.Applied,
			architecturekit.AlreadyApplied, architecturekit.AlreadyApplied,
		}
		assert.Equal(t, want, catalog.acquired, "the first part has to add the item, and the second one has to change it")
		assert.Equal(t, 2, catalog.changes)
		assert.Equal(t, book{ID: "42", Title: "Dune", Revision: "0"}, mustGet(t, catalog.view, "42"))
	})

	t.Run("treats every event as part 0 without upcasters", func(t *testing.T) {
		catalog, projection := newBookCatalog(insertBook, updateBook, deleteBook)

		require.NoError(t, apply(t, projection,
			storedBook("0", bookAcquired{}.EventType(), `{"title":"Dune"}`),
			storedBook("1", bookShelved{}.EventType(), `{"shelf":"A3"}`),
			storedBook("1", bookShelved{}.EventType(), `{"shelf":"B7"}`),
			storedBook("0", bookAcquired{}.EventType(), `{"title":"Dune, again"}`),
		))

		want := book{ID: "42", Title: "Dune", Shelf: "A3", Revision: "1"}
		assert.Equal(t, want, mustGet(t, catalog.view, "42"), "an event applied again has to be skipped, as before")
		assert.Equal(t, 1, catalog.changes)
		assert.Equal(t, []architecturekit.Outcome{architecturekit.Added, architecturekit.AlreadyApplied}, catalog.acquired)
	})
}
