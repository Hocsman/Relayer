package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/policy"
)

// markAgentExitedForTest ends the agent's process as its own exit would: the
// agent is no longer running, shows exited with code 0, and its prompts go.
func markAgentExitedForTest(application *App, sessionID string) {
	application.handleAdapterEventForRun(activeRunForTest(application), exitEventForTest(sessionID))
}

func agentStateForTest(application *App, sessionID string) AgentState {
	state, err := application.GetState()
	if err != nil {
		return AgentState{}
	}
	for _, agent := range state.Agents {
		if strings.EqualFold(strings.TrimSpace(agent.SessionID), strings.TrimSpace(sessionID)) {
			return agent
		}
	}
	return AgentState{}
}

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

// exitEventForTest is the exit of one process. Each call is another process:
// its exit carries the same sequence as the one before, but an ID of its own,
// as the processor salts it with the process instance.
func exitEventForTest(sessionID string) adapters.Event {
	code := 0
	event := adapters.NewProcessExitEvent(sessionID, sessionID, "generic", 1, &code, false)
	event.ID = fmt.Sprintf("%s-process-%d", event.ID, processesForTest.Add(1))
	return event
}

var processesForTest atomic.Uint64

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

// TestTheReplacementsFirstPromptSurvivesItsRestart: the replacement's first
// prompt usually carries the previous process's prompt ID. Taken in while the
// restart was still completing, it was refused as a duplicate of the old one,
// which was still pending, and never shown or audited.
func TestTheReplacementsFirstPromptSurvivesItsRestart(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	started := make(chan string, 1)
	release := make(chan struct{})
	engine.mu.Lock()
	engine.agentRestartStarted = started
	engine.agentRestartRelease = release
	engine.mu.Unlock()
	application := newBridgeForTest(engine)
	runID := activeRunIDForTest(application)
	run := activeRunForTest(application)

	application.handleAdapterEventForRun(run, bridgeEvent("agent-a", "prompt-1"))
	restarted := make(chan error, 1)
	go func() { restarted <- application.RestartSession(runID, "agent-a") }()
	<-started

	// The replacement is up and asks the same question before the restart
	// has finished on the desktop's side.
	replacement := bridgeEvent("agent-a", "prompt-1")
	replacement.Timestamp = time.Now().UTC()
	replacement.Summary = "replacement question"
	application.handleAdapterEventForRun(run, replacement)
	close(release)
	if err := <-restarted; err != nil {
		t.Fatalf("RestartSession: %v", err)
	}

	state, _ := application.GetState()
	if len(state.PendingEvents) != 1 || state.PendingEvents[0].Summary != "replacement question" {
		t.Fatalf("pending after the restart = %#v, want the replacement's own prompt", state.PendingEvents)
	}
}

// TestAnAnsweredPromptOfThePreviousProcessStaysAnswered: the replacement's
// first prompt has an ID of its own, so it is taken in although the previous
// process's prompt was answered; and a late copy of that old prompt is still
// refused. v0.8.6 forgot the answered IDs at each start, which let the copy in.
func TestAnAnsweredPromptOfThePreviousProcessStaysAnswered(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	runID := activeRunIDForTest(application)
	run := activeRunForTest(application)

	application.handleAdapterEventForRun(run, bridgeEvent("agent-a", "prompt-previous"))
	if err := application.SubmitDecision(runID, "agent-a", "prompt-previous", "Y"); err != nil {
		t.Fatalf("answering the previous process's prompt: %v", err)
	}
	if err := application.RestartSession(runID, "agent-a"); err != nil {
		t.Fatalf("RestartSession: %v", err)
	}

	late := bridgeEvent("agent-a", "prompt-previous")
	application.handleAdapterEventForRun(run, late)
	if state, _ := application.GetState(); len(state.PendingEvents) != 0 {
		t.Fatalf("a late copy of an answered prompt was taken in again: %#v", state.PendingEvents)
	}

	replacement := bridgeEvent("agent-a", "prompt-replacement")
	replacement.Timestamp = time.Now().UTC()
	application.handleAdapterEventForRun(run, replacement)
	if state, _ := application.GetState(); len(state.PendingEvents) != 1 || state.PendingEvents[0].ID != "prompt-replacement" {
		t.Fatalf("pending after the replacement's prompt = %#v, want it", state.PendingEvents)
	}
}

// TestAPromptRaisedWhileTheAgentStartsIsKept: during a Start the agent is not
// yet marked running, and a prompt its new process raised then was dropped as
// the prompt of a stopped agent, and remembered as answered.
func TestAPromptRaisedWhileTheAgentStartsIsKept(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	started := make(chan string, 1)
	release := make(chan struct{})
	engine.mu.Lock()
	engine.agentStartStarted = started
	engine.agentStartRelease = release
	engine.mu.Unlock()
	application := newBridgeForTest(engine)
	runID := activeRunIDForTest(application)
	run := activeRunForTest(application)

	application.handleAdapterEventForRun(run, exitEventForTest("agent-a"))
	startDone := make(chan error, 1)
	go func() { startDone <- application.StartSession(runID, "agent-a") }()
	<-started
	prompt := bridgeEvent("agent-a", "prompt-early")
	prompt.Timestamp = time.Now().UTC()
	application.handleAdapterEventForRun(run, prompt)
	close(release)
	if err := <-startDone; err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if state, _ := application.GetState(); len(state.PendingEvents) != 1 || state.PendingEvents[0].ID != "prompt-early" {
		t.Fatalf("pending after the start = %#v, want the prompt raised while it started", state.PendingEvents)
	}
}

// TestALivePromptStampedBeforeTheStartIsStillShown: prompts used to be dropped
// at ingestion when their detection time preceded the session's last start.
// After the clock steps back, an NTP correction for instance, that is every
// prompt of a live agent, dropped without being shown, decided or journaled.
func TestALivePromptStampedBeforeTheStartIsStillShown(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	runID := activeRunIDForTest(application)
	run := activeRunForTest(application)

	if err := application.RestartSession(runID, "agent-a"); err != nil {
		t.Fatalf("RestartSession: %v", err)
	}
	stepped := bridgeEvent("agent-a", "prompt-stepped")
	stepped.Timestamp = time.Now().UTC().Add(-time.Minute)
	application.handleAdapterEventForRun(run, stepped)

	state, _ := application.GetState()
	if len(state.PendingEvents) != 1 || state.PendingEvents[0].ID != "prompt-stepped" {
		t.Fatalf("pending = %#v, want the live agent's prompt", state.PendingEvents)
	}
}

// TestAnAutomaticPromptRaisedWhileTheAgentStartsIsDecidedAfterIt: no automatic
// decision is taken while a session is changing, so a prompt the new process
// raised during its Start waited for a human although the policy decides it.
// It is decided once, when the start completes.
func TestAnAutomaticPromptRaisedWhileTheAgentStartsIsDecidedAfterIt(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	engine.evaluation = policy.Evaluation{
		Action:         policy.ActionAllow,
		ProposedAction: policy.ActionAllow,
		RuleName:       "allow-safe",
		Reason:         policy.ReasonRule,
		Automatic:      true,
	}
	started := make(chan string, 1)
	release := make(chan struct{})
	engine.mu.Lock()
	engine.agentStartStarted = started
	engine.agentStartRelease = release
	engine.mu.Unlock()
	application := newBridgeForTest(engine)
	application.ctx = context.Background()
	runID := activeRunIDForTest(application)
	run := activeRunForTest(application)

	application.handleAdapterEventForRun(run, exitEventForTest("agent-a"))
	startDone := make(chan error, 1)
	go func() { startDone <- application.StartSession(runID, "agent-a") }()
	<-started
	prompt := bridgeEvent("agent-a", "prompt-automatic")
	prompt.Timestamp = time.Now().UTC()
	application.handleAdapterEventForRun(run, prompt)
	// A decision is claimed synchronously, before the goroutine that writes
	// it runs; counting writes here raced that goroutine and saw none.
	if state, _ := application.GetState(); len(state.PendingEvents) != 1 || state.PendingEvents[0].DeliveryStatus != "pending" {
		t.Fatalf("an automatic decision was claimed while the agent was starting: %#v", state.PendingEvents)
	}
	close(release)
	if err := <-startDone; err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	waitForCondition(t, 2*time.Second, func() bool { return len(engine.applySnapshot()) == 1 })
	run.sup.BeginDrain()
	run.sup.Wait()
	if calls := engine.applySnapshot(); len(calls) != 1 {
		t.Fatalf("automatic decisions after the start = %d, want exactly 1", len(calls))
	}
}

// TestARestartedAgentIsNeverShownRunningWithItsPredecessorsOutput: the bridge
// drops a session's output, under its own lock, when the core reports the new
// process, and GetState lays the core's agent over that output. The core used
// to show the agent running first, so a GetState between the two steps showed
// the new process running with the previous process's output. The test holds
// the bridge's lock, which parks the start on the output reset, and reads what
// a GetState would show at that moment.
func TestARestartedAgentIsNeverShownRunningWithItsPredecessorsOutput(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	runID := activeRunIDForTest(application)
	run := activeRunForTest(application)
	markAgentExitedForTest(application, "agent-a")
	before := agentStateForTest(application, "agent-a")
	engine.mu.Lock()
	engine.outputs["agent-a"] = []string{"new process"}
	engine.outputReads["agent-a"] = 0
	engine.mu.Unlock()

	application.mu.RLock()
	started := make(chan error, 1)
	go func() { started <- application.StartSession(runID, "agent-a") }()
	// A writer waiting on the bridge's lock makes TryRLock fail. Nothing but
	// the output reset takes that lock for writing on a start's way.
	deadline := time.Now().Add(2 * time.Second)
	for application.mu.TryRLock() {
		application.mu.RUnlock()
		if time.Now().After(deadline) {
			application.mu.RUnlock()
			t.Fatal("the start never reached the output reset")
		}
		time.Sleep(time.Millisecond)
	}
	supervised, _ := run.sup.Agent("agent-a")
	shown := application.stateLocked()
	application.mu.RUnlock()
	if err := <-started; err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if supervised.Running || supervised.Status == "running" {
		t.Fatalf("the core showed the new process running before its predecessor's output was dropped: %#v", supervised)
	}
	if len(shown.Agents) != 1 || shown.Agents[0].Running || shown.Agents[0].Status != "starting" {
		t.Fatalf("shown during the start = %#v, want the agent still starting", shown.Agents)
	}
	after := agentStateForTest(application, "agent-a")
	if !after.Running || after.Status != "running" || after.Output != "new process" || after.Revision != before.Revision+2 {
		t.Fatalf("after the start = %#v, want the new process's output at revision %d", after, before.Revision+2)
	}
}

// TestARestartedAgentShowsNoneOfItsPredecessorsOutputWhenItsOwnCannotBeRead:
// the output is dropped when the new process is reported, not replaced by the
// refresh that follows, so a refresh that fails leaves it empty rather than
// showing the previous process's. The revision still moves past every
// snapshot of the previous process.
func TestARestartedAgentShowsNoneOfItsPredecessorsOutputWhenItsOwnCannotBeRead(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	engine.outputs["agent-a"] = []string{"previous process output"}
	application := newBridgeForTest(engine)
	runID := activeRunIDForTest(application)
	markAgentExitedForTest(application, "agent-a")
	before := agentStateForTest(application, "agent-a")
	if before.Output != "previous process output" {
		t.Fatalf("output before the start = %q", before.Output)
	}
	engine.mu.Lock()
	engine.outputErr = errors.New("fixture output unavailable")
	engine.mu.Unlock()

	if err := application.StartSession(runID, "agent-a"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	after := agentStateForTest(application, "agent-a")
	if !after.Running || after.Output != "" || after.Revision != before.Revision+1 {
		t.Fatalf("after the start = %#v, want no output at revision %d", after, before.Revision+1)
	}
}
