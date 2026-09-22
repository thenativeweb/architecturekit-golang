package architecturekit_test

import (
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

func TestSubjectSchemeBuildsAndMatches(t *testing.T) {
	scheme := architecturekit.NewSubjectScheme("/tenant/{tenant}/workshop/{workshop}")

	subject := scheme.Build("acme", "go-cqrs")
	if want := "/tenant/acme/workshop/go-cqrs"; subject != want {
		t.Fatalf("got %q, want %q", subject, want)
	}

	values, ok := scheme.Match(subject)
	if !ok {
		t.Fatalf("%q should match %q", subject, scheme.Pattern())
	}
	if values["tenant"] != "acme" || values["workshop"] != "go-cqrs" {
		t.Fatalf("got %v", values)
	}
}

func TestSubjectSchemeWithoutPlaceholders(t *testing.T) {
	scheme := architecturekit.NewSubjectScheme("/system/health")

	if subject := scheme.Build(); subject != "/system/health" {
		t.Fatalf("got %q", subject)
	}
	if _, ok := scheme.Match("/system/health"); !ok {
		t.Fatal("should match itself")
	}
}

func TestSubjectSchemeReportsItsPlaceholders(t *testing.T) {
	scheme := architecturekit.NewSubjectScheme("/tenant/{tenant}/workshop/{workshop}")

	names := scheme.Placeholders()
	if len(names) != 2 || names[0] != "tenant" || names[1] != "workshop" {
		t.Fatalf("got %v", names)
	}

	// The returned slice is a copy, so a caller cannot reach into the scheme.
	names[0] = "tampered"
	if again := scheme.Placeholders(); again[0] != "tenant" {
		t.Fatalf("scheme was modified from outside: %v", again)
	}
}

func TestSubjectSchemeRejectsSubjectsThatDoNotFit(t *testing.T) {
	scheme := architecturekit.NewSubjectScheme("/tenant/{tenant}/workshop/{workshop}")

	for _, subject := range []string{
		"tenant/acme/workshop/go-cqrs",       // no leading slash
		"/tenant/acme/workshop",              // too few segments
		"/tenant/acme/workshop/go-cqrs/more", // too many segments
		"/client/acme/workshop/go-cqrs",      // literal does not match
		"/tenant/acme/workshop/",             // empty placeholder value
	} {
		if _, ok := scheme.Match(subject); ok {
			t.Fatalf("%q should not match %q", subject, scheme.Pattern())
		}
	}
}

func TestSubjectSchemePanicsOnMalformedPatterns(t *testing.T) {
	for _, pattern := range []string{
		"workshop/{id}",     // no leading slash
		"/workshop//{id}",   // empty segment
		"/workshop/{id",     // unclosed placeholder
		"/workshop/{}",      // unnamed placeholder
		"/a/{id}/b/{id}",    // duplicate placeholder
		"/workshop/pre{id}", // malformed segment
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("pattern %q should panic", pattern)
				}
			}()
			architecturekit.NewSubjectScheme(pattern)
		}()
	}
}

func TestSubjectSchemePanicsOnBadValues(t *testing.T) {
	scheme := architecturekit.NewSubjectScheme("/workshop/{workshop}")

	for _, values := range [][]string{
		{},             // too few
		{"a", "b"},     // too many
		{""},           // empty
		{"with/slash"}, // contains a slash
	} {
		func() {
			defer func() {
				if recover() == nil {
					t.Fatalf("values %v should panic", values)
				}
			}()
			scheme.Build(values...)
		}()
	}
}

func TestSubjectSchemeReportsItsPattern(t *testing.T) {
	pattern := "/tenant/{tenant}/workshop/{workshop}"

	if got := architecturekit.NewSubjectScheme(pattern).Pattern(); got != pattern {
		t.Fatalf("got %q, want %q", got, pattern)
	}
}
