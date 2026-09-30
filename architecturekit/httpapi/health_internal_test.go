package httpapi

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/thenativeweb/architecturekit-golang/architecturekit"
)

func TestHealthVerdicts(t *testing.T) {
	tests := []struct {
		name        string
		phase       architecturekit.Phase
		hasCaughtUp bool
		isReady     bool
		isAlive     bool
	}{
		{"catching up for the first time", architecturekit.PhaseCatchingUp, false, false, true},
		{"reconnecting before it has ever caught up", architecturekit.PhaseReconnecting, false, false, true},
		{"live", architecturekit.PhaseLive, true, true, true},
		{"reconnecting after it has caught up", architecturekit.PhaseReconnecting, true, true, true},
		{"stopped before it has ever caught up", architecturekit.PhaseStopped, false, false, false},
		{"stopped after it has caught up", architecturekit.PhaseStopped, true, false, false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			status := architecturekit.ProjectionStatus{Phase: test.phase, HasCaughtUp: test.hasCaughtUp}

			assert.Equal(t, test.isReady, isReady(status), "ready")
			assert.Equal(t, test.isAlive, isAlive(status), "alive")
		})
	}
}
