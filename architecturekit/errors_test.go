package architecturekit_test

import (
	"errors"
	"testing"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// small helper so that the tests do not all have to import errors
func errorsIs(err, target error) bool { return errors.Is(err, target) }

func TestErrUnverifiedIsPermanent(t *testing.T) {
	if !errors.Is(architecturekit.ErrUnverified, architecturekit.ErrPermanent) {
		t.Fatal("an unverified event is expected to be permanent")
	}
	if errors.Is(architecturekit.ErrUnverified, architecturekit.ErrTransient) {
		t.Fatal("an unverified event must not be retried")
	}
}
