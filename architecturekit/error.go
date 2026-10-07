package architecturekit

import (
	"errors"
	"fmt"
)

// The kit sorts its own failures in reading and writing onto two axes: whether
// it is a matter of the domain or of the machinery, and whether trying again
// unchanged could help. Callers ask for the category with errors.Is instead of
// matching concrete errors, so that new failures do not break existing code.
// An error of the application's own code, such as one a decider returns,
// passes through unchanged, unless it is wrapped with a category. A write
// whose outcome is unknown belongs to no category (see ErrOutcomeUnknown), and
// neither does a view that did not catch up with a write that has succeeded
// (see ErrNotCaughtUp).
//
// A failure of the database wraps the error of the client after its category,
// or after ErrOutcomeUnknown, so that errors.As finds an
// *eventsourcingdb.DBAPIError with the status code and the reason the
// database gave. It is the category, though, that tells what the failure
// means.
//
// The texts of the categories leave out the name of the package, since an
// error that is written for a caller, such as one of ErrDomain, may reach them
// with its text.
var (
	// ErrDomain means a business rule rejected the command. The caller has to
	// change what it asks for; asking again will not help.
	ErrDomain = errors.New("domain rule violated")

	// ErrTransient means the same attempt may succeed later, unchanged. A
	// write that failed with it has certainly stored nothing, so trying it
	// again can not store its events twice.
	ErrTransient = errors.New("transient failure")

	// ErrPermanent means trying again will not help, and something is wrong
	// with the code, the data or the configuration, for example an event whose
	// data can not be encoded as JSON, such as a float NaN.
	ErrPermanent = errors.New("permanent failure")
)

// ErrConflict means a precondition of the write did not hold. It is transient,
// because the state it disagreed with has moved on.
//
// An event that does not match its schema is not a conflict, although the
// EventSourcingDB answers it with the same status. Writing it again yields the
// same result, so it is reported as ErrPermanent.
var ErrConflict = fmt.Errorf("%w: a precondition did not hold", ErrTransient)

// ErrUnverified means that an event the store has read failed its
// verification: its hash does not match its content, or its signature is
// missing or does not match the verification key. It is permanent, because
// reading the same event again yields the same result, but it may point to a
// security incident rather than a mistake, which is why it can be told apart
// (see NewStore and WithSignatureVerification).
var ErrUnverified = fmt.Errorf("%w: an event could not be verified", ErrPermanent)

// ErrOutcomeUnknown means that a write failed in a way that leaves open
// whether the database stored the events: the request had left, but no
// complete answer arrived, since the connection broke, the answer was cut off
// or could not be decoded, or a timeout of the client ran out, or the answer
// was one that may come after the events were stored, such as 500, or one
// that does not come from an EventSourcingDB, such as a 502 or 504 of a proxy
// in front of it.
//
// It belongs to no category. It is not ErrTransient, since trying again may
// store the events twice, and not ErrPermanent, since trying again may be
// right, once it is clear that nothing was stored. So find that out first,
// for example by reading the subject, or try again only with a precondition
// that refuses the same events a second time, such as OnEventID with the
// revision the command was decided on. The kit never tries it again by
// itself, not even with WithConflictRetries. Only a write that certainly
// stored nothing fails with a category, so ErrTransient keeps its promise.
var ErrOutcomeUnknown = errors.New("outcome unknown")

// DomainError means that a business rule applies. It is not a failure in the
// technical sense, but a valid answer.
type DomainError struct{ message string }

// NewDomainError creates a rejection carrying the given message.
func NewDomainError(format string, args ...any) error {
	return &DomainError{message: fmt.Sprintf(format, args...)}
}

// Error returns the message, exactly as NewDomainError formatted it.
func (e *DomainError) Error() string { return e.message }

// Unwrap sorts every domain error under ErrDomain, so that a caller can ask
// for the category without knowing this type.
func (e *DomainError) Unwrap() error { return ErrDomain }
