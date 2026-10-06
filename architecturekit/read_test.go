package architecturekit_test

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// readAll collects what Read hands out, and the errors separately.
func readAll(
	t *testing.T,
	store *architecturekit.Store,
	subjects architecturekit.Subjects,
	options ...architecturekit.ReadOption,
) ([]eventsourcingdb.Event, []error) {
	t.Helper()

	var events []eventsourcingdb.Event
	var errs []error
	for event, err := range architecturekit.Read(context.Background(), store, subjects, options...) {
		if err != nil {
			errs = append(errs, err)
			continue
		}
		events = append(events, event)
	}

	return events, errs
}

func idsOf(events []eventsourcingdb.Event) []string {
	ids := make([]string, len(events))
	for i, event := range events {
		ids[i] = event.ID
	}

	return ids
}

// writeIDs writes events past the framework and returns their IDs, in the
// order they were written.
func writeIDs(t *testing.T, subject string, events ...architecturekit.Event) []string {
	t.Helper()

	candidates := make([]eventsourcingdb.EventCandidate, len(events))
	for i, event := range events {
		candidates[i] = eventsourcingdb.EventCandidate{
			Source:  "https://thenativeweb.io",
			Subject: subject,
			Type:    event.EventType(),
			Data:    event,
		}
	}

	written, err := rawClient(t).WriteEvents(candidates, nil)
	require.NoError(t, err, "failed to write to %q", subject)

	return idsOf(written)
}

// fiveIn writes five events to a subject of its own and returns their IDs.
func fiveIn(t *testing.T) (string, []string) {
	t.Helper()

	subject := subjectFor(t)
	ids := writeIDs(t, subject,
		incremented{By: 1}, incremented{By: 2}, incremented{By: 3}, incremented{By: 4}, incremented{By: 5})

	return subject, ids
}

// unaskedDatabase returns a store for a database that only records whether it
// was asked at all.
func unaskedDatabase(t *testing.T) (*architecturekit.Store, *atomic.Bool) {
	t.Helper()

	asked := &atomic.Bool{}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		asked.Store(true)
		writer.Header().Set("Server", "EventSourcingDB/test")
	}))
	t.Cleanup(server.Close)

	return architecturekit.NewStore(clientFor(t, server), "https://thenativeweb.io"), asked
}

func TestRead(t *testing.T) {
	t.Run("reads the events of a subject in order", func(t *testing.T) {
		subject, ids := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject))
		require.Empty(t, errs)
		assert.Equal(t, ids, idsOf(events), "the events come in the order they were written")
		assert.Equal(t, subject, events[0].Subject)
	})

	t.Run("reads an exact subject without the subjects below it", func(t *testing.T) {
		subject := subjectFor(t)
		own := writeIDs(t, subject, incremented{By: 1}, incremented{By: 2})
		writeIDs(t, subject+"/nested", incremented{By: 3})

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject))
		require.Empty(t, errs)
		assert.Equal(t, own, idsOf(events))
	})

	t.Run("reads a subject tree with the subjects below it", func(t *testing.T) {
		subject := subjectFor(t)
		first := writeIDs(t, subject, incremented{By: 1})
		nested := writeIDs(t, subject+"/nested", incremented{By: 2})
		last := writeIDs(t, subject, incremented{By: 3})
		writeIDs(t, subject+"-sibling", incremented{By: 4})

		events, errs := readAll(t, requireStore(t), architecturekit.SubjectTree(subject))
		require.Empty(t, errs)
		assert.Equal(t, []string{first[0], nested[0], last[0]}, idsOf(events),
			"a tree holds the subject and the ones below it, but not a sibling that only starts alike")
	})

	t.Run("stops when the caller stops", func(t *testing.T) {
		subject, ids := fiveIn(t)

		var first []eventsourcingdb.Event
		for event, err := range architecturekit.Read(context.Background(), requireStore(t), architecturekit.ExactSubject(subject)) {
			require.NoError(t, err)
			first = append(first, event)
			break
		}

		assert.Equal(t, ids[:1], idsOf(first))
	})

	t.Run("refuses an event whose hash does not match", func(t *testing.T) {
		store := architecturekit.NewStore(newFakeDatabase(t, &fakeDatabase{events: []int{0, 1}, tampered: true}), "https://thenativeweb.io")

		events, errs := readAll(t, store, architecturekit.ExactSubject("/test"))

		assert.Empty(t, events, "an unverified event must not be handed out")
		require.Len(t, errs, 1, "the iteration ends with the first error")
		expectUnverified(t, errs[0])
	})

	t.Run("hands out events whose hashes match", func(t *testing.T) {
		store := architecturekit.NewStore(newFakeDatabase(t, &fakeDatabase{events: []int{0, 1}}), "https://thenativeweb.io")

		events, errs := readAll(t, store, architecturekit.ExactSubject("/test"))

		require.Empty(t, errs)
		assert.Equal(t, []string{"0", "1"}, idsOf(events))
	})

	t.Run("sorts a rejected API token as permanent", func(t *testing.T) {
		store := architecturekit.NewStore(
			refusingDatabase(t, "/api/v1/read-events", http.StatusUnauthorized, "unauthorized"), "https://thenativeweb.io")

		_, errs := readAll(t, store, architecturekit.ExactSubject("/test"))

		require.Len(t, errs, 1)
		assert.ErrorIs(t, errs[0], architecturekit.ErrPermanent)
		assert.ErrorContains(t, errs[0], "the database rejected the API token")
		assert.ErrorContains(t, errs[0], `reading "/test"`, "the error has to name the subject")
	})

	t.Run("sorts an unreachable database as transient", func(t *testing.T) {
		server := httptest.NewServer(http.NotFoundHandler())
		server.Close()
		store := architecturekit.NewStore(clientFor(t, server), "https://thenativeweb.io")

		_, errs := readAll(t, store, architecturekit.ExactSubject("/test"))

		require.Len(t, errs, 1)
		assert.ErrorIs(t, errs[0], architecturekit.ErrTransient)
		assert.NotErrorIs(t, errs[0], architecturekit.ErrPermanent)
	})

	t.Run("panics for the zero value of the subjects, before reading", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit: reading needs the subjects of SubjectTree or ExactSubject, not none", func() {
			architecturekit.Read(context.Background(), nil, architecturekit.Subjects{})
		})
	})
}

func TestReadBounds(t *testing.T) {
	t.Run("FromEvent includes the event with the given ID", func(t *testing.T) {
		subject, ids := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject), architecturekit.FromEvent(ids[1]))
		require.Empty(t, errs)
		assert.Equal(t, ids[1:], idsOf(events))
	})

	t.Run("AfterEvent leaves out the event with the given ID", func(t *testing.T) {
		subject, ids := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject), architecturekit.AfterEvent(ids[1]))
		require.Empty(t, errs)
		assert.Equal(t, ids[2:], idsOf(events))
	})

	t.Run("UpToEvent includes the event with the given ID", func(t *testing.T) {
		subject, ids := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject), architecturekit.UpToEvent(ids[3]))
		require.Empty(t, errs)
		assert.Equal(t, ids[:4], idsOf(events))
	})

	t.Run("BeforeEvent leaves out the event with the given ID", func(t *testing.T) {
		subject, ids := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject), architecturekit.BeforeEvent(ids[3]))
		require.Empty(t, errs)
		assert.Equal(t, ids[:3], idsOf(events))
	})

	t.Run("a lower and an upper bound read what lies between them", func(t *testing.T) {
		subject, ids := fiveIn(t)
		store := requireStore(t)

		for name, test := range map[string]struct {
			options  []architecturekit.ReadOption
			expected []string
		}{
			"FromEvent and UpToEvent":    {[]architecturekit.ReadOption{architecturekit.FromEvent(ids[1]), architecturekit.UpToEvent(ids[3])}, ids[1:4]},
			"FromEvent and BeforeEvent":  {[]architecturekit.ReadOption{architecturekit.FromEvent(ids[1]), architecturekit.BeforeEvent(ids[3])}, ids[1:3]},
			"AfterEvent and UpToEvent":   {[]architecturekit.ReadOption{architecturekit.AfterEvent(ids[1]), architecturekit.UpToEvent(ids[3])}, ids[2:4]},
			"AfterEvent and BeforeEvent": {[]architecturekit.ReadOption{architecturekit.AfterEvent(ids[1]), architecturekit.BeforeEvent(ids[3])}, ids[2:3]},
			"FromEvent and UpToEvent of the same event": {
				[]architecturekit.ReadOption{architecturekit.FromEvent(ids[2]), architecturekit.UpToEvent(ids[2])}, ids[2:3],
			},
			"the upper bound before the lower one in the code": {
				[]architecturekit.ReadOption{architecturekit.UpToEvent(ids[3]), architecturekit.FromEvent(ids[1])}, ids[1:4],
			},
		} {
			t.Run(name, func(t *testing.T) {
				events, errs := readAll(t, store, architecturekit.ExactSubject(subject), test.options...)
				require.Empty(t, errs)
				assert.Equal(t, test.expected, idsOf(events))
			})
		}
	})

	t.Run("a bound does not have to be an event of the subjects that are read", func(t *testing.T) {
		subject := subjectFor(t)
		before := writeIDs(t, subject, incremented{By: 1})
		elsewhere := writeIDs(t, subject+"-elsewhere", incremented{By: 2})
		after := writeIDs(t, subject, incremented{By: 3})

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject), architecturekit.AfterEvent(elsewhere[0]))
		require.Empty(t, errs)
		assert.Equal(t, after, idsOf(events))

		events, errs = readAll(t, requireStore(t), architecturekit.ExactSubject(subject), architecturekit.BeforeEvent(elsewhere[0]))
		require.Empty(t, errs)
		assert.Equal(t, before, idsOf(events))
	})

	t.Run("the highest possible ID is an ID", func(t *testing.T) {
		subject, ids := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject), architecturekit.UpToEvent("9223372036854775807"))
		require.Empty(t, errs)
		assert.Equal(t, ids, idsOf(events))
	})

	t.Run("bounds that leave no room for an event are refused before the database is asked", func(t *testing.T) {
		subject, ids := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject),
			architecturekit.AfterEvent(ids[1]), architecturekit.BeforeEvent(ids[2]))

		assert.Empty(t, events)
		require.Len(t, errs, 1)
		assert.ErrorIs(t, errs[0], architecturekit.ErrEmptyRange)
		assert.NotErrorIs(t, errs[0], architecturekit.ErrPermanent)
		assert.EqualError(t, errs[0], `empty range: no event can lie after "`+ids[1]+`" and before "`+ids[2]+`"`)
	})
}

func TestReadNewestFirst(t *testing.T) {
	t.Run("hands out the newest event first", func(t *testing.T) {
		subject, ids := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject), architecturekit.NewestFirst())
		require.Empty(t, errs)
		assert.Equal(t, []string{ids[4], ids[3], ids[2], ids[1], ids[0]}, idsOf(events))
	})

	t.Run("keeps the bounds", func(t *testing.T) {
		subject, ids := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject),
			architecturekit.NewestFirst(), architecturekit.AfterEvent(ids[0]), architecturekit.BeforeEvent(ids[4]))
		require.Empty(t, errs)
		assert.Equal(t, []string{ids[3], ids[2], ids[1]}, idsOf(events))
	})

	t.Run("reads a subject tree newest first", func(t *testing.T) {
		subject := subjectFor(t)
		first := writeIDs(t, subject, incremented{By: 1})
		nested := writeIDs(t, subject+"/nested", incremented{By: 2})

		events, errs := readAll(t, requireStore(t), architecturekit.SubjectTree(subject), architecturekit.NewestFirst())
		require.Empty(t, errs)
		assert.Equal(t, []string{nested[0], first[0]}, idsOf(events))
	})
}

func TestReadFromLatestEvent(t *testing.T) {
	resetType := reset{}.EventType()

	t.Run("reads from the latest event of the type and includes it", func(t *testing.T) {
		subject := subjectFor(t)
		ids := writeIDs(t, subject, incremented{By: 1}, reset{}, incremented{By: 2}, reset{}, incremented{By: 3})

		for name, ifMissing := range map[string]architecturekit.IfLatestEventIsMissing{
			"ReadEverything": architecturekit.ReadEverything,
			"ReadNothing":    architecturekit.ReadNothing,
		} {
			t.Run(name, func(t *testing.T) {
				events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject),
					architecturekit.FromLatestEvent(subject, resetType, ifMissing))
				require.Empty(t, errs)
				assert.Equal(t, ids[3:], idsOf(events), "the missing case does not matter if the event is there")
			})
		}
	})

	t.Run("reads everything if the event is missing and ReadEverything is given", func(t *testing.T) {
		subject, ids := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject),
			architecturekit.FromLatestEvent(subject, resetType, architecturekit.ReadEverything))
		require.Empty(t, errs)
		assert.Equal(t, ids, idsOf(events))
	})

	t.Run("reads nothing if the event is missing and ReadNothing is given", func(t *testing.T) {
		subject, _ := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject),
			architecturekit.FromLatestEvent(subject, resetType, architecturekit.ReadNothing))
		require.Empty(t, errs)
		assert.Empty(t, events)
	})

	t.Run("looks for the event on the given subject alone and not below it", func(t *testing.T) {
		subject := subjectFor(t)
		ids := writeIDs(t, subject, incremented{By: 1})
		writeIDs(t, subject+"/nested", reset{})

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject),
			architecturekit.FromLatestEvent(subject, resetType, architecturekit.ReadEverything))
		require.Empty(t, errs)
		assert.Equal(t, ids, idsOf(events), "the reset below the subject does not count")
	})

	t.Run("reads a subject tree from the latest event of a subject in it", func(t *testing.T) {
		subject := subjectFor(t)
		writeIDs(t, subject, incremented{By: 1})
		latest := writeIDs(t, subject+"/nested", reset{})
		after := writeIDs(t, subject, incremented{By: 2})

		events, errs := readAll(t, requireStore(t), architecturekit.SubjectTree(subject),
			architecturekit.FromLatestEvent(subject+"/nested", resetType, architecturekit.ReadNothing))
		require.Empty(t, errs)
		assert.Equal(t, []string{latest[0], after[0]}, idsOf(events))
	})

	t.Run("goes with an upper bound", func(t *testing.T) {
		subject := subjectFor(t)
		ids := writeIDs(t, subject, incremented{By: 1}, reset{}, incremented{By: 2}, incremented{By: 3})

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject),
			architecturekit.FromLatestEvent(subject, resetType, architecturekit.ReadNothing), architecturekit.UpToEvent(ids[2]))
		require.Empty(t, errs)
		assert.Equal(t, ids[1:3], idsOf(events))

		events, errs = readAll(t, requireStore(t), architecturekit.ExactSubject(subject),
			architecturekit.BeforeEvent(ids[2]), architecturekit.FromLatestEvent(subject, resetType, architecturekit.ReadNothing))
		require.Empty(t, errs)
		assert.Equal(t, ids[1:2], idsOf(events))
	})

	t.Run("reads everything up to an upper bound if the event is missing and ReadEverything is given", func(t *testing.T) {
		subject, ids := fiveIn(t)

		events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject),
			architecturekit.FromLatestEvent(subject, resetType, architecturekit.ReadEverything), architecturekit.UpToEvent(ids[2]))
		require.Empty(t, errs)
		assert.Equal(t, ids[:3], idsOf(events))
	})

	t.Run("is refused by the database as permanent if the latest event comes after the upper bound", func(t *testing.T) {
		subject := subjectFor(t)
		ids := writeIDs(t, subject, incremented{By: 1}, reset{}, incremented{By: 2})

		// The database answers with 409, which would be a conflict if it were a
		// write. A read has no precondition that could hold later, though, and
		// new events only move the latest one further away.
		for name, upperBound := range map[string]architecturekit.ReadOption{
			"UpToEvent":   architecturekit.UpToEvent(ids[0]),
			"BeforeEvent": architecturekit.BeforeEvent(ids[1]),
		} {
			t.Run(name, func(t *testing.T) {
				events, errs := readAll(t, requireStore(t), architecturekit.ExactSubject(subject),
					architecturekit.FromLatestEvent(subject, resetType, architecturekit.ReadEverything), upperBound)

				assert.Empty(t, events)
				require.Len(t, errs, 1)
				assert.ErrorIs(t, errs[0], architecturekit.ErrPermanent)
				assert.NotErrorIs(t, errs[0], architecturekit.ErrConflict, "a read has no precondition that did not hold")
				assert.NotErrorIs(t, errs[0], architecturekit.ErrTransient, "trying again never helps")
				assert.ErrorContains(t, errs[0], "fromLatestEvent results in an event ID greater than upperBound ID",
					"the message has to keep the reason of the database")
				assert.ErrorContains(t, errs[0], `reading "`+subject+`"`)
			})
		}
	})
}

func TestReadWithInvalidEventIDs(t *testing.T) {
	for name, option := range map[string]func(string) architecturekit.ReadOption{
		"FromEvent":   architecturekit.FromEvent,
		"AfterEvent":  architecturekit.AfterEvent,
		"UpToEvent":   architecturekit.UpToEvent,
		"BeforeEvent": architecturekit.BeforeEvent,
	} {
		for _, id := range []string{"", "abc", "-1", "1.5", " 1", "9223372036854775808"} {
			t.Run(name+" refuses "+id+" before asking the database", func(t *testing.T) {
				store, asked := unaskedDatabase(t)

				events, errs := readAll(t, store, architecturekit.ExactSubject("/books/42"), option(id))

				assert.Empty(t, events)
				require.Len(t, errs, 1, "the iteration ends with the error")
				assert.ErrorIs(t, errs[0], architecturekit.ErrNotARevision)
				assert.NotErrorIs(t, errs[0], architecturekit.ErrPermanent)
				assert.NotErrorIs(t, errs[0], architecturekit.ErrTransient)
				assert.EqualError(t, errs[0], `not a revision: "`+id+`"`,
					"the text is for the caller, who knows neither the subject nor the option")
				assert.False(t, asked.Load(), "the database must not be asked")
			})
		}
	}

	t.Run("refuses the second bound as well, if the first one is fine", func(t *testing.T) {
		store, asked := unaskedDatabase(t)

		_, errs := readAll(t, store, architecturekit.ExactSubject("/books/42"),
			architecturekit.FromEvent("1"), architecturekit.BeforeEvent("abc"))

		require.Len(t, errs, 1)
		assert.EqualError(t, errs[0], `not a revision: "abc"`)
		assert.False(t, asked.Load(), "the database must not be asked")
	})

	t.Run("refuses the lower bound first, if neither is fine", func(t *testing.T) {
		store, _ := unaskedDatabase(t)

		_, errs := readAll(t, store, architecturekit.ExactSubject("/books/42"),
			architecturekit.AfterEvent("abc"), architecturekit.UpToEvent("xyz"))

		require.Len(t, errs, 1)
		assert.EqualError(t, errs[0], `not a revision: "abc"`)
	})

	t.Run("asks the database for IDs that are fine", func(t *testing.T) {
		store, asked := unaskedDatabase(t)

		events, errs := readAll(t, store, architecturekit.ExactSubject("/books/42"),
			architecturekit.FromEvent("0"), architecturekit.UpToEvent("9223372036854775807"))

		assert.Empty(t, events)
		assert.Empty(t, errs)
		assert.True(t, asked.Load())
	})

	t.Run("asks the database for FromLatestEvent, which has no ID", func(t *testing.T) {
		store, asked := unaskedDatabase(t)

		_, errs := readAll(t, store, architecturekit.ExactSubject("/books/42"),
			architecturekit.FromLatestEvent("/books/42", reset{}.EventType(), architecturekit.ReadEverything))

		assert.Empty(t, errs)
		assert.True(t, asked.Load())
	})

	t.Run("refuses the ID on every iteration", func(t *testing.T) {
		store, asked := unaskedDatabase(t)
		events := architecturekit.Read(context.Background(), store, architecturekit.ExactSubject("/books/42"), architecturekit.AfterEvent("abc"))

		for range 2 {
			var errs []error
			for _, err := range events {
				errs = append(errs, err)
			}

			require.Len(t, errs, 1)
			assert.ErrorIs(t, errs[0], architecturekit.ErrNotARevision)
		}
		assert.False(t, asked.Load())
	})
}

// contradiction is a set of read options that contradict each other, and the
// message Read panics with for them.
type contradiction struct {
	options []architecturekit.ReadOption
	message string
}

// contradictions holds every way read options can contradict each other, by
// name.
func contradictions() map[string]contradiction {
	fromLatest := architecturekit.FromLatestEvent("/books/42", "io.eventsourcingdb.library.book-audited", architecturekit.ReadEverything)
	const fromLatestText = `FromLatestEvent("/books/42", "io.eventsourcingdb.library.book-audited")`

	return map[string]contradiction{
		"FromEvent and AfterEvent": {
			[]architecturekit.ReadOption{architecturekit.FromEvent("3"), architecturekit.AfterEvent("5")},
			`architecturekit: a read has one lower bound, but got FromEvent("3") and AfterEvent("5")`,
		},
		"AfterEvent and FromEvent": {
			[]architecturekit.ReadOption{architecturekit.AfterEvent("5"), architecturekit.FromEvent("3")},
			`architecturekit: a read has one lower bound, but got AfterEvent("5") and FromEvent("3")`,
		},
		"FromEvent twice": {
			[]architecturekit.ReadOption{architecturekit.FromEvent("3"), architecturekit.FromEvent("3")},
			`architecturekit: a read has one lower bound, but got FromEvent("3") and FromEvent("3")`,
		},
		"AfterEvent twice": {
			[]architecturekit.ReadOption{architecturekit.AfterEvent("3"), architecturekit.AfterEvent("4")},
			`architecturekit: a read has one lower bound, but got AfterEvent("3") and AfterEvent("4")`,
		},
		"UpToEvent and BeforeEvent": {
			[]architecturekit.ReadOption{architecturekit.UpToEvent("7"), architecturekit.BeforeEvent("9")},
			`architecturekit: a read has one upper bound, but got UpToEvent("7") and BeforeEvent("9")`,
		},
		"BeforeEvent and UpToEvent": {
			[]architecturekit.ReadOption{architecturekit.BeforeEvent("9"), architecturekit.UpToEvent("7")},
			`architecturekit: a read has one upper bound, but got BeforeEvent("9") and UpToEvent("7")`,
		},
		"UpToEvent twice": {
			[]architecturekit.ReadOption{architecturekit.UpToEvent("7"), architecturekit.UpToEvent("8")},
			`architecturekit: a read has one upper bound, but got UpToEvent("7") and UpToEvent("8")`,
		},
		"BeforeEvent twice": {
			[]architecturekit.ReadOption{architecturekit.BeforeEvent("9"), architecturekit.BeforeEvent("9")},
			`architecturekit: a read has one upper bound, but got BeforeEvent("9") and BeforeEvent("9")`,
		},
		"NewestFirst twice": {
			[]architecturekit.ReadOption{architecturekit.NewestFirst(), architecturekit.NewestFirst()},
			`architecturekit: a read has one order, but got NewestFirst() and NewestFirst()`,
		},
		"FromLatestEvent and FromEvent": {
			[]architecturekit.ReadOption{fromLatest, architecturekit.FromEvent("3")},
			`architecturekit: a read has one lower bound, but got ` + fromLatestText + ` and FromEvent("3")`,
		},
		"AfterEvent and FromLatestEvent": {
			[]architecturekit.ReadOption{architecturekit.AfterEvent("3"), fromLatest},
			`architecturekit: a read has one lower bound, but got AfterEvent("3") and ` + fromLatestText,
		},
		"FromLatestEvent twice": {
			[]architecturekit.ReadOption{
				fromLatest, architecturekit.FromLatestEvent("/books/43", "io.eventsourcingdb.library.book-audited", architecturekit.ReadNothing),
			},
			`architecturekit: a read has one lower bound, but got ` + fromLatestText +
				` and FromLatestEvent("/books/43", "io.eventsourcingdb.library.book-audited")`,
		},
		"FromLatestEvent and NewestFirst": {
			[]architecturekit.ReadOption{fromLatest, architecturekit.NewestFirst()},
			`architecturekit: the database reads from the latest event of a type only oldest first, but got ` +
				fromLatestText + ` and NewestFirst()`,
		},
		"NewestFirst and FromLatestEvent": {
			[]architecturekit.ReadOption{architecturekit.NewestFirst(), architecturekit.UpToEvent("9"), fromLatest},
			`architecturekit: the database reads from the latest event of a type only oldest first, but got ` +
				fromLatestText + ` and NewestFirst()`,
		},
	}
}

func TestReadOptionContradictions(t *testing.T) {
	read := func(options ...architecturekit.ReadOption) func() {
		return func() {
			architecturekit.Read(context.Background(), nil, architecturekit.ExactSubject("/books/42"), options...)
		}
	}
	fromLatest := architecturekit.FromLatestEvent("/books/42", "io.eventsourcingdb.library.book-audited", architecturekit.ReadEverything)

	for name, test := range contradictions() {
		t.Run(name+" panic", func(t *testing.T) {
			assert.PanicsWithValue(t, test.message, read(test.options...))
		})
	}

	t.Run("a nil option panics, also one that was declared but never set", func(t *testing.T) {
		// Read panics when it is called, as for options that contradict each
		// other, not once the events are iterated.
		var declared architecturekit.ReadOption

		for _, option := range []architecturekit.ReadOption{nil, declared} {
			assert.PanicsWithValue(t, "architecturekit: Read got a nil option", read(architecturekit.FromEvent("1"), option))
		}
	})

	t.Run("options that do not contradict each other do not panic", func(t *testing.T) {
		assert.NotPanics(t, read(architecturekit.FromEvent("1"), architecturekit.UpToEvent("2"), architecturekit.NewestFirst()))
		assert.NotPanics(t, read(architecturekit.AfterEvent("1"), architecturekit.BeforeEvent("2")))
		assert.NotPanics(t, read(fromLatest, architecturekit.UpToEvent("2")))
		assert.NotPanics(t, read(fromLatest, architecturekit.BeforeEvent("2")))
	})

	t.Run("FromLatestEvent panics for a subject without a leading slash", func(t *testing.T) {
		assert.PanicsWithValue(t, `architecturekit: a subject starts with a slash, which "books/42" does not`, func() {
			architecturekit.FromLatestEvent("books/42", "io.eventsourcingdb.library.book-audited", architecturekit.ReadEverything)
		})
		assert.PanicsWithValue(t, `architecturekit: a subject starts with a slash, which "" does not`, func() {
			architecturekit.FromLatestEvent("", "io.eventsourcingdb.library.book-audited", architecturekit.ReadNothing)
		})
	})

	t.Run("FromLatestEvent panics for neither ReadEverything nor ReadNothing", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit: FromLatestEvent needs ReadEverything or ReadNothing, not IfLatestEventIsMissing(0)", func() {
			architecturekit.FromLatestEvent("/books/42", "io.eventsourcingdb.library.book-audited", architecturekit.IfLatestEventIsMissing(0))
		})
		assert.PanicsWithValue(t, "architecturekit: FromLatestEvent needs ReadEverything or ReadNothing, not IfLatestEventIsMissing(3)", func() {
			architecturekit.FromLatestEvent("/books/42", "io.eventsourcingdb.library.book-audited", architecturekit.IfLatestEventIsMissing(3))
		})
	})
}
