package architecturekittest

import (
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// Precondition is what a command declared, in a shape a test can compare.
//
// The client keeps its precondition types unexported, so this is reconstructed
// from the methods they expose. One consequence: a check for a pristine
// subject and one for a populated subject both surface as a bare Subject,
// because the client gives nothing else away to tell them apart.
type Precondition struct {
	// Subject is set for every precondition of the client that guards one.
	Subject string

	// EventID is set only for a revision check.
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
			described = append(described, Precondition{Subject: typed.Subject()})
		}
	}

	return described
}

// OnSubject describes a precondition that only guards a subject.
func OnSubject(subject string) Precondition {
	return Precondition{Subject: subject}
}

// OnEventID describes a revision check.
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
