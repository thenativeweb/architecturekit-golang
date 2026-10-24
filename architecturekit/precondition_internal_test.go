package architecturekit

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/eventsourcingdb-client-golang/v2/eventsourcingdb"
)

// The client keeps its precondition types unexported, but not their methods.
type subjectPrecondition interface{ Subject() string }

type revisionPrecondition interface {
	Subject() string
	EventID() string
}

func TestResolvePreconditions(t *testing.T) {
	t.Run("fills in OnStateRead with the last event read", func(t *testing.T) {
		resolved := resolvePreconditions("/books/42", []Precondition{OnStateRead()}, "7")

		require.Len(t, resolved, 1)
		revision, ok := resolved[0].(revisionPrecondition)
		require.True(t, ok, "got %#v", resolved[0])
		assert.Equal(t, "/books/42", revision.Subject())
		assert.Equal(t, "7", revision.EventID())
	})

	t.Run("fills in OnStateRead on a subject without events with a pristine check", func(t *testing.T) {
		resolved := resolvePreconditions("/books/42", []Precondition{OnStateRead()}, "")

		require.Len(t, resolved, 1)
		_, isRevision := resolved[0].(revisionPrecondition)
		assert.False(t, isRevision, "got a revision check: %#v", resolved[0])
		subject, ok := resolved[0].(subjectPrecondition)
		require.True(t, ok, "got %#v", resolved[0])
		assert.Equal(t, "/books/42", subject.Subject())
	})

	t.Run("passes required preconditions on as they are, in order", func(t *testing.T) {
		first := eventsourcingdb.NewIsSubjectPopulatedPrecondition("/books/1")
		second := eventsourcingdb.NewIsEventQLQueryTruePrecondition("FROM e IN events PROJECT INTO true")

		resolved := resolvePreconditions("/books/42", []Precondition{Require(first), OnStateRead(), Require(second)}, "7")

		require.Len(t, resolved, 3)
		assert.Equal(t, first, resolved[0])
		assert.Equal(t, second, resolved[2])
	})

	t.Run("turns OnEventID into the revision check of the database", func(t *testing.T) {
		resolved := resolvePreconditions("/books/42", []Precondition{OnEventID("/books/23", "5"), OnStateRead()}, "7")

		require.Len(t, resolved, 2)
		assert.Equal(t, eventsourcingdb.NewIsSubjectOnEventIDPrecondition("/books/23", "5"), resolved[0])
		assert.Equal(t, eventsourcingdb.NewIsSubjectOnEventIDPrecondition("/books/42", "7"), resolved[1])
	})

	t.Run("checks nothing for Unconditionally", func(t *testing.T) {
		resolved := resolvePreconditions("/books/42", []Precondition{Unconditionally()}, "7")

		assert.Empty(t, resolved)
	})

	t.Run("resolves every declared precondition at the index it is declared at, adding none", func(t *testing.T) {
		// The database names a precondition that did not hold by its index in
		// the write, so that index has to be the one of the declared
		// precondition (see violatedPrecondition).
		declared := severalOfAKind()

		resolved := resolvePreconditions("/books/42", declared, "7")

		require.Len(t, resolved, len(declared))
		for i, precondition := range declared {
			if precondition.IsOnStateRead() {
				assert.Equal(t, eventsourcingdb.NewIsSubjectOnEventIDPrecondition("/books/42", "7"), resolved[i], "index %d", i)
				continue
			}

			database, _ := precondition.Database()
			assert.Equal(t, database, resolved[i], "index %d", i)
		}
	})
}

// severalOfAKind are the preconditions of a command on /books/42 that checks
// several subjects, three of them as the same kind of the database,
// isSubjectOnEventId, which only their index tells apart: OnStateRead, once
// Execute has read an event, and both of OnEventID.
func severalOfAKind() []Precondition {
	return []Precondition{
		OnEventID("/books/23", "3"),
		OnStateRead(),
		OnEventID("/books/42", "5"),
		OnPristineSubject("/books/7"),
		Require(eventsourcingdb.NewIsEventQLQueryTruePrecondition("FROM e IN events PROJECT INTO COUNT() == 0")),
	}
}

// violatedWith is a precondition that did not hold, as Execute reports it for
// a database that offers the v2 API, which reports it with the status 200,
// the code precondition-violated, and the given details.
func violatedWith(details string) error {
	return databaseFailure(&eventsourcingdb.DBAPIError{
		Action:      "write events",
		Status:      http.StatusOK,
		Code:        eventsourcingdb.CodePreconditionViolated,
		Description: "/preconditions/1: precondition isSubjectOnEventId does not hold",
		Details:     json.RawMessage(details),
	}, `writing "/books/42"`)
}

func TestViolatedPrecondition(t *testing.T) {
	declared := severalOfAKind()

	t.Run("is the declared precondition at the index the details name", func(t *testing.T) {
		for i, want := range declared {
			violated, isNamed := violatedPrecondition(violatedWith(fmt.Sprintf(`{"precondition":%d}`, i)), declared)

			require.True(t, isNamed, "index %d", i)
			assert.Equal(t, want, violated, "index %d", i)
		}
	})

	t.Run("tells OnStateRead from the others of the same kind, on its own subject and on another one", func(t *testing.T) {
		violated, isNamed := violatedPrecondition(violatedWith(`{"precondition":1}`), declared)
		require.True(t, isNamed)
		assert.True(t, violated.IsOnStateRead())

		for _, i := range []int{0, 2} {
			violated, isNamed := violatedPrecondition(violatedWith(fmt.Sprintf(`{"precondition":%d}`, i)), declared)
			require.True(t, isNamed, "index %d", i)
			assert.False(t, violated.IsOnStateRead(), "index %d", i)
		}
	})

	t.Run("is none for an index beyond the declared preconditions", func(t *testing.T) {
		for _, details := range []string{
			`{"precondition":5}`,
			`{"precondition":-1}`,
			`{"precondition":1099511627776}`,
		} {
			_, isNamed := violatedPrecondition(violatedWith(details), declared)

			assert.False(t, isNamed, "details %s", details)
		}
	})

	t.Run("is none for details without an index", func(t *testing.T) {
		for _, details := range []string{
			``,
			`null`,
			`{}`,
			`{"precondition":null}`,
			`{"event":1}`,
		} {
			_, isNamed := violatedPrecondition(violatedWith(details), declared)

			assert.False(t, isNamed, "details %q", details)
		}
	})

	t.Run("is none for details that can not be read", func(t *testing.T) {
		for _, details := range []string{
			`{"precondition":"1"}`,
			`{"precondition":1.5}`,
			`{"precondition":true}`,
			`{"precondition":`,
			`[1]`,
			`1`,
		} {
			_, isNamed := violatedPrecondition(violatedWith(details), declared)

			assert.False(t, isNamed, "details %q", details)
		}
	})

	t.Run("is none for a database that offers just the v1 API, which names none", func(t *testing.T) {
		// The client gives the 409 the code of the v2 API, but there are no
		// details to give it.
		err := databaseFailure(&eventsourcingdb.DBAPIError{
			Action:      "write events",
			Status:      http.StatusConflict,
			Code:        eventsourcingdb.CodePreconditionViolated,
			Description: "state conflict: precondition failed",
		}, `writing "/books/42"`)
		require.ErrorIs(t, err, ErrConflict)

		_, isNamed := violatedPrecondition(err, declared)

		assert.False(t, isNamed)
	})

	t.Run("is none for another code, even if its details name a precondition", func(t *testing.T) {
		// A query that is malformed is named by its index as well, but it is a
		// mistake of the request, not a precondition that did not hold.
		err := databaseFailure(&eventsourcingdb.DBAPIError{
			Action:      "write events",
			Status:      http.StatusOK,
			Code:        eventsourcingdb.CodeQueryMalformed,
			Description: "/preconditions/4: query is malformed",
			Details:     json.RawMessage(`{"precondition":4}`),
		}, `writing "/books/42"`)

		_, isNamed := violatedPrecondition(err, declared)

		assert.False(t, isNamed)
	})

	t.Run("is none for a failure without an answer", func(t *testing.T) {
		_, isNamed := violatedPrecondition(errors.New("connection refused"), declared)

		assert.False(t, isNamed)
	})
}
