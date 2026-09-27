package architecturekit

import (
	"fmt"

	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// Precondition is a condition that must hold for the events of a command to be
// written. Every command declares at least one, so that writing without any
// check is always a decision, never an oversight.
//
// Create one with Require, OnStateRead, or Unconditionally.
type Precondition struct {
	kind     preconditionKind
	database eventsourcingdb.Precondition
}

type preconditionKind int

const (
	requiredKind preconditionKind = iota + 1
	onStateReadKind
	unconditionallyKind
)

// Require makes a precondition of the client SDK one of the command, for
// example a revision the caller hands over:
//
//	architecturekit.Require(eventsourcingdb.NewIsSubjectOnEventIDPrecondition(c.Subject(), c.ExpectedEventID))
func Require(precondition eventsourcingdb.Precondition) Precondition {
	return Precondition{kind: requiredKind, database: precondition}
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

// Database returns the precondition of the client SDK that Require made this
// one from, or false if it was not made with Require.
func (p Precondition) Database() (eventsourcingdb.Precondition, bool) {
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
		default:
			return nil, fmt.Errorf("%w: %T declares a precondition not made with Require, OnStateRead, or Unconditionally",
				ErrPermanent, cmd)
		}
	}

	return declared, nil
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
		case requiredKind:
			resolved = append(resolved, precondition.database)
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
