package architecturekittest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// uncomparable stands for a precondition of the client whose type cannot be
// compared, which comparing with == would panic on. It satisfies the client's
// sealed interface by embedding it.
type uncomparable struct {
	eventsourcingdb.Precondition

	subjects []string
}

func TestIsEqual(t *testing.T) {
	t.Run("tells a pristine from a populated subject", func(t *testing.T) {
		pristine := eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1")
		populated := eventsourcingdb.NewIsSubjectPopulatedPrecondition("/account/1")

		assert.True(t, isEqual(pristine, eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1")))
		assert.True(t, isEqual(populated, eventsourcingdb.NewIsSubjectPopulatedPrecondition("/account/1")))
		assert.False(t, isEqual(pristine, populated))
		assert.False(t, isEqual(populated, pristine))
	})

	t.Run("tells subjects apart", func(t *testing.T) {
		assert.False(t, isEqual(
			eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1"),
			eventsourcingdb.NewIsSubjectPristinePrecondition("/account/2"),
		))
	})

	t.Run("reports a type that cannot be compared as unequal instead of panicking", func(t *testing.T) {
		declared := uncomparable{subjects: []string{"/account/1"}}
		built := uncomparable{subjects: []string{"/account/1"}}

		assert.NotPanics(t, func() {
			assert.False(t, isEqual(declared, built))
		})
	})
}
