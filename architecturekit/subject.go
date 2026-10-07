package architecturekit

import (
	"fmt"
	"slices"
	"strings"
)

// SubjectScheme describes how a subject is composed, in the style of the
// patterns that http.ServeMux understands: literal segments and placeholders
// in braces, as in "/tenant/{tenant}/workshop/{workshop}".
//
// A scheme works in both directions. Build composes a subject from values, and
// Match takes one apart again, or Value takes a single value out of it, which
// a projection needs to recover the aggregate ID that the events themselves do
// not carry.
//
// EventSourcingDB allows only ASCII letters and digits, underscores, and
// hyphens in a segment of a subject, so this is what the literal segments of
// a pattern and the values of its placeholders may contain. The names of its
// placeholders follow the same rule, so that a typo, such as a brace too many,
// is not taken for a name.
type SubjectScheme struct {
	pattern      string
	segments     []string
	placeholders []string
}

// NewSubjectScheme parses a pattern. A malformed pattern, including one with a
// literal segment or a placeholder name that contains a character
// EventSourcingDB does not allow in a subject, is a programming error and
// panics while the scheme is being built.
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
			if !hasOnlySubjectCharacters(segment) {
				panic(fmt.Sprintf("architecturekit: segment %q in %q may only contain %s",
					segment, pattern, subjectCharacters))
			}
			continue
		}

		// A placeholder is a whole segment in a single pair of braces, so a
		// brace anywhere else, as in "{id}}" or "{{id}}", is a mistake.
		name, isClosed := strings.CutSuffix(segment[1:], "}")
		if !isClosed || strings.ContainsAny(name, "{}") {
			panic(fmt.Sprintf("architecturekit: segment %q in %q is malformed", segment, pattern))
		}

		if name == "" {
			panic(fmt.Sprintf("architecturekit: pattern %q has an unnamed placeholder", pattern))
		}
		if !hasOnlySubjectCharacters(name) {
			panic(fmt.Sprintf("architecturekit: placeholder %q in %q may only contain %s",
				name, pattern, subjectCharacters))
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

// Root returns the subject that every subject of the scheme lies under: the
// literal segments before the first placeholder, as "/tenant" for
// "/tenant/{tenant}/workshop/{workshop}", or "/" if the pattern starts with a
// placeholder. Reading or observing recursively from it yields the events of
// all subjects the scheme describes, and possibly of others under the same
// root, which Match tells apart.
func (s *SubjectScheme) Root() string {
	var literals []string

	for _, segment := range s.segments {
		if strings.HasPrefix(segment, "{") {
			break
		}

		literals = append(literals, segment)
	}

	return "/" + strings.Join(literals, "/")
}

// Placeholders returns the placeholder names, in the order Build expects them.
func (s *SubjectScheme) Placeholders() []string {
	names := make([]string, len(s.placeholders))
	copy(names, s.placeholders)
	return names
}

// Build composes a subject. The values fill the placeholders in the order they
// appear in the pattern. A wrong number of values, or one that Check refuses,
// such as an empty one or one with a slash or a dot, is a programming error
// and panics. Values that come from outside, such as an ID in a request, may
// well be like that, so check them with Check first.
func (s *SubjectScheme) Build(values ...string) string {
	if err := s.check(values, true); err != nil {
		panic(err.Error())
	}

	var builder strings.Builder
	next := 0

	for _, segment := range s.segments {
		builder.WriteString("/")

		if !strings.HasPrefix(segment, "{") {
			builder.WriteString(segment)
			continue
		}

		builder.WriteString(values[next])
		next++
	}

	return builder.String()
}

// Check tells whether the values can compose a subject: one per placeholder,
// none of them empty, and each made only of the characters EventSourcingDB
// allows in a segment of a subject, which are A-Z, a-z, 0-9, underscores, and
// hyphens. It is what Build insists on, as an error rather than a panic, for
// values that come from outside.
//
// The error may reach the caller of an API, for example through ToCommand of
// httpapi, so it names the placeholder of a value, but neither the package nor
// the pattern, as in: value for "id" must not be empty. The panic of Build
// names both, since it is for the developer.
func (s *SubjectScheme) Check(values ...string) error {
	return s.check(values, false)
}

// check tells whether the values can compose a subject, for Check, or for
// Build if isForDeveloper is set, in which case the error names the package
// and the pattern as well.
func (s *SubjectScheme) check(values []string, isForDeveloper bool) error {
	if len(values) != len(s.placeholders) {
		if isForDeveloper {
			return fmt.Errorf("architecturekit: pattern %q needs %d value(s), got %d",
				s.pattern, len(s.placeholders), len(values))
		}

		return fmt.Errorf("the subject needs %d value(s), got %d", len(s.placeholders), len(values))
	}

	prefix, inPattern := "", ""
	if isForDeveloper {
		prefix, inPattern = "architecturekit: ", fmt.Sprintf(" in %q", s.pattern)
	}

	for i, value := range values {
		if value == "" {
			return fmt.Errorf("%svalue for %q%s must not be empty",
				prefix, s.placeholders[i], inPattern)
		}
		if !hasOnlySubjectCharacters(value) {
			return fmt.Errorf("%svalue %q for %q%s may only contain %s",
				prefix, value, s.placeholders[i], inPattern, subjectCharacters)
		}
	}

	return nil
}

// Match takes a subject apart. It reports false if the subject does not follow
// the pattern, which is an ordinary case for a projection reading recursively
// across several schemes. A subject with a value that Check refuses does not
// follow it either, so that Match takes apart only what Build composes.
//
// The map holds the value of every placeholder of the pattern, and looking up
// any other name in it, such as one with a typo, yields an empty string. To
// take a single value out of a subject, use Value, which panics for such a
// name instead.
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

		if parts[i] == "" || !hasOnlySubjectCharacters(parts[i]) {
			return nil, false
		}

		values[s.placeholders[next]] = parts[i]
		next++
	}

	return values, true
}

// Value takes the value of a single placeholder out of a subject. It reports
// false if the subject does not follow the pattern, as Match does. To take
// several values out of the same subject, use Match.
//
// A placeholder that the pattern does not have, such as one with a typo, is a
// programming error and panics, whatever the subject is, rather than yield an
// empty value, as the map that Match returns would.
func (s *SubjectScheme) Value(subject, placeholder string) (string, bool) {
	if !slices.Contains(s.placeholders, placeholder) {
		panic(fmt.Sprintf("architecturekit: pattern %q has no placeholder %q", s.pattern, placeholder))
	}

	values, ok := s.Match(subject)
	if !ok {
		return "", false
	}

	return values[placeholder], true
}

// subjectCharacters names the characters that EventSourcingDB allows in a
// segment of a subject, for the messages of errors and panics.
const subjectCharacters = "A-Z, a-z, 0-9, underscores, and hyphens"

// hasOnlySubjectCharacters tells whether a segment of a subject contains only
// characters that EventSourcingDB allows there. It is true for an empty
// segment, which the callers refuse on their own.
func hasOnlySubjectCharacters(segment string) bool {
	for _, character := range segment {
		allowed := 'A' <= character && character <= 'Z' ||
			'a' <= character && character <= 'z' ||
			'0' <= character && character <= '9' ||
			character == '_' || character == '-'

		if !allowed {
			return false
		}
	}

	return true
}
