package architecturekit

import (
	"reflect"
	"testing"
	"time"
)

func TestStateCacheReturnsWhatWasPut(t *testing.T) {
	cache := newStateCache(10)
	key := stateCacheKey{state: "state", subject: "/books/42"}

	cache.put(key, 7, "3")

	entry, isFound := cache.get(key)
	if !isFound {
		t.Fatal("expected the entry to be found")
	}
	if entry.state != 7 || entry.lastEventID != "3" {
		t.Fatalf("got %v at %q, want 7 at \"3\"", entry.state, entry.lastEventID)
	}
}

func TestStateCacheEvictsTheLeastRecentlyUsedSubject(t *testing.T) {
	cache := newStateCache(2)
	first := stateCacheKey{state: "state", subject: "/books/1"}
	second := stateCacheKey{state: "state", subject: "/books/2"}
	third := stateCacheKey{state: "state", subject: "/books/3"}

	cache.put(first, 1, "1")
	cache.put(second, 2, "2")

	// Using the first one makes the second one the least recently used.
	if _, isFound := cache.get(first); !isFound {
		t.Fatal("expected the first entry to be found")
	}

	cache.put(third, 3, "3")

	if _, isFound := cache.get(second); isFound {
		t.Fatal("expected the second entry to be evicted")
	}
	if _, isFound := cache.get(first); !isFound {
		t.Fatal("expected the first entry to be kept")
	}
	if _, isFound := cache.get(third); !isFound {
		t.Fatal("expected the third entry to be kept")
	}
}

func TestStateCacheKeepsTheStateBuiltFromTheLaterEvent(t *testing.T) {
	cache := newStateCache(10)
	key := stateCacheKey{state: "state", subject: "/books/42"}

	cache.put(key, 10, "10")
	cache.put(key, 9, "9")

	entry, _ := cache.get(key)
	if entry.state != 10 || entry.lastEventID != "10" {
		t.Fatalf("got %v at %q, want 10 at \"10\"", entry.state, entry.lastEventID)
	}
}

func TestStateCacheKeepsStatesOfTheSameSubjectApart(t *testing.T) {
	cache := newStateCache(10)
	first := stateCacheKey{state: "first state", subject: "/books/42"}
	second := stateCacheKey{state: "second state", subject: "/books/42"}

	cache.put(first, 1, "1")
	cache.put(second, 2, "1")

	entry, _ := cache.get(first)
	if entry.state != 1 {
		t.Fatalf("got %v, want 1", entry.state)
	}
}

func TestStateCacheHoldsAtLeastOneSubject(t *testing.T) {
	cache := newStateCache(0)
	key := stateCacheKey{state: "state", subject: "/books/42"}

	cache.put(key, 7, "3")

	if _, isFound := cache.get(key); !isFound {
		t.Fatal("expected a cache for at least one subject")
	}
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
		value   any
		isValue bool
	}{
		{value: 0, isValue: true},
		{value: "", isValue: true},
		{value: values{hidden: 1}, isValue: true},
		{value: time.Time{}, isValue: true},
		{value: [2]string{}, isValue: true},
		{value: []int{}, isValue: false},
		{value: map[string]int{}, isValue: false},
		{value: new(int), isValue: false},
		{value: withSlice{}, isValue: false},
		{value: withNestedMap{}, isValue: false},
		{value: [2][]int{}, isValue: false},
		{value: struct{ Any any }{}, isValue: false},
		{value: struct{ Callback func() }{}, isValue: false},
	} {
		valueType := reflect.TypeOf(testCase.value)
		if got := isValueType(valueType); got != testCase.isValue {
			t.Errorf("%v: got %t, want %t", valueType, got, testCase.isValue)
		}
	}
}

func TestClonePanicsWhenCalledTwice(t *testing.T) {
	defer func() {
		if recover() == nil {
			t.Fatal("expected a panic for a second clone function")
		}
	}()

	NewState([]int{}).
		Clone(func(state []int) []int { return state }).
		Clone(func(state []int) []int { return state })
}
