package architecturekit

import (
	"fmt"
	"runtime/debug"
)

// panicError is a panic while a projection ran or caught up, such as one in
// Apply that writes into a nil map. A run has a goroutine of its own, where a
// panic would end the whole process, so it ends the run instead, with this
// error. It belongs to ErrPermanent, since a panic is a mistake in the code,
// and trying again would only panic again.
//
// Its message holds the value and the stack of the panic, so that logging the
// error of the run shows where it happened.
//
// It does not unwrap to the value, even if that is an error, since the value
// says nothing about whether trying again helps: a panic with an error of the
// category ErrTransient would otherwise keep the run trying forever.
type panicError struct {
	value any
	stack []byte
}

func (failure *panicError) Error() string {
	return fmt.Sprintf("architecturekit: panic while running the projection: %v\n\n%s", failure.value, failure.stack)
}

// Unwrap sorts every panic under ErrPermanent.
func (*panicError) Unwrap() error { return ErrPermanent }

// recoverInto turns a panic into the error that err points to. It has to be
// deferred, so that recover sees the panic, and the stack it takes is then
// still the one the panic happened on.
func recoverInto(err *error) {
	if value := recover(); value != nil {
		*err = &panicError{value: value, stack: debug.Stack()}
	}
}
