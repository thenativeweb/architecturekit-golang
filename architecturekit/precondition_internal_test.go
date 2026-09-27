package architecturekit

import (
	"testing"

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

		if len(resolved) != 1 {
			t.Fatalf("got %d preconditions, want 1", len(resolved))
		}
		revision, ok := resolved[0].(revisionPrecondition)
		if !ok || revision.Subject() != "/books/42" || revision.EventID() != "7" {
			t.Fatalf("got %#v", resolved[0])
		}
	})

	t.Run("fills in OnStateRead on a subject without events with a pristine check", func(t *testing.T) {
		resolved := resolvePreconditions("/books/42", []Precondition{OnStateRead()}, "")

		if len(resolved) != 1 {
			t.Fatalf("got %d preconditions, want 1", len(resolved))
		}
		if _, isRevision := resolved[0].(revisionPrecondition); isRevision {
			t.Fatalf("got a revision check: %#v", resolved[0])
		}
		subject, ok := resolved[0].(subjectPrecondition)
		if !ok || subject.Subject() != "/books/42" {
			t.Fatalf("got %#v", resolved[0])
		}
	})

	t.Run("passes required preconditions on as they are, in order", func(t *testing.T) {
		first := eventsourcingdb.NewIsSubjectPopulatedPrecondition("/books/1")
		second := eventsourcingdb.NewIsEventQLQueryTruePrecondition("FROM e IN events PROJECT INTO true")

		resolved := resolvePreconditions("/books/42", []Precondition{Require(first), OnStateRead(), Require(second)}, "7")

		if len(resolved) != 3 || resolved[0] != first || resolved[2] != second {
			t.Fatalf("got %#v", resolved)
		}
	})

	t.Run("checks nothing for Unconditionally", func(t *testing.T) {
		resolved := resolvePreconditions("/books/42", []Precondition{Unconditionally()}, "7")

		if len(resolved) != 0 {
			t.Fatalf("got %#v", resolved)
		}
	})
}
