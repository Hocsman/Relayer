package main

import (
	"errors"
	"strings"
	"testing"
)

func markAgentExitedForTest(application *App, sessionID string) {
	application.mu.Lock()
	defer application.mu.Unlock()
	key := strings.ToLower(strings.TrimSpace(sessionID))
	delete(application.frozen, key)
	application.state.PendingEvents = nil
	if index, found := application.agentIndex[key]; found {
		application.state.Agents[index].Running = false
		application.state.Agents[index].Status = "exited"
		application.state.Agents[index].ExitCode = intPtrForTest(0)
	}
}

func agentStateForTest(application *App, sessionID string) AgentState {
	application.mu.RLock()
	defer application.mu.RUnlock()
	key := strings.ToLower(strings.TrimSpace(sessionID))
	if index, found := application.agentIndex[key]; found {
		return application.state.Agents[index]
	}
	return AgentState{}
}

func intPtrForTest(value int) *int { return &value }

func TestStartSessionRefusesUnknownAgentsAndRunningProcesses(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	runID := activeRunIDForTest(application)

	if err := application.StartSession(runID, "agent-a"); !errors.Is(err, errAgentStillRunning) {
		t.Fatalf("StartSession on running agent = %v, want errAgentStillRunning", err)
	}
	if err := application.StartSession(runID, "ghost"); !errors.Is(err, errAgentUnknown) {
		t.Fatalf("StartSession on unknown agent = %v, want errAgentUnknown", err)
	}
	if err := application.StartSession("stale-run", "agent-a"); !errors.Is(err, errRunStale) {
		t.Fatalf("StartSession on stale run = %v, want errRunStale", err)
	}
	engine.mu.Lock()
	calls := len(engine.agentStartCalls)
	engine.mu.Unlock()
	if calls != 0 {
		t.Fatalf("refused StartSession reached the engine: %d call(s)", calls)
	}
}

func TestStartSessionRepublishesAFreshAgent(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	runID := activeRunIDForTest(application)
	markAgentExitedForTest(application, "agent-a")

	if err := application.StartSession(runID, "agent-a"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	engine.mu.Lock()
	calls := append([]string(nil), engine.agentStartCalls...)
	engine.mu.Unlock()
	if len(calls) != 1 || !strings.EqualFold(calls[0], "agent-a") {
		t.Fatalf("engine start calls = %#v, want one for agent-a", calls)
	}
	state := agentStateForTest(application, "agent-a")
	if !state.Running || state.Status != "running" || state.ExitCode != nil || state.InputFrozen {
		t.Fatalf("state after hot start = %#v, want a fresh running agent", state)
	}
}

func TestStartSessionFailureMarksTheAgentFailed(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	engine.mu.Lock()
	engine.agentStartErr = errors.New("fixture start refused")
	engine.mu.Unlock()
	application := newBridgeForTest(engine)
	runID := activeRunIDForTest(application)
	markAgentExitedForTest(application, "agent-a")

	if err := application.StartSession(runID, "agent-a"); err == nil {
		t.Fatal("StartSession accepted a refused start")
	}
	state := agentStateForTest(application, "agent-a")
	if state.Running || state.Status != "failed" {
		t.Fatalf("state after failed start = %#v, want an explicit failed state", state)
	}
}
