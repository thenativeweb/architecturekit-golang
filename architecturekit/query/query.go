// Package query runs queries over a view. It knows filtering, ordering,
// paging and projecting, and deliberately nothing else: no joins, no
// aggregation across views, no query language.
//
// Everything here works on iter.Seq2 of an item and an error, the shape in
// which a view hands out its items, so the steps compose and nothing is
// materialised until a result is collected. Sorting is the exception, because
// it cannot be done lazily.
//
// A view that fails while its items are read, such as a cursor of a database
// that loses its connection, hands out the error as an element of its own.
// Every step hands it on unchanged, with the zero value of the item, and ends
// its sequence there, without calling a predicate or any other function for
// it. The functions that turn a sequence into a result, such as Collect,
// return it, unless they have their answer before it comes, as First, Single,
// and Any may.
//
// The package is for reading a view. Items that are already in a slice, such
// as a snapshot that an answer groups in several ways, are better served by
// the slices package and a loop: a slice can not fail, so the errors here
// would never occur.
package query

import (
	"cmp"
	"errors"
	"iter"
	"slices"
)

// ErrNoItems means a query that expected a result found none. An application
// may hand it on to a caller, so its text names neither the package nor a
// sequence.
var ErrNoItems = errors.New("no items")

// ErrTooManyItems means a query that expected one result found several. Like
// ErrNoItems, its text names neither the package nor a sequence.
var ErrTooManyItems = errors.New("more than one item")

// fail hands on the error of a sequence, as its last element.
func fail[TItem any](yield func(TItem, error) bool, err error) {
	var zero TItem
	yield(zero, err)
}

// Where keeps the items the predicate accepts.
func Where[TItem any](
	items iter.Seq2[TItem, error],
	keep func(TItem) bool,
) iter.Seq2[TItem, error] {
	return func(yield func(TItem, error) bool) {
		for item, err := range items {
			if err != nil {
				fail(yield, err)
				return
			}
			if !keep(item) {
				continue
			}
			if !yield(item, nil) {
				return
			}
		}
	}
}

// Select turns every item into something else, which is how a view item
// becomes the shape an answer needs.
func Select[TItem any, TResult any](
	items iter.Seq2[TItem, error],
	convert func(TItem) TResult,
) iter.Seq2[TResult, error] {
	return func(yield func(TResult, error) bool) {
		for item, err := range items {
			if err != nil {
				fail(yield, err)
				return
			}
			if !yield(convert(item), nil) {
				return
			}
		}
	}
}

// OrderBy sorts by a key and hands back a sequence, so that Skip and Take can
// follow. Sorting reads everything, so put Where in front of it.
func OrderBy[TItem any, TKey cmp.Ordered](
	items iter.Seq2[TItem, error],
	key func(TItem) TKey,
) iter.Seq2[TItem, error] {
	return OrderByFunc(items, func(left, right TItem) int {
		return cmp.Compare(key(left), key(right))
	})
}

// OrderByFunc sorts with a comparison function, for orders that no single key
// can express: several fields, a field that is not ordered on its own such as
// a bool, or a custom collation.
//
// The signature is the one the standard library uses, so cmp.Compare and
// cmp.Or compose with it:
//
//	query.OrderByFunc(items, func(left, right ToDo) int {
//		return cmp.Or(
//			left.DueDate.Compare(right.DueDate),
//			cmp.Compare(rank(right.IsPrioritized), rank(left.IsPrioritized)),
//			strings.Compare(left.Title, right.Title),
//		)
//	})
//
// The sort is stable, so items the comparison calls equal keep their order.
//
// The items are read once the sequence is, not before. If reading them fails,
// the sequence hands out the error alone, and none of the items read before
// it, since they are not all there is.
func OrderByFunc[TItem any](
	items iter.Seq2[TItem, error],
	compare func(left, right TItem) int,
) iter.Seq2[TItem, error] {
	return func(yield func(TItem, error) bool) {
		sorted, err := Collect(items)
		if err != nil {
			fail(yield, err)
			return
		}

		slices.SortStableFunc(sorted, compare)

		for _, item := range sorted {
			if !yield(item, nil) {
				return
			}
		}
	}
}

// OrderByDescending sorts the other way round.
func OrderByDescending[TItem any, TKey cmp.Ordered](
	items iter.Seq2[TItem, error],
	key func(TItem) TKey,
) iter.Seq2[TItem, error] {
	return OrderByFunc(items, func(left, right TItem) int {
		return cmp.Compare(key(right), key(left))
	})
}

// Skip passes over the first count items. An error among them is handed on
// all the same, since it is not an item.
func Skip[TItem any](items iter.Seq2[TItem, error], count int) iter.Seq2[TItem, error] {
	return func(yield func(TItem, error) bool) {
		remaining := count
		for item, err := range items {
			if err != nil {
				fail(yield, err)
				return
			}
			if remaining > 0 {
				remaining--
				continue
			}
			if !yield(item, nil) {
				return
			}
		}
	}
}

// Take stops after count items, and reads no further, so an error that would
// come after them goes unnoticed.
func Take[TItem any](items iter.Seq2[TItem, error], count int) iter.Seq2[TItem, error] {
	return func(yield func(TItem, error) bool) {
		if count <= 0 {
			return
		}

		remaining := count
		for item, err := range items {
			if err != nil {
				fail(yield, err)
				return
			}
			if !yield(item, nil) {
				return
			}

			remaining--
			if remaining == 0 {
				return
			}
		}
	}
}

// Collect returns the items as a slice, which is nil if there are none, as
// with slices.Collect. It reads all of them. If that fails, it returns the
// error, and none of the items read before it, so that a part of the result
// is never taken for all of it.
func Collect[TItem any](items iter.Seq2[TItem, error]) ([]TItem, error) {
	var collected []TItem
	for item, err := range items {
		if err != nil {
			return nil, err
		}
		collected = append(collected, item)
	}

	return collected, nil
}

// First returns the first item, if there is one. It reads no further than
// that, so an error that would come after the first item goes unnoticed. An
// error before it is returned, with false.
func First[TItem any](items iter.Seq2[TItem, error]) (TItem, bool, error) {
	for item, err := range items {
		if err != nil {
			var none TItem
			return none, false, err
		}

		return item, true, nil
	}

	var none TItem

	return none, false, nil
}

// Single returns the one item a query expected, and says so when there is none
// or more than one. It reads up to the second item. So an error that comes
// after the first item, but before a second one or the end, is returned, since
// the item can only be called the single one once the sequence has ended. An
// error after a second item goes unnoticed, since there are too many either
// way. With every error, the item is the zero value.
func Single[TItem any](items iter.Seq2[TItem, error]) (TItem, error) {
	var found, none TItem
	seen := false

	for item, err := range items {
		if err != nil {
			return none, err
		}
		if seen {
			return none, ErrTooManyItems
		}
		found = item
		seen = true
	}

	if !seen {
		return none, ErrNoItems
	}

	return found, nil
}

// Count counts the items, reading all of them. If that fails, it returns the
// error with 0, since the items read before it are not all there are.
func Count[TItem any](items iter.Seq2[TItem, error]) (int, error) {
	count := 0
	for _, err := range items {
		if err != nil {
			return 0, err
		}
		count++
	}

	return count, nil
}

// Any reports whether at least one item passes, and stops at the first. An
// error before it is returned, with false, while one that would come after it
// goes unnoticed, since the answer is known by then.
func Any[TItem any](items iter.Seq2[TItem, error], match func(TItem) bool) (bool, error) {
	for item, err := range items {
		if err != nil {
			return false, err
		}
		if match(item) {
			return true, nil
		}
	}

	return false, nil
}
