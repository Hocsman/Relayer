package server

import (
	"context"
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
	prompt := adapters.Event{ID: "evt-1", SessionID: id, Type: adapters.EventConfirmation}
	ctrl.mu.Lock()
	ctrl.pending[id+":"+prompt.ID] = pendingItem{event: prompt, view: SupervisionEvent{ID: prompt.ID, SessionID: id}}
	ctrl.rebuildPendingEventsLocked()
	ctrl.mu.Unlock()

	ctrl.handleEvent(context.Background(), nil, session.Exited{SessionID: id, Err: errors.New("tmux supervision interrupted")})

	assertLost := func(when string) {
		t.Helper()
		state := ctrl.GetState()
		for _, agent := range state.Agents {
			if strings.EqualFold(agent.SessionID, id) && (agent.Running || agent.Status != "failed") {
				t.Fatalf("%s: agent = running %v, status %q; want not running and failed", when, agent.Running, agent.Status)
			}
		}
		if len(state.PendingEvents) != 0 {
			t.Fatalf("%s: %d prompt(s) of the lost session still pending, and answering one marked the agent running", when, len(state.PendingEvents))
		}
	}
	assertLost("after the loss")

	// A withdrawal that arrives afterwards must not revive the agent.
	ctrl.handleEvent(context.Background(), nil, session.AdapterEventWithdrawn{Event: prompt})
	assertLost("after a late withdrawal")
}

// seedPrompt makes a prompt of the given session pending, detected at the
// given time, as if the event pump had just queued it.
func seedPrompt(ctrl *Controller, sessionID, id string, detected time.Time) adapters.Event {
	prompt := adapters.Event{ID: id, SessionID: sessionID, Type: adapters.EventConfirmation, Timestamp: detected}
	ctrl.mu.Lock()
	ctrl.pending[sessionID+":"+id] = pendingItem{event: prompt, view: SupervisionEvent{ID: id, SessionID: sessionID, Timestamp: detected.Format(time.RFC3339Nano)}}
	ctrl.rebuildPendingEventsLocked()
	ctrl.mu.Unlock()
	return prompt
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
	if err := ctrl.StopSession(state.RunID, id); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	agentStateEventually(t, ctrl, id, func(agent AgentState) bool { return !agent.Running })

	prompt := seedPrompt(ctrl, id, "evt-exit", time.Now().UTC())
	ctrl.mu.RLock()
	rt := ctrl.runtime
	ctrl.mu.RUnlock()
	exit := adapters.Event{ID: "exit-1", SessionID: id, Type: adapters.EventProcessExit, Timestamp: time.Now().UTC()}
	ctrl.handleEvent(context.Background(), rt, session.AdapterEvent{Event: exit})
	if pending := ctrl.GetState().PendingEvents; len(pending) != 0 {
		t.Fatalf("%d prompt(s) of the exited agent still pending", len(pending))
	}

	ctrl.handleEvent(context.Background(), rt, session.AdapterEventWithdrawn{Event: prompt})
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
	seedPrompt(ctrl, id, "evt-old", time.Now().UTC().Add(-time.Second))
	if err := ctrl.RestartSession(state.RunID, id); err != nil {
		t.Fatalf("RestartSession: %v", err)
	}
	for _, pending := range ctrl.GetState().PendingEvents {
		if pending.ID == "evt-old" {
			t.Fatal("the previous process's prompt is still offered after the restart")
		}
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
	ctrl.mu.RLock()
	rt := ctrl.runtime
	ctrl.mu.RUnlock()
	stepped := adapters.Event{ID: "evt-stepped", SessionID: id, AgentID: id, Type: adapters.EventConfirmation, Summary: "a live question", Timestamp: time.Now().UTC().Add(-time.Minute)}
	ctrl.handleEvent(context.Background(), rt, session.AdapterEvent{Event: stepped})

	shown := false
	for _, pending := range ctrl.GetState().PendingEvents {
		if pending.ID == "evt-stepped" {
			shown = true
		}
	}
	if !shown {
		t.Fatal("a live agent's prompt was dropped because its timestamp preceded the last start")
	}
}
