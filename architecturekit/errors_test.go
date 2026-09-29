package architecturekit_test

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

func TestErrUnverified(t *testing.T) {
	t.Run("is permanent", func(t *testing.T) {
		assert.ErrorIs(t, architecturekit.ErrUnverified, architecturekit.ErrPermanent)
		assert.NotErrorIs(t, architecturekit.ErrUnverified, architecturekit.ErrTransient,
			"an unverified event must not be retried")
	})
}
