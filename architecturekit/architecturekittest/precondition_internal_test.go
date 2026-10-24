package architecturekittest

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/thenativeweb/eventsourcingdb-client-golang/v2/eventsourcingdb"
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

	t.Run("takes checks with options as equal if they were built alike", func(t *testing.T) {
		// The client holds the options behind a pointer, which differs for
		// every check, so == would tell these apart.
		recursive := eventsourcingdb.PreconditionOptions{Recursive: true}

		assert.True(t, isEqual(
			eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1", recursive),
			eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1", recursive),
		))
		assert.True(t, isEqual(
			eventsourcingdb.NewIsSubjectPopulatedPrecondition("/account/1", recursive),
			eventsourcingdb.NewIsSubjectPopulatedPrecondition("/account/1", recursive),
		))
		assert.True(t, isEqual(
			eventsourcingdb.NewIsSubjectOnEventIDPrecondition("/account/1", "7", recursive),
			eventsourcingdb.NewIsSubjectOnEventIDPrecondition("/account/1", "7", recursive),
		))
	})

	t.Run("tells a recursive check from one of the subject alone", func(t *testing.T) {
		recursive := eventsourcingdb.PreconditionOptions{Recursive: true}
		alone := eventsourcingdb.PreconditionOptions{Recursive: false}

		assert.False(t, isEqual(
			eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1", recursive),
			eventsourcingdb.NewIsSubjectPristinePrecondition("/account/1"),
		))
		assert.False(t, isEqual(
			eventsourcingdb.NewIsSubjectPopulatedPrecondition("/account/1", recursive),
			eventsourcingdb.NewIsSubjectPopulatedPrecondition("/account/1", alone),
		))
		assert.False(t, isEqual(
			eventsourcingdb.NewIsSubjectOnEventIDPrecondition("/account/1", "7", recursive),
			eventsourcingdb.NewIsSubjectOnEventIDPrecondition("/account/1", "7"),
		))
	})

	t.Run("compares a type that cannot be compared by its values instead of panicking", func(t *testing.T) {
		declared := uncomparable{subjects: []string{"/account/1"}}

		assert.NotPanics(t, func() {
			assert.True(t, isEqual(declared, uncomparable{subjects: []string{"/account/1"}}))
			assert.False(t, isEqual(declared, uncomparable{subjects: []string{"/account/2"}}))
		})
	})
}
