package architecturekittest

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/thenativeweb/architecturekit-golang/v2/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/v2/eventsourcingdb"
)

// Precondition is what a command declared, in a shape a test can compare.
//
// The client keeps its precondition types unexported, so this is reconstructed
// from the methods they expose. A check for a pristine subject and one for a
// populated subject expose the same method, and no check exposes whether it is
// recursive, so they are told apart by comparing them with ones built anew.
type Precondition struct {
	// Subject is set for every precondition of the client that guards one.
	Subject string

	// Pristine is set only for a check that the subject has no events yet, as
	// built by architecturekit.OnPristineSubject, or by Require with
	// eventsourcingdb.NewIsSubjectPristinePrecondition. For a recursive one,
	// Recursive is set as well.
	Pristine bool

	// Populated is set only for a check that the subject has events, as built
	// by architecturekit.OnPopulatedSubject, or by Require with
	// eventsourcingdb.NewIsSubjectPopulatedPrecondition. For a recursive one,
	// Recursive is set as well.
	Populated bool

	// EventID is set only for a revision check, as built by
	// architecturekit.OnEventID, or by Require with
	// eventsourcingdb.NewIsSubjectOnEventIDPrecondition. For a recursive one,
	// Recursive is set as well.
	EventID string

	// Recursive is set only for a pristine, populated, or revision check that
	// applies to the subject together with all subjects below it, as built by
	// Require with eventsourcingdb.PreconditionOptions{Recursive: true}. Such
	// a check is another one than the same check of the subject alone, so the
	// two never compare as equal. Options that leave a check to the subject
	// alone mean the same as none, so Recursive is not set for them.
	Recursive bool

	// Query is set only for an EventQL precondition.
	Query string

	// OnStateRead is set only for architecturekit.OnStateRead. Which event it
	// stands for is only known once Execute has read the state.
	OnStateRead bool

	// Unconditional is set only for architecturekit.Unconditionally.
	Unconditional bool
}

// The client's concrete types are unexported, but their methods are not, so a
// test can recognise them by the methods they carry.
type subjectPrecondition interface {
	Subject() string
}

type revisionPrecondition interface {
	Subject() string
	EventID() string
}

type queryPrecondition interface {
	Query() string
}

// recursive makes a check of the client apply to the subject together with
// all subjects below it.
var recursive = eventsourcingdb.PreconditionOptions{Recursive: true}

// PreconditionsOf reports what a command declared. A command that declares
// none, which Execute rejects, yields an empty slice.
func PreconditionsOf(cmd architecturekit.Command) []Precondition {
	declared := cmd.Preconditions()
	described := make([]Precondition, 0, len(declared))

	for _, precondition := range declared {
		switch {
		case precondition.IsOnStateRead():
			described = append(described, Precondition{OnStateRead: true})
			continue
		case precondition.IsUnconditional():
			described = append(described, Precondition{Unconditional: true})
			continue
		}

		database, ok := precondition.Database()
		if !ok || database == nil {
			// Execute rejects such a precondition, so the test should see it
			// rather than have it silently dropped.
			described = append(described, Precondition{})
			continue
		}

		// The client's Precondition interface is sealed by an unexported
		// method, so only its own four types can occur here, and the cases
		// below cover all of them. That is why there is no fallback: an
		// unknown kind cannot exist without a change to the client itself.
		switch typed := database.(type) {
		// The revision check comes first, because it also carries a subject.
		case revisionPrecondition:
			// It exposes its subject and its ID, so only whether it is
			// recursive takes a comparison.
			subject, eventID := typed.Subject(), typed.EventID()
			recursively := eventsourcingdb.NewIsSubjectOnEventIDPrecondition(subject, eventID, recursive)

			described = append(described, Precondition{
				Subject:   subject,
				EventID:   eventID,
				Recursive: isEqual(database, recursively),
			})
		case queryPrecondition:
			described = append(described, Precondition{Query: typed.Query()})
		case subjectPrecondition:
			subject := typed.Subject()
			isPristine, isRecursivelyPristine := isBuiltBy(
				database, eventsourcingdb.NewIsSubjectPristinePrecondition, subject)
			isPopulated, isRecursivelyPopulated := isBuiltBy(
				database, eventsourcingdb.NewIsSubjectPopulatedPrecondition, subject)

			described = append(described, Precondition{
				Subject:   subject,
				Pristine:  isPristine,
				Populated: isPopulated,
				Recursive: isRecursivelyPristine || isRecursivelyPopulated,
			})
		}
	}

	return described
}

// isBuiltBy tells whether a check of the client on a subject equals one that
// build makes for the same subject, and if so, whether it is recursive.
// Options that leave the check to the subject alone mean the same to the
// database as none, so a check built with them counts as one built without.
func isBuiltBy(
	declared eventsourcingdb.Precondition,
	build func(subject string, options ...eventsourcingdb.PreconditionOptions) eventsourcingdb.Precondition,
	subject string,
) (isBuilt, isRecursive bool) {
	if isEqual(declared, build(subject, recursive)) {
		return true, true
	}

	isBuilt = isEqual(declared, build(subject)) ||
		isEqual(declared, build(subject, eventsourcingdb.PreconditionOptions{}))

	return isBuilt, false
}

// isEqual tells whether a precondition of the client equals one built anew.
// The client's preconditions are plain structs, so they are equal exactly when
// the same function built both from the same values.
//
// They are compared deeply, since a check on a subject holds its options
// behind a pointer, which differs for every check built with options, so that
// == would tell even two checks apart that were built alike. Unlike ==,
// reflect.DeepEqual does not panic for a type that cannot be compared either,
// which would take a change to the client, but compares its values.
func isEqual(declared, built eventsourcingdb.Precondition) bool {
	return reflect.DeepEqual(declared, built)
}

// describePrecondition spells out the fields that are set, so that a failure
// shows at a glance which kind was declared and which was expected.
func describePrecondition(precondition Precondition) string {
	value := reflect.ValueOf(precondition)
	fields := make([]string, 0, value.NumField())

	for i := range value.NumField() {
		if value.Field(i).IsZero() {
			continue
		}

		fields = append(fields, fmt.Sprintf("%s: %#v", value.Type().Field(i).Name, value.Field(i).Interface()))
	}

	return "{" + strings.Join(fields, ", ") + "}"
}

func describePreconditions(preconditions []Precondition) string {
	described := make([]string, len(preconditions))
	for i, precondition := range preconditions {
		described[i] = describePrecondition(precondition)
	}

	return "[" + strings.Join(described, ", ") + "]"
}

// OnPristineSubject describes architecturekit.OnPristineSubject, a check that
// the subject has no events yet, which is what a command that creates
// something usually declares. It does not describe the recursive check, which
// OnPristineSubjectRecursively does.
func OnPristineSubject(subject string) Precondition {
	return Precondition{Subject: subject, Pristine: true}
}

// OnPristineSubjectRecursively describes a check that neither the subject nor
// any subject below it has events yet, as built by Require with
// eventsourcingdb.NewIsSubjectPristinePrecondition and
// eventsourcingdb.PreconditionOptions{Recursive: true}.
func OnPristineSubjectRecursively(subject string) Precondition {
	return Precondition{Subject: subject, Pristine: true, Recursive: true}
}

// OnPopulatedSubject describes architecturekit.OnPopulatedSubject, a check that
// the subject has events. It does not describe the recursive check, which
// OnPopulatedSubjectRecursively does.
func OnPopulatedSubject(subject string) Precondition {
	return Precondition{Subject: subject, Populated: true}
}

// OnPopulatedSubjectRecursively describes a check that the subject or a
// subject below it has events, as built by Require with
// eventsourcingdb.NewIsSubjectPopulatedPrecondition and
// eventsourcingdb.PreconditionOptions{Recursive: true}.
func OnPopulatedSubjectRecursively(subject string) Precondition {
	return Precondition{Subject: subject, Populated: true, Recursive: true}
}

// OnEventID describes architecturekit.OnEventID, a revision check. It does not
// describe the recursive check, which OnEventIDRecursively does.
func OnEventID(subject, eventID string) Precondition {
	return Precondition{Subject: subject, EventID: eventID}
}

// OnEventIDRecursively describes a check that the last event among the subject
// and all subjects below it has the given ID, as built by Require with
// eventsourcingdb.NewIsSubjectOnEventIDPrecondition and
// eventsourcingdb.PreconditionOptions{Recursive: true}.
func OnEventIDRecursively(subject, eventID string) Precondition {
	return Precondition{Subject: subject, EventID: eventID, Recursive: true}
}

// OnQuery describes an EventQL precondition.
func OnQuery(query string) Precondition {
	return Precondition{Query: query}
}

// OnStateRead describes architecturekit.OnStateRead.
func OnStateRead() Precondition {
	return Precondition{OnStateRead: true}
}

// Unconditionally describes architecturekit.Unconditionally.
func Unconditionally() Precondition {
	return Precondition{Unconditional: true}
}
