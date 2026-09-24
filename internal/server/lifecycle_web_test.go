package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
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

// livePrompt is a question of the given session, detected at the given time,
// taken in through the run's event handling as the event loop takes one in.
// The gateway's tests seeded the gateway's own table of prompts instead; the
// prompts are the supervision core's now, and a seeded one would test nothing
// the gateway does.
func livePrompt(ctrl *Controller, sessionID, id string, detected time.Time) adapters.Event {
	prompt := adapters.Event{
		ID:        id,
		Signature: "signature-" + id,
		SessionID: sessionID,
		AgentID:   sessionID,
		Adapter:   "generic",
		Type:      adapters.EventConfirmation,
		Summary:   "a live question",
		Risk:      adapters.RiskLow,
		Timestamp: detected,
	}
	injectEvent(ctrl, session.AdapterEvent{Event: prompt})
	return prompt
}

// promptOffered reports whether a prompt is among those every client reads.
func promptOffered(ctrl *Controller, id string) bool {
	for _, pending := range ctrl.GetState().PendingEvents {
		if pending.ID == id {
			return true
		}
	}
	return false
}

// TestLosingATmuxSessionClearsRunning: when Relayer loses ownership of a tmux
// session, the gateway marked the agent "stopped", a status the interface does
// not clear Running on, so the card kept a Stop button that could only fail.
func TestLosingATmuxSessionClearsRunning(t *testing.T) {
	ctrl, _ := startSettingsController(t, policy.ProfileConfig(policy.ProfileDeveloperFriendly, ""))
	agents := ctrl.GetState().Agents
	if len(agents) == 0 {
		t.Fatal("the default configuration has no agent")
	}
	id := agents[0].SessionID
	// A prompt was waiting when the session was lost.
	prompt := livePrompt(ctrl, id, "evt-1", time.Now().UTC())
	if !promptOffered(ctrl, prompt.ID) {
		t.Fatal("the prompt of a live session was not taken in")
	}

	injectEvent(ctrl, session.Exited{SessionID: id, Err: errors.New("tmux supervision interrupted")})

	assertLost := func(when string) {
		t.Helper()
		state := ctrl.GetState()
		for _, agent := range state.Agents {
			if strings.EqualFold(agent.SessionID, id) && (agent.Running || agent.Status != "failed") {
				t.Fatalf("%s: agent = running %v, status %q; want not running and failed", when, agent.Running, agent.Status)
			}
		}
		for _, pending := range state.PendingEvents {
			if strings.EqualFold(pending.SessionID, id) {
				t.Fatalf("%s: a prompt of the lost session is still pending, and answering it marked the agent running", when)
			}
		}
	}
	assertLost("after the loss")

	// A withdrawal that arrives afterwards must not revive the agent.
	injectEvent(ctrl, session.AdapterEventWithdrawn{Event: prompt})
	assertLost("after a late withdrawal")
}

// TestAnExitedAgentLosesItsPromptsOnTheWeb: the prompts of an agent that
// exited stayed pending, and answering one set the dead agent running again.
func TestAnExitedAgentLosesItsPromptsOnTheWeb(t *testing.T) {
	ctrl, _ := startSettingsController(t, policy.DefaultConfig())
	state := ctrl.GetState()
	if len(state.Agents) == 0 {
		t.Skip("the default configuration started no agent on this platform")
	}
	id := state.Agents[0].SessionID
	prompt := livePrompt(ctrl, id, "evt-exit", time.Now().UTC())
	if !promptOffered(ctrl, prompt.ID) {
		t.Fatal("the prompt of a live session was not taken in")
	}
	if err := ctrl.StopSession(state.RunID, id); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	agentStateEventually(t, ctrl, id, func(agent AgentState) bool { return !agent.Running })
	// The stop shows the agent exited once it returns; its prompts go with
	// the process's exit, which follows.
	for deadline := time.Now().Add(10 * time.Second); promptOffered(ctrl, prompt.ID); {
		if time.Now().After(deadline) {
			t.Fatal("the prompt of the exited agent is still pending")
		}
		time.Sleep(20 * time.Millisecond)
	}

	injectEvent(ctrl, session.AdapterEventWithdrawn{Event: prompt})
	for _, agent := range ctrl.GetState().Agents {
		if strings.EqualFold(agent.SessionID, id) && agent.Running {
			t.Fatal("a late withdrawal marked the exited agent running")
		}
	}
}

// TestARestartDropsThePreviousProcesssPrompts: the previous process's exit is
// set aside as stale during a restart, and its prompts used to stay offered on
// the replacement, which never raised them.
func TestARestartDropsThePreviousProcesssPrompts(t *testing.T) {
	ctrl, _ := startSettingsController(t, policy.DefaultConfig())
	state := ctrl.GetState()
	if len(state.Agents) == 0 {
		t.Skip("the default configuration started no agent on this platform")
	}
	id := state.Agents[0].SessionID
	livePrompt(ctrl, id, "evt-old", time.Now().UTC().Add(-time.Second))
	if !promptOffered(ctrl, "evt-old") {
		t.Fatal("the prompt of a live session was not taken in")
	}
	if err := ctrl.RestartSession(state.RunID, id); err != nil {
		t.Fatalf("RestartSession: %v", err)
	}
	if promptOffered(ctrl, "evt-old") {
		t.Fatal("the previous process's prompt is still offered after the restart")
	}
}

// TestAWebLivePromptStampedBeforeTheStartIsStillShown: the gateway used to drop
// at ingestion every prompt detected before the session's last start. After the
// clock steps back, that is every prompt of a live agent.
func TestAWebLivePromptStampedBeforeTheStartIsStillShown(t *testing.T) {
	ctrl, _ := startSettingsController(t, policy.DefaultConfig())
	state := ctrl.GetState()
	if len(state.Agents) == 0 {
		t.Skip("the default configuration started no agent on this platform")
	}
	id := state.Agents[0].SessionID
	if err := ctrl.RestartSession(state.RunID, id); err != nil {
		t.Fatalf("RestartSession: %v", err)
	}
	livePrompt(ctrl, id, "evt-stepped", time.Now().UTC().Add(-time.Minute))
	if !promptOffered(ctrl, "evt-stepped") {
		t.Fatal("a live agent's prompt was dropped because its timestamp preceded the last start")
	}
}
