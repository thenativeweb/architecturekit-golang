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

func TestCategoryTexts(t *testing.T) {
	// An error that is written for the caller reaches them with the text of its
	// category, such as one that is not a revision, so the text names what is
	// wrong rather than the package it comes from.
	for _, test := range []struct {
		err  error
		text string
	}{
		{architecturekit.ErrDomain, "domain rule violated"},
		{architecturekit.ErrTransient, "transient failure"},
		{architecturekit.ErrPermanent, "permanent failure"},
		{architecturekit.ErrConflict, "transient failure: a precondition did not hold"},
		{architecturekit.ErrUnverified, "permanent failure: an event could not be verified"},
		{architecturekit.ErrNotARevision, "not a revision"},
	} {
		t.Run(test.text, func(t *testing.T) {
			assert.EqualError(t, test.err, test.text)
		})
	}
}
