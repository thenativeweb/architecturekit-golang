package architecturekit_test

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/v2/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/v2/eventsourcingdb"
)

// interfered counts the decisions of an interfering decider.
type interfered struct {
	decisions int
}

// interferingDecider decides like counterDecider, but for its first decisions,
// as many as times says, somebody else writes to the subject while it decides,
// so that writing what it decided conflicts.
func interferingDecider(t *testing.T, times int) (architecturekit.Decider[increment, counter], *interfered) {
	t.Helper()

	counted := &interfered{}
	decide := counterDecider().Decide

	return architecturekit.NewDecider(counterState(),
		func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
			counted.decisions++

			if counted.decisions <= times {
				_, err := rawClient(t).WriteEvents(context.Background(), []eventsourcingdb.EventCandidate{{
					Source:  "https://thenativeweb.io",
					Subject: cmd.subject,
					Type:    (incremented{}).EventType(),
					Data:    incremented{By: 100},
				}}, nil)
				require.NoError(t, err)
			}

			return decide(ctx, cmd, current)
		}), counted
}

// assertDecisionsByAPI asserts that Execute reported a conflict after the
// given number of decisions, which depends on the API the database offers. A
// database that offers the v2 API reports the conflict with the status 200,
// and names the precondition that did not hold, by its index among the
// declared ones, in the details, so there the decisions have to be
// throughV2, and the index violated. A database that offers just the v1 API
// answers with 409 and names none, so there they have to be throughV1.
func assertDecisionsByAPI(t *testing.T, err error, decisions, throughV2, throughV1, violated int) {
	t.Helper()

	require.ErrorIs(t, err, architecturekit.ErrConflict)
	answer, isAnswer := errors.AsType[*eventsourcingdb.DBAPIError](err)
	require.True(t, isAnswer, "errors.As has to reach the answer of the database, got: %v", err)
	assert.Equal(t, eventsourcingdb.CodePreconditionViolated, answer.Code)

	if answer.Status == http.StatusConflict {
		assert.Nil(t, answer.Details, "a database that offers just the v1 API names no precondition")
		assert.Equal(t, throughV1, decisions, "through the v1 API")
		return
	}

	assert.Equal(t, http.StatusOK, answer.Status)
	assert.JSONEq(t, fmt.Sprintf(`{"precondition":%d}`, violated), string(answer.Details))
	assert.Equal(t, throughV2, decisions, "through the v2 API")
}

// violatingDatabase answers every write as a database that offers the v2 API
// answers one whose precondition did not hold, with the given details, and
// every read as for a subject without events. It returns a store for it that
// decides again up to twice.
func violatingDatabase(t *testing.T, details string) *architecturekit.Store {
	t.Helper()

	item := `{"type":"error","data":{"code":"` + eventsourcingdb.CodePreconditionViolated +
		`","description":"a precondition does not hold","details":` + details + `}}`

	return architecturekit.NewStore(v2Database(t, "/api/v2/data/write-events", item),
		"https://thenativeweb.io", architecturekit.WithConflictRetries(2))
}

func retryingStore(t *testing.T, retries int) *architecturekit.Store {
	t.Helper()

	return architecturekit.NewStore(rawClient(t), "https://thenativeweb.io", architecturekit.WithConflictRetries(retries))
}

func TestWithConflictRetries(t *testing.T) {
	t.Run("decides again on a conflict and writes then", func(t *testing.T) {
		store := retryingStore(t, 2)
		subject := subjectFor(t)
		decider, counted := interferingDecider(t, 1)

		written, err := architecturekit.Execute(context.Background(), store, decider,
			increment{subject: subject, By: 1}.onStateRead())
		require.NoError(t, err)
		assert.Len(t, written, 1)
		assert.Equal(t, 2, counted.decisions)

		current, err := architecturekit.Load(context.Background(), store, counterState(), subject)
		require.NoError(t, err)
		assert.Equal(t, 101, current.Total, "the second decision builds on what was written in between")
	})

	t.Run("decides on the new state", func(t *testing.T) {
		decider, counted := interferingDecider(t, 1)

		_, err := architecturekit.Execute(context.Background(), retryingStore(t, 2), decider,
			increment{subject: subjectFor(t), By: 1, Limit: 50}.onStateRead())

		assert.ErrorIs(t, err, architecturekit.ErrDomain, "what was written in between exceeds the limit")
		assert.Equal(t, 2, counted.decisions)
	})

	t.Run("reports the conflict once the retries are used up", func(t *testing.T) {
		decider, counted := interferingDecider(t, 10)

		_, err := architecturekit.Execute(context.Background(), retryingStore(t, 2), decider,
			increment{subject: subjectFor(t), By: 1}.onStateRead())

		assert.ErrorIs(t, err, architecturekit.ErrConflict)
		assert.Equal(t, 3, counted.decisions, "one decision and two more")
	})

	t.Run("reports a conflict right away without the option", func(t *testing.T) {
		store := architecturekit.NewStore(rawClient(t), "https://thenativeweb.io")
		decider, counted := interferingDecider(t, 1)

		_, err := architecturekit.Execute(context.Background(), store, decider,
			increment{subject: subjectFor(t), By: 1}.onStateRead())

		assert.ErrorIs(t, err, architecturekit.ErrConflict)
		assert.Equal(t, 1, counted.decisions)
	})

	t.Run("never decides again on a revision the caller hands over", func(t *testing.T) {
		store := retryingStore(t, 2)
		subject := subjectFor(t)

		seeded, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1})
		require.NoError(t, err)

		decider, counted := interferingDecider(t, 1)
		_, err = architecturekit.Execute(context.Background(), store, decider,
			increment{subject: subject, By: 1}.onEventID(seeded[0].ID))

		assert.ErrorIs(t, err, architecturekit.ErrConflict, "the caller has to learn about the conflict")
		assert.Equal(t, 1, counted.decisions)
	})

	t.Run("reports an outdated revision of the caller next to OnStateRead at once through the v2 API", func(t *testing.T) {
		store := retryingStore(t, 2)
		subject := subjectFor(t)

		seeded, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1})
		require.NoError(t, err)
		_, err = architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1})
		require.NoError(t, err)

		// The decider never interferes, so OnStateRead holds every time, and
		// only the revision of the caller, which the second event outdated,
		// does not.
		decider, counted := interferingDecider(t, 0)
		_, err = architecturekit.Execute(context.Background(), store, decider,
			increment{subject: subject, By: 1}.declaring(
				architecturekit.OnStateRead(),
				architecturekit.OnEventID(subject, seeded[0].ID),
			))

		assertDecisionsByAPI(t, err, counted.decisions, 1, 3, 1)
		assert.Equal(t, 2, totalIn(t, store, subject), "nothing was written")
	})

	t.Run("reports a precondition made with Require next to OnStateRead at once through the v2 API", func(t *testing.T) {
		store := retryingStore(t, 2)
		subject := subjectFor(t)

		_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1})
		require.NoError(t, err)

		// The query requires the subject to have no events, as a query may
		// require an ISBN not to be taken, so deciding again can not make it
		// hold.
		query := fmt.Sprintf("FROM e IN events WHERE e.subject == '%s' PROJECT INTO COUNT() == 0", subject)
		decider, counted := interferingDecider(t, 0)
		_, err = architecturekit.Execute(context.Background(), store, decider,
			increment{subject: subject, By: 1}.declaring(
				architecturekit.OnStateRead(),
				architecturekit.Require(eventsourcingdb.NewIsEventQLQueryTruePrecondition(query)),
			))

		assertDecisionsByAPI(t, err, counted.decisions, 1, 3, 1)
		assert.Equal(t, 1, totalIn(t, store, subject), "nothing was written")
	})

	t.Run("decides again through either API if OnStateRead did not hold among preconditions on several subjects that hold", func(t *testing.T) {
		store := retryingStore(t, 2)
		subject := subjectFor(t)
		other := subject + "/other"

		_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1})
		require.NoError(t, err)
		onOther, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: other, By: 1})
		require.NoError(t, err)

		// OnStateRead is checked as the same kind as OnEventID, since the
		// subject has an event, and the two are told apart by their index.
		decider, counted := interferingDecider(t, 1)
		written, err := architecturekit.Execute(context.Background(), store, decider,
			increment{subject: subject, By: 1}.declaring(
				architecturekit.OnEventID(other, onOther[0].ID),
				architecturekit.OnStateRead(),
				architecturekit.OnPopulatedSubject(other),
			))
		require.NoError(t, err)
		assert.Len(t, written, 1)
		assert.Equal(t, 2, counted.decisions)
		assert.Equal(t, 102, totalIn(t, store, subject), "the second decision builds on what was written in between")
	})

	t.Run("uses up the retries through either API if OnStateRead does not hold among other preconditions", func(t *testing.T) {
		store := retryingStore(t, 2)
		subject := subjectFor(t)

		_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1})
		require.NoError(t, err)

		decider, counted := interferingDecider(t, 10)
		_, err = architecturekit.Execute(context.Background(), store, decider,
			increment{subject: subject, By: 1}.declaring(
				architecturekit.OnPopulatedSubject(subject),
				architecturekit.OnStateRead(),
			))

		assertDecisionsByAPI(t, err, counted.decisions, 3, 3, 1)
	})

	t.Run("decides again only if the database names OnStateRead or no precondition", func(t *testing.T) {
		tests := []struct {
			name      string
			details   string
			decisions int
		}{
			{"OnStateRead", `{"precondition":0}`, 3},
			{"the revision of the caller", `{"precondition":1}`, 1},
			{"a precondition made with Require", `{"precondition":2}`, 1},
			{"no precondition", `{}`, 3},
			{"one beyond the declared preconditions", `{"precondition":3}`, 3},
		}

		for _, test := range tests {
			t.Run(test.name, func(t *testing.T) {
				decider, counted := interferingDecider(t, 0)

				_, err := architecturekit.Execute(context.Background(), violatingDatabase(t, test.details), decider,
					increment{subject: "/test", By: 1}.declaring(
						architecturekit.OnStateRead(),
						architecturekit.OnEventID("/test", "0"),
						architecturekit.Require(eventsourcingdb.NewIsEventQLQueryTruePrecondition(
							"FROM e IN events PROJECT INTO COUNT() == 0")),
					))

				assert.ErrorIs(t, err, architecturekit.ErrConflict, "the category stays the same")
				assert.Equal(t, test.decisions, counted.decisions)

				answer, isAnswer := errors.AsType[*eventsourcingdb.DBAPIError](err)
				require.True(t, isAnswer, "errors.As has to reach the answer of the database, got: %v", err)
				assert.JSONEq(t, test.details, string(answer.Details), "so that the app can read the details itself")
			})
		}
	})

	t.Run("stops deciding again once the context has ended", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		decider, counted := interferingDecider(t, 10)
		decide := decider.Decide
		decider = architecturekit.NewDecider(decider.State(),
			func(ctx context.Context, cmd increment, current counter) ([]architecturekit.Event, error) {
				cancel()
				return decide(ctx, cmd, current)
			})

		_, err := architecturekit.Execute(ctx, retryingStore(t, 2), decider,
			increment{subject: subjectFor(t), By: 1}.onStateRead())

		// The context ended while deciding, so nothing is written at all, and
		// there is no conflict left to decide again on.
		assert.ErrorIs(t, err, context.Canceled)
		assert.NotErrorIs(t, err, architecturekit.ErrConflict)
		assert.Equal(t, 1, counted.decisions)
	})

	t.Run("panics on a negative number of retries", func(t *testing.T) {
		assert.PanicsWithValue(t,
			"architecturekit: WithConflictRetries needs a number of retries that is not negative, not -1",
			func() { architecturekit.WithConflictRetries(-1) })
	})
}
