package architecturekit

import (
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestStateCache(t *testing.T) {
	t.Run("returns what was put", func(t *testing.T) {
		cache := newStateCache(10)
		key := stateCacheKey{stateType: reflect.TypeFor[int](), subject: "/books/42"}

		cache.put(key, stateShape{}, 7, "3")

		entry, isFound := cache.get(key)
		require.True(t, isFound, "expected the entry to be found")
		assert.Equal(t, 7, entry.state)
		assert.Equal(t, "3", entry.lastEventID)
	})

	t.Run("evicts the least recently used subject", func(t *testing.T) {
		cache := newStateCache(2)
		first := stateCacheKey{stateType: reflect.TypeFor[int](), subject: "/books/1"}
		second := stateCacheKey{stateType: reflect.TypeFor[int](), subject: "/books/2"}
		third := stateCacheKey{stateType: reflect.TypeFor[int](), subject: "/books/3"}

		cache.put(first, stateShape{}, 1, "1")
		cache.put(second, stateShape{}, 2, "2")

		// Using the first one makes the second one the least recently used.
		_, isFound := cache.get(first)
		require.True(t, isFound, "expected the first entry to be found")

		cache.put(third, stateShape{}, 3, "3")

		_, isFound = cache.get(second)
		assert.False(t, isFound, "expected the second entry to be evicted")
		_, isFound = cache.get(first)
		assert.True(t, isFound, "expected the first entry to be kept")
		_, isFound = cache.get(third)
		assert.True(t, isFound, "expected the third entry to be kept")
	})

	t.Run("keeps the state built from the later event", func(t *testing.T) {
		cache := newStateCache(10)
		key := stateCacheKey{stateType: reflect.TypeFor[int](), subject: "/books/42"}

		cache.put(key, stateShape{}, 10, "10")
		cache.put(key, stateShape{}, 9, "9")

		entry, _ := cache.get(key)
		assert.Equal(t, 10, entry.state)
		assert.Equal(t, "10", entry.lastEventID)
	})

	t.Run("keeps states of different types on the same subject apart", func(t *testing.T) {
		cache := newStateCache(10)
		first := stateCacheKey{stateType: reflect.TypeFor[int](), subject: "/books/42"}
		second := stateCacheKey{stateType: reflect.TypeFor[string](), subject: "/books/42"}

		cache.put(first, stateShape{}, 1, "1")
		cache.put(second, stateShape{}, 2, "1")

		entry, _ := cache.get(first)
		assert.Equal(t, 1, entry.state)
	})

	t.Run("keeps a state of another shape from replacing the cached one", func(t *testing.T) {
		cache := newStateCache(10)
		key := stateCacheKey{stateType: reflect.TypeFor[int](), subject: "/books/42"}

		cache.put(key, stateShape{evolved: []string{"borrowed"}}, 1, "1")
		cache.put(key, stateShape{evolved: []string{"returned"}}, 2, "2")

		entry, _ := cache.get(key)
		assert.Equal(t, 1, entry.state, "the cached state must stay")
		assert.Equal(t, []string{"borrowed"}, entry.shape.evolved)
	})

	t.Run("holds at least one subject", func(t *testing.T) {
		cache := newStateCache(0)
		key := stateCacheKey{stateType: reflect.TypeFor[int](), subject: "/books/42"}

		cache.put(key, stateShape{}, 7, "3")

		_, isFound := cache.get(key)
		assert.True(t, isFound, "expected a cache for at least one subject")
	})
}

func TestStateShape(t *testing.T) {
	type book struct{ IsBorrowed bool }

	base := stateShape{
		initial:    book{},
		evolved:    []string{"acquired", "borrowed"},
		upcasted:   []string{"lent"},
		fromLatest: "acquired",
	}

	for _, testCase := range []struct {
		name   string
		change func(shape stateShape) stateShape
		equals bool
	}{
		{"is equal to itself", func(shape stateShape) stateShape { return shape }, true},
		{"is equal to one built alike", func(shape stateShape) stateShape {
			shape.evolved = []string{"acquired", "borrowed"}
			return shape
		}, true},
		{"differs by the initial value", func(shape stateShape) stateShape {
			shape.initial = book{IsBorrowed: true}
			return shape
		}, false},
		{"differs by the evolved event types", func(shape stateShape) stateShape {
			shape.evolved = []string{"acquired", "returned"}
			return shape
		}, false},
		{"differs by the upcasted event types", func(shape stateShape) stateShape {
			shape.upcasted = nil
			return shape
		}, false},
		{"differs by FromLatest", func(shape stateShape) stateShape {
			shape.fromLatest = ""
			return shape
		}, false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.equals, base.equals(testCase.change(base)))
		})
	}

	t.Run("is read from how a state is built", func(t *testing.T) {
		upcasters := NewUpcasters()
		upcasters.byType["lent"] = nil

		state := NewState(book{IsBorrowed: true})
		state.evolve["borrowed"] = nil
		state.evolve["acquired"] = nil
		state.fromLatest = "acquired"
		state.UpcastWith(upcasters)

		assert.Equal(t, stateShape{
			initial:    book{IsBorrowed: true},
			evolved:    []string{"acquired", "borrowed"},
			upcasted:   []string{"lent"},
			fromLatest: "acquired",
		}, shapeOf(state))
	})
}

func TestIsValueType(t *testing.T) {
	type values struct {
		Count    int
		Name     string
		IsActive bool
		Scores   [3]float64
		At       time.Time
		Nested   struct{ Level int }
		hidden   uint8
	}

	type withSlice struct {
		Items []string
	}

	type withNestedMap struct {
		Nested struct{ Index map[string]int }
	}

	for _, testCase := range []struct {
		name    string
		value   any
		isValue bool
	}{
		{name: "an int", value: 0, isValue: true},
		{name: "a string", value: "", isValue: true},
		{name: "a struct of values", value: values{hidden: 1}, isValue: true},
		{name: "a time", value: time.Time{}, isValue: true},
		{name: "an array of values", value: [2]string{}, isValue: true},
		{name: "a slice", value: []int{}, isValue: false},
		{name: "a map", value: map[string]int{}, isValue: false},
		{name: "a pointer", value: new(int), isValue: false},
		{name: "a struct with a slice", value: withSlice{}, isValue: false},
		{name: "a struct with a nested map", value: withNestedMap{}, isValue: false},
		{name: "an array of slices", value: [2][]int{}, isValue: false},
		{name: "a struct with an interface", value: struct{ Any any }{}, isValue: false},
		{name: "a struct with a function", value: struct{ Callback func() }{}, isValue: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			valueType := reflect.TypeOf(testCase.value)
			assert.Equal(t, testCase.isValue, isValueType(valueType))
		})
	}
}

func TestClone(t *testing.T) {
	t.Run("panics when called twice", func(t *testing.T) {
		assert.Panics(t, func() {
			NewState([]int{}).
				Clone(func(state []int) []int { return state }).
				Clone(func(state []int) []int { return state })
		})
	})
}
