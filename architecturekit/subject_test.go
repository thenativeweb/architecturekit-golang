package architecturekit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

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
		} {
			t.Run(test.name, func(t *testing.T) {
				assert.Panics(t, func() {
					architecturekit.NewSubjectScheme(test.pattern)
				})
			})
		}
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

	t.Run("reports its pattern", func(t *testing.T) {
		pattern := "/tenant/{tenant}/workshop/{workshop}"

		assert.Equal(t, pattern, architecturekit.NewSubjectScheme(pattern).Pattern())
	})
}
