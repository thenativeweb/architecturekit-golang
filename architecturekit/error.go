package architecturekit

import (
	"errors"
	"fmt"
)

// The kit sorts every failure onto two axes: whether it is a matter of the
// domain or of the machinery, and whether trying again unchanged could help.
// Callers ask for the category with errors.Is instead of matching concrete
// errors, so that new failures do not break existing code.
var (
	// ErrDomain means a business rule rejected the command. The caller has to
	// change what it asks for; asking again will not help.
	ErrDomain = errors.New("architecturekit: domain rule violated")

	// ErrTransient means the same attempt may succeed later, unchanged.
	ErrTransient = errors.New("architecturekit: transient failure")

	// ErrPermanent means trying again will not help, and something is wrong
	// with the code, the data or the configuration.
	ErrPermanent = errors.New("architecturekit: permanent failure")
)

// ErrConflict means a precondition of the write did not hold. It is transient,
// because the state it disagreed with has moved on.
//
// Note that the EventSourcingDB answers a violated precondition and a schema
// violation with the same status, and its Go client exports no typed error. A
// schema violation therefore also arrives here, although it is permanent.
var ErrConflict = fmt.Errorf("%w: a precondition did not hold", ErrTransient)

// ErrUnverified means that an event the store has read failed its
// verification: its hash does not match its content, or its signature is
// missing or does not match the verification key. It is permanent, because
// reading the same event again yields the same result, but it may point to a
// security incident rather than a mistake, which is why it can be told apart
// (see WithHashVerification and WithSignatureVerification).
var ErrUnverified = fmt.Errorf("%w: an event could not be verified", ErrPermanent)

// DomainError means that a business rule applies. It is not a failure in the
// technical sense, but a valid answer.
type DomainError struct{ message string }

// NewDomainError creates a rejection carrying the given message.
func NewDomainError(format string, args ...any) error {
	return &DomainError{message: fmt.Sprintf(format, args...)}
}

func (e *DomainError) Error() string { return e.message }

// Unwrap sorts every domain error under ErrDomain, so that a caller can ask
// for the category without knowing this type.
func (e *DomainError) Unwrap() error { return ErrDomain }
