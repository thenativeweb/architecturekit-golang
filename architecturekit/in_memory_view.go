package architecturekit

import (
	"cmp"
	"context"
	"fmt"
	"iter"
	"slices"
	"sync"
)

// InMemoryView holds the items of a read model in memory, each under a key of
// its own. It is the one view the kit ships, and because it keeps no
// checkpoint, its projection is rebuilt on every start.
//
// Every item carries the revision of its own, which is the ID of the last event
// that changed it. An event that is not newer than the item it is about is
// skipped, so that applying the same event twice changes nothing. Apart from
// that, the view has a revision as a whole, which is the last event it has
// seen at all (see Tracking).
//
// All operations take a context and return an error, as a view in a database
// would need to. The view in memory uses neither the context nor, mostly, the
// error, but so a view in a database can offer the same functions later on,
// without the projections that write to it having to change.
//
// Note that this is the stored shape, not the answer to a query. Use the query
// package to filter, order and project it into whatever an answer needs.
type InMemoryView[TKey comparable, TItem any] struct {
	mutex sync.RWMutex

	keyOf      func(TItem) TKey
	revisionIn func(*TItem) *string

	// entries holds the items by key. order keeps the keys in the order in
	// which they were inserted, and may still hold keys that have been deleted
	// since. An entry knows its position in order, so that a key that was
	// deleted and inserted again is only handed out at its new position.
	entries map[TKey]*inMemoryEntry[TItem]
	order   []TKey

	indexes []inMemoryIndexer[TKey, TItem]

	// revision is how far the projection has come, and changed is closed
	// whenever it moves, so that waiting readers wake up.
	revision string
	changed  chan struct{}
}

type inMemoryEntry[TItem any] struct {
	item     TItem
	revision string
	position int
}

// inMemoryIndexer keeps a secondary index up to date. The view calls it with
// every item it adds and every item it removes, and a change is a removal of
// the old item followed by an addition of the new one.
type inMemoryIndexer[TKey comparable, TItem any] interface {
	add(key TKey, item TItem)
	remove(key TKey, item TItem)
}

// InMemoryViewOption configures an InMemoryView.
type InMemoryViewOption[TItem any] func(*inMemoryViewOptions[TItem])

type inMemoryViewOptions[TItem any] struct {
	revisionIn func(*TItem) *string
}

// RevisionIn makes the view keep the revision of every item in a field of the
// item, so that it travels with the item, for example to a caller that hands
// it over to a command later. The function returns the address of that field.
// The view sets the field on every change, so handlers do not.
//
// Without this option, the view keeps the revisions to itself.
func RevisionIn[TItem any](field func(item *TItem) *string) InMemoryViewOption[TItem] {
	return func(options *inMemoryViewOptions[TItem]) {
		options.revisionIn = field
	}
}

// NewInMemoryView creates an empty view, which takes the key of an item from
// the given function.
func NewInMemoryView[TKey comparable, TItem any](
	keyOf func(TItem) TKey,
	options ...InMemoryViewOption[TItem],
) *InMemoryView[TKey, TItem] {
	configured := inMemoryViewOptions[TItem]{}
	for _, option := range options {
		option(&configured)
	}

	return &InMemoryView[TKey, TItem]{
		keyOf:      keyOf,
		revisionIn: configured.revisionIn,
		entries:    map[TKey]*inMemoryEntry[TItem]{},
		changed:    make(chan struct{}),
	}
}

// Revision is the last event the view has seen.
func (v *InMemoryView[TKey, TItem]) Revision() string {
	v.mutex.RLock()
	defer v.mutex.RUnlock()

	return v.revision
}

// Seen records an event as processed. An event the view has already passed
// does not move the revision backwards, which is what makes it safe to apply
// the same event twice after a restart.
func (v *InMemoryView[TKey, TItem]) Seen(eventID string) {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	if newer, err := CompareRevisions(eventID, v.revision); err != nil || newer <= 0 {
		return
	}

	v.revision = eventID

	// Everyone waiting is woken by closing the channel; the next waiter gets a
	// fresh one.
	close(v.changed)
	v.changed = make(chan struct{})
}

// WaitFor returns once the view has reached the revision, or when the context
// ends.
func (v *InMemoryView[TKey, TItem]) WaitFor(ctx context.Context, revision string) error {
	for {
		v.mutex.RLock()
		current, changed := v.revision, v.changed
		v.mutex.RUnlock()

		reached, err := CompareRevisions(current, revision)
		if err != nil {
			return err
		}
		if reached >= 0 {
			return nil
		}

		select {
		case <-changed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
}

// Get returns the item with the given key, and false if there is none.
func (v *InMemoryView[TKey, TItem]) Get(_ context.Context, key TKey) (TItem, bool, error) {
	v.mutex.RLock()
	defer v.mutex.RUnlock()

	entry, isFound := v.entries[key]
	if !isFound {
		var zero TItem
		return zero, false, nil
	}

	return entry.item, true, nil
}

// All hands out every item, in the order in which they were inserted. The
// items are copied under the lock, so a query can take its time without
// blocking the projection that feeds the view.
func (v *InMemoryView[TKey, TItem]) All(context.Context) (iter.Seq[TItem], error) {
	v.mutex.RLock()
	defer v.mutex.RUnlock()

	items := make([]TItem, 0, len(v.entries))
	for position, key := range v.order {
		if entry, isFound := v.entries[key]; isFound && entry.position == position {
			items = append(items, entry.item)
		}
	}

	return slices.Values(items), nil
}

// Insert adds an item for the given event. An item whose key is already taken
// is skipped if the event is not newer than that item, since the event has
// been applied before. If the event is newer, a second item with the same key
// points to a mistake in the events or the key, and Insert fails with an error
// of the category ErrPermanent. To add an item or change an existing one, use
// Upsert.
func (v *InMemoryView[TKey, TItem]) Insert(_ context.Context, eventID string, item TItem) error {
	if err := requireEventID(eventID); err != nil {
		return err
	}

	v.mutex.Lock()
	defer v.mutex.Unlock()

	key := v.keyOf(item)

	if entry, isFound := v.entries[key]; isFound {
		isNewer, err := isNewerThan(eventID, entry.revision)
		if err != nil || !isNewer {
			return err
		}

		return fmt.Errorf("%w: event %s inserts an item with the key %v, which is already taken",
			ErrPermanent, eventID, key)
	}

	v.add(key, item, eventID)

	return nil
}

// Upsert changes the item with the key of the given item, or adds the given
// item if there is none. An existing item is skipped if the event is not newer
// than it.
func (v *InMemoryView[TKey, TItem]) Upsert(
	_ context.Context,
	eventID string,
	item TItem,
	change func(item *TItem),
) error {
	if err := requireEventID(eventID); err != nil {
		return err
	}

	v.mutex.Lock()
	defer v.mutex.Unlock()

	key := v.keyOf(item)

	if _, isFound := v.entries[key]; isFound {
		_, err := v.change(key, eventID, change)
		return err
	}

	v.add(key, item, eventID)

	return nil
}

// Update changes the item with the given key, and reports whether it did. It
// changes nothing if there is no such item, or if the event is not newer than
// the item. Neither is an error, since both happen when events are applied a
// second time: a later event may have deleted the item already.
//
// Changing the key of the item is a mistake, and makes Update fail with an
// error of the category ErrPermanent.
func (v *InMemoryView[TKey, TItem]) Update(
	_ context.Context,
	key TKey,
	eventID string,
	change func(item *TItem),
) (bool, error) {
	if err := requireEventID(eventID); err != nil {
		return false, err
	}

	v.mutex.Lock()
	defer v.mutex.Unlock()

	return v.change(key, eventID, change)
}

// Delete removes the item with the given key, and reports whether it did. Like
// Update, it removes nothing if there is no such item, or if the event is not
// newer than the item.
func (v *InMemoryView[TKey, TItem]) Delete(_ context.Context, key TKey, eventID string) (bool, error) {
	if err := requireEventID(eventID); err != nil {
		return false, err
	}

	v.mutex.Lock()
	defer v.mutex.Unlock()

	return v.delete(key, eventID)
}

// UpdateWhere changes every matching item for which the event is newer, and
// reports how many it changed.
func (v *InMemoryView[TKey, TItem]) UpdateWhere(
	_ context.Context,
	match func(TItem) bool,
	eventID string,
	change func(item *TItem),
) (int, error) {
	if err := requireEventID(eventID); err != nil {
		return 0, err
	}

	v.mutex.Lock()
	defer v.mutex.Unlock()

	return v.changeAll(v.keysWhere(match), eventID, change)
}

// DeleteWhere removes every matching item for which the event is newer, and
// reports how many it removed.
func (v *InMemoryView[TKey, TItem]) DeleteWhere(
	_ context.Context,
	match func(TItem) bool,
	eventID string,
) (int, error) {
	if err := requireEventID(eventID); err != nil {
		return 0, err
	}

	v.mutex.Lock()
	defer v.mutex.Unlock()

	return v.deleteAll(v.keysWhere(match), eventID)
}

// keysWhere returns the keys of the matching items, in the order of the view.
// It expects the lock to be held.
func (v *InMemoryView[TKey, TItem]) keysWhere(match func(TItem) bool) []TKey {
	var keys []TKey
	for position, key := range v.order {
		if entry, isFound := v.entries[key]; isFound && entry.position == position && match(entry.item) {
			keys = append(keys, key)
		}
	}

	return keys
}

// add stores a new item. It expects the lock to be held, and the key to be
// free.
func (v *InMemoryView[TKey, TItem]) add(key TKey, item TItem, eventID string) {
	if v.revisionIn != nil {
		*v.revisionIn(&item) = eventID
	}

	v.entries[key] = &inMemoryEntry[TItem]{item: item, revision: eventID, position: len(v.order)}
	v.order = append(v.order, key)

	for _, index := range v.indexes {
		index.add(key, item)
	}
}

// change applies a change to one item, if the event is newer. It expects the
// lock to be held.
func (v *InMemoryView[TKey, TItem]) change(key TKey, eventID string, change func(item *TItem)) (bool, error) {
	entry, isFound := v.entries[key]
	if !isFound {
		return false, nil
	}

	isNewer, err := isNewerThan(eventID, entry.revision)
	if err != nil || !isNewer {
		return false, err
	}

	changed := entry.item
	change(&changed)

	if newKey := v.keyOf(changed); newKey != key {
		return false, fmt.Errorf("%w: event %s changes the key of an item from %v to %v",
			ErrPermanent, eventID, key, newKey)
	}

	if v.revisionIn != nil {
		*v.revisionIn(&changed) = eventID
	}

	for _, index := range v.indexes {
		index.remove(key, entry.item)
		index.add(key, changed)
	}

	entry.item = changed
	entry.revision = eventID

	return true, nil
}

// changeAll applies a change to the items with the given keys, and counts the
// ones it changed. It expects the lock to be held.
func (v *InMemoryView[TKey, TItem]) changeAll(keys []TKey, eventID string, change func(item *TItem)) (int, error) {
	changed := 0
	for _, key := range keys {
		isChanged, err := v.change(key, eventID, change)
		if err != nil {
			return changed, err
		}
		if isChanged {
			changed++
		}
	}

	return changed, nil
}

// delete removes one item, if the event is newer. It expects the lock to be
// held.
func (v *InMemoryView[TKey, TItem]) delete(key TKey, eventID string) (bool, error) {
	entry, isFound := v.entries[key]
	if !isFound {
		return false, nil
	}

	isNewer, err := isNewerThan(eventID, entry.revision)
	if err != nil || !isNewer {
		return false, err
	}

	for _, index := range v.indexes {
		index.remove(key, entry.item)
	}

	delete(v.entries, key)
	v.compact()

	return true, nil
}

// deleteAll removes the items with the given keys, and counts the ones it
// removed. It expects the lock to be held.
func (v *InMemoryView[TKey, TItem]) deleteAll(keys []TKey, eventID string) (int, error) {
	removed := 0
	for _, key := range keys {
		isRemoved, err := v.delete(key, eventID)
		if err != nil {
			return removed, err
		}
		if isRemoved {
			removed++
		}
	}

	return removed, nil
}

// compact drops the keys of deleted items from the order once they make up
// most of it, so that deleting stays cheap, and the order does not grow
// without bounds. It expects the lock to be held.
func (v *InMemoryView[TKey, TItem]) compact() {
	if len(v.order) < 2*len(v.entries)+16 {
		return
	}

	order := make([]TKey, 0, len(v.entries))
	for position, key := range v.order {
		if entry, isFound := v.entries[key]; isFound && entry.position == position {
			entry.position = len(order)
			order = append(order, key)
		}
	}

	v.order = order
}

// Index adds a secondary index to the view, which finds the items by the value
// the given function takes from them, such as the user they belong to. Several
// items may share a value. The index is kept up to date with every change,
// also when the value of an item changes.
//
// Add indexes before the view is used, since adding one reads every item.
func (v *InMemoryView[TKey, TItem]) Index[TValue comparable](
	valueOf func(TItem) TValue,
) *InMemoryIndex[TKey, TItem, TValue] {
	v.mutex.Lock()
	defer v.mutex.Unlock()

	index := &InMemoryIndex[TKey, TItem, TValue]{
		view:    v,
		valueOf: valueOf,
		keys:    map[TValue]map[TKey]struct{}{},
	}

	for _, key := range v.order {
		if entry, isFound := v.entries[key]; isFound {
			index.add(key, entry.item)
		}
	}

	v.indexes = append(v.indexes, index)

	return index
}

// InMemoryIndex is a secondary index of an InMemoryView, created with Index.
type InMemoryIndex[TKey comparable, TItem any, TValue comparable] struct {
	view    *InMemoryView[TKey, TItem]
	valueOf func(TItem) TValue
	keys    map[TValue]map[TKey]struct{}
}

func (i *InMemoryIndex[TKey, TItem, TValue]) add(key TKey, item TItem) {
	value := i.valueOf(item)

	keys, isFound := i.keys[value]
	if !isFound {
		keys = map[TKey]struct{}{}
		i.keys[value] = keys
	}

	keys[key] = struct{}{}
}

func (i *InMemoryIndex[TKey, TItem, TValue]) remove(key TKey, item TItem) {
	value := i.valueOf(item)

	delete(i.keys[value], key)
	if len(i.keys[value]) == 0 {
		delete(i.keys, value)
	}
}

// keysOf returns the keys of the items with the given value, in the order of
// the view. It expects the lock of the view to be held.
func (i *InMemoryIndex[TKey, TItem, TValue]) keysOf(value TValue) []TKey {
	keys := make([]TKey, 0, len(i.keys[value]))
	for key := range i.keys[value] {
		keys = append(keys, key)
	}

	slices.SortFunc(keys, func(left, right TKey) int {
		return cmp.Compare(i.view.entries[left].position, i.view.entries[right].position)
	})

	return keys
}

// Lookup hands out the items with the given value, in the order of the view.
func (i *InMemoryIndex[TKey, TItem, TValue]) Lookup(_ context.Context, value TValue) (iter.Seq[TItem], error) {
	i.view.mutex.RLock()
	defer i.view.mutex.RUnlock()

	keys := i.keysOf(value)
	items := make([]TItem, 0, len(keys))
	for _, key := range keys {
		items = append(items, i.view.entries[key].item)
	}

	return slices.Values(items), nil
}

// Update changes every item with the given value for which the event is
// newer, and reports how many it changed.
func (i *InMemoryIndex[TKey, TItem, TValue]) Update(
	_ context.Context,
	value TValue,
	eventID string,
	change func(item *TItem),
) (int, error) {
	if err := requireEventID(eventID); err != nil {
		return 0, err
	}

	i.view.mutex.Lock()
	defer i.view.mutex.Unlock()

	return i.view.changeAll(i.keysOf(value), eventID, change)
}

// Delete removes every item with the given value for which the event is
// newer, and reports how many it removed.
func (i *InMemoryIndex[TKey, TItem, TValue]) Delete(_ context.Context, value TValue, eventID string) (int, error) {
	if err := requireEventID(eventID); err != nil {
		return 0, err
	}

	i.view.mutex.Lock()
	defer i.view.mutex.Unlock()

	return i.view.deleteAll(i.keysOf(value), eventID)
}

// requireEventID refuses an empty event ID, which would come before every
// revision, so that every operation with it would be skipped silently.
func requireEventID(eventID string) error {
	if eventID == "" {
		return fmt.Errorf("%w: an operation on a view needs the ID of the event it applies", ErrPermanent)
	}

	return nil
}

// isNewerThan reports whether an event is newer than the revision of an item.
func isNewerThan(eventID, revision string) (bool, error) {
	newer, err := CompareRevisions(eventID, revision)
	if err != nil {
		return false, fmt.Errorf("%w: %w", ErrPermanent, err)
	}

	return newer > 0, nil
}
