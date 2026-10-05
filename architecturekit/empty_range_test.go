package architecturekit_test

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Bounds that can never hold an event are a mistake of whoever chose them,
// usually the caller of an API, so Read refuses them before the database is
// asked. A range that is only empty for now, such as the one after the last
// event, is no mistake, since events can still come there.

// largestRevision is the largest ID the database can hand out, 2^63-1.
const largestRevision = "9223372036854775807"

// boundedRead is a read with bounds, and what Read makes of them: the text of
// the error if they leave no room for an event, or an empty one if they do.
type boundedRead struct {
	name    string
	options []architecturekit.ReadOption
	refusal string
}

// boundedReads holds every combination of the four kinds of bounds, alone and
// together, in arrangements that leave room for an event and ones that do
// not.
var boundedReads = []boundedRead{
	// A lower bound alone leaves room, since events can still come, unless it
	// is after the largest revision, which no event can ever come after.
	{"FromEvent alone", []architecturekit.ReadOption{architecturekit.FromEvent("2")}, ""},
	{"AfterEvent alone", []architecturekit.ReadOption{architecturekit.AfterEvent("2")}, ""},
	{"AfterEvent of the largest revision alone", []architecturekit.ReadOption{architecturekit.AfterEvent(largestRevision)},
		`empty range: no event can lie after "9223372036854775807"`},

	// An upper bound alone leaves no room only before the first event.
	{"UpToEvent of the first event", []architecturekit.ReadOption{architecturekit.UpToEvent("0")}, ""},
	{"BeforeEvent of the second event", []architecturekit.ReadOption{architecturekit.BeforeEvent("1")}, ""},
	{"BeforeEvent of the first event", []architecturekit.ReadOption{architecturekit.BeforeEvent("0")},
		`empty range: no event can lie before "0"`},

	{"FromEvent and UpToEvent of the same event",
		[]architecturekit.ReadOption{architecturekit.FromEvent("1"), architecturekit.UpToEvent("1")}, ""},
	{"FromEvent and UpToEvent in order",
		[]architecturekit.ReadOption{architecturekit.FromEvent("1"), architecturekit.UpToEvent("2")}, ""},
	{"FromEvent and UpToEvent the wrong way round",
		[]architecturekit.ReadOption{architecturekit.FromEvent("2"), architecturekit.UpToEvent("1")},
		`empty range: no event can lie from "2" up to "1"`},

	{"FromEvent and BeforeEvent of the next event",
		[]architecturekit.ReadOption{architecturekit.FromEvent("1"), architecturekit.BeforeEvent("2")}, ""},
	{"FromEvent and BeforeEvent of the same event",
		[]architecturekit.ReadOption{architecturekit.FromEvent("1"), architecturekit.BeforeEvent("1")},
		`empty range: no event can lie from "1" and before "1"`},
	{"FromEvent and BeforeEvent the wrong way round",
		[]architecturekit.ReadOption{architecturekit.FromEvent("2"), architecturekit.BeforeEvent("1")},
		`empty range: no event can lie from "2" and before "1"`},
	{"FromEvent and BeforeEvent of the first event",
		[]architecturekit.ReadOption{architecturekit.FromEvent("0"), architecturekit.BeforeEvent("0")},
		`empty range: no event can lie from "0" and before "0"`},

	{"AfterEvent and UpToEvent of the next event",
		[]architecturekit.ReadOption{architecturekit.AfterEvent("0"), architecturekit.UpToEvent("1")}, ""},
	{"AfterEvent and UpToEvent of the same event",
		[]architecturekit.ReadOption{architecturekit.AfterEvent("1"), architecturekit.UpToEvent("1")},
		`empty range: no event can lie after "1" and up to "1"`},
	{"AfterEvent and UpToEvent the wrong way round",
		[]architecturekit.ReadOption{architecturekit.AfterEvent("2"), architecturekit.UpToEvent("1")},
		`empty range: no event can lie after "2" and up to "1"`},

	{"AfterEvent and BeforeEvent with one event between them",
		[]architecturekit.ReadOption{architecturekit.AfterEvent("0"), architecturekit.BeforeEvent("2")}, ""},
	{"AfterEvent and BeforeEvent of neighboring events",
		[]architecturekit.ReadOption{architecturekit.AfterEvent("0"), architecturekit.BeforeEvent("1")},
		`empty range: no event can lie after "0" and before "1"`},
	{"AfterEvent and BeforeEvent of the same event",
		[]architecturekit.ReadOption{architecturekit.AfterEvent("1"), architecturekit.BeforeEvent("1")},
		`empty range: no event can lie after "1" and before "1"`},
	{"AfterEvent and BeforeEvent the wrong way round",
		[]architecturekit.ReadOption{architecturekit.AfterEvent("5"), architecturekit.BeforeEvent("0")},
		`empty range: no event can lie after "5" and before "0"`},

	{"the upper bound before the lower one in the code",
		[]architecturekit.ReadOption{architecturekit.UpToEvent("1"), architecturekit.FromEvent("2")},
		`empty range: no event can lie from "2" up to "1"`},
	{"IDs with leading zeros, which are named as they are given",
		[]architecturekit.ReadOption{architecturekit.AfterEvent("007"), architecturekit.BeforeEvent("8")},
		`empty range: no event can lie after "007" and before "8"`},
}

// refusalOf returns the one error Read ends with, and fails the test if Read
// hands out an event or more than one error.
func refusalOf(t *testing.T, store *architecturekit.Store, options ...architecturekit.ReadOption) error {
	t.Helper()

	events, errs := readAll(t, store, architecturekit.ExactSubject("/books/42"), options...)
	require.Empty(t, events)
	require.Len(t, errs, 1, "the iteration ends with the error")

	return errs[0]
}

func TestReadEmptyRanges(t *testing.T) {
	for _, read := range boundedReads {
		if read.refusal == "" {
			t.Run(read.name+" asks the database", func(t *testing.T) {
				store, asked := unaskedDatabase(t)

				_, errs := readAll(t, store, architecturekit.ExactSubject("/books/42"), read.options...)

				assert.Empty(t, errs)
				assert.True(t, asked.Load())
			})

			continue
		}

		t.Run(read.name+" refuses the range before asking the database", func(t *testing.T) {
			store, asked := unaskedDatabase(t)

			err := refusalOf(t, store, read.options...)

			assert.ErrorIs(t, err, architecturekit.ErrEmptyRange)
			assert.NotErrorIs(t, err, architecturekit.ErrPermanent, "the bounds are the caller's mistake")
			assert.NotErrorIs(t, err, architecturekit.ErrTransient)
			assert.NotErrorIs(t, err, architecturekit.ErrNotARevision, "the IDs are fine")
			assert.EqualError(t, err, read.refusal, "the text is for the caller, who knows neither the subject nor the options")
			assert.False(t, asked.Load(), "the database must not be asked")
		})
	}

	t.Run("refuses the same ranges newest first, since the order does not change the bounds", func(t *testing.T) {
		for _, read := range boundedReads {
			store, asked := unaskedDatabase(t)
			options := append([]architecturekit.ReadOption{architecturekit.NewestFirst()}, read.options...)

			_, errs := readAll(t, store, architecturekit.ExactSubject("/books/42"), options...)

			if read.refusal == "" {
				assert.Empty(t, errs, read.name)
				assert.True(t, asked.Load(), read.name)
				continue
			}

			require.Len(t, errs, 1, read.name)
			assert.EqualError(t, errs[0], read.refusal, read.name)
			assert.False(t, asked.Load(), read.name)
		}
	})

	t.Run("refuses an ID that is not a revision first", func(t *testing.T) {
		store, asked := unaskedDatabase(t)

		assert.EqualError(t, refusalOf(t, store, architecturekit.AfterEvent("abc"), architecturekit.BeforeEvent("0")),
			`not a revision: "abc"`)
		assert.EqualError(t, refusalOf(t, store, architecturekit.FromEvent("5"), architecturekit.BeforeEvent("abc")),
			`not a revision: "abc"`)
		assert.False(t, asked.Load(), "the database must not be asked")
	})

	t.Run("refuses the range on every iteration", func(t *testing.T) {
		store, asked := unaskedDatabase(t)
		events := architecturekit.Read(context.Background(), store, architecturekit.ExactSubject("/books/42"),
			architecturekit.BeforeEvent("0"))

		for range 2 {
			var errs []error
			for _, err := range events {
				errs = append(errs, err)
			}

			require.Len(t, errs, 1)
			assert.ErrorIs(t, errs[0], architecturekit.ErrEmptyRange)
		}
		assert.False(t, asked.Load())
	})
}

func TestReadEmptyRangesAtTheLargestRevision(t *testing.T) {
	// ParseRevision accepts IDs up to 2^63-1, so the range after the largest one
	// starts at 2^63, which still fits the numbers the bounds are compared as,
	// but which no event can ever reach.
	for _, read := range []boundedRead{
		{"AfterEvent alone", []architecturekit.ReadOption{architecturekit.AfterEvent(largestRevision)},
			`empty range: no event can lie after "9223372036854775807"`},
		{"AfterEvent alone, given with a leading zero", []architecturekit.ReadOption{architecturekit.AfterEvent("0" + largestRevision)},
			`empty range: no event can lie after "09223372036854775807"`},
		{"AfterEvent of the one before alone", []architecturekit.ReadOption{architecturekit.AfterEvent("9223372036854775806")}, ""},
		{"FromEvent alone", []architecturekit.ReadOption{architecturekit.FromEvent(largestRevision)}, ""},
		{"UpToEvent alone", []architecturekit.ReadOption{architecturekit.UpToEvent(largestRevision)}, ""},
		{"BeforeEvent alone", []architecturekit.ReadOption{architecturekit.BeforeEvent(largestRevision)}, ""},
		{"FromEvent and UpToEvent of it",
			[]architecturekit.ReadOption{architecturekit.FromEvent(largestRevision), architecturekit.UpToEvent(largestRevision)}, ""},
		{"AfterEvent of the one before and UpToEvent of it",
			[]architecturekit.ReadOption{architecturekit.AfterEvent("9223372036854775806"), architecturekit.UpToEvent(largestRevision)}, ""},
		{"FromEvent of the first event and UpToEvent of it",
			[]architecturekit.ReadOption{architecturekit.FromEvent("0"), architecturekit.UpToEvent(largestRevision)}, ""},
		{"AfterEvent and UpToEvent of it",
			[]architecturekit.ReadOption{architecturekit.AfterEvent(largestRevision), architecturekit.UpToEvent(largestRevision)},
			`empty range: no event can lie after "9223372036854775807" and up to "9223372036854775807"`},
		{"AfterEvent and BeforeEvent of it",
			[]architecturekit.ReadOption{architecturekit.AfterEvent(largestRevision), architecturekit.BeforeEvent(largestRevision)},
			`empty range: no event can lie after "9223372036854775807" and before "9223372036854775807"`},
		{"FromEvent and BeforeEvent of it",
			[]architecturekit.ReadOption{architecturekit.FromEvent(largestRevision), architecturekit.BeforeEvent(largestRevision)},
			`empty range: no event can lie from "9223372036854775807" and before "9223372036854775807"`},
		{"AfterEvent of it and BeforeEvent of the first event",
			[]architecturekit.ReadOption{architecturekit.AfterEvent(largestRevision), architecturekit.BeforeEvent("0")},
			`empty range: no event can lie after "9223372036854775807" and before "0"`},
	} {
		t.Run(read.name, func(t *testing.T) {
			store, asked := unaskedDatabase(t)

			_, errs := readAll(t, store, architecturekit.ExactSubject("/books/42"), read.options...)

			if read.refusal == "" {
				assert.Empty(t, errs)
				assert.True(t, asked.Load())
				return
			}

			require.Len(t, errs, 1)
			assert.ErrorIs(t, errs[0], architecturekit.ErrEmptyRange)
			assert.EqualError(t, errs[0], read.refusal)
			assert.False(t, asked.Load())
		})
	}
}

func TestReadEmptyRangesFromTheLatestEvent(t *testing.T) {
	// The lower bound is only known once the database has found the latest
	// event of the type, so only an upper bound that leaves no room on its own
	// is refused.
	fromLatest := architecturekit.FromLatestEvent("/books/42", reset{}.EventType(), architecturekit.ReadEverything)

	t.Run("refuses BeforeEvent of the first event before asking the database", func(t *testing.T) {
		store, asked := unaskedDatabase(t)

		err := refusalOf(t, store, fromLatest, architecturekit.BeforeEvent("0"))

		assert.ErrorIs(t, err, architecturekit.ErrEmptyRange)
		assert.EqualError(t, err, `empty range: no event can lie before "0"`)
		assert.False(t, asked.Load(), "the database must not be asked")
	})

	for name, upperBound := range map[string]architecturekit.ReadOption{
		"UpToEvent of the first event":    architecturekit.UpToEvent("0"),
		"BeforeEvent of the second event": architecturekit.BeforeEvent("1"),
	} {
		t.Run("leaves "+name+" to the database", func(t *testing.T) {
			store, asked := unaskedDatabase(t)

			_, errs := readAll(t, store, architecturekit.ExactSubject("/books/42"), fromLatest, upperBound)

			assert.Empty(t, errs)
			assert.True(t, asked.Load())
		})
	}
}

func TestReadEmptyRangesAgainstTheDatabase(t *testing.T) {
	t.Run("hands out no events for a range that is only empty for now but the events that come there later", func(t *testing.T) {
		store := requireStore(t)
		subject, ids := fiveIn(t)

		events, errs := readAll(t, store, architecturekit.ExactSubject(subject), architecturekit.AfterEvent(ids[4]))
		require.Empty(t, errs, "events can still come after the last one")
		assert.Empty(t, events)

		later := writeIDs(t, subject, incremented{By: 6})

		events, errs = readAll(t, store, architecturekit.ExactSubject(subject), architecturekit.AfterEvent(ids[4]))
		require.Empty(t, errs)
		assert.Equal(t, later, idsOf(events))
	})

	t.Run("refuses a read after the largest revision rather than hand out every event", func(t *testing.T) {
		// EventSourcingDB 1.2.0 counts past the largest revision, and so hands
		// out every event after it, while later versions refuse the read as a
		// malformed request. Neither answer is asked for here, since the kit
		// does not ask the database at all.
		subject, _ := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject), architecturekit.AfterEvent(largestRevision))

		assert.Empty(t, events)
		require.Len(t, errs, 1)
		assert.ErrorIs(t, errs[0], architecturekit.ErrEmptyRange)
		assert.EqualError(t, errs[0], `empty range: no event can lie after "9223372036854775807"`)
	})

	t.Run("refuses exactly the bounds the database would refuse", func(t *testing.T) {
		// Without the check, the database answers bounds that leave no room for
		// an event with 400, so the kit has to refuse the same ones, and no
		// others.
		store := requireStore(t)
		subject, _ := fiveIn(t)

		// A bound sets an option of Read, and the same option of the database.
		type bound func(id string, options *eventsourcingdb.ReadEventsOptions) architecturekit.ReadOption

		boundOf := func(option func(string) architecturekit.ReadOption, isLower bool, boundType eventsourcingdb.BoundType) bound {
			return func(id string, options *eventsourcingdb.ReadEventsOptions) architecturekit.ReadOption {
				if isLower {
					options.LowerBound = &eventsourcingdb.Bound{ID: id, Type: boundType}
				} else {
					options.UpperBound = &eventsourcingdb.Bound{ID: id, Type: boundType}
				}

				return option(id)
			}
		}
		fromLatest := func(_ string, options *eventsourcingdb.ReadEventsOptions) architecturekit.ReadOption {
			options.FromLatestEvent = &eventsourcingdb.ReadFromLatestEvent{
				Subject:          subject,
				Type:             reset{}.EventType(),
				IfEventIsMissing: eventsourcingdb.ReadIfEventIsMissingReadEverything,
			}

			return architecturekit.FromLatestEvent(subject, reset{}.EventType(), architecturekit.ReadEverything)
		}

		lowerBounds := map[string]bound{
			"none":            nil,
			"FromEvent":       boundOf(architecturekit.FromEvent, true, eventsourcingdb.BoundTypeInclusive),
			"AfterEvent":      boundOf(architecturekit.AfterEvent, true, eventsourcingdb.BoundTypeExclusive),
			"FromLatestEvent": fromLatest,
		}
		upperBounds := map[string]bound{
			"none":        nil,
			"UpToEvent":   boundOf(architecturekit.UpToEvent, false, eventsourcingdb.BoundTypeInclusive),
			"BeforeEvent": boundOf(architecturekit.BeforeEvent, false, eventsourcingdb.BoundTypeExclusive),
		}

		for lowerName, lower := range lowerBounds {
			for upperName, upper := range upperBounds {
				for lowerID := range 4 {
					for upperID := range 4 {
						name := fmt.Sprintf("%s(%d) and %s(%d)", lowerName, lowerID, upperName, upperID)

						var options []architecturekit.ReadOption
						databaseOptions := eventsourcingdb.ReadEventsOptions{}
						if lower != nil {
							options = append(options, lower(strconv.Itoa(lowerID), &databaseOptions))
						}
						if upper != nil {
							options = append(options, upper(strconv.Itoa(upperID), &databaseOptions))
						}

						isRefusedByDatabase := false
						for _, err := range rawClient(t).ReadEvents(context.Background(), subject, databaseOptions) {
							if err != nil {
								isRefusedByDatabase = true
								break
							}
						}

						_, errs := readAll(t, store, architecturekit.ExactSubject(subject), options...)
						isRefused := len(errs) > 0 && errors.Is(errs[0], architecturekit.ErrEmptyRange)

						assert.Equal(t, isRefusedByDatabase, isRefused, name)
						if !isRefused {
							assert.Empty(t, errs, name)
						}
					}
				}
			}
		}
	})
}
