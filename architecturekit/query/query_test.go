package query_test

import (
	"cmp"
	"errors"
	"iter"
	"slices"
	"strings"
	"testing"

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

func TestWhereKeepsWhatMatches(t *testing.T) {
	got := titles(query.Where(books(), func(b book) bool { return b.Year > 1980 }))

	if !slices.Equal(got, []string{"Neuromancer", "Snow Crash"}) {
		t.Fatalf("got %v", got)
	}
}

func TestWhereCanKeepNothing(t *testing.T) {
	got := titles(query.Where(books(), func(b book) bool { return b.Year > 3000 }))

	if len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestSelectTurnsItemsIntoSomethingElse(t *testing.T) {
	years := slices.Collect(query.Select(books(), func(b book) int { return b.Year }))

	if !slices.Equal(years, []int{1984, 1965, 1992, 1951}) {
		t.Fatalf("got %v", years)
	}
}

func TestOrderBySortsAscending(t *testing.T) {
	got := titles(query.OrderBy(books(), func(b book) int { return b.Year }))

	if !slices.Equal(got, []string{"Foundation", "Dune", "Neuromancer", "Snow Crash"}) {
		t.Fatalf("got %v", got)
	}
}

func TestOrderByDescendingSortsTheOtherWay(t *testing.T) {
	got := titles(query.OrderByDescending(books(), func(b book) int { return b.Year }))

	if !slices.Equal(got, []string{"Snow Crash", "Neuromancer", "Dune", "Foundation"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSkipPassesOverTheFirstItems(t *testing.T) {
	got := titles(query.Skip(books(), 2))

	if !slices.Equal(got, []string{"Snow Crash", "Foundation"}) {
		t.Fatalf("got %v", got)
	}
}

func TestSkipBeyondTheEndYieldsNothing(t *testing.T) {
	if got := titles(query.Skip(books(), 99)); len(got) != 0 {
		t.Fatalf("got %v", got)
	}
}

func TestSkipOfNothingKeepsEverything(t *testing.T) {
	if got := titles(query.Skip(books(), 0)); len(got) != 4 {
		t.Fatalf("got %v", got)
	}
}

func TestTakeStopsAfterTheGivenCount(t *testing.T) {
	got := titles(query.Take(books(), 2))

	if !slices.Equal(got, []string{"Neuromancer", "Dune"}) {
		t.Fatalf("got %v", got)
	}
}

func TestTakeReadsNoFurtherThanItMust(t *testing.T) {
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

	if read != 2 {
		t.Fatalf("Take read %d items, want 2", read)
	}
}

func TestTakeOfNothingYieldsNothing(t *testing.T) {
	for _, count := range []int{0, -1} {
		if got := titles(query.Take(books(), count)); len(got) != 0 {
			t.Fatalf("count %d: got %v", count, got)
		}
	}
}

func TestTakeBeyondTheEndYieldsEverything(t *testing.T) {
	if got := titles(query.Take(books(), 99)); len(got) != 4 {
		t.Fatalf("got %v", got)
	}
}

func TestFirstReportsWhetherThereWasOne(t *testing.T) {
	first, ok := query.First(books())
	if !ok || first.Title != "Neuromancer" {
		t.Fatalf("got %v, %v", first, ok)
	}

	_, ok = query.First(query.Where(books(), func(book) bool { return false }))
	if ok {
		t.Fatal("an empty sequence has no first item")
	}
}

func TestSingleInsistsOnExactlyOne(t *testing.T) {
	only, err := query.Single(query.Where(books(), func(b book) bool { return b.Year == 1965 }))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if only.Title != "Dune" {
		t.Fatalf("got %q", only.Title)
	}

	_, err = query.Single(query.Where(books(), func(book) bool { return false }))
	if !errors.Is(err, query.ErrNoItems) {
		t.Fatalf("got %v", err)
	}

	_, err = query.Single(books())
	if !errors.Is(err, query.ErrTooManyItems) {
		t.Fatalf("got %v", err)
	}
}

func TestCountCountsEverything(t *testing.T) {
	if got := query.Count(books()); got != 4 {
		t.Fatalf("got %d, want 4", got)
	}
	if got := query.Count(query.Where(books(), func(b book) bool { return b.Year < 1970 })); got != 2 {
		t.Fatalf("got %d, want 2", got)
	}
}

func TestAnyStopsAtTheFirstMatch(t *testing.T) {
	if !query.Any(books(), func(b book) bool { return b.Year == 1992 }) {
		t.Fatal("Snow Crash is from 1992")
	}
	if query.Any(books(), func(b book) bool { return b.Year == 2525 }) {
		t.Fatal("nothing is from 2525")
	}

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

	if read != 2 {
		t.Fatalf("Any read %d items, want 2", read)
	}
}

func TestStepsCompose(t *testing.T) {
	// Filter, then order, then page: the order of the steps is the caller's,
	// and nothing is materialised until the end.
	got := titles(
		query.Take(
			query.OrderBy(
				query.Where(books(), func(b book) bool { return b.Year > 1960 }),
				func(b book) int { return b.Year },
			), 2),
	)

	if !slices.Equal(got, []string{"Dune", "Neuromancer"}) {
		t.Fatalf("got %v", got)
	}
}

func TestEarlyStopIsPassedThrough(t *testing.T) {
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

	if read != 1 {
		t.Fatalf("read %d items after breaking, want 1", read)
	}

	read = 0
	for range query.Select(counting, func(b book) string { return b.Title }) {
		break
	}
	if read != 1 {
		t.Fatalf("Select read %d items after breaking, want 1", read)
	}

	read = 0
	for range query.Skip(counting, 1) {
		break
	}
	if read != 2 {
		t.Fatalf("Skip read %d items after breaking, want 2", read)
	}

	read = 0
	for range query.Take(counting, 5) {
		break
	}
	if read != 1 {
		t.Fatalf("Take read %d items after breaking, want 1", read)
	}
}

func TestOrderByFuncSortsOnSeveralFields(t *testing.T) {
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

	if !slices.Equal(got, want) {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestOrderByFuncIsStable(t *testing.T) {
	// Items the comparison calls equal keep the order they came in.
	got := titles(query.OrderByFunc(books(), func(left, right book) int {
		return 0
	}))

	if !slices.Equal(got, []string{"Neuromancer", "Dune", "Snow Crash", "Foundation"}) {
		t.Fatalf("got %v", got)
	}
}

func TestOrderByFuncComposesWithTheOtherSteps(t *testing.T) {
	got := titles(
		query.Take(
			query.OrderByFunc(
				query.Where(books(), func(b book) bool { return b.Year > 1960 }),
				func(left, right book) int { return cmp.Compare(right.Year, left.Year) },
			), 2),
	)

	if !slices.Equal(got, []string{"Snow Crash", "Neuromancer"}) {
		t.Fatalf("got %v", got)
	}
}
