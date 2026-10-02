package architecturekittest_test

import (
	"regexp"
	"runtime/debug"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDependencies(t *testing.T) {
	t.Run("leave out Testcontainers and the Docker client", func(t *testing.T) {
		// A test that only checks a decider has to build without Docker, which
		// is why the database helpers live in dbtest. The test binary holds
		// this package and everything it imports.
		info, ok := debug.ReadBuildInfo()
		require.True(t, ok, "the test binary carries no build information")

		forbidden := regexp.MustCompile(`(?i)testcontainers|docker`)
		for _, dependency := range info.Deps {
			assert.False(t, forbidden.MatchString(dependency.Path), "depends on %s", dependency.Path)
		}
	})
}
