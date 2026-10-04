package architecturekit

import (
	"container/list"
	"maps"
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

func (s stateShape) equals(other stateShape) bool {
	return reflect.DeepEqual(s.initial, other.initial) &&
		slices.Equal(s.evolved, other.evolved) &&
		slices.Equal(s.ignored, other.ignored) &&
		slices.Equal(s.upcasted, other.upcasted) &&
		s.fromLatest == other.fromLatest
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
