package architecturekittest

import (
	"fmt"
	"reflect"
	"strings"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Precondition is what a command declared, in a shape a test can compare.
//
// The client keeps its precondition types unexported, so this is reconstructed
// from the methods they expose. A check for a pristine subject and one for a
// populated subject expose the same method, so they are told apart by
// comparing them with one built anew.
type Precondition struct {
	// Subject is set for every precondition of the client that guards one.
	Subject string

	// Pristine is set only for a check that the subject has no events yet, as
	// built by architecturekit.OnPristineSubject, or by Require with
	// eventsourcingdb.NewIsSubjectPristinePrecondition.
	Pristine bool

	// Populated is set only for a check that the subject has events, as built
	// by architecturekit.OnPopulatedSubject, or by Require with
	// eventsourcingdb.NewIsSubjectPopulatedPrecondition.
	Populated bool

	// EventID is set only for a revision check, as built by
	// architecturekit.OnEventID, or by Require with
	// eventsourcingdb.NewIsSubjectOnEventIDPrecondition.
	EventID string

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
			described = append(described, Precondition{
				Subject: typed.Subject(),
				EventID: typed.EventID(),
			})
		case queryPrecondition:
			described = append(described, Precondition{Query: typed.Query()})
		case subjectPrecondition:
			subject := typed.Subject()

			described = append(described, Precondition{
				Subject:   subject,
				Pristine:  isEqual(database, eventsourcingdb.NewIsSubjectPristinePrecondition(subject)),
				Populated: isEqual(database, eventsourcingdb.NewIsSubjectPopulatedPrecondition(subject)),
			})
		}
	}

	return described
}

// isEqual tells whether a precondition of the client equals one built anew.
// The client's preconditions are plain structs, so they are equal exactly when
// the same function built both from the same values.
//
// Comparing values of a type that cannot be compared panics. The client's
// types can be compared, so such a type would take a change to the client.
// It is reported as unequal then, so that the test that expects it fails,
// instead of the whole run panicking.
func isEqual(declared, built eventsourcingdb.Precondition) bool {
	if !reflect.ValueOf(declared).Comparable() {
		return false
	}

	return declared == built
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
// something usually declares.
func OnPristineSubject(subject string) Precondition {
	return Precondition{Subject: subject, Pristine: true}
}

// OnPopulatedSubject describes architecturekit.OnPopulatedSubject, a check that
// the subject has events.
func OnPopulatedSubject(subject string) Precondition {
	return Precondition{Subject: subject, Populated: true}
}

// OnEventID describes architecturekit.OnEventID, a revision check.
func OnEventID(subject, eventID string) Precondition {
	return Precondition{Subject: subject, EventID: eventID}
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
