package architecturekit_test

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/v2/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/v2/eventsourcingdb"
)

// The client refuses a request before it sends it if the database does not
// support what the request needs, such as a recursive precondition, which a
// database that offers just the v1 API can not check. Only upgrading the
// database helps, so the failure is permanent, and since the request never
// left, a write has certainly stored nothing.

// recursivelyPristine lets the events be written only if neither the subject
// nor any subject below it has events yet, which only a database that offers
// the v2 API can check.
func recursivelyPristine(subject string) architecturekit.Precondition {
	return architecturekit.Require(eventsourcingdb.NewIsSubjectPristinePrecondition(subject,
		eventsourcingdb.PreconditionOptions{Recursive: true}))
}

// recursiveWrites are the two ways the kit writes, each with a recursive
// precondition on the given subject: Execute with a command that declares it
// with Require, and Write.
var recursiveWrites = []struct {
	name  string
	write func(ctx context.Context, store *architecturekit.Store, subject string) error
}{
	{"Execute", func(ctx context.Context, store *architecturekit.Store, subject string) error {
		_, err := architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: 1}.declaring(recursivelyPristine(subject)))

		return err
	}},
	{"Write", func(ctx context.Context, store *architecturekit.Store, subject string) error {
		_, err := architecturekit.Write(ctx, store,
			[]architecturekit.EventOn{{Subject: subject, Event: incremented{By: 1}}}, recursivelyPristine(subject))

		return err
	}},
}

// assertNotSupported asserts that a write failed because the database does
// not support what it needs: permanently, since only upgrading the database
// helps, and not with an unknown outcome, since the client never sent it.
func assertNotSupported(t *testing.T, err error) {
	t.Helper()

	assert.ErrorIs(t, err, architecturekit.ErrPermanent, "only upgrading the database helps")
	assert.NotErrorIs(t, err, architecturekit.ErrTransient, "trying again never helps")
	assert.NotErrorIs(t, err, architecturekit.ErrOutcomeUnknown, "the client never sent the write")
	assert.ErrorIs(t, err, eventsourcingdb.ErrIncompatibleServer, "errors.Is has to reach the error of the client")
	assert.ErrorContains(t, err, "the database does not support what the request needs, and has to be upgraded")
}

// offersJustTheV1API reports whether the database offers just the v1 API. It
// tells them apart as assertMalformedSubject does, by how the database refuses
// an event for a malformed subject: one that offers the v2 API names every
// error with a code, while one that offers just the v1 API reports this one
// without a code. The event is written past the store, so that nothing of the
// kit decides.
func offersJustTheV1API(t *testing.T) bool {
	t.Helper()

	_, err := rawClient(t).WriteEvents(context.Background(), []eventsourcingdb.EventCandidate{{
		Source:  "https://thenativeweb.io",
		Subject: "malformed",
		Type:    (incremented{}).EventType(),
		Data:    incremented{By: 1},
	}}, nil)

	answer, isAnswer := errors.AsType[*eventsourcingdb.DBAPIError](err)
	require.True(t, isAnswer, "the database has to refuse a malformed subject, got: %v", err)

	return answer.Code == ""
}

func TestAWriteThatTheDatabaseDoesNotSupport(t *testing.T) {
	t.Run("fails permanently without being sent if the database offers just the v1 API", func(t *testing.T) {
		for _, write := range recursiveWrites {
			t.Run(write.name, func(t *testing.T) {
				store := architecturekit.NewStore(writingDatabase(t, "/api/v1/write-events",
					func(writer http.ResponseWriter, _ *http.Request) {
						t.Error("the client must not send a write that the database does not support")
						writer.WriteHeader(http.StatusInternalServerError)
					}), "https://thenativeweb.io")

				err := write.write(context.Background(), store, "/test")

				assertNotSupported(t, err)
			})
		}
	})

	t.Run("against a real database", func(t *testing.T) {
		store := requireStore(t)

		if offersJustTheV1API(t) {
			t.Run("fails permanently if it offers just the v1 API", func(t *testing.T) {
				for _, write := range recursiveWrites {
					t.Run(write.name, func(t *testing.T) {
						subject := subjectFor(t)

						err := write.write(context.Background(), store, subject)

						assertNotSupported(t, err)
						assert.Zero(t, totalIn(t, store, subject), "nothing may have been written")
					})
				}
			})

			return
		}

		t.Run("succeeds if it offers the v2 API", func(t *testing.T) {
			for _, write := range recursiveWrites {
				t.Run(write.name, func(t *testing.T) {
					subject := subjectFor(t)

					err := write.write(context.Background(), store, subject)

					require.NoError(t, err)
					assert.Equal(t, 1, totalIn(t, store, subject))
				})
			}
		})
	})
}
