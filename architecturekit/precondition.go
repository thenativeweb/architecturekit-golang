package architecturekit

import (
	"fmt"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Precondition is a condition that must hold for the events of a command to be
// written. Every command declares at least one, so that writing without any
// check is always a decision, never an oversight.
//
// Create one with OnPristineSubject, OnPopulatedSubject, OnEventID,
// OnStateRead, or Unconditionally, or with Require for any other precondition
// of the client SDK, such as an EventQL query.
type Precondition struct {
	kind     preconditionKind
	database eventsourcingdb.Precondition

	// subject and eventID are what OnEventID was given. The ID is checked
	// before anything is read, since it usually comes from the caller.
	subject string
	eventID string
}

type preconditionKind int

const (
	requiredKind preconditionKind = iota + 1
	onStateReadKind
	unconditionallyKind
	onEventIDKind
)

// Require makes a precondition of the client SDK one of the command, for one
// that the kit has no function of its own for, such as an EventQL query:
//
//	architecturekit.Require(eventsourcingdb.NewIsEventQLQueryTruePrecondition(query))
//
// For a subject that has to be pristine or populated, or on a certain event,
// use OnPristineSubject, OnPopulatedSubject, or OnEventID, which need no
// import of the client SDK.
func Require(precondition eventsourcingdb.Precondition) Precondition {
	return Precondition{kind: requiredKind, database: precondition}
}

// OnPristineSubject lets the events be written only if the subject has no
// events yet, which is what a command that creates something usually
// declares. It is the same as Require with
// eventsourcingdb.NewIsSubjectPristinePrecondition.
func OnPristineSubject(subject string) Precondition {
	return Require(eventsourcingdb.NewIsSubjectPristinePrecondition(subject))
}

// OnPopulatedSubject lets the events be written only if the subject has events
// already, for a command that acts on something that has to exist. It is the
// same as Require with eventsourcingdb.NewIsSubjectPopulatedPrecondition.
func OnPopulatedSubject(subject string) Precondition {
	return Require(eventsourcingdb.NewIsSubjectPopulatedPrecondition(subject))
}

// OnEventID lets the events be written only if the last event of the subject
// is the one with the given ID, for a command that was decided on a revision
// the caller hands over:
//
//	architecturekit.OnEventID(c.Subject(), c.ExpectedEventID)
//
// The ID usually comes from the caller, so Execute checks it with
// ParseRevision before it reads anything. One that is not a revision, such as
// an empty one, makes Execute fail with the error of ParseRevision, which
// wraps ErrNotARevision and names the ID, as Read does for a bound, rather
// than with a malformed request that the database refuses. Write refuses it
// the same way. Otherwise, it is the same as Require with
// eventsourcingdb.NewIsSubjectOnEventIDPrecondition, which leaves the ID to
// the database. For the revision of the state that Execute reads, use
// OnStateRead instead.
func OnEventID(subject, eventID string) Precondition {
	return Precondition{kind: onEventIDKind, subject: subject, eventID: eventID}
}

// OnStateRead lets the events be written only if nothing has been written to
// the subject since Execute read the state the command was decided on. For a
// subject without events, it requires the subject to still be pristine.
//
// Use it when the command decides on the state that Execute reads, rather than
// on a revision the caller hands over, which only Execute knows.
func OnStateRead() Precondition {
	return Precondition{kind: onStateReadKind}
}

// Unconditionally writes the events whatever has been written to the subject
// in the meantime. It can not be combined with other preconditions.
func Unconditionally() Precondition {
	return Precondition{kind: unconditionallyKind}
}

// Database returns the precondition of the client SDK that the database
// checks for this one: the one Require made it from, which is how
// OnPristineSubject and OnPopulatedSubject make theirs, or the one of
// eventsourcingdb.NewIsSubjectOnEventIDPrecondition for OnEventID. For any
// other, it returns false.
func (p Precondition) Database() (eventsourcingdb.Precondition, bool) {
	if p.kind == onEventIDKind {
		return eventsourcingdb.NewIsSubjectOnEventIDPrecondition(p.subject, p.eventID), true
	}

	return p.database, p.kind == requiredKind
}

// IsOnStateRead reports whether this precondition was made with OnStateRead.
func (p Precondition) IsOnStateRead() bool {
	return p.kind == onStateReadKind
}

// IsUnconditional reports whether this precondition was made with
// Unconditionally.
func (p Precondition) IsUnconditional() bool {
	return p.kind == unconditionallyKind
}

// CheckPreconditions fails with an error of the category ErrPermanent if a
// command declares its preconditions in a way Execute can not honor: none at
// all, a requirement of nil, Unconditionally together with others, or a zero
// Precondition, which none of the functions that create one returns. If
// nothing of that is wrong, it fails with the error of ParseRevision for the
// first ID of OnEventID that is not a revision, which wraps ErrNotARevision,
// so that a mistake in the code is not hidden behind one of the caller.
// Execute checks this before it reads anything, and the test fixture of
// architecturekittest uses CheckPreconditions to refuse such a command the
// same way.
func CheckPreconditions(cmd Command) error {
	_, err := checkPreconditions(cmd)
	return err
}

// checkPreconditions makes sure that a command declares its preconditions in
// a way Execute can honor, before anything is read.
func checkPreconditions(cmd Command) ([]Precondition, error) {
	declared := cmd.Preconditions()
	if len(declared) == 0 {
		return nil, fmt.Errorf("%w: %T declares no preconditions, use Unconditionally to write without any",
			ErrPermanent, cmd)
	}

	for _, precondition := range declared {
		switch precondition.kind {
		case requiredKind:
			if precondition.database == nil {
				return nil, fmt.Errorf("%w: %T requires a precondition that is nil", ErrPermanent, cmd)
			}
		case onStateReadKind:
			// Execute fills it in once it has read the state.
		case unconditionallyKind:
			if len(declared) > 1 {
				return nil, fmt.Errorf("%w: %T combines Unconditionally with other preconditions",
					ErrPermanent, cmd)
			}
		case onEventIDKind:
			// Its ID is checked below, once nothing else is wrong.
		default:
			return nil, fmt.Errorf("%w: %T declares a zero Precondition, which none of OnPristineSubject, "+
				"OnPopulatedSubject, OnEventID, OnStateRead, Require, or Unconditionally returns", ErrPermanent, cmd)
		}
	}

	if err := checkEventIDs(declared); err != nil {
		return nil, err
	}

	return declared, nil
}

// checkEventIDs returns the error of ParseRevision for the first ID of
// OnEventID that is not a revision, or nil if there is none. The ID usually
// comes from the caller, so, as with a bound of Read, the error names neither
// the command nor the subject, and the database, which would refuse the ID as
// a malformed request, is not asked.
func checkEventIDs(declared []Precondition) error {
	for _, precondition := range declared {
		if precondition.kind != onEventIDKind {
			continue
		}

		if _, err := ParseRevision(precondition.eventID); err != nil {
			return err
		}
	}

	return nil
}

// resolvePreconditions turns the declared preconditions into the ones the
// database checks, given the ID of the last event the state was read up to,
// which is empty for a subject without events.
func resolvePreconditions(
	subject string,
	declared []Precondition,
	lastEventID string,
) []eventsourcingdb.Precondition {
	resolved := make([]eventsourcingdb.Precondition, 0, len(declared))

	for _, precondition := range declared {
		switch precondition.kind {
		case requiredKind, onEventIDKind:
			database, _ := precondition.Database()
			resolved = append(resolved, database)
		case onStateReadKind:
			if lastEventID == "" {
				resolved = append(resolved, eventsourcingdb.NewIsSubjectPristinePrecondition(subject))
			} else {
				resolved = append(resolved, eventsourcingdb.NewIsSubjectOnEventIDPrecondition(subject, lastEventID))
			}
		case unconditionallyKind:
			// Nothing to check.
		}
	}

	return resolved
}
