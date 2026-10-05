package query_test

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"iter"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/query"
)

type book struct {
	Title string
	Year  int
}

// String lets a trace read as titles.
func (b book) String() string { return b.Title }

var library = []book{
	{Title: "Neuromancer", Year: 1984},
	{Title: "Dune", Year: 1965},
	{Title: "Snow Crash", Year: 1992},
	{Title: "Foundation", Year: 1951},
}

// sequenceOf hands out the items the way a view does that never fails.
func sequenceOf[TItem any](items []TItem) iter.Seq2[TItem, error] {
	return func(yield func(TItem, error) bool) {
		for _, item := range items {
			if !yield(item, nil) {
				return
			}
		}
	}
}

func books() iter.Seq2[book, error] {
	return sequenceOf(library)
}

func titlesOf(items []book) []string {
	var titles []string
	for _, item := range items {
		titles = append(titles, item.Title)
	}

	return titles
}

func titles(t *testing.T, items iter.Seq2[book, error]) []string {
	t.Helper()

	got, err := query.Collect(query.Select(items, func(b book) string { return b.Title }))
	require.NoError(t, err)

	return got
}

var errCursor = errors.New("cursor lost")

// garbage is what the cursor hands out along with its error, so that a step
// that handed on that item, rather than the zero value, would show.
var garbage = book{Title: "garbage", Year: 9999}

// cursor is a view that hands out the library like a cursor of a database
// which fails at the given position: before the first book for 0, after the
// last one for len(library), and never for -1. Unlike a real view, it goes on
// after the error for as long as the caller does, so that a step that read on
// would show. It counts every element it hands out.
type cursor struct {
	failAt int
	pulled int
}

var _ architecturekit.View[book] = (*cursor)(nil)

func failingAt(position int) *cursor {
	return &cursor{failAt: position}
}

func counting() *cursor {
	return failingAt(-1)
}

func (c *cursor) All(context.Context) iter.Seq2[book, error] {
	return func(yield func(book, error) bool) {
		for position := 0; position <= len(library); position++ {
			if position == c.failAt {
				c.pulled++
				if !yield(garbage, errCursor) {
					return
				}
			}
			if position == len(library) {
				return
			}

			c.pulled++
			if !yield(library[position], nil) {
				return
			}
		}
	}
}

func (c *cursor) items() iter.Seq2[book, error] {
	return c.All(context.Background())
}

// trace runs over a sequence to its end, also past an error, and writes down
// every element, so that anything handed out after an error shows, and so
// does an error that comes with an item rather than the zero value.
func trace[TItem comparable](items iter.Seq2[TItem, error]) []string {
	var got []string
	for item, err := range items {
		var zero TItem
		switch {
		case err != nil && item != zero:
			got = append(got, fmt.Sprintf("error %q with %v", err, item))
		case err != nil:
			got = append(got, fmt.Sprintf("error %q", err))
		default:
			got = append(got, fmt.Sprint(item))
		}
	}

	return got
}

const handedOn = `error "cursor lost"`

// positions are where the cursor fails, relative to the four books.
var positions = []struct {
	label  string
	failAt int
}{
	{"at the start", 0},
	{"in the middle", 2},
	{"at the end", 4},
}

// expectHandedOn checks that a step handed out what it was expected to before
// the error, then the error, and nothing after it, and that it read no further
// than the error.
func expectHandedOn(t *testing.T, source *cursor, got []string, before ...string) {
	t.Helper()

	assert.Equal(t, slices.Concat(before, []string{handedOn}), got)
	assert.Equal(t, source.failAt+1, source.pulled, "read on after the error")
}

func TestWhere(t *testing.T) {
	t.Run("keeps what matches", func(t *testing.T) {
		got := titles(t, query.Where(books(), func(b book) bool { return b.Year > 1980 }))

		assert.Equal(t, []string{"Neuromancer", "Snow Crash"}, got)
	})

	t.Run("can keep nothing", func(t *testing.T) {
		got := titles(t, query.Where(books(), func(b book) bool { return b.Year > 3000 }))

		assert.Empty(t, got)
	})

	t.Run("hands on an error and stops", func(t *testing.T) {
		for _, position := range positions {
			t.Run(position.label, func(t *testing.T) {
				source := failingAt(position.failAt)
				var asked []string

				got := trace(query.Where(source.items(), func(b book) bool {
					asked = append(asked, b.Title)
					return b.Year > 1960
				}))

				// Foundation, the last book, is the one the predicate drops.
				before := []string{"Neuromancer", "Dune", "Snow Crash"}[:min(position.failAt, 3)]
				expectHandedOn(t, source, got, before...)
				assert.Equal(t, titlesOf(library[:position.failAt]), asked,
					"the predicate must not be asked about the error, or anything after it")
			})
		}
	})
}

func TestSelect(t *testing.T) {
	t.Run("turns items into something else", func(t *testing.T) {
		years, err := query.Collect(query.Select(books(), func(b book) int { return b.Year }))
		require.NoError(t, err)

		assert.Equal(t, []int{1984, 1965, 1992, 1951}, years)
	})

	t.Run("hands on an error and stops", func(t *testing.T) {
		for _, position := range positions {
			t.Run(position.label, func(t *testing.T) {
				source := failingAt(position.failAt)
				var asked []string

				got := trace(query.Select(source.items(), func(b book) string {
					asked = append(asked, b.Title)
					return strings.ToUpper(b.Title)
				}))

				before := []string{"NEUROMANCER", "DUNE", "SNOW CRASH", "FOUNDATION"}[:position.failAt]
				expectHandedOn(t, source, got, before...)
				assert.Equal(t, titlesOf(library[:position.failAt]), asked,
					"the selector must not be called for the error, or anything after it")
			})
		}
	})
}

// sorts are the three ways of ordering, each sorting by year, and writing
// down the titles it is asked about.
func sorts(asked *[]string) map[string]func(iter.Seq2[book, error]) iter.Seq2[book, error] {
	year := func(b book) int {
		*asked = append(*asked, b.Title)
		return b.Year
	}

	return map[string]func(iter.Seq2[book, error]) iter.Seq2[book, error]{
		"OrderBy": func(items iter.Seq2[book, error]) iter.Seq2[book, error] {
			return query.OrderBy(items, year)
		},
		"OrderByDescending": func(items iter.Seq2[book, error]) iter.Seq2[book, error] {
			return query.OrderByDescending(items, year)
		},
		"OrderByFunc": func(items iter.Seq2[book, error]) iter.Seq2[book, error] {
			return query.OrderByFunc(items, func(left, right book) int {
				return cmp.Compare(year(left), year(right))
			})
		},
	}
}

func TestOrderBy(t *testing.T) {
	t.Run("sorts ascending", func(t *testing.T) {
		got := titles(t, query.OrderBy(books(), func(b book) int { return b.Year }))

		assert.Equal(t, []string{"Foundation", "Dune", "Neuromancer", "Snow Crash"}, got)
	})

	t.Run("hands on an error alone", func(t *testing.T) {
		// Sorting has to read everything first, so the items read before the
		// error are not handed out: they are not all there is.
		for _, position := range positions {
			t.Run(position.label, func(t *testing.T) {
				var asked []string
				for name, sort := range sorts(&asked) {
					t.Run(name, func(t *testing.T) {
						source := failingAt(position.failAt)
						asked = nil

						got := trace(sort(source.items()))

						expectHandedOn(t, source, got)
						assert.Empty(t, asked, "nothing is to be sorted after an error")
					})
				}
			})
		}
	})

	t.Run("reads nothing before it is read", func(t *testing.T) {
		var asked []string
		for name, sort := range sorts(&asked) {
			t.Run(name, func(t *testing.T) {
				source := counting()

				sorted := sort(source.items())
				assert.Zero(t, source.pulled, "the input was read before the sorted sequence was")

				for range sorted {
				}
				for range sorted {
				}
				assert.Equal(t, 2*len(library), source.pulled, "every reading of the sequence reads the input anew")
			})
		}
	})

	t.Run("stops when the caller does", func(t *testing.T) {
		var asked []string
		for name, sort := range sorts(&asked) {
			t.Run(name, func(t *testing.T) {
				source := counting()

				var got []string
				for item, err := range sort(source.items()) {
					require.NoError(t, err)
					got = append(got, item.Title)
					break
				}

				assert.Len(t, got, 1)
				assert.Equal(t, len(library), source.pulled, "sorting reads everything, but once")
			})
		}
	})
}

func TestOrderByDescending(t *testing.T) {
	t.Run("sorts the other way", func(t *testing.T) {
		got := titles(t, query.OrderByDescending(books(), func(b book) int { return b.Year }))

		assert.Equal(t, []string{"Snow Crash", "Neuromancer", "Dune", "Foundation"}, got)
	})
}

func TestSkip(t *testing.T) {
	t.Run("passes over the first items", func(t *testing.T) {
		got := titles(t, query.Skip(books(), 2))

		assert.Equal(t, []string{"Snow Crash", "Foundation"}, got)
	})

	t.Run("beyond the end yields nothing", func(t *testing.T) {
		assert.Empty(t, titles(t, query.Skip(books(), 99)))
	})

	t.Run("of nothing keeps everything", func(t *testing.T) {
		assert.Len(t, titles(t, query.Skip(books(), 0)), 4)
	})

	t.Run("hands on an error and stops", func(t *testing.T) {
		for _, position := range positions {
			t.Run(position.label, func(t *testing.T) {
				source := failingAt(position.failAt)

				got := trace(query.Skip(source.items(), 1))

				before := []string{"Dune", "Snow Crash", "Foundation"}[:max(position.failAt-1, 0)]
				expectHandedOn(t, source, got, before...)
			})
		}
	})

	t.Run("hands on an error among the items it passes over", func(t *testing.T) {
		source := failingAt(2)

		got := trace(query.Skip(source.items(), 3))

		expectHandedOn(t, source, got)
	})
}

func TestTake(t *testing.T) {
	t.Run("stops after the given count", func(t *testing.T) {
		got := titles(t, query.Take(books(), 2))

		assert.Equal(t, []string{"Neuromancer", "Dune"}, got)
	})

	t.Run("reads no further than it must", func(t *testing.T) {
		source := counting()

		_ = titles(t, query.Take(source.items(), 2))

		assert.Equal(t, 2, source.pulled)
	})

	t.Run("of nothing yields nothing", func(t *testing.T) {
		for _, c := range []struct {
			label string
			count int
		}{
			{"zero", 0},
			{"negative", -1},
		} {
			t.Run(c.label, func(t *testing.T) {
				source := failingAt(0)

				assert.Empty(t, trace(query.Take(source.items(), c.count)))
				assert.Zero(t, source.pulled, "taking nothing reads nothing, not even an error")
			})
		}
	})

	t.Run("beyond the end yields everything", func(t *testing.T) {
		assert.Len(t, titles(t, query.Take(books(), 99)), 4)
	})

	t.Run("hands on an error and stops", func(t *testing.T) {
		for _, position := range positions {
			t.Run(position.label, func(t *testing.T) {
				source := failingAt(position.failAt)

				got := trace(query.Take(source.items(), 99))

				expectHandedOn(t, source, got, titlesOf(library[:position.failAt])...)
			})
		}
	})

	t.Run("does not read an error after the items it takes", func(t *testing.T) {
		source := failingAt(2)

		got := trace(query.Take(source.items(), 2))

		assert.Equal(t, []string{"Neuromancer", "Dune"}, got)
		assert.Equal(t, 2, source.pulled)
	})
}

func TestCollect(t *testing.T) {
	t.Run("collects every item", func(t *testing.T) {
		got, err := query.Collect(books())
		require.NoError(t, err)

		assert.Equal(t, library, got)
	})

	t.Run("collects nothing as nil", func(t *testing.T) {
		got, err := query.Collect(query.Where(books(), func(book) bool { return false }))
		require.NoError(t, err)

		assert.Nil(t, got)
	})

	t.Run("returns an error without the items read before it", func(t *testing.T) {
		for _, position := range positions {
			t.Run(position.label, func(t *testing.T) {
				source := failingAt(position.failAt)

				got, err := query.Collect(source.items())

				require.ErrorIs(t, err, errCursor)
				assert.Nil(t, got)
				assert.Equal(t, position.failAt+1, source.pulled, "Collect read on after the error")
			})
		}
	})
}

func TestFirst(t *testing.T) {
	t.Run("reports whether there was one", func(t *testing.T) {
		first, ok, err := query.First(books())
		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, "Neuromancer", first.Title)

		_, ok, err = query.First(query.Where(books(), func(book) bool { return false }))
		require.NoError(t, err)
		assert.False(t, ok, "an empty sequence has no first item")
	})

	t.Run("returns an error before the first item", func(t *testing.T) {
		first, ok, err := query.First(failingAt(0).items())

		require.ErrorIs(t, err, errCursor)
		assert.False(t, ok)
		assert.Zero(t, first)
	})

	t.Run("does not read an error after the first item", func(t *testing.T) {
		source := failingAt(1)

		first, ok, err := query.First(source.items())

		require.NoError(t, err)
		assert.True(t, ok)
		assert.Equal(t, "Neuromancer", first.Title)
		assert.Equal(t, 1, source.pulled, "First read on after the first item")
	})
}

func TestSingle(t *testing.T) {
	isDune := func(b book) bool { return b.Year == 1965 }

	t.Run("says what it found, without the name of the package", func(t *testing.T) {
		// An application may hand the error on to a caller, wrapped together
		// with an error of its own, so the text speaks of items, not of Go.
		assert.EqualError(t, query.ErrNoItems, "no items")
		assert.EqualError(t, query.ErrTooManyItems, "more than one item")
	})

	t.Run("insists on exactly one", func(t *testing.T) {
		only, err := query.Single(query.Where(books(), isDune))
		require.NoError(t, err)
		assert.Equal(t, "Dune", only.Title)

		none, err := query.Single(query.Where(books(), func(book) bool { return false }))
		assert.ErrorIs(t, err, query.ErrNoItems)
		assert.Zero(t, none)

		source := counting()
		several, err := query.Single(source.items())
		assert.ErrorIs(t, err, query.ErrTooManyItems)
		assert.Zero(t, several, "with an error, the item is the zero value")
		assert.Equal(t, 2, source.pulled, "Single read on after the second item")
	})

	t.Run("returns an error before the first item", func(t *testing.T) {
		only, err := query.Single(failingAt(0).items())

		require.ErrorIs(t, err, errCursor)
		assert.Zero(t, only)
	})

	t.Run("returns an error after the one item", func(t *testing.T) {
		// Dune is the only match, but the sequence fails before it has ended, so
		// it can not tell whether there is another one.
		only, err := query.Single(query.Where(failingAt(2).items(), isDune))

		require.ErrorIs(t, err, errCursor)
		assert.Zero(t, only, "with an error, the item is the zero value")
	})

	t.Run("does not read an error after the second item", func(t *testing.T) {
		source := failingAt(2)

		several, err := query.Single(source.items())

		require.ErrorIs(t, err, query.ErrTooManyItems)
		assert.Zero(t, several)
		assert.Equal(t, 2, source.pulled)
	})
}

func TestCount(t *testing.T) {
	t.Run("counts everything", func(t *testing.T) {
		count, err := query.Count(books())
		require.NoError(t, err)
		assert.Equal(t, 4, count)

		count, err = query.Count(query.Where(books(), func(b book) bool { return b.Year < 1970 }))
		require.NoError(t, err)
		assert.Equal(t, 2, count)
	})

	t.Run("returns an error without a count", func(t *testing.T) {
		for _, position := range positions {
			t.Run(position.label, func(t *testing.T) {
				source := failingAt(position.failAt)

				count, err := query.Count(source.items())

				require.ErrorIs(t, err, errCursor)
				assert.Zero(t, count, "the items before an error are not all there are")
				assert.Equal(t, position.failAt+1, source.pulled, "Count read on after the error")
			})
		}
	})
}

func TestAny(t *testing.T) {
	t.Run("stops at the first match", func(t *testing.T) {
		found, err := query.Any(books(), func(b book) bool { return b.Year == 1992 })
		require.NoError(t, err)
		assert.True(t, found, "Snow Crash is from 1992")

		found, err = query.Any(books(), func(b book) bool { return b.Year == 2525 })
		require.NoError(t, err)
		assert.False(t, found, "nothing is from 2525")

		source := counting()
		_, err = query.Any(source.items(), func(b book) bool { return b.Title == "Dune" })
		require.NoError(t, err)

		assert.Equal(t, 2, source.pulled, "Any read on after the first match")
	})

	t.Run("returns an error before a match", func(t *testing.T) {
		var asked []string

		found, err := query.Any(failingAt(1).items(), func(b book) bool {
			asked = append(asked, b.Title)
			return b.Year == 1992
		})

		require.ErrorIs(t, err, errCursor)
		assert.False(t, found)
		assert.Equal(t, []string{"Neuromancer"}, asked, "the function must not be asked about the error")
	})

	t.Run("does not read an error after a match", func(t *testing.T) {
		source := failingAt(2)

		found, err := query.Any(source.items(), func(b book) bool { return b.Title == "Dune" })

		require.NoError(t, err)
		assert.True(t, found)
		assert.Equal(t, 2, source.pulled)
	})
}

func TestSteps(t *testing.T) {
	t.Run("compose", func(t *testing.T) {
		// Filter, then order, then page: the order of the steps is the caller's,
		// and nothing is materialised until the end.
		got := titles(t,
			query.Take(
				query.OrderBy(
					query.Where(books(), func(b book) bool { return b.Year > 1960 }),
					func(b book) int { return b.Year },
				), 2),
		)

		assert.Equal(t, []string{"Dune", "Neuromancer"}, got)
	})

	t.Run("carry an error of a view through to the result", func(t *testing.T) {
		// The view fails after three books, as a cursor of a database whose
		// connection breaks. Taking two would not need the third one, but the
		// order needs all of them, so the error reaches the result.
		var view architecturekit.View[book] = failingAt(3)

		got, err := query.Collect(
			query.Take(
				query.OrderBy(
					query.Where(view.All(context.Background()), func(b book) bool { return b.Year > 1960 }),
					func(b book) int { return b.Year },
				), 2),
		)

		require.ErrorIs(t, err, errCursor)
		assert.Nil(t, got)
	})

	t.Run("read no further than the result needs", func(t *testing.T) {
		// Without an order, the two books are there before the view fails.
		view := failingAt(3)

		got, err := query.Collect(
			query.Take(
				query.Where(view.All(context.Background()), func(b book) bool { return b.Year > 1960 }),
				2),
		)

		require.NoError(t, err)
		assert.Equal(t, []book{library[0], library[1]}, got)
		assert.Equal(t, 2, view.pulled)
	})
}

func TestEarlyStop(t *testing.T) {
	t.Run("is passed through", func(t *testing.T) {
		// Breaking out of a range over a composed query must not read on.
		source := counting()
		for range query.Where(source.items(), func(book) bool { return true }) {
			break
		}
		assert.Equal(t, 1, source.pulled, "Where read on after breaking")

		source = counting()
		for range query.Select(source.items(), func(b book) string { return b.Title }) {
			break
		}
		assert.Equal(t, 1, source.pulled, "Select read on after breaking")

		source = counting()
		for range query.Skip(source.items(), 1) {
			break
		}
		assert.Equal(t, 2, source.pulled, "Skip read on after breaking")

		source = counting()
		for range query.Take(source.items(), 5) {
			break
		}
		assert.Equal(t, 1, source.pulled, "Take read on after breaking")
	})
}

func TestOrderByFunc(t *testing.T) {
	t.Run("sorts on several fields", func(t *testing.T) {
		// The kind of order no single key can express: a bool that is not ordered
		// on its own, and a tie broken by a third field.
		type task struct {
			Due         int
			Prioritized bool
			Title       string
		}

		rank := func(prioritized bool) int {
			if prioritized {
				return 0
			}
			return 1
		}

		tasks := sequenceOf([]task{
			{Due: 2, Prioritized: false, Title: "later, plain"},
			{Due: 1, Prioritized: false, Title: "soon, plain"},
			{Due: 1, Prioritized: true, Title: "soon, urgent"},
			{Due: 1, Prioritized: false, Title: "another, plain"},
		})

		sorted := query.OrderByFunc(tasks, func(left, right task) int {
			return cmp.Or(
				cmp.Compare(left.Due, right.Due),
				cmp.Compare(rank(left.Prioritized), rank(right.Prioritized)),
				strings.Compare(left.Title, right.Title),
			)
		})

		got, err := query.Collect(query.Select(sorted, func(t task) string { return t.Title }))
		require.NoError(t, err)
		want := []string{"soon, urgent", "another, plain", "soon, plain", "later, plain"}

		assert.Equal(t, want, got)
	})

	t.Run("is stable", func(t *testing.T) {
		// Items the comparison calls equal keep the order they came in.
		got := titles(t, query.OrderByFunc(books(), func(left, right book) int {
			return 0
		}))

		assert.Equal(t, []string{"Neuromancer", "Dune", "Snow Crash", "Foundation"}, got)
	})

	t.Run("is stable for more than a handful of items", func(t *testing.T) {
		// Sorting a handful of items is stable even with an unstable sort, so it
		// takes more of them to tell the two apart.
		var shelf []book
		for i := range 100 {
			shelf = append(shelf, book{Title: fmt.Sprintf("%03d", i), Year: i % 3})
		}

		sorted, err := query.Collect(query.OrderByFunc(sequenceOf(shelf), func(left, right book) int {
			return cmp.Compare(left.Year, right.Year)
		}))
		require.NoError(t, err)

		for i := 1; i < len(sorted); i++ {
			if sorted[i-1].Year == sorted[i].Year {
				require.Less(t, sorted[i-1].Title, sorted[i].Title, "books of the same year changed their order")
			}
		}
	})

	t.Run("composes with the other steps", func(t *testing.T) {
		got := titles(t,
			query.Take(
				query.OrderByFunc(
					query.Where(books(), func(b book) bool { return b.Year > 1960 }),
					func(left, right book) int { return cmp.Compare(right.Year, left.Year) },
				), 2),
		)

		assert.Equal(t, []string{"Snow Crash", "Neuromancer"}, got)
	})
}
