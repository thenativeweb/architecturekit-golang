package query_test

import (
	"cmp"
	"iter"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/query"
)

type book struct {
	Title string
	Year  int
}

func books() iter.Seq[book] {
	return slices.Values([]book{
		{Title: "Neuromancer", Year: 1984},
		{Title: "Dune", Year: 1965},
		{Title: "Snow Crash", Year: 1992},
		{Title: "Foundation", Year: 1951},
	})
}

func titles(items iter.Seq[book]) []string {
	return slices.Collect(query.Select(items, func(b book) string { return b.Title }))
}

func TestWhere(t *testing.T) {
	t.Run("keeps what matches", func(t *testing.T) {
		got := titles(query.Where(books(), func(b book) bool { return b.Year > 1980 }))

		assert.Equal(t, []string{"Neuromancer", "Snow Crash"}, got)
	})

	t.Run("can keep nothing", func(t *testing.T) {
		got := titles(query.Where(books(), func(b book) bool { return b.Year > 3000 }))

		assert.Empty(t, got)
	})
}

func TestSelect(t *testing.T) {
	t.Run("turns items into something else", func(t *testing.T) {
		years := slices.Collect(query.Select(books(), func(b book) int { return b.Year }))

		assert.Equal(t, []int{1984, 1965, 1992, 1951}, years)
	})
}

func TestOrderBy(t *testing.T) {
	t.Run("sorts ascending", func(t *testing.T) {
		got := titles(query.OrderBy(books(), func(b book) int { return b.Year }))

		assert.Equal(t, []string{"Foundation", "Dune", "Neuromancer", "Snow Crash"}, got)
	})
}

func TestOrderByDescending(t *testing.T) {
	t.Run("sorts the other way", func(t *testing.T) {
		got := titles(query.OrderByDescending(books(), func(b book) int { return b.Year }))

		assert.Equal(t, []string{"Snow Crash", "Neuromancer", "Dune", "Foundation"}, got)
	})
}

func TestSkip(t *testing.T) {
	t.Run("passes over the first items", func(t *testing.T) {
		got := titles(query.Skip(books(), 2))

		assert.Equal(t, []string{"Snow Crash", "Foundation"}, got)
	})

	t.Run("beyond the end yields nothing", func(t *testing.T) {
		assert.Empty(t, titles(query.Skip(books(), 99)))
	})

	t.Run("of nothing keeps everything", func(t *testing.T) {
		assert.Len(t, titles(query.Skip(books(), 0)), 4)
	})
}

func TestTake(t *testing.T) {
	t.Run("stops after the given count", func(t *testing.T) {
		got := titles(query.Take(books(), 2))

		assert.Equal(t, []string{"Neuromancer", "Dune"}, got)
	})

	t.Run("reads no further than it must", func(t *testing.T) {
		read := 0
		counting := func(yield func(book) bool) {
			for b := range books() {
				read++
				if !yield(b) {
					return
				}
			}
		}

		_ = slices.Collect(query.Take(counting, 2))

		assert.Equal(t, 2, read)
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
				assert.Empty(t, titles(query.Take(books(), c.count)))
			})
		}
	})

	t.Run("beyond the end yields everything", func(t *testing.T) {
		assert.Len(t, titles(query.Take(books(), 99)), 4)
	})
}

func TestFirst(t *testing.T) {
	t.Run("reports whether there was one", func(t *testing.T) {
		first, ok := query.First(books())
		assert.True(t, ok)
		assert.Equal(t, "Neuromancer", first.Title)

		_, ok = query.First(query.Where(books(), func(book) bool { return false }))
		assert.False(t, ok, "an empty sequence has no first item")
	})
}

func TestSingle(t *testing.T) {
	t.Run("insists on exactly one", func(t *testing.T) {
		only, err := query.Single(query.Where(books(), func(b book) bool { return b.Year == 1965 }))
		require.NoError(t, err)
		assert.Equal(t, "Dune", only.Title)

		_, err = query.Single(query.Where(books(), func(book) bool { return false }))
		assert.ErrorIs(t, err, query.ErrNoItems)

		_, err = query.Single(books())
		assert.ErrorIs(t, err, query.ErrTooManyItems)
	})
}

func TestCount(t *testing.T) {
	t.Run("counts everything", func(t *testing.T) {
		assert.Equal(t, 4, query.Count(books()))
		assert.Equal(t, 2, query.Count(query.Where(books(), func(b book) bool { return b.Year < 1970 })))
	})
}

func TestAny(t *testing.T) {
	t.Run("stops at the first match", func(t *testing.T) {
		assert.True(t, query.Any(books(), func(b book) bool { return b.Year == 1992 }), "Snow Crash is from 1992")
		assert.False(t, query.Any(books(), func(b book) bool { return b.Year == 2525 }), "nothing is from 2525")

		read := 0
		counting := func(yield func(book) bool) {
			for b := range books() {
				read++
				if !yield(b) {
					return
				}
			}
		}

		query.Any(counting, func(b book) bool { return b.Title == "Dune" })

		assert.Equal(t, 2, read, "Any read on after the first match")
	})
}

func TestSteps(t *testing.T) {
	t.Run("compose", func(t *testing.T) {
		// Filter, then order, then page: the order of the steps is the caller's,
		// and nothing is materialised until the end.
		got := titles(
			query.Take(
				query.OrderBy(
					query.Where(books(), func(b book) bool { return b.Year > 1960 }),
					func(b book) int { return b.Year },
				), 2),
		)

		assert.Equal(t, []string{"Dune", "Neuromancer"}, got)
	})
}

func TestEarlyStop(t *testing.T) {
	t.Run("is passed through", func(t *testing.T) {
		// Breaking out of a range over a composed query must not read on.
		read := 0
		counting := func(yield func(book) bool) {
			for b := range books() {
				read++
				if !yield(b) {
					return
				}
			}
		}

		for range query.Where(counting, func(book) bool { return true }) {
			break
		}

		assert.Equal(t, 1, read, "Where read on after breaking")

		read = 0
		for range query.Select(counting, func(b book) string { return b.Title }) {
			break
		}
		assert.Equal(t, 1, read, "Select read on after breaking")

		read = 0
		for range query.Skip(counting, 1) {
			break
		}
		assert.Equal(t, 2, read, "Skip read on after breaking")

		read = 0
		for range query.Take(counting, 5) {
			break
		}
		assert.Equal(t, 1, read, "Take read on after breaking")
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

		tasks := slices.Values([]task{
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

		got := slices.Collect(query.Select(sorted, func(t task) string { return t.Title }))
		want := []string{"soon, urgent", "another, plain", "soon, plain", "later, plain"}

		assert.Equal(t, want, got)
	})

	t.Run("is stable", func(t *testing.T) {
		// Items the comparison calls equal keep the order they came in.
		got := titles(query.OrderByFunc(books(), func(left, right book) int {
			return 0
		}))

		assert.Equal(t, []string{"Neuromancer", "Dune", "Snow Crash", "Foundation"}, got)
	})

	t.Run("composes with the other steps", func(t *testing.T) {
		got := titles(
			query.Take(
				query.OrderByFunc(
					query.Where(books(), func(b book) bool { return b.Year > 1960 }),
					func(left, right book) int { return cmp.Compare(right.Year, left.Year) },
				), 2),
		)

		assert.Equal(t, []string{"Snow Crash", "Neuromancer"}, got)
	})
}
