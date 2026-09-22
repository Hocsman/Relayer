package main

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
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

func exitEventForTest(sessionID string) adapters.Event {
	// A fresh process numbers its events from 1 again, so each instance's
	// first exit carries the same ID.
	code := 0
	return adapters.NewProcessExitEvent(sessionID, sessionID, "generic", 1, &code, false)
}

func activeRunForTest(application *App) *runGeneration {
	application.mu.RLock()
	defer application.mu.RUnlock()
	return application.active
}

// TestRestartAfterASecondNaturalExit is the v0.8.5 regression: a restarted
// agent's exit carries the same event ID as the exit before it, the desktop kept
// that ID as resolved, and the second exit was dropped. The agent then looked
// running forever, and Start was refused as "still running".
func TestRestartAfterASecondNaturalExit(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	runID := activeRunIDForTest(application)
	run := activeRunForTest(application)

	for cycle := 1; cycle <= 3; cycle++ {
		application.handleAdapterEventForRun(run, exitEventForTest("agent-a"))
		if state := agentStateForTest(application, "agent-a"); state.Running || state.Status != "exited" {
			t.Fatalf("cycle %d: state after the exit = %#v, want exited", cycle, state)
		}
		if err := application.StartSession(runID, "agent-a"); err != nil {
			t.Fatalf("cycle %d: StartSession after a natural exit: %v", cycle, err)
		}
		if state := agentStateForTest(application, "agent-a"); !state.Running {
			t.Fatalf("cycle %d: state after the start = %#v, want running", cycle, state)
		}
	}
}

// TestAStaleExitDoesNotStopTheReplacement: the previous process's exit can be
// emitted after its replacement started. The lifecycle then reports it stale,
// and the desktop must keep showing the replacement as running.
func TestAStaleExitDoesNotStopTheReplacement(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	run := activeRunForTest(application)

	engine.mu.Lock()
	engine.staleExits = true
	engine.mu.Unlock()
	application.handleAdapterEventForRun(run, exitEventForTest("agent-a"))

	if state := agentStateForTest(application, "agent-a"); !state.Running || state.Status != "running" {
		t.Fatalf("state after a stale exit = %#v, want the replacement still running", state)
	}
}

// TestARestartDropsThePreviousProcesssPrompts: during a restart the previous
// process's exit is set aside as stale, and its prompts used to stay pending on
// the replacement. They blocked its automatic decisions, and its own first
// prompt, which carries the same ID, was refused as a duplicate.
func TestARestartDropsThePreviousProcesssPrompts(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	runID := activeRunIDForTest(application)
	run := activeRunForTest(application)

	application.handleAdapterEventForRun(run, bridgeEvent("agent-a", "prompt-1"))
	if state, _ := application.GetState(); len(state.PendingEvents) != 1 {
		t.Fatalf("pending before the restart = %d, want the previous process's prompt", len(state.PendingEvents))
	}

	engine.mu.Lock()
	engine.staleExits = true
	engine.mu.Unlock()
	if err := application.RestartSession(runID, "agent-a"); err != nil {
		t.Fatalf("RestartSession: %v", err)
	}
	application.handleAdapterEventForRun(run, exitEventForTest("agent-a"))

	state, _ := application.GetState()
	if len(state.PendingEvents) != 0 {
		t.Fatalf("pending after the restart = %#v, want the previous process's prompt gone", state.PendingEvents)
	}

	// The replacement raises the same prompt: it must be pending, not refused.
	repeated := bridgeEvent("agent-a", "prompt-1")
	repeated.Timestamp = time.Now().UTC()
	application.handleAdapterEventForRun(run, repeated)
	if state, _ := application.GetState(); len(state.PendingEvents) != 1 {
		t.Fatalf("pending after the replacement's prompt = %d, want it pending", len(state.PendingEvents))
	}
}
