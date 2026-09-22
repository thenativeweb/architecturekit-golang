package architecturekit

import (
	"fmt"
	"strings"
)

// SubjectScheme describes how a subject is composed, in the style of the
// patterns that http.ServeMux understands: literal segments and placeholders
// in braces, as in "/tenant/{tenant}/workshop/{workshop}".
//
// A scheme works in both directions. Build composes a subject from values, and
// Match takes one apart again, which a projection needs to recover the
// aggregate ID that the events themselves do not carry.
type SubjectScheme struct {
	pattern      string
	segments     []string
	placeholders []string
}

// NewSubjectScheme parses a pattern. A malformed pattern is a programming
// error and panics while the scheme is being built.
func NewSubjectScheme(pattern string) *SubjectScheme {
	if !strings.HasPrefix(pattern, "/") {
		panic(fmt.Sprintf("architecturekit: subject pattern %q must start with a slash", pattern))
	}

	scheme := &SubjectScheme{pattern: pattern}
	seen := map[string]bool{}

	for _, segment := range strings.Split(strings.TrimPrefix(pattern, "/"), "/") {
		if segment == "" {
			panic(fmt.Sprintf("architecturekit: subject pattern %q has an empty segment", pattern))
		}

		scheme.segments = append(scheme.segments, segment)

		if !strings.HasPrefix(segment, "{") {
			if strings.ContainsAny(segment, "{}") {
				panic(fmt.Sprintf("architecturekit: segment %q in %q is malformed", segment, pattern))
			}
			continue
		}

		if !strings.HasSuffix(segment, "}") {
			panic(fmt.Sprintf("architecturekit: segment %q in %q is malformed", segment, pattern))
		}

		name := segment[1 : len(segment)-1]
		if name == "" {
			panic(fmt.Sprintf("architecturekit: pattern %q has an unnamed placeholder", pattern))
		}
		if seen[name] {
			panic(fmt.Sprintf("architecturekit: pattern %q uses the placeholder %q twice", pattern, name))
		}

		seen[name] = true
		scheme.placeholders = append(scheme.placeholders, name)
	}

	return scheme
}

// Pattern returns the pattern the scheme was built from.
func (s *SubjectScheme) Pattern() string { return s.pattern }

// Placeholders returns the placeholder names, in the order Build expects them.
func (s *SubjectScheme) Placeholders() []string {
	names := make([]string, len(s.placeholders))
	copy(names, s.placeholders)
	return names
}

// Build composes a subject. The values fill the placeholders in the order they
// appear in the pattern. A wrong number of values, or one that is empty or
// contains a slash, is a programming error and panics.
func (s *SubjectScheme) Build(values ...string) string {
	if len(values) != len(s.placeholders) {
		panic(fmt.Sprintf("architecturekit: pattern %q needs %d value(s), got %d",
			s.pattern, len(s.placeholders), len(values)))
	}

	var builder strings.Builder
	next := 0

	for _, segment := range s.segments {
		builder.WriteString("/")

		if !strings.HasPrefix(segment, "{") {
			builder.WriteString(segment)
			continue
		}

		value := values[next]
		if value == "" {
			panic(fmt.Sprintf("architecturekit: value for %q in %q must not be empty",
				s.placeholders[next], s.pattern))
		}
		if strings.Contains(value, "/") {
			panic(fmt.Sprintf("architecturekit: value %q for %q in %q must not contain a slash",
				value, s.placeholders[next], s.pattern))
		}

		builder.WriteString(value)
		next++
	}

	return builder.String()
}

// Match takes a subject apart. It reports false if the subject does not follow
// the pattern, which is an ordinary case for a projection reading recursively
// across several schemes.
func (s *SubjectScheme) Match(subject string) (map[string]string, bool) {
	if !strings.HasPrefix(subject, "/") {
		return nil, false
	}

	parts := strings.Split(strings.TrimPrefix(subject, "/"), "/")
	if len(parts) != len(s.segments) {
		return nil, false
	}

	values := map[string]string{}
	next := 0

	for i, segment := range s.segments {
		if !strings.HasPrefix(segment, "{") {
			if parts[i] != segment {
				return nil, false
			}
			continue
		}

		if parts[i] == "" {
			return nil, false
		}

		values[s.placeholders[next]] = parts[i]
		next++
	}

	return values, true
}
