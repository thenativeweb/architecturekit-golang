package architecturekit

import (
	"container/list"
	"sync"
)

// stateCacheKey names a cached state. The state is part of the key, because
// two states can be built from the same subject, and each of them folds the
// events into something else.
type stateCacheKey struct {
	state   any
	subject string
}

type stateCacheEntry struct {
	key         stateCacheKey
	state       any
	lastEventID string
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

func newStateCache(maxSubjects int) *stateCache {
	return &stateCache{
		maxSubjects: max(maxSubjects, 1),
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
func (c *stateCache) put(key stateCacheKey, state any, lastEventID string) {
	c.mutex.Lock()
	defer c.mutex.Unlock()

	if element, isCached := c.entries[key]; isCached {
		entry := element.Value.(*stateCacheEntry)
		if isNewer, err := CompareRevisions(lastEventID, entry.lastEventID); err == nil && isNewer >= 0 {
			entry.state = state
			entry.lastEventID = lastEventID
		}

		c.order.MoveToFront(element)
		return
	}

	c.entries[key] = c.order.PushFront(&stateCacheEntry{
		key:         key,
		state:       state,
		lastEventID: lastEventID,
	})

	if c.order.Len() > c.maxSubjects {
		oldest := c.order.Back()
		c.order.Remove(oldest)
		delete(c.entries, oldest.Value.(*stateCacheEntry).key)
	}
}
