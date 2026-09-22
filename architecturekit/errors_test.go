package architecturekit_test

import "errors"

// small helper so that the tests do not all have to import errors
func errorsIs(err, target error) bool { return errors.Is(err, target) }
