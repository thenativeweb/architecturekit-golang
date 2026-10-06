package architecturekit_test

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// The ID that OnEventID checks usually comes from the caller, so one that is
// not a revision is the caller's mistake. It is refused before anything is
// read, as Read refuses a bound, rather than by the database, which takes it
// for a malformed request.

func TestOnEventID(t *testing.T) {
	t.Run("stands for the revision check of the client SDK, but is a kind of its own", func(t *testing.T) {
		made := architecturekit.OnEventID("/books/42", "7")

		database, ok := made.Database()
		assert.True(t, ok, "the database checks the revision")
		assert.Equal(t, eventsourcingdb.NewIsSubjectOnEventIDPrecondition("/books/42", "7"), database)
		assert.NotEqual(t, architecturekit.Require(database), made, "Require leaves the ID unchecked")
		assert.False(t, made.IsOnStateRead())
		assert.False(t, made.IsUnconditional())
	})

	notEventIDs := append([]struct {
		name     string
		revision string
	}{{"an empty one", ""}}, notRevisions...)

	for _, test := range notEventIDs {
		id, text := test.revision, `not a revision: "`+test.revision+`"`

		t.Run("makes Execute refuse "+test.name+" before reading", func(t *testing.T) {
			store, asked := unaskedDatabase(t)

			written, err := architecturekit.Execute(context.Background(), store, counterDecider(),
				increment{subject: "/books/42", By: 1}.declaring(architecturekit.OnEventID("/books/42", id)))

			assert.Nil(t, written)
			assert.ErrorIs(t, err, architecturekit.ErrNotARevision)
			assert.NotErrorIs(t, err, architecturekit.ErrPermanent, "the ID is the caller's mistake")
			assert.NotErrorIs(t, err, architecturekit.ErrTransient)
			assert.EqualError(t, err, text, "the text is for the caller, who knows neither the subject nor the command")
			assert.False(t, asked.Load(), "the database must not be asked")
		})

		t.Run("makes Write refuse "+test.name+" before writing", func(t *testing.T) {
			store, asked := unaskedDatabase(t)

			written, err := architecturekit.Write(context.Background(), store, []architecturekit.EventOn{
				{Subject: "/books/42", Event: incremented{By: 1}},
			}, architecturekit.OnEventID("/books/42", id))

			assert.Nil(t, written)
			assert.ErrorIs(t, err, architecturekit.ErrNotARevision)
			assert.NotErrorIs(t, err, architecturekit.ErrPermanent, "the ID is the caller's mistake")
			assert.EqualError(t, err, text)
			assert.False(t, asked.Load(), "the database must not be asked")
		})
	}

	t.Run("makes Execute refuse it with the same error as CheckPreconditions, Write, and the test fixture", func(t *testing.T) {
		store, _ := unaskedDatabase(t)
		cmd := increment{subject: "/books/42", By: 1}.declaring(architecturekit.OnEventID("/books/42", "abc"))

		_, err := architecturekit.Execute(context.Background(), store, counterDecider(), cmd)
		require.ErrorIs(t, err, architecturekit.ErrNotARevision, "Execute has to refuse the command")

		_, refusedByWrite := architecturekit.Write(context.Background(), store, nil, cmd.Preconditions()...)

		assert.Equal(t, err, architecturekit.CheckPreconditions(cmd))
		assert.Equal(t, err, refusedByWrite, "Write checks the preconditions also for no events")
		assert.Equal(t, err.Error(), fixtureRefusal(t, counterDecider(), cmd))
	})

	t.Run("refuses the first ID that is not a revision", func(t *testing.T) {
		store, asked := unaskedDatabase(t)
		preconditions := []architecturekit.Precondition{
			architecturekit.OnEventID("/books/42", "7"),
			architecturekit.OnPopulatedSubject("/books/42"),
			architecturekit.OnEventID("/books/23", "abc"),
			architecturekit.OnEventID("/books/42", "xyz"),
		}

		_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: "/books/42", By: 1}.declaring(preconditions...))
		assert.EqualError(t, err, `not a revision: "abc"`)

		_, err = architecturekit.Write(context.Background(), store, []architecturekit.EventOn{
			{Subject: "/books/42", Event: incremented{By: 1}},
		}, preconditions...)
		assert.EqualError(t, err, `not a revision: "abc"`)

		assert.False(t, asked.Load(), "the database must not be asked")
	})

	t.Run("reports a mistake in the code before an ID that is not a revision", func(t *testing.T) {
		store, asked := unaskedDatabase(t)

		for name, test := range map[string]struct {
			preconditions []architecturekit.Precondition
			says          string
		}{
			"Unconditionally combined with it": {
				[]architecturekit.Precondition{architecturekit.OnEventID("/books/42", "abc"), architecturekit.Unconditionally()},
				"combines Unconditionally with other preconditions",
			},
			"a zero value after it": {
				[]architecturekit.Precondition{architecturekit.OnEventID("/books/42", "abc"), {}},
				"declares a zero Precondition",
			},
			"a requirement of nothing after it": {
				[]architecturekit.Precondition{architecturekit.OnEventID("/books/42", "abc"), architecturekit.Require(nil)},
				"requires a precondition that is nil",
			},
		} {
			t.Run("for Execute, with "+name, func(t *testing.T) {
				_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
					increment{subject: "/books/42", By: 1}.declaring(test.preconditions...))

				require.ErrorIs(t, err, architecturekit.ErrPermanent)
				assert.NotErrorIs(t, err, architecturekit.ErrNotARevision)
				assert.ErrorContains(t, err, test.says)
			})

			t.Run("for Write, with "+name, func(t *testing.T) {
				_, err := architecturekit.Write(context.Background(), store, []architecturekit.EventOn{
					{Subject: "/books/42", Event: incremented{By: 1}},
				}, test.preconditions...)

				require.ErrorIs(t, err, architecturekit.ErrPermanent)
				assert.NotErrorIs(t, err, architecturekit.ErrNotARevision)
				assert.ErrorContains(t, err, test.says)
			})
		}

		t.Run("for Write, with OnStateRead after it", func(t *testing.T) {
			_, err := architecturekit.Write(context.Background(), store, []architecturekit.EventOn{
				{Subject: "/books/42", Event: incremented{By: 1}},
			}, architecturekit.OnEventID("/books/42", "abc"), architecturekit.OnStateRead())

			require.ErrorIs(t, err, architecturekit.ErrPermanent)
			assert.ErrorContains(t, err, "OnStateRead has nothing to guard")
		})

		assert.False(t, asked.Load(), "the database must not be asked")
	})

	t.Run("lets the database check an ID that is a revision", func(t *testing.T) {
		store := requireStore(t)
		ctx := context.Background()
		subject := subjectFor(t)
		seeded := writeIDs(t, subject, incremented{By: 1}, incremented{By: 2})

		_, err := architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: 1}.declaring(architecturekit.OnEventID(subject, seeded[0])))
		assert.ErrorIs(t, err, architecturekit.ErrConflict, "a revision that is not the last event conflicts")

		_, err = architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: 1}.declaring(architecturekit.OnEventID(subject, "9223372036854775807")))
		assert.ErrorIs(t, err, architecturekit.ErrConflict, "the largest revision is one, too")

		_, err = architecturekit.Write(ctx, store, []architecturekit.EventOn{
			{Subject: subject, Event: incremented{By: 1}},
		}, architecturekit.OnEventID(subject, seeded[0]))
		assert.ErrorIs(t, err, architecturekit.ErrConflict, "a revision that is not the last event conflicts")

		written, err := architecturekit.Execute(ctx, store, counterDecider(),
			increment{subject: subject, By: 4}.declaring(architecturekit.OnEventID(subject, seeded[1])))
		require.NoError(t, err, "the last event is the one to be on")

		_, err = architecturekit.Write(ctx, store, []architecturekit.EventOn{
			{Subject: subject, Event: incremented{By: 8}},
		}, architecturekit.OnEventID(subject, written[0].ID))
		require.NoError(t, err, "the last event is the one to be on")

		assert.Equal(t, 15, totalIn(t, store, subject))
	})

	t.Run("leaves Require with the revision check of the client SDK unchecked", func(t *testing.T) {
		store := requireStore(t)
		subject := subjectFor(t)

		_, err := architecturekit.Execute(context.Background(), store, counterDecider(),
			increment{subject: subject, By: 1}.onEventID("abc"))

		require.ErrorIs(t, err, architecturekit.ErrPermanent, "the database refuses the ID as a malformed request")
		assert.NotErrorIs(t, err, architecturekit.ErrNotARevision, "the kit has not looked at the ID")
		assert.NoError(t, architecturekit.CheckPreconditions(increment{subject: subject, By: 1}.onEventID("abc")))
	})
}
