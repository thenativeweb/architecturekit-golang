package architecturekit

import (
	"math"
	"reflect"
	"slices"
	"strconv"
	"testing"
	"time"
	"unsafe"

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

		cache.put(key, stateShape{source: NewState(0), evolved: []string{"borrowed"}}, 1, "1")
		cache.put(key, stateShape{source: NewState(0), evolved: []string{"returned"}}, 2, "2")

		entry, _ := cache.get(key)
		assert.Equal(t, 1, entry.state, "the cached state must stay")
		assert.Equal(t, []string{"borrowed"}, entry.shape.evolved)
	})
}

func TestStateShape(t *testing.T) {
	type book struct{ IsBorrowed bool }

	base := stateShape{
		source:     NewState(book{}),
		initial:    book{},
		evolved:    []string{"acquired", "borrowed", "reviewed"},
		ignored:    []string{"reviewed"},
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
			shape.evolved = []string{"acquired", "borrowed", "reviewed"}
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
		{"differs by the ignored event types", func(shape stateShape) stateShape {
			// One state ignores what the other evolves by.
			shape.ignored = nil
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
			// The shapes are of two different states, so they are compared.
			changed := testCase.change(base)
			changed.source = NewState(book{})

			assert.Equal(t, testCase.equals, base.equals(changed))
		})
	}

	t.Run("is equal to a shape of the very same state without comparing anything", func(t *testing.T) {
		changed := base
		changed.initial = book{IsBorrowed: true}

		assert.True(t, base.equals(changed))
	})

	t.Run("is equal for the very same state although its initial value does not equal itself", func(t *testing.T) {
		// Go never finds a NaN key again, so the initial value differs from
		// itself even when compared by isSame, but the very same state is not
		// compared at all.
		state := NewState(map[float64]int{math.NaN(): 1})
		alike := NewState(map[float64]int{math.NaN(): 1})

		assert.True(t, shapeOf(state).equals(shapeOf(state)))
		assert.False(t, shapeOf(state).equals(shapeOf(alike)))
	})

	t.Run("is equal for states built alike whose initial values hold a function or a NaN", func(t *testing.T) {
		type formatted struct {
			Format func(int) string
			Mean   float64
		}

		build := func() *State[formatted] {
			return NewState(formatted{Format: strconv.Itoa, Mean: math.NaN()})
		}

		assert.True(t, shapeOf(build()).equals(shapeOf(build())))
		assert.False(t, shapeOf(build()).equals(shapeOf(NewState(formatted{Mean: math.NaN()}))),
			"a nil function must differ from one that is not")
		assert.False(t, shapeOf(build()).equals(shapeOf(NewState(formatted{Format: strconv.Itoa}))),
			"a NaN must differ from 0")
	})

	t.Run("is read from how a state is built", func(t *testing.T) {
		upcasters := NewUpcasters()
		upcasters.byType["lent"] = nil

		state := NewState(book{IsBorrowed: true})
		state.evolve["borrowed"] = nil
		state.evolve["acquired"] = nil
		state.evolve["reviewed"] = nil
		state.ignored["reviewed"] = true
		state.fromLatest = "acquired"
		state.UpcastWith(upcasters)

		assert.Equal(t, stateShape{
			source:     state,
			initial:    book{IsBorrowed: true},
			evolved:    []string{"acquired", "borrowed", "reviewed"},
			ignored:    []string{"reviewed"},
			upcasted:   []string{"lent"},
			fromLatest: "acquired",
		}, shapeOf(state))
	})
}

func TestIsSame(t *testing.T) {
	type withHidden struct {
		count  int
		format func(int) string
		mean   float64
	}

	type node struct {
		Value float64
		Next  *node
	}

	nan := math.NaN()
	negativeZero := math.Copysign(0, -1)
	pointsTo := func(value float64) *float64 { return &value }
	shared := pointsTo(1)
	channel := make(chan int)

	for _, testCase := range []struct {
		name        string
		left, right any
		isSame      bool
	}{
		{"two nil values", nil, nil, true},
		{"nil and a value", nil, 0, false},
		{"values of different types", 1, int64(1), false},
		{"floats of different types", float32(1), 1.0, false},
		{"equal ints", 1, 1, true},
		{"different ints", 1, 2, false},
		{"equal strings", "42", "42", true},
		{"different strings", "42", "23", false},
		{"the same channel", channel, channel, true},
		{"different channels", channel, make(chan int), false},
		{"two nil functions", (func())(nil), (func())(nil), true},
		{"two functions that are not nil", strconv.Itoa, func(int) string { return "" }, true},
		{"a nil function and one that is not", (func(int) string)(nil), strconv.Itoa, false},
		{"NaN and NaN", nan, nan, true},
		{"NaN of float32 and NaN of float32", float32(nan), float32(nan), true},
		{"equal floats", 1.5, 1.5, true},
		{"different floats", 1.5, 2.5, false},
		{"0 and -0, whose bits differ", 0.0, negativeZero, false},
		{"complex numbers with NaN", complex(nan, nan), complex(nan, nan), true},
		{"complex numbers of float32 with NaN", complex64(complex(nan, 1)), complex64(complex(nan, 1)), true},
		{"complex numbers with different real parts", complex(1, 2), complex(3, 2), false},
		{"complex numbers with different imaginary parts", complex(1, 2), complex(1, 3), false},
		{"interfaces that hold NaN", struct{ Any any }{nan}, struct{ Any any }{nan}, true},
		{"interfaces that hold different values", struct{ Any any }{1}, struct{ Any any }{2}, false},
		{"a nil interface and one that holds a value", struct{ Any any }{}, struct{ Any any }{1}, false},
		{"two nil pointers", (*float64)(nil), (*float64)(nil), true},
		{"a nil pointer and one that is not", (*float64)(nil), pointsTo(1), false},
		{"pointers to NaN", pointsTo(nan), pointsTo(nan), true},
		{"pointers to different values", pointsTo(1), pointsTo(2), false},
		{"a pointer that appears twice and two pointers to different values", []*float64{shared, shared},
			[]*float64{pointsTo(1), pointsTo(2)}, false},
		{"maps with NaN values", map[string]float64{"mean": nan}, map[string]float64{"mean": nan}, true},
		{"a nil map and an empty one", map[string]int(nil), map[string]int{}, false},
		{"maps of different lengths", map[string]int{"a": 1}, map[string]int{"a": 1, "b": 2}, false},
		{"maps with different keys", map[string]int{"a": 1}, map[string]int{"b": 1}, false},
		{"maps with different values", map[string]int{"a": 1}, map[string]int{"a": 2}, false},
		{"maps with a NaN key, which Go never finds again", map[float64]int{nan: 1}, map[float64]int{nan: 1}, false},
		{"slices of functions", []func(){func() {}}, []func(){func() {}}, true},
		{"slices with NaN", []float64{1, nan}, []float64{1, nan}, true},
		{"a nil slice and an empty one", []int(nil), []int{}, false},
		{"slices of different lengths", []int{1}, []int{1, 2}, false},
		{"slices with different elements", []int{1, 2}, []int{1, 3}, false},
		{"arrays with NaN", [2]float64{nan, 1}, [2]float64{nan, 1}, true},
		{"arrays with different elements", [2]int{1, 2}, [2]int{1, 3}, false},
		{"structs with unexported functions and NaN", withHidden{1, strconv.Itoa, nan}, withHidden{1, strconv.Itoa, nan}, true},
		{"structs that differ in an unexported field", withHidden{1, nil, nan}, withHidden{2, nil, nan}, false},
		{"times with a location", time.Date(2026, 10, 5, 0, 0, 0, 0, time.FixedZone("CEST", 7200)),
			time.Date(2026, 10, 5, 0, 0, 0, 0, time.FixedZone("CEST", 7200)), true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			left, right := reflect.ValueOf(testCase.left), reflect.ValueOf(testCase.right)

			assert.Equal(t, testCase.isSame, isSame(left, right, map[sameVisit]bool{}))
			assert.Equal(t, testCase.isSame, isSame(right, left, map[sameVisit]bool{}), "the comparison must be symmetric")
		})
	}

	t.Run("ends a cycle of pointers", func(t *testing.T) {
		cycle := func(value float64) *node {
			first := &node{Value: value}
			first.Next = &node{Value: nan, Next: first}
			return first
		}

		assert.True(t, isSame(reflect.ValueOf(cycle(1)), reflect.ValueOf(cycle(1)), map[sameVisit]bool{}))
		assert.False(t, isSame(reflect.ValueOf(cycle(1)), reflect.ValueOf(cycle(2)), map[sameVisit]bool{}))
	})

	t.Run("ends a cycle of maps", func(t *testing.T) {
		cycle := func(value float64) map[string]any {
			cycle := map[string]any{"value": value}
			cycle["self"] = cycle
			return cycle
		}

		assert.True(t, isSame(reflect.ValueOf(cycle(nan)), reflect.ValueOf(cycle(nan)), map[sameVisit]bool{}))
		assert.False(t, isSame(reflect.ValueOf(cycle(1)), reflect.ValueOf(cycle(2)), map[sameVisit]bool{}))
	})

	t.Run("ends a cycle of slices", func(t *testing.T) {
		cycle := func(value float64) []any {
			cycle := []any{nil, value}
			cycle[0] = cycle
			return cycle
		}

		assert.True(t, isSame(reflect.ValueOf(cycle(nan)), reflect.ValueOf(cycle(nan)), map[sameVisit]bool{}))
		assert.False(t, isSame(reflect.ValueOf(cycle(1)), reflect.ValueOf(cycle(2)), map[sameVisit]bool{}))
	})

	t.Run("tells apart pointers of different types to the same address", func(t *testing.T) {
		type pair struct{ First, Second int }

		left := &pair{First: 1, Second: 2}
		right := &pair{First: 1, Second: 3}

		// The first fields are the same, and their addresses are those of the
		// pairs, which differ in their second fields.
		assert.False(t, isSame(
			reflect.ValueOf([]any{&left.First, left}),
			reflect.ValueOf([]any{&right.First, right}),
			map[sameVisit]bool{}))
	})

	t.Run("tells apart slices of different lengths that start at the same elements", func(t *testing.T) {
		left := []int{1, 2}
		right := []int{1, 3}

		// The first elements are the same, the second ones are not, so the
		// longer slices differ, although they start where the shorter ones do.
		assert.False(t, isSame(
			reflect.ValueOf([][]int{left[:1], left[:2]}),
			reflect.ValueOf([][]int{right[:1], right[:2]}),
			map[sameVisit]bool{}))
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

func TestSharesData(t *testing.T) {
	type withMap struct {
		Index map[string]int
	}

	type withHiddenSlice struct {
		Count int
		items []string
	}

	type withNestedPointer struct {
		Nested struct{ Next *int }
	}

	type withTime struct {
		At time.Time
	}

	for _, testCase := range []struct {
		name   string
		value  any
		shares bool
	}{
		{name: "an int", value: 0, shares: false},
		{name: "a string", value: "text", shares: false},
		{name: "a struct of values", value: struct{ Count int }{Count: 1}, shares: false},
		{name: "a nil map", value: withMap{}, shares: false},
		{name: "an empty map", value: withMap{Index: map[string]int{}}, shares: true},
		{name: "a nil slice", value: []int(nil), shares: false},
		{name: "an empty slice without room for elements", value: []int{}, shares: false},
		{name: "an empty slice with room for elements", value: make([]int, 0, 1), shares: true},
		{name: "a slice with elements", value: []int{1}, shares: true},
		{name: "a nil pointer", value: (*int)(nil), shares: false},
		{name: "a pointer", value: new(int), shares: true},
		{name: "a nil unsafe pointer", value: unsafe.Pointer(nil), shares: false},
		{name: "an unsafe pointer", value: unsafe.Pointer(new(int)), shares: true},
		{name: "a nil channel", value: (chan int)(nil), shares: false},
		{name: "a channel", value: make(chan int), shares: true},
		{name: "a function", value: func() {}, shares: false},
		{name: "a nil interface", value: struct{ Any any }{}, shares: false},
		{name: "an interface that holds a value", value: struct{ Any any }{Any: 1}, shares: false},
		{name: "an interface that holds a nil map", value: struct{ Any any }{Any: map[string]int(nil)}, shares: false},
		{name: "an interface that holds a map", value: struct{ Any any }{Any: map[string]int{}}, shares: true},
		{name: "an array of nil maps", value: [2]map[string]int{}, shares: false},
		{name: "an array with a map in its last element", value: [2]map[string]int{1: {}}, shares: true},
		{name: "an empty array", value: [0]map[string]int{}, shares: false},
		{name: "an unexported slice with elements", value: withHiddenSlice{items: []string{"a"}}, shares: true},
		{name: "an unexported nil slice", value: withHiddenSlice{Count: 1}, shares: false},
		{name: "a nested pointer", value: withNestedPointer{Nested: struct{ Next *int }{Next: new(int)}}, shares: true},
		{name: "a nested nil pointer", value: withNestedPointer{}, shares: false},
		{name: "a time with a location", value: withTime{At: time.Date(2026, 10, 2, 0, 0, 0, 0, time.FixedZone("CEST", 7200))}, shares: false},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			assert.Equal(t, testCase.shares, sharesData(reflect.ValueOf(testCase.value)))
		})
	}

	t.Run("looks at a state of an interface type through the interface", func(t *testing.T) {
		var shared any = map[string]int{}
		var empty any

		assert.True(t, sharesData(reflect.ValueOf(&shared).Elem()))
		assert.False(t, sharesData(reflect.ValueOf(&empty).Elem()))
	})
}

func TestShapeOf(t *testing.T) {
	type shelf struct{ BookIDs []string }

	t.Run("holds a copy of the initial value", func(t *testing.T) {
		state := NewState(shelf{BookIDs: []string{"42"}}).
			Clone(func(current shelf) shelf { return shelf{BookIDs: slices.Clone(current.BookIDs)} })

		shape := shapeOf(state)
		shape.initial.(shelf).BookIDs[0] = "23"

		assert.Equal(t, []string{"42"}, state.initial.BookIDs, "changing the shape changed the initial value")
	})
}

func TestCopyOfInitial(t *testing.T) {
	t.Run("returns a copy that shares no data with the initial value", func(t *testing.T) {
		type shelf struct{ BookIDs []string }

		state := NewState(shelf{BookIDs: []string{"42"}}).
			Clone(func(current shelf) shelf { return shelf{BookIDs: slices.Clone(current.BookIDs)} })

		initial, err := state.copyOfInitial()
		require.NoError(t, err)
		initial.BookIDs[0] = "23"

		assert.Equal(t, []string{"42"}, state.initial.BookIDs, "changing the copy changed the initial value")
	})

	t.Run("refuses an initial value that shares data, without a Clone function", func(t *testing.T) {
		state := NewState(map[string]int{})

		initial, err := state.copyOfInitial()

		require.ErrorIs(t, err, ErrPermanent)
		assert.EqualError(t, err, "permanent failure: map[string]int holds slices, maps, pointers or "+
			"channels in its initial value, so it needs a Clone function to start every read from a copy of the "+
			"initial value")
		assert.Nil(t, initial, "the initial value must not be handed out")
	})
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
