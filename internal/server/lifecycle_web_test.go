package server

import (
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/policy"
)

func agentStateEventually(t *testing.T, ctrl *Controller, sessionID string, want func(AgentState) bool) AgentState {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	var last AgentState
	for time.Now().Before(deadline) {
		for _, agent := range ctrl.GetState().Agents {
			if strings.EqualFold(agent.SessionID, sessionID) {
				last = agent
				if want(agent) {
					return agent
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("agent %s never reached the expected state; last %+v", sessionID, last)
	return last
}

// TestWebAgentCanBeStoppedAndStartedAgainAndAgain covers the web half of the
// restart-after-exit fix. v0.8.5 broadcast "stopped" on an exit, a status the
// interface does not clear Running on, and never set Running back after a
// start: the card offered Start for a running agent and Stop for a dead one.
func TestWebAgentCanBeStoppedAndStartedAgainAndAgain(t *testing.T) {
	ctrl, _ := startSettingsController(t, policy.DefaultConfig())
	state := ctrl.GetState()
	if len(state.Agents) == 0 {
		t.Skip("the default configuration started no agent on this platform")
	}
	sessionID := state.Agents[0].SessionID

	for cycle := 1; cycle <= 2; cycle++ {
		if err := ctrl.StopSession(state.RunID, sessionID); err != nil {
			t.Fatalf("cycle %d: StopSession: %v", cycle, err)
		}
		stopped := agentStateEventually(t, ctrl, sessionID, func(agent AgentState) bool { return !agent.Running })
		if stopped.Status != "exited" && stopped.Status != "failed" {
			t.Fatalf("cycle %d: status after the exit = %q, want exited or failed", cycle, stopped.Status)
		}

		if err := ctrl.StartSession(state.RunID, sessionID); err != nil {
			t.Fatalf("cycle %d: StartSession: %v", cycle, err)
		}
		started := agentStateEventually(t, ctrl, sessionID, func(agent AgentState) bool { return agent.Running })
		if started.Status != "running" {
			t.Fatalf("cycle %d: status after the start = %q, want running", cycle, started.Status)
		}
	}
}
