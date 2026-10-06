package architecturekit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// CheckReadOptions makes the checks Read makes before it asks the database, so
// that an application can refuse a read before a side effect. Every case is
// held against what Read does with the same options.

func TestCheckReadOptions(t *testing.T) {
	fromLatest := architecturekit.FromLatestEvent("/books/42", reset{}.EventType(), architecturekit.ReadEverything)

	t.Run("returns nil for the options Read asks the database with", func(t *testing.T) {
		accepted := [][]architecturekit.ReadOption{
			nil,
			{architecturekit.NewestFirst()},
			{architecturekit.FromEvent("0"), architecturekit.UpToEvent(largestRevision)},
			{fromLatest},
			{fromLatest, architecturekit.UpToEvent("0")},
			{fromLatest, architecturekit.BeforeEvent("1")},
		}
		for _, read := range boundedReads {
			if read.refusal == "" {
				accepted = append(accepted,
					read.options, append([]architecturekit.ReadOption{architecturekit.NewestFirst()}, read.options...))
			}
		}

		for _, options := range accepted {
			store, asked := unaskedDatabase(t)

			_, errs := readAll(t, store, architecturekit.ExactSubject("/books/42"), options...)
			require.Empty(t, errs, "Read has to accept the options")
			require.True(t, asked.Load())

			assert.NoError(t, architecturekit.CheckReadOptions(options...))
		}
	})

	t.Run("refuses an ID that is not a revision with the error of Read", func(t *testing.T) {
		for name, option := range map[string]func(string) architecturekit.ReadOption{
			"FromEvent":   architecturekit.FromEvent,
			"AfterEvent":  architecturekit.AfterEvent,
			"UpToEvent":   architecturekit.UpToEvent,
			"BeforeEvent": architecturekit.BeforeEvent,
		} {
			for _, id := range []string{"", "abc", "-1", "1.5", " 1", "9223372036854775808"} {
				store, _ := unaskedDatabase(t)

				err := architecturekit.CheckReadOptions(option(id))

				assert.ErrorIs(t, err, architecturekit.ErrNotARevision, name+" "+id)
				assert.NotErrorIs(t, err, architecturekit.ErrPermanent, name+" "+id)
				assert.EqualError(t, err, `not a revision: "`+id+`"`, name+" "+id)
				assert.Equal(t, refusalOf(t, store, option(id)), err, name+" "+id)
			}
		}
	})

	t.Run("refuses bounds that leave no room for an event with the error of Read", func(t *testing.T) {
		refused := []boundedRead{
			{"FromLatestEvent and BeforeEvent of the first event",
				[]architecturekit.ReadOption{fromLatest, architecturekit.BeforeEvent("0")},
				`empty range: no event can lie before "0"`},
		}
		for _, read := range boundedReads {
			if read.refusal != "" {
				refused = append(refused, read, boundedRead{
					read.name + ", newest first",
					append([]architecturekit.ReadOption{architecturekit.NewestFirst()}, read.options...),
					read.refusal,
				})
			}
		}

		for _, read := range refused {
			store, _ := unaskedDatabase(t)

			err := architecturekit.CheckReadOptions(read.options...)

			assert.ErrorIs(t, err, architecturekit.ErrEmptyRange, read.name)
			assert.NotErrorIs(t, err, architecturekit.ErrNotARevision, read.name)
			assert.EqualError(t, err, read.refusal, read.name)
			assert.Equal(t, refusalOf(t, store, read.options...), err, read.name)
		}
	})

	t.Run("refuses the lower bound first, and an ID that is not a revision before the range, as Read does", func(t *testing.T) {
		for _, options := range [][]architecturekit.ReadOption{
			{architecturekit.AfterEvent("abc"), architecturekit.UpToEvent("xyz")},
			{architecturekit.UpToEvent("xyz"), architecturekit.AfterEvent("abc")},
			{architecturekit.FromEvent("5"), architecturekit.BeforeEvent("abc")},
			{architecturekit.AfterEvent("abc"), architecturekit.BeforeEvent("0")},
		} {
			store, _ := unaskedDatabase(t)

			err := architecturekit.CheckReadOptions(options...)

			assert.EqualError(t, err, `not a revision: "abc"`)
			assert.Equal(t, refusalOf(t, store, options...), err)
		}
	})

	for name, test := range contradictions() {
		t.Run(name+" panic as with Read", func(t *testing.T) {
			assert.PanicsWithValue(t, test.message, func() {
				_ = architecturekit.CheckReadOptions(test.options...)
			})
		})
	}

	t.Run("a nil option panics, naming CheckReadOptions, also one that was declared but never set", func(t *testing.T) {
		var declared architecturekit.ReadOption

		for _, option := range []architecturekit.ReadOption{nil, declared} {
			assert.PanicsWithValue(t, "architecturekit: CheckReadOptions got a nil option", func() {
				_ = architecturekit.CheckReadOptions(architecturekit.FromEvent("1"), option)
			})
		}
	})

	t.Run("panics before it checks the IDs, as Read does", func(t *testing.T) {
		// Read panics when it is called, and checks the IDs only once the events
		// are iterated, so a mistake in the code is never hidden behind one of
		// the caller.
		for _, test := range []struct {
			options []architecturekit.ReadOption
			message string
		}{
			{
				[]architecturekit.ReadOption{architecturekit.FromEvent("abc"), architecturekit.AfterEvent("xyz")},
				`architecturekit: a read has one lower bound, but got FromEvent("abc") and AfterEvent("xyz")`,
			},
			{
				[]architecturekit.ReadOption{fromLatest, architecturekit.BeforeEvent("0"), architecturekit.NewestFirst()},
				`architecturekit: the database reads from the latest event of a type only oldest first, but got ` +
					`FromLatestEvent("/books/42", "` + reset{}.EventType() + `") and NewestFirst()`,
			},
		} {
			store, _ := unaskedDatabase(t)

			assert.PanicsWithValue(t, test.message, func() {
				readAll(t, store, architecturekit.ExactSubject("/books/42"), test.options...)
			})
			assert.PanicsWithValue(t, test.message, func() {
				_ = architecturekit.CheckReadOptions(test.options...)
			})
		}

		assert.PanicsWithValue(t, "architecturekit: CheckReadOptions got a nil option", func() {
			_ = architecturekit.CheckReadOptions(architecturekit.AfterEvent("abc"), nil)
		})
	})
}
