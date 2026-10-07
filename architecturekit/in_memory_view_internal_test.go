package architecturekit

import (
	"context"
	"maps"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// shelvedBook is an item whose shelf changes, as a status or a date does.
type shelvedBook struct {
	ID    string
	Shelf string
}

// sizesOf returns how many keys the index holds for every value it knows.
func sizesOf(index *InMemoryIndex[string, shelvedBook, string]) map[string]int {
	sizes := map[string]int{}
	for value, keys := range maps.All(index.keys) {
		sizes[value] = len(keys)
	}

	return sizes
}

func TestInMemoryIndexValues(t *testing.T) {
	t.Run("forgets a value once no item has it", func(t *testing.T) {
		// An index over a value that changes would otherwise grow by one entry
		// for every value it has ever seen, although none of them finds an item.
		ctx := context.Background()
		view := NewInMemoryView(func(item shelvedBook) string { return item.ID })
		byShelf := view.Index(func(item shelvedBook) string { return item.Shelf })

		_, err := view.Insert(ctx, "1", shelvedBook{ID: "a", Shelf: "new"})
		require.NoError(t, err)
		_, err = view.Insert(ctx, "2", shelvedBook{ID: "b", Shelf: "new"})
		require.NoError(t, err)

		_, err = view.Update(ctx, "a", "3", func(item *shelvedBook) { item.Shelf = "returned" })
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"new": 1, "returned": 1}, sizesOf(byShelf), "a value is kept while an item has it")

		_, err = view.Update(ctx, "b", "4", func(item *shelvedBook) { item.Shelf = "returned" })
		require.NoError(t, err)
		assert.Equal(t, map[string]int{"returned": 2}, sizesOf(byShelf), "a value that an update left behind is kept")

		_, err = view.Delete(ctx, "a", "5")
		require.NoError(t, err)
		_, err = view.Delete(ctx, "b", "6")
		require.NoError(t, err)
		assert.Empty(t, sizesOf(byShelf), "a value that a delete left behind is kept")
	})
}
