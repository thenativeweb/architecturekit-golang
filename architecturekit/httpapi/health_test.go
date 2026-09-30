package httpapi_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
	"github.com/thenativeweb/architecturekit-golang/architecturekit/httpapi"
	"github.com/thenativeweb/eventsourcingdb-client-golang/eventsourcingdb"
)

// ignoring is a projection that accepts every event.
type ignoring struct{}

func (ignoring) Apply(context.Context, eventsourcingdb.Event) error { return nil }

// emptyDatabase answers like an EventSourcingDB without any events. It keeps
// an observed stream open, sending heartbeats, or ends it at once, which a
// real database only does when it restarts. Its store waits an hour before it
// reads again, so that a run stays in whatever phase the test put it in.
func emptyDatabase(t *testing.T, keepsObserving bool) *architecturekit.Store {
	t.Helper()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Server", "EventSourcingDB/test")

		if r.URL.Path != "/api/v1/observe-events" || !keepsObserving {
			return
		}

		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()

		for {
			select {
			case <-r.Context().Done():
				return
			case <-ticker.C:
				_, _ = fmt.Fprintln(w, `{"type":"heartbeat"}`)
				w.(http.Flusher).Flush()
			}
		}
	}))
	t.Cleanup(server.Close)

	serverURL, err := url.Parse(server.URL)
	require.NoError(t, err)
	client, err := eventsourcingdb.NewClient(serverURL, "secret")
	require.NoError(t, err)

	return architecturekit.NewStore(client, "https://thenativeweb.io",
		architecturekit.WithReconnectDelays(time.Hour, time.Hour))
}

// startRun starts a projection on the given store, and ends it with the test.
func startRun(t *testing.T, store *architecturekit.Store) (*architecturekit.ProjectionRun, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	run := architecturekit.StartProjection(ctx, store, "/", true, ignoring{})
	t.Cleanup(func() {
		cancel()
		<-run.Done()
	})

	return run, cancel
}

func waitUntil(t *testing.T, condition func() bool) {
	t.Helper()

	require.Eventually(t, condition, 5*time.Second, 5*time.Millisecond)
}

type healthResponse struct {
	IsReady     *bool                     `json:"isReady"`
	IsAlive     *bool                     `json:"isAlive"`
	Projections map[string]map[string]any `json:"projections"`
}

func askHealth(t *testing.T, handler http.Handler) (int, healthResponse, http.Header, string) {
	t.Helper()

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))

	var body healthResponse
	require.NoError(t, json.Unmarshal(recorder.Body.Bytes(), &body))

	return recorder.Code, body, recorder.Header(), recorder.Body.String()
}

func TestHealth(t *testing.T) {
	t.Run("is not ready while a projection has never caught up, but alive", func(t *testing.T) {
		run, _ := startRun(t, deadStore(t))
		waitUntil(t, func() bool { return run.Status().Attempts >= 1 })
		projections := map[string]*architecturekit.ProjectionRun{"catalog": run}

		code, body, _, _ := askHealth(t, httpapi.Readiness(projections))
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.False(t, *body.IsReady)

		code, body, _, _ = askHealth(t, httpapi.Liveness(projections))
		assert.Equal(t, http.StatusOK, code)
		assert.True(t, *body.IsAlive)
	})

	t.Run("is ready and alive once every projection is live", func(t *testing.T) {
		run, _ := startRun(t, emptyDatabase(t, true))
		<-run.CaughtUp()
		projections := map[string]*architecturekit.ProjectionRun{"catalog": run}

		code, body, _, _ := askHealth(t, httpapi.Readiness(projections))
		assert.Equal(t, http.StatusOK, code)
		assert.True(t, *body.IsReady)
		assert.Equal(t, "live", body.Projections["catalog"]["phase"])
		assert.Equal(t, true, body.Projections["catalog"]["hasCaughtUp"])

		code, body, _, _ = askHealth(t, httpapi.Liveness(projections))
		assert.Equal(t, http.StatusOK, code)
		assert.True(t, *body.IsAlive)
	})

	t.Run("stays ready and alive while a projection reconnects after it has caught up", func(t *testing.T) {
		run, _ := startRun(t, emptyDatabase(t, false))
		waitUntil(t, func() bool {
			status := run.Status()
			return status.HasCaughtUp && status.Phase == architecturekit.PhaseReconnecting
		})
		projections := map[string]*architecturekit.ProjectionRun{"catalog": run}

		code, body, _, _ := askHealth(t, httpapi.Readiness(projections))
		assert.Equal(t, http.StatusOK, code)
		assert.True(t, *body.IsReady)
		assert.Equal(t, "reconnecting", body.Projections["catalog"]["phase"])
		assert.InDelta(t, 1, body.Projections["catalog"]["attempts"], 0)

		code, _, _, _ = askHealth(t, httpapi.Liveness(projections))
		assert.Equal(t, http.StatusOK, code)
	})

	t.Run("is neither ready nor alive once a projection has stopped", func(t *testing.T) {
		run, cancel := startRun(t, emptyDatabase(t, true))
		<-run.CaughtUp()
		cancel()
		<-run.Done()
		projections := map[string]*architecturekit.ProjectionRun{"catalog": run}

		code, body, _, _ := askHealth(t, httpapi.Readiness(projections))
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.False(t, *body.IsReady)
		assert.Equal(t, "stopped", body.Projections["catalog"]["phase"])

		code, body, _, _ = askHealth(t, httpapi.Liveness(projections))
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.False(t, *body.IsAlive)
	})

	t.Run("judges by every projection", func(t *testing.T) {
		live, _ := startRun(t, emptyDatabase(t, true))
		<-live.CaughtUp()
		stopped, cancel := startRun(t, emptyDatabase(t, true))
		<-stopped.CaughtUp()
		cancel()
		<-stopped.Done()
		projections := map[string]*architecturekit.ProjectionRun{"catalog": live, "loans": stopped}

		code, body, _, _ := askHealth(t, httpapi.Readiness(projections))
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.Equal(t, "live", body.Projections["catalog"]["phase"])
		assert.Equal(t, "stopped", body.Projections["loans"]["phase"])

		code, _, _, _ = askHealth(t, httpapi.Liveness(projections))
		assert.Equal(t, http.StatusServiceUnavailable, code)
	})

	t.Run("is ready and alive without any projection", func(t *testing.T) {
		code, body, _, _ := askHealth(t, httpapi.Readiness(nil))
		assert.Equal(t, http.StatusOK, code)
		assert.True(t, *body.IsReady)
		assert.Empty(t, body.Projections)

		code, _, _, _ = askHealth(t, httpapi.Liveness(nil))
		assert.Equal(t, http.StatusOK, code)
	})

	t.Run("answers with JSON that must not be cached", func(t *testing.T) {
		_, _, header, _ := askHealth(t, httpapi.Readiness(nil))

		assert.Equal(t, "application/json", header.Get("Content-Type"))
		assert.Equal(t, "no-store", header.Get("Cache-Control"))
	})

	t.Run("tells where each projection stands, but not why", func(t *testing.T) {
		run, _ := startRun(t, deadStore(t))
		waitUntil(t, func() bool { return run.Status().Attempts >= 1 })
		require.Error(t, run.Status().Err, "the run must have a reason to reconnect")

		_, body, _, raw := askHealth(t, httpapi.Liveness(map[string]*architecturekit.ProjectionRun{"catalog": run}))

		assert.ElementsMatch(t, []string{"phase", "since", "hasCaughtUp", "attempts", "revision"},
			keysOf(body.Projections["catalog"]))
		assert.NotContains(t, raw, "127.0.0.1", "the reason may name internal addresses")
	})

	t.Run("keeps watching the projections it was given", func(t *testing.T) {
		run, cancel := startRun(t, emptyDatabase(t, true))
		<-run.CaughtUp()
		projections := map[string]*architecturekit.ProjectionRun{"catalog": run}
		handler := httpapi.Liveness(projections)

		delete(projections, "catalog")
		cancel()
		<-run.Done()

		code, _, _, _ := askHealth(t, handler)
		assert.Equal(t, http.StatusServiceUnavailable, code, "changing the map later must not change what is watched")
	})

	t.Run("panics for a projection without a run", func(t *testing.T) {
		assert.PanicsWithValue(t, `architecturekit/httpapi: the projection "catalog" has no run`, func() {
			httpapi.Readiness(map[string]*architecturekit.ProjectionRun{"catalog": nil})
		})
	})
}

func keysOf(values map[string]any) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}

	return keys
}
