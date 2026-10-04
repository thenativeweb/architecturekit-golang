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
// passes through unchanged, unless it is wrapped with a category.
//
// A failure of the database wraps the error of the client after its category,
// so that errors.As finds an *eventsourcingdb.DBAPIError with the status code
// and the reason the database gave. It is the category, though, that tells
// what the failure means.
var (
	// ErrDomain means a business rule rejected the command. The caller has to
	// change what it asks for; asking again will not help.
	ErrDomain = errors.New("architecturekit: domain rule violated")

	// ErrTransient means the same attempt may succeed later, unchanged.
	ErrTransient = errors.New("architecturekit: transient failure")

	// ErrPermanent means trying again will not help, and something is wrong
	// with the code, the data or the configuration, for example an event whose
	// data can not be encoded as JSON, such as a float NaN.
	ErrPermanent = errors.New("architecturekit: permanent failure")
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
