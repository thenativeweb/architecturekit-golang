package architecturekit

import (
	"net/url"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// offlineClient is a client for a database that is never asked, for a test of
// how NewStore builds a store.
func offlineClient(t *testing.T) *eventsourcingdb.Client {
	t.Helper()

	baseURL, err := url.Parse("http://localhost:1")
	require.NoError(t, err)

	client, err := eventsourcingdb.NewClient(baseURL, "token")
	require.NoError(t, err)

	return client
}

func TestStoreOption(t *testing.T) {
	t.Run("sets the settings of a new store, not a store", func(t *testing.T) {
		// An option that took the store could be applied to a store in use,
		// past the checks of NewStore and while others read from it, and it
		// could be written outside the kit.
		assert.Equal(t, reflect.TypeFor[*storeSettings](), reflect.TypeFor[StoreOption]().In(0))
	})
}

func TestNewStore(t *testing.T) {
	t.Run("has no state cache without WithStateCache", func(t *testing.T) {
		store := NewStore(offlineClient(t), "https://thenativeweb.io")

		assert.Nil(t, store.states)
	})

	t.Run("has a state cache for the number of subjects WithStateCache gives", func(t *testing.T) {
		for _, maxSubjects := range []int{1, 10} {
			store := NewStore(offlineClient(t), "https://thenativeweb.io", WithStateCache(maxSubjects))

			require.NotNil(t, store.states, "expected a cache for %d subjects", maxSubjects)
			assert.Equal(t, maxSubjects, store.states.maxSubjects)
		}
	})

	t.Run("has no state cache for 0 subjects, as without WithStateCache", func(t *testing.T) {
		store := NewStore(offlineClient(t), "https://thenativeweb.io", WithStateCache(0))

		assert.Nil(t, store.states)
	})

	t.Run("has no state cache if the last WithStateCache gives 0 subjects", func(t *testing.T) {
		store := NewStore(offlineClient(t), "https://thenativeweb.io", WithStateCache(10), WithStateCache(0))

		assert.Nil(t, store.states)
	})

	t.Run("waits 1 second at first and 1 minute at most without WithReconnectDelays", func(t *testing.T) {
		store := NewStore(offlineClient(t), "https://thenativeweb.io")

		assert.Equal(t, time.Second, store.settings.reconnectInitialDelay)
		assert.Equal(t, time.Minute, store.settings.reconnectMaxDelay)
	})

	t.Run("gives every store a cache of its own, also from the same option", func(t *testing.T) {
		option := WithStateCache(10)

		first := NewStore(offlineClient(t), "https://thenativeweb.io", option)
		second := NewStore(offlineClient(t), "https://thenativeweb.io", option)

		assert.NotSame(t, first.states, second.states)
	})
}
