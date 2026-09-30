package httpapi

import (
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

// Readiness answers whether the application can serve requests, judged by its
// projections, so that an orchestrator such as Kubernetes only sends requests
// once the views are built. It answers 200 once every projection has caught
// up, and 503 while one of them is still catching up for the first time, or
// has stopped.
//
// A projection that reconnects after it has caught up keeps the application
// ready: its view is behind, but consistent, and every instance shares the
// database it waits for, so taking them all out would answer nothing instead
// of something that is behind.
//
// The projections are the runs started with StartProjection or
// StartTransactionalProjection, by name. A nil run is a programming error, so
// Readiness panics.
func Readiness(projections map[string]*architecturekit.ProjectionRun) http.Handler {
	return healthHandler(projections, "isReady", isReady)
}

// Liveness answers whether the application is alive, judged by its
// projections, so that an orchestrator such as Kubernetes restarts it once a
// projection has stopped. It answers 200 as long as every projection runs,
// even while it catches up or reconnects, since a restart would only start
// that over, and 503 once one of them has stopped: its view never changes
// again, and a restart builds it anew, with a configuration that may have been
// fixed in the meantime.
//
// The projections are the runs started with StartProjection or
// StartTransactionalProjection, by name. A nil run is a programming error, so
// Liveness panics.
func Liveness(projections map[string]*architecturekit.ProjectionRun) http.Handler {
	return healthHandler(projections, "isAlive", isAlive)
}

func isReady(status architecturekit.ProjectionStatus) bool {
	return status.HasCaughtUp && status.Phase != architecturekit.PhaseStopped
}

func isAlive(status architecturekit.ProjectionStatus) bool {
	return status.Phase != architecturekit.PhaseStopped
}

// projectionHealth is what the health handlers tell about a projection. It
// leaves out why a projection reconnects or has stopped, since health checks
// are usually reachable without signing in, and the reason may name internal
// addresses. The application logs it instead.
type projectionHealth struct {
	Phase       architecturekit.Phase `json:"phase"`
	Since       time.Time             `json:"since"`
	HasCaughtUp bool                  `json:"hasCaughtUp"`
	Attempts    int                   `json:"attempts"`
	Revision    string                `json:"revision"`
}

func healthHandler(
	projections map[string]*architecturekit.ProjectionRun,
	verdict string,
	isHealthy func(architecturekit.ProjectionStatus) bool,
) http.Handler {
	for name, run := range projections {
		if run == nil {
			panic(fmt.Sprintf("architecturekit/httpapi: the projection %q has no run", name))
		}
	}

	// The handler keeps its own copy, so that changing the given map later on
	// does not change what it watches.
	projections = maps.Clone(projections)

	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		isHealthyOverall := true
		details := make(map[string]projectionHealth, len(projections))

		for name, run := range projections {
			status := run.Status()
			isHealthyOverall = isHealthyOverall && isHealthy(status)

			details[name] = projectionHealth{
				Phase:       status.Phase,
				Since:       status.Since,
				HasCaughtUp: status.HasCaughtUp,
				Attempts:    status.Attempts,
				Revision:    status.Revision,
			}
		}

		code := http.StatusOK
		if !isHealthyOverall {
			code = http.StatusServiceUnavailable
		}

		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Cache-Control", "no-store")
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(map[string]any{verdict: isHealthyOverall, "projections": details})
	})
}
