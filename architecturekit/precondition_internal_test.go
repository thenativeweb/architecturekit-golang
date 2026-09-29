package architecturekit

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
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

	t.Run("checks nothing for Unconditionally", func(t *testing.T) {
		resolved := resolvePreconditions("/books/42", []Precondition{Unconditionally()}, "7")

		assert.Empty(t, resolved)
	})
}
