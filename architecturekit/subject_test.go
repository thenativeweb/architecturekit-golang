package architecturekit_test

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// databaseCharacters are the characters that EventSourcingDB allows in a
// segment of a subject, written out rather than taken from the code under
// test.
const databaseCharacters = "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-"

// someCharacters returns every ASCII character, and some beyond: letters
// outside ASCII, characters that look like the ones the database allows, and
// the one that invalid UTF-8 decodes to.
func someCharacters() []rune {
	var characters []rune
	for character := range rune(utf8.RuneSelf) {
		characters = append(characters, character)
	}

	return append(characters,
		'\u0080', '\u00fc', '\u00df', '\u00e9', '\u212a', '\u2010',
		'\uff11', '\uff21', '\uff3f', '\U0001f600', utf8.RuneError,
	)
}

func TestSubjectScheme(t *testing.T) {
	t.Run("builds and matches", func(t *testing.T) {
		scheme := architecturekit.NewSubjectScheme("/tenant/{tenant}/workshop/{workshop}")

		subject := scheme.Build("acme", "go-cqrs")
		assert.Equal(t, "/tenant/acme/workshop/go-cqrs", subject)

		values, ok := scheme.Match(subject)
		require.True(t, ok, "%q should match %q", subject, scheme.Pattern())
		assert.Equal(t, "acme", values["tenant"])
		assert.Equal(t, "go-cqrs", values["workshop"])
	})

	t.Run("without placeholders", func(t *testing.T) {
		scheme := architecturekit.NewSubjectScheme("/system/health")

		assert.Equal(t, "/system/health", scheme.Build())
		_, ok := scheme.Match("/system/health")
		assert.True(t, ok, "should match itself")
	})

	t.Run("reports its placeholders", func(t *testing.T) {
		scheme := architecturekit.NewSubjectScheme("/tenant/{tenant}/workshop/{workshop}")

		names := scheme.Placeholders()
		require.Len(t, names, 2)
		assert.Equal(t, []string{"tenant", "workshop"}, names)

		// The returned slice is a copy, so a caller cannot reach into the scheme.
		names[0] = "tampered"
		again := scheme.Placeholders()
		assert.Equal(t, "tenant", again[0], "scheme was modified from outside")
	})

	t.Run("rejects subjects that do not fit", func(t *testing.T) {
		scheme := architecturekit.NewSubjectScheme("/tenant/{tenant}/workshop/{workshop}")

		for _, test := range []struct {
			name    string
			subject string
		}{
			{name: "no leading slash", subject: "tenant/acme/workshop/go-cqrs"},
			{name: "too few segments", subject: "/tenant/acme/workshop"},
			{name: "too many segments", subject: "/tenant/acme/workshop/go-cqrs/more"},
			{name: "literal does not match", subject: "/client/acme/workshop/go-cqrs"},
			{name: "empty placeholder value", subject: "/tenant/acme/workshop/"},
			{name: "value with a character the database refuses", subject: "/tenant/acme/workshop/go.cqrs"},
		} {
			t.Run(test.name, func(t *testing.T) {
				_, ok := scheme.Match(test.subject)
				assert.False(t, ok, "%q should not match %q", test.subject, scheme.Pattern())
			})
		}
	})

	t.Run("panics on malformed patterns", func(t *testing.T) {
		for _, test := range []struct {
			name    string
			pattern string
		}{
			{name: "no leading slash", pattern: "workshop/{id}"},
			{name: "empty segment", pattern: "/workshop//{id}"},
			{name: "unclosed placeholder", pattern: "/workshop/{id"},
			{name: "unnamed placeholder", pattern: "/workshop/{}"},
			{name: "duplicate placeholder", pattern: "/a/{id}/b/{id}"},
			{name: "malformed segment", pattern: "/workshop/pre{id}"},
			{name: "dot in a literal", pattern: "/books.v2/{book}"},
			{name: "space in a literal", pattern: "/my books/{book}"},
			{name: "umlaut in a literal", pattern: "/b\u00fccher/{book}"},
			{name: "dots as a literal", pattern: "/../{book}"},
		} {
			t.Run(test.name, func(t *testing.T) {
				assert.Panics(t, func() {
					architecturekit.NewSubjectScheme(test.pattern)
				})
			})
		}
	})

	t.Run("names what is wrong with a literal", func(t *testing.T) {
		assert.PanicsWithValue(t,
			`architecturekit: segment "books.v2" in "/books.v2/{book}" may only contain A-Z, a-z, 0-9, underscores, and hyphens`,
			func() { architecturekit.NewSubjectScheme("/books.v2/{book}") })

		// A brace means a placeholder that is not the whole segment, which says
		// more than the characters.
		assert.PanicsWithValue(t,
			`architecturekit: segment "pre{id}" in "/workshop/pre{id}" is malformed`,
			func() { architecturekit.NewSubjectScheme("/workshop/pre{id}") })
	})

	t.Run("accepts literals with every character the database allows", func(t *testing.T) {
		scheme := architecturekit.NewSubjectScheme("/Tenant_2/{tenant}/work-shop/{workshop}")

		subject := scheme.Build("acme", "42")
		assert.Equal(t, "/Tenant_2/acme/work-shop/42", subject)

		values, ok := scheme.Match(subject)
		require.True(t, ok, "%q should match %q", subject, scheme.Pattern())
		assert.Equal(t, map[string]string{"tenant": "acme", "workshop": "42"}, values)
		assert.Equal(t, "/Tenant_2", scheme.Root())
	})

	t.Run("panics on bad values", func(t *testing.T) {
		scheme := architecturekit.NewSubjectScheme("/workshop/{workshop}")

		for _, test := range []struct {
			name   string
			values []string
		}{
			{name: "too few", values: []string{}},
			{name: "too many", values: []string{"a", "b"}},
			{name: "empty", values: []string{""}},
			{name: "contains a slash", values: []string{"with/slash"}},
		} {
			t.Run(test.name, func(t *testing.T) {
				assert.Panics(t, func() {
					scheme.Build(test.values...)
				})
			})
		}
	})

	t.Run("checks values", func(t *testing.T) {
		scheme := architecturekit.NewSubjectScheme("/workshop/{workshop}")

		assert.NoError(t, scheme.Check("42"), "values that fit should pass")

		for _, test := range []struct {
			name   string
			values []string
		}{
			{name: "too few", values: []string{}},
			{name: "too many", values: []string{"a", "b"}},
			{name: "empty", values: []string{""}},
			{name: "contains a slash", values: []string{"with/slash"}},
		} {
			t.Run(test.name, func(t *testing.T) {
				assert.Error(t, scheme.Check(test.values...))
			})
		}
	})

	t.Run("names the placeholder of a bad value", func(t *testing.T) {
		scheme := architecturekit.NewSubjectScheme("/tenant/{tenant}/workshop/{workshop}")

		err := scheme.Check("acme", "")
		assert.ErrorContains(t, err, `"workshop"`)
	})

	t.Run("says which characters a value may contain", func(t *testing.T) {
		scheme := architecturekit.NewSubjectScheme("/tenant/{tenant}/workshop/{workshop}")
		message := `architecturekit: value "go.cqrs" for "workshop" in "/tenant/{tenant}/workshop/{workshop}" ` +
			`may only contain A-Z, a-z, 0-9, underscores, and hyphens`

		assert.EqualError(t, scheme.Check("acme", "go.cqrs"), message)
		assert.PanicsWithValue(t, message, func() { scheme.Build("acme", "go.cqrs") })
	})

	t.Run("refuses values with characters the database does not allow", func(t *testing.T) {
		scheme := architecturekit.NewSubjectScheme("/workshop/{workshop}")

		for _, test := range []struct {
			name  string
			value string
		}{
			{name: "dot", value: "a.b"},
			{name: "only a dot", value: "."},
			{name: "only dots", value: ".."},
			{name: "space", value: "with space"},
			{name: "umlaut", value: "\u00fcmlaut"},
			{name: "colon", value: "a:b"},
			{name: "at sign", value: "a@b"},
			{name: "tilde", value: "a~b"},
			{name: "asterisk", value: "*"},
			{name: "question mark", value: "a?b"},
			{name: "hash", value: "a#b"},
			{name: "encoded slash", value: "a%2Fb"},
			{name: "comma", value: "a,b"},
			{name: "line feed", value: "a\nb"},
			{name: "null character", value: "a\x00b"},
			{name: "invalid UTF-8", value: "a\xffb"},
		} {
			t.Run(test.name, func(t *testing.T) {
				err := scheme.Check(test.value)
				require.Error(t, err, "%q should be refused", test.value)
				assert.ErrorContains(t, err, `"workshop"`)
				assert.ErrorContains(t, err, "may only contain A-Z, a-z, 0-9, underscores, and hyphens")

				assert.PanicsWithValue(t, err.Error(), func() { scheme.Build(test.value) })
			})
		}
	})

	t.Run("accepts values with characters the database allows", func(t *testing.T) {
		scheme := architecturekit.NewSubjectScheme("/workshop/{workshop}")

		for _, value := range []string{
			"UPPER", "lower", "MiXeD", "0123456789", "a_b", "a-b", "_", "-", "-_-", databaseCharacters,
		} {
			t.Run(value, func(t *testing.T) {
				require.NoError(t, scheme.Check(value))

				subject := scheme.Build(value)
				assert.Equal(t, "/workshop/"+value, subject)

				values, ok := scheme.Match(subject)
				require.True(t, ok, "%q should match %q", subject, scheme.Pattern())
				assert.Equal(t, value, values["workshop"])
			})
		}
	})

	t.Run("allows exactly the characters the database allows", func(t *testing.T) {
		scheme := architecturekit.NewSubjectScheme("/workshop/{workshop}")

		for _, character := range someCharacters() {
			// In the middle, so that neither the first nor the last character
			// alone decides.
			value := "a" + string(character) + "b"
			allowed := strings.ContainsRune(databaseCharacters, character)

			if allowed {
				assert.NoError(t, scheme.Check(value), "%q should pass", value)
				assert.NotPanics(t, func() { scheme.Build(value) }, "%q should build", value)
			} else {
				assert.Error(t, scheme.Check(value), "%q should be refused", value)
				assert.Panics(t, func() { scheme.Build(value) }, "%q should not build", value)
			}

			_, ok := scheme.Match("/workshop/" + value)
			assert.Equal(t, allowed, ok, "match of %q", value)

			// A slash in a literal separates two segments rather than being
			// part of one.
			if character == '/' {
				continue
			}

			build := func() { architecturekit.NewSubjectScheme("/" + value + "/{workshop}") }
			if allowed {
				assert.NotPanics(t, build, "literal %q should pass", value)
			} else {
				assert.Panics(t, build, "literal %q should be refused", value)
			}
		}
	})

	t.Run("agrees with the database on every character", func(t *testing.T) {
		store := requireStore(t)
		ctx := context.Background()
		scheme := architecturekit.NewSubjectScheme(subjectFor(t) + "/{value}")

		// The characters that Check accepts go into a single value, since the
		// database takes a subject only if it takes every character in it, and
		// writing an event takes far longer than having one refused.
		var accepted strings.Builder

		for _, character := range someCharacters() {
			// A slash separates segments, so the database takes it, but the
			// subject would no longer follow the pattern. Check refuses it for
			// that reason, as tested above.
			if character == '/' {
				continue
			}

			value := "a" + string(character) + "b"
			if scheme.Check(value) == nil {
				accepted.WriteRune(character)
				continue
			}

			// One write each, since the database refuses a write as a whole.
			_, err := architecturekit.Write(ctx, store,
				[]architecturekit.EventOn{{Subject: subjectFor(t) + "/" + value, Event: incremented{By: 1}}},
				architecturekit.Unconditionally())
			assert.ErrorContains(t, err, "malformed subject", "the database should refuse %q, as Check does", value)
		}

		var events []architecturekit.EventOn
		for _, value := range []string{accepted.String(), "_", "-"} {
			require.NoError(t, scheme.Check(value))
			events = append(events, architecturekit.EventOn{Subject: scheme.Build(value), Event: incremented{By: 1}})
		}

		_, err := architecturekit.Write(ctx, store, events, architecturekit.Unconditionally())
		require.NoError(t, err, "the database should take every value that Check accepts")

		for _, event := range events {
			current, err := architecturekit.Load(ctx, store, counterState(), event.Subject)
			require.NoError(t, err, "the database should read %q", event.Subject)
			assert.Equal(t, 1, current.Total, "%q should hold what was written", event.Subject)
		}
	})

	t.Run("reports its root", func(t *testing.T) {
		for _, test := range []struct {
			pattern string
			root    string
		}{
			{pattern: "/instances/{instance}", root: "/instances"},
			{pattern: "/tenant/{tenant}/workshop/{workshop}", root: "/tenant"},
			{pattern: "/system/tenants/{tenant}", root: "/system/tenants"},
			{pattern: "/{tenant}/orders/{order}", root: "/"},
			{pattern: "/system/health", root: "/system/health"},
		} {
			t.Run(test.pattern, func(t *testing.T) {
				assert.Equal(t, test.root, architecturekit.NewSubjectScheme(test.pattern).Root())
			})
		}
	})

	t.Run("builds every subject under its root", func(t *testing.T) {
		// A recursive read from a subject covers that subject and every one below
		// it, so this is what makes reading from the root complete.
		for _, scheme := range []*architecturekit.SubjectScheme{
			architecturekit.NewSubjectScheme("/tenant/{tenant}/workshop/{workshop}"),
			architecturekit.NewSubjectScheme("/{tenant}/orders/{order}"),
			architecturekit.NewSubjectScheme("/system/health"),
		} {
			values := make([]string, len(scheme.Placeholders()))
			for i := range values {
				values[i] = "value"
			}

			subject := scheme.Build(values...)
			root := scheme.Root()

			isUnder := subject == root || root == "/" || strings.HasPrefix(subject, root+"/")
			assert.True(t, isUnder, "%q is not under %q", subject, root)
		}
	})

	t.Run("reports its pattern", func(t *testing.T) {
		pattern := "/tenant/{tenant}/workshop/{workshop}"

		assert.Equal(t, pattern, architecturekit.NewSubjectScheme(pattern).Pattern())
	})
}

func TestSubjectSchemeValue(t *testing.T) {
	scheme := architecturekit.NewSubjectScheme("/tenant/{tenant}/workshop/{workshop}")

	t.Run("takes the value of a placeholder out of a subject", func(t *testing.T) {
		tenant, ok := scheme.Value("/tenant/acme/workshop/go-cqrs", "tenant")
		require.True(t, ok)
		assert.Equal(t, "acme", tenant)

		workshop, ok := scheme.Value("/tenant/acme/workshop/go-cqrs", "workshop")
		require.True(t, ok)
		assert.Equal(t, "go-cqrs", workshop)
	})

	t.Run("reports false for a subject that does not follow the pattern, as Match does", func(t *testing.T) {
		for _, subject := range []string{
			"tenant/acme/workshop/go-cqrs",
			"/tenant/acme/workshop",
			"/client/acme/workshop/go-cqrs",
			"/tenant/acme/workshop/go.cqrs",
		} {
			t.Run(subject, func(t *testing.T) {
				_, matches := scheme.Match(subject)
				require.False(t, matches)

				value, ok := scheme.Value(subject, "tenant")
				assert.False(t, ok, "%q should not match %q", subject, scheme.Pattern())
				assert.Empty(t, value)
			})
		}
	})

	t.Run("panics for a placeholder the pattern does not have", func(t *testing.T) {
		message := `architecturekit: pattern "/tenant/{tenant}/workshop/{workshop}" has no placeholder "tennant"`

		assert.PanicsWithValue(t, message, func() {
			scheme.Value("/tenant/acme/workshop/go-cqrs", "tennant")
		})

		// The mistake is in the code, so it is found whatever the subject is.
		assert.PanicsWithValue(t, message, func() {
			scheme.Value("/client/acme", "tennant")
		})
	})

	t.Run("panics for a literal segment or a placeholder in braces, neither of which is the name of one", func(t *testing.T) {
		books := architecturekit.NewSubjectScheme("/books/{book}")

		for _, name := range []string{"books", "{book}", ""} {
			assert.PanicsWithValue(t,
				fmt.Sprintf("architecturekit: pattern %q has no placeholder %q", "/books/{book}", name),
				func() { books.Value("/books/42", name) })
		}
	})
}
