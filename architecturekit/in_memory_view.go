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
// An item is shared with every reader who got it, since Get and All hand out
// plain copies, which share the slices, maps, and pointees of the item with
// the view. So a reader never changes an item it got, and a change replaces
// the fields that hold such data rather than writing into them, unless the
// view clones every item before a change (see CloneWith). A time.Time counts
// as a value.
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
	clone      func(TItem) TItem

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
	clone      func(TItem) TItem
}

// RevisionIn makes the view keep the revision of every item in a field of the
// item, so that it travels with the item, for example to a caller that hands
// it over to a command later. The function returns the address of that field.
// The view sets the field on every change, so handlers do not.
//
// The revision of an item fits a precondition on the last event of a subject,
// such as eventsourcingdb.NewIsSubjectOnEventIDPrecondition, only if the item
// stands for exactly one subject, and the projection applies every event type
// of that subject to the item. Otherwise the two drift apart as soon as an
// event lands in the subject that the view does not apply to the item, and
// every command with the revision of the item fails with a conflict, until an
// event changes the item again. For an event type the item does not change
// for, such as one the state ignores, call Update with a change that does
// nothing, which only moves the revision on. For an item that gathers several
// subjects, use OnStateRead instead.
//
// Without this option, the view keeps the revisions to itself.
func RevisionIn[TItem any](field func(item *TItem) *string) InMemoryViewOption[TItem] {
	return func(options *inMemoryViewOptions[TItem]) {
		options.revisionIn = field
	}
}

// CloneWith makes the view clone an item before every change, and hand the
// clone to the change, so that the change may write into the slices, maps,
// and pointees of the item without changing what a reader got before. The
// function must return a copy that shares no data with the original, like the
// Clone function of a State. The view calls it whenever it changes an existing
// item, with Update, Upsert, UpdateWhere, or the Update function of an index,
// but not for a new item, which nobody has read yet.
//
// Without this option, a change gets a plain copy of the item, which shares
// its slices, maps, and pointees with every reader who got the item before, so
// it replaces the fields that hold such data rather than writing into them.
//
// A nil function, or giving CloneWith twice, is a programming error, so it
// panics while the view is being built.
func CloneWith[TItem any](clone func(item TItem) TItem) InMemoryViewOption[TItem] {
	if clone == nil {
		panic("architecturekit: CloneWith needs a function, not nil")
	}

	return func(options *inMemoryViewOptions[TItem]) {
		if options.clone != nil {
			panic("architecturekit: CloneWith is given twice")
		}

		options.clone = clone
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
		clone:      configured.clone,
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
//
// The item is a plain copy, which shares its slices, maps, and pointees with
// the view and with every other reader. So never change it, not even through
// a pointer. A later change of the item leaves what you got alone only if it
// replaces such data rather than writing into it, or if the view clones items
// with CloneWith (see Update).
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
//
// Like with Get, every item is a plain copy, which shares its slices, maps,
// and pointees with the view and with every other reader, so never change it.
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

// Upsert changes the item with the given key, or adds one if there is none.
// The change starts from the existing item, or from the zero value of TItem,
// so a single function describes both, and it has to set the fields that make
// up the key. An existing item is skipped if the event is not newer than it.
//
// Like with Update, the change of an existing item replaces the fields that
// hold slices, maps, or pointers rather than writing into them, since every
// reader who got the item before shares them, unless the view clones items
// with CloneWith. A new item starts from the zero value, which nobody shares.
//
// An item whose key, after the change, differs from the given one is a mistake,
// and makes Upsert fail with an error of the category ErrPermanent.
func (v *InMemoryView[TKey, TItem]) Upsert(
	_ context.Context,
	key TKey,
	eventID string,
	change func(item *TItem),
) error {
	if err := requireEventID(eventID); err != nil {
		return err
	}

	v.mutex.Lock()
	defer v.mutex.Unlock()

	if _, isFound := v.entries[key]; isFound {
		_, err := v.change(key, eventID, change)
		return err
	}

	var item TItem
	change(&item)

	if itemKey := v.keyOf(item); itemKey != key {
		return fmt.Errorf("%w: event %s upserts an item with the key %v under the key %v",
			ErrPermanent, eventID, itemKey, key)
	}

	v.add(key, item, eventID)

	return nil
}

// Update changes the item with the given key, and reports what it did: Applied
// if it changed the item, Missing if there is no such item, and AlreadyApplied
// if the event is not newer than the item. Neither of the latter is an error,
// since both happen when events are applied a second time: a later event may
// have deleted the item already. A projection for which a missing item means
// that something is wrong says so itself.
//
// The change gets a plain copy of the item, which shares its slices, maps, and
// pointees with every reader who got the item before. So it replaces the
// fields that hold such data rather than writing into them, for example with
// append(slices.Clone(item.BookIDs), bookID), since writing into them would
// change what those readers hold while they may be reading it. With
// CloneWith, the change gets a clone instead, and may write freely. A change
// that does nothing still moves the revision of the item on.
//
// Changing the key of the item is a mistake, and makes Update fail with an
// error of the category ErrPermanent.
func (v *InMemoryView[TKey, TItem]) Update(
	_ context.Context,
	key TKey,
	eventID string,
	change func(item *TItem),
) (Outcome, error) {
	if err := requireEventID(eventID); err != nil {
		return 0, err
	}

	v.mutex.Lock()
	defer v.mutex.Unlock()

	return v.change(key, eventID, change)
}

// Delete removes the item with the given key, and reports what it did, like
// Update: Applied if it removed the item, Missing if there is no such item, and
// AlreadyApplied if the event is not newer than the item.
func (v *InMemoryView[TKey, TItem]) Delete(_ context.Context, key TKey, eventID string) (Outcome, error) {
	if err := requireEventID(eventID); err != nil {
		return 0, err
	}

	v.mutex.Lock()
	defer v.mutex.Unlock()

	return v.delete(key, eventID)
}

// UpdateWhere changes every matching item for which the event is newer, and
// reports how many it changed.
//
// Like with Update, the change replaces the fields that hold slices, maps, or
// pointers rather than writing into them, since every reader who got an item
// before shares them, unless the view clones items with CloneWith.
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
func (v *InMemoryView[TKey, TItem]) change(key TKey, eventID string, change func(item *TItem)) (Outcome, error) {
	entry, isFound := v.entries[key]
	if !isFound {
		return Missing, nil
	}

	isNewer, err := isNewerThan(eventID, entry.revision)
	if err != nil {
		return 0, err
	}
	if !isNewer {
		return AlreadyApplied, nil
	}

	changed := entry.item
	if v.clone != nil {
		changed = v.clone(entry.item)
	}
	change(&changed)

	if newKey := v.keyOf(changed); newKey != key {
		return 0, fmt.Errorf("%w: event %s changes the key of an item from %v to %v",
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

	return Applied, nil
}

// changeAll applies a change to the items with the given keys, and counts the
// ones it changed. It expects the lock to be held.
func (v *InMemoryView[TKey, TItem]) changeAll(keys []TKey, eventID string, change func(item *TItem)) (int, error) {
	changed := 0
	for _, key := range keys {
		outcome, err := v.change(key, eventID, change)
		if err != nil {
			return changed, err
		}
		if outcome == Applied {
			changed++
		}
	}

	return changed, nil
}

// delete removes one item, if the event is newer. It expects the lock to be
// held.
func (v *InMemoryView[TKey, TItem]) delete(key TKey, eventID string) (Outcome, error) {
	entry, isFound := v.entries[key]
	if !isFound {
		return Missing, nil
	}

	isNewer, err := isNewerThan(eventID, entry.revision)
	if err != nil {
		return 0, err
	}
	if !isNewer {
		return AlreadyApplied, nil
	}

	for _, index := range v.indexes {
		index.remove(key, entry.item)
	}

	delete(v.entries, key)
	v.compact()

	return Applied, nil
}

// deleteAll removes the items with the given keys, and counts the ones it
// removed. It expects the lock to be held.
func (v *InMemoryView[TKey, TItem]) deleteAll(keys []TKey, eventID string) (int, error) {
	removed := 0
	for _, key := range keys {
		outcome, err := v.delete(key, eventID)
		if err != nil {
			return removed, err
		}
		if outcome == Applied {
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
// also when the value of an item changes. A value that is taken from a slice,
// a map, or a pointee of the item is only followed if a change replaces that
// data rather than writing into it, or if the view clones items with
// CloneWith, since the index takes the old value from the item as it was
// before the change.
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
// Like with All, they share their slices, maps, and pointees with the view, so
// never change them.
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
// newer, and reports how many it changed. Like with the Update function of the
// view, the change replaces the fields that hold slices, maps, or pointers
// rather than writing into them, unless the view clones items with CloneWith.
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
