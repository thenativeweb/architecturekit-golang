// Package query runs queries over a view. It knows filtering, ordering,
// paging and projecting, and deliberately nothing else: no joins, no
// aggregation across views, no query language.
//
// Everything here works on iter.Seq, so the steps compose and nothing is
// materialised until a result is collected. Sorting is the exception, because
// it cannot be done lazily.
//
// What the standard library already does is not repeated here. Use
// slices.Collect to get a slice, and slices.Sorted when the items themselves
// are ordered.
package query

import (
	"cmp"
	"errors"
	"iter"
	"slices"
)

// ErrNoItems means a query that expected a result found none.
var ErrNoItems = errors.New("query: sequence contains no items")

// ErrTooManyItems means a query that expected one result found several.
var ErrTooManyItems = errors.New("query: sequence contains more than one item")

// Where keeps the items the predicate accepts.
func Where[TItem any](items iter.Seq[TItem], keep func(TItem) bool) iter.Seq[TItem] {
	return func(yield func(TItem) bool) {
		for item := range items {
			if !keep(item) {
				continue
			}
			if !yield(item) {
				return
			}
		}
	}
}

// Select turns every item into something else, which is how a view item
// becomes the shape an answer needs.
func Select[TItem any, TResult any](
	items iter.Seq[TItem],
	convert func(TItem) TResult,
) iter.Seq[TResult] {
	return func(yield func(TResult) bool) {
		for item := range items {
			if !yield(convert(item)) {
				return
			}
		}
	}
}

// OrderBy sorts by a key and hands back a sequence, so that Skip and Take can
// follow. Sorting reads everything, so put Where in front of it.
func OrderBy[TItem any, TKey cmp.Ordered](
	items iter.Seq[TItem],
	key func(TItem) TKey,
) iter.Seq[TItem] {
	return slices.Values(slices.SortedStableFunc(items, func(left, right TItem) int {
		return cmp.Compare(key(left), key(right))
	}))
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
func OrderByFunc[TItem any](
	items iter.Seq[TItem],
	compare func(left, right TItem) int,
) iter.Seq[TItem] {
	return slices.Values(slices.SortedStableFunc(items, compare))
}

// OrderByDescending sorts the other way round.
func OrderByDescending[TItem any, TKey cmp.Ordered](
	items iter.Seq[TItem],
	key func(TItem) TKey,
) iter.Seq[TItem] {
	return slices.Values(slices.SortedStableFunc(items, func(left, right TItem) int {
		return cmp.Compare(key(right), key(left))
	}))
}

// Skip passes over the first count items.
func Skip[TItem any](items iter.Seq[TItem], count int) iter.Seq[TItem] {
	return func(yield func(TItem) bool) {
		remaining := count
		for item := range items {
			if remaining > 0 {
				remaining--
				continue
			}
			if !yield(item) {
				return
			}
		}
	}
}

// Take stops after count items, and reads no further.
func Take[TItem any](items iter.Seq[TItem], count int) iter.Seq[TItem] {
	return func(yield func(TItem) bool) {
		if count <= 0 {
			return
		}

		remaining := count
		for item := range items {
			if !yield(item) {
				return
			}

			remaining--
			if remaining == 0 {
				return
			}
		}
	}
}

// First returns the first item, if there is one.
func First[TItem any](items iter.Seq[TItem]) (TItem, bool) {
	for item := range items {
		return item, true
	}

	var none TItem

	return none, false
}

// Single returns the one item a query expected, and says so when there is none
// or more than one.
func Single[TItem any](items iter.Seq[TItem]) (TItem, error) {
	var found TItem
	seen := false

	for item := range items {
		if seen {
			return found, ErrTooManyItems
		}
		found = item
		seen = true
	}

	if !seen {
		return found, ErrNoItems
	}

	return found, nil
}

// Count counts the items, reading all of them.
func Count[TItem any](items iter.Seq[TItem]) int {
	count := 0
	for range items {
		count++
	}

	return count
}

// Any reports whether at least one item passes, and stops at the first.
func Any[TItem any](items iter.Seq[TItem], match func(TItem) bool) bool {
	for item := range items {
		if match(item) {
			return true
		}
	}

	return false
}
