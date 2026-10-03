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

// panickingCheckpoint is a resumable projection that panics when it is asked
// where it stopped, as one with a bug does.
type panickingCheckpoint struct{ ignoring }

func (panickingCheckpoint) Checkpoint(context.Context) (string, error) {
	panic("the checkpoint is broken")
}

func (panickingCheckpoint) SaveCheckpoint(context.Context, string) error { return nil }

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

// startRun starts a projection of the given name on the given store, and ends
// it with the test.
func startRun(t *testing.T, store *architecturekit.Store, name string) (*architecturekit.ProjectionRun, context.CancelFunc) {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	run := architecturekit.StartProjection(ctx, store, architecturekit.SubjectTree("/"), ignoring{}, architecturekit.Named(name))
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
		run, _ := startRun(t, deadStore(t), "catalog")
		waitUntil(t, func() bool { return run.Status().Attempts >= 1 })
		projections := []*architecturekit.ProjectionRun{run}

		code, body, _, _ := askHealth(t, httpapi.Readiness(projections...))
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.False(t, *body.IsReady)

		code, body, _, _ = askHealth(t, httpapi.Liveness(projections...))
		assert.Equal(t, http.StatusOK, code)
		assert.True(t, *body.IsAlive)
	})

	t.Run("is ready and alive once every projection is live", func(t *testing.T) {
		run, _ := startRun(t, emptyDatabase(t, true), "catalog")
		<-run.CaughtUp()
		projections := []*architecturekit.ProjectionRun{run}

		code, body, _, _ := askHealth(t, httpapi.Readiness(projections...))
		assert.Equal(t, http.StatusOK, code)
		assert.True(t, *body.IsReady)
		assert.Equal(t, "live", body.Projections["catalog"]["phase"])
		assert.Equal(t, true, body.Projections["catalog"]["hasCaughtUp"])

		code, body, _, _ = askHealth(t, httpapi.Liveness(projections...))
		assert.Equal(t, http.StatusOK, code)
		assert.True(t, *body.IsAlive)
	})

	t.Run("stays ready and alive while a projection reconnects after it has caught up", func(t *testing.T) {
		run, _ := startRun(t, emptyDatabase(t, false), "catalog")
		waitUntil(t, func() bool {
			status := run.Status()
			return status.HasCaughtUp && status.Phase == architecturekit.PhaseReconnecting
		})
		projections := []*architecturekit.ProjectionRun{run}

		code, body, _, _ := askHealth(t, httpapi.Readiness(projections...))
		assert.Equal(t, http.StatusOK, code)
		assert.True(t, *body.IsReady)
		assert.Equal(t, "reconnecting", body.Projections["catalog"]["phase"])
		assert.InDelta(t, 1, body.Projections["catalog"]["attempts"], 0)

		code, _, _, _ = askHealth(t, httpapi.Liveness(projections...))
		assert.Equal(t, http.StatusOK, code)
	})

	t.Run("is neither ready nor alive once a projection has stopped", func(t *testing.T) {
		run, cancel := startRun(t, emptyDatabase(t, true), "catalog")
		<-run.CaughtUp()
		cancel()
		<-run.Done()
		projections := []*architecturekit.ProjectionRun{run}

		code, body, _, _ := askHealth(t, httpapi.Readiness(projections...))
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.False(t, *body.IsReady)
		assert.Equal(t, "stopped", body.Projections["catalog"]["phase"])

		code, body, _, _ = askHealth(t, httpapi.Liveness(projections...))
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.False(t, *body.IsAlive)
	})

	t.Run("is neither ready nor alive once a projection has panicked", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)

		run := architecturekit.StartProjection(ctx, emptyDatabase(t, true), architecturekit.SubjectTree("/"), panickingCheckpoint{},
			architecturekit.Named("catalog"))
		<-run.Done()
		require.ErrorIs(t, run.Err(), architecturekit.ErrPermanent, "the panic has to end the run, not the process")
		projections := []*architecturekit.ProjectionRun{run}

		code, body, _, _ := askHealth(t, httpapi.Readiness(projections...))
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.False(t, *body.IsReady)
		assert.Equal(t, "stopped", body.Projections["catalog"]["phase"])

		code, body, _, _ = askHealth(t, httpapi.Liveness(projections...))
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.False(t, *body.IsAlive)
	})

	t.Run("judges by every projection", func(t *testing.T) {
		live, _ := startRun(t, emptyDatabase(t, true), "catalog")
		<-live.CaughtUp()
		stopped, cancel := startRun(t, emptyDatabase(t, true), "loans")
		<-stopped.CaughtUp()
		cancel()
		<-stopped.Done()
		projections := []*architecturekit.ProjectionRun{live, stopped}

		code, body, _, _ := askHealth(t, httpapi.Readiness(projections...))
		assert.Equal(t, http.StatusServiceUnavailable, code)
		assert.Equal(t, "live", body.Projections["catalog"]["phase"])
		assert.Equal(t, "stopped", body.Projections["loans"]["phase"])

		code, _, _, _ = askHealth(t, httpapi.Liveness(projections...))
		assert.Equal(t, http.StatusServiceUnavailable, code)
	})

	t.Run("is ready and alive without any projection", func(t *testing.T) {
		code, body, _, _ := askHealth(t, httpapi.Readiness())
		assert.Equal(t, http.StatusOK, code)
		assert.True(t, *body.IsReady)
		assert.Empty(t, body.Projections)

		code, _, _, _ = askHealth(t, httpapi.Liveness())
		assert.Equal(t, http.StatusOK, code)
	})

	t.Run("answers with JSON that must not be cached", func(t *testing.T) {
		_, _, header, _ := askHealth(t, httpapi.Readiness())

		assert.Equal(t, "application/json", header.Get("Content-Type"))
		assert.Equal(t, "no-store", header.Get("Cache-Control"))
	})

	t.Run("tells where each projection stands, but not why", func(t *testing.T) {
		run, _ := startRun(t, deadStore(t), "catalog")
		waitUntil(t, func() bool { return run.Status().Attempts >= 1 })
		require.Error(t, run.Status().Err, "the run must have a reason to reconnect")

		_, body, _, raw := askHealth(t, httpapi.Liveness(run))

		assert.ElementsMatch(t, []string{"phase", "since", "hasCaughtUp", "attempts", "revision"},
			keysOf(body.Projections["catalog"]))
		assert.NotContains(t, raw, "127.0.0.1", "the reason may name internal addresses")
	})

	t.Run("keeps watching the projections it was given", func(t *testing.T) {
		run, cancel := startRun(t, emptyDatabase(t, true), "catalog")
		<-run.CaughtUp()
		projections := []*architecturekit.ProjectionRun{run}
		handler := httpapi.Liveness(projections...)

		projections[0] = nil
		cancel()
		<-run.Done()

		code, _, _, _ := askHealth(t, handler)
		assert.Equal(t, http.StatusServiceUnavailable, code, "changing the slice later must not change what is watched")
	})

	t.Run("panics for a projection without a run", func(t *testing.T) {
		assert.PanicsWithValue(t, "architecturekit/httpapi: projection 0 has no run", func() {
			httpapi.Readiness(nil)
		})
	})

	t.Run("panics for a projection without a name", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		run := architecturekit.StartProjection(ctx, deadStore(t), architecturekit.SubjectTree("/"), ignoring{})
		t.Cleanup(func() {
			cancel()
			<-run.Done()
		})

		assert.PanicsWithValue(t, "architecturekit/httpapi: projection 0 has no name, give it one with architecturekit.Named", func() {
			httpapi.Liveness(run)
		})
	})

	t.Run("panics for two projections of the same name", func(t *testing.T) {
		first, _ := startRun(t, deadStore(t), "catalog")
		second, _ := startRun(t, deadStore(t), "catalog")

		assert.PanicsWithValue(t, `architecturekit/httpapi: two projections are named "catalog"`, func() {
			httpapi.Readiness(first, second)
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
