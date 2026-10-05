package architecturekit

import (
	"container/list"
	"maps"
	"math"
	"reflect"
	"slices"
	"sync"
)

// stateCacheKey names a cached state. Besides the subject, the type of the
// state is part of the key, because two states can be built from the same
// subject, and each of them folds the events into something else. It is the
// type rather than the *State, since an application may build a new *State
// for every command, e.g. in a function that returns it.
type stateCacheKey struct {
	stateType reflect.Type
	subject   string
}

type stateCacheEntry struct {
	key         stateCacheKey
	shape       stateShape
	state       any
	lastEventID string
}

// stateShape describes how a state is built, as far as that can be seen from
// outside: the functions themselves can not be compared. Two states of the
// same type on the same subject have to be built alike to share a cached
// state; if they are not, they are two different states, which need two
// different types.
type stateShape struct {
	// source is the *State the shape was built from. Holding it keeps the
	// *State alive, so that no other one can take its address.
	source any

	initial    any
	evolved    []string
	ignored    []string
	upcasted   []string
	fromLatest string
}

// shapeOf describes how the given state is built. It holds a copy of the
// initial value, since the cache keeps the shape for as long as the entry.
func shapeOf[TState any](state *State[TState]) stateShape {
	shape := stateShape{
		source:     state,
		initial:    state.copyOf(state.initial),
		evolved:    slices.Sorted(maps.Keys(state.evolve)),
		ignored:    slices.Sorted(maps.Keys(state.ignored)),
		fromLatest: state.fromLatest,
	}
	if state.upcasters != nil {
		shape.upcasted = slices.Sorted(maps.Keys(state.upcasters.byType))
	}

	return shape
}

// equals reports whether two shapes describe states that are built alike. Two
// shapes of the very same *State are equal without comparing anything. Of two
// different ones, the initial values are compared with isSame, so that an
// initial value equals itself even if it holds a function or a NaN.
func (s stateShape) equals(other stateShape) bool {
	if s.source == other.source {
		return true
	}

	return isSame(reflect.ValueOf(s.initial), reflect.ValueOf(other.initial), map[sameVisit]bool{}) &&
		slices.Equal(s.evolved, other.evolved) &&
		slices.Equal(s.ignored, other.ignored) &&
		slices.Equal(s.upcasted, other.upcasted) &&
		s.fromLatest == other.fromLatest
}

// sameVisit names two references that isSame compares, a pointer, a map or a
// slice each. A slice is named by its length as well, since two slices of
// different lengths may start at the same element.
type sameVisit struct {
	left, right uintptr
	length      int
	valueType   reflect.Type
}

// isSame reports whether two values are deeply equal, as reflect.DeepEqual
// does, except that every value equals itself, which DeepEqual does not
// promise. Two functions are the same if both are nil or both are not, since
// functions can not be compared, and two floats are the same if they are
// equal or both NaN, as are the parts of two complex numbers. Map keys are
// looked up as Go looks them up, though, so a map with a NaN key, which Go
// never finds again, does not equal itself.
//
// visited holds the references compared so far. One that is compared again is
// taken to be the same, so that a cycle ends; if it is not, the comparison
// that is under way finds out.
func isSame(left, right reflect.Value, visited map[sameVisit]bool) bool {
	if !left.IsValid() || !right.IsValid() {
		return left.IsValid() == right.IsValid()
	}
	if left.Type() != right.Type() {
		return false
	}

	switch left.Kind() {
	case reflect.Func:
		return left.IsNil() == right.IsNil()

	case reflect.Float32, reflect.Float64:
		return isSameFloat(left.Float(), right.Float())

	case reflect.Complex64, reflect.Complex128:
		return isSameFloat(real(left.Complex()), real(right.Complex())) &&
			isSameFloat(imag(left.Complex()), imag(right.Complex()))

	case reflect.Interface:
		// The value of a nil interface is invalid, as is the value a nil
		// pointer points to.
		return isSame(left.Elem(), right.Elem(), visited)

	case reflect.Pointer:
		return isVisited(left, right, 0, visited) || isSame(left.Elem(), right.Elem(), visited)

	case reflect.Map:
		if left.IsNil() != right.IsNil() || left.Len() != right.Len() {
			return false
		}
		if isVisited(left, right, 0, visited) {
			return true
		}
		for key, value := range left.Seq2() {
			if !isSame(value, right.MapIndex(key), visited) {
				return false
			}
		}
		return true

	case reflect.Slice:
		if left.IsNil() != right.IsNil() || left.Len() != right.Len() {
			return false
		}
		return isVisited(left, right, left.Len(), visited) || isSameElements(left, right, visited)

	case reflect.Array:
		return isSameElements(left, right, visited)

	case reflect.Struct:
		for i := range left.NumField() {
			if !isSame(left.Field(i), right.Field(i), visited) {
				return false
			}
		}
		return true

	default:
		return left.Equal(right)
	}
}

// isSameFloat reports whether two floats are equal, or both NaN.
func isSameFloat(left, right float64) bool {
	return left == right || (math.IsNaN(left) && math.IsNaN(right))
}

// isSameElements compares the elements of two arrays, or of two slices of
// the same length.
func isSameElements(left, right reflect.Value, visited map[sameVisit]bool) bool {
	for i := range left.Len() {
		if !isSame(left.Index(i), right.Index(i), visited) {
			return false
		}
	}

	return true
}

// isVisited reports whether two references are compared already, and records
// them otherwise.
func isVisited(left, right reflect.Value, length int, visited map[sameVisit]bool) bool {
	visit := sameVisit{left: left.Pointer(), right: right.Pointer(), length: length, valueType: left.Type()}
	if visited[visit] {
		return true
	}

	visited[visit] = true

	return false
}

// stateCache holds the states of the most recently used subjects, together
// with the ID of the last event each of them was built from. Only what was
// read is cached, never what a command has written: without a precondition,
// someone else may have written in between, and reading from the last read
// event onwards picks that up, as well as the command's own events.
type stateCache struct {
	mutex       sync.Mutex
	maxSubjects int
	entries     map[stateCacheKey]*list.Element

	// order holds the entries from the most to the least recently used one,
	// so that the last one is the one to evict.
	order *list.List
}

// newStateCache creates a cache for the given number of subjects, which
// NewStore hands over only if it is at least 1.
func newStateCache(maxSubjects int) *stateCache {
	return &stateCache{
		maxSubjects: maxSubjects,
		entries:     map[stateCacheKey]*list.Element{},
		order:       list.New(),
	}
}

func (c *stateCache) get(key stateCacheKey) (stateCacheEntry, bool) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	element, isCached := c.entries[key]
	if !isCached {
		return stateCacheEntry{}, false
	}

	c.order.MoveToFront(element)

	return *element.Value.(*stateCacheEntry), true
}

// put caches a state, unless a state built from a later event is cached
// already. Two commands on the same subject may finish in any order, and the
// one that read less must not replace the one that read more.
//
// A state of another shape does not replace the cached one either. It is a
// different state of the same type, which get reports the next time.
func (c *stateCache) put(key stateCacheKey, shape stateShape, state any, lastEventID string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if element, isCached := c.entries[key]; isCached {
		entry := element.Value.(*stateCacheEntry)
		if !entry.shape.equals(shape) {
			return
		}
		if isNewer, err := CompareRevisions(lastEventID, entry.lastEventID); err == nil && isNewer >= 0 {
			entry.state = state
			entry.lastEventID = lastEventID
		}

		c.order.MoveToFront(element)
		return
	}

	c.entries[key] = c.order.PushFront(&stateCacheEntry{
		key:         key,
		shape:       shape,
		state:       state,
		lastEventID: lastEventID,
	})

	if c.order.Len() > c.maxSubjects {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*stateCacheEntry).key)
	}
}
