package supervise_test

import (
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
)

// finishedEntries returns the session_finished entries of the journal, by
// their reason.
func finishedEntries(engine *fakeEngine) []string {
	var reasons []string
	for _, entry := range engine.auditSnapshot() {
		if entry.Kind == audit.KindSessionFinished {
			reasons = append(reasons, entry.Reason)
		}
	}
	return reasons
}

// The exit of the process a Restart stops is read while the Restart is
// between its stop and its start, when nothing runs, and the runtime calls it
// current. The core journaled it, then applied it once the Restart had shown
// the replacement running: on Linux, every time. The agent was shown failed
// and not running while its new process ran, a session_finished followed the
// new process's session_started, every prompt the new process raised was
// dropped as a stopped agent's, and both a Stop and a Start were refused. An
// exit that arrives during a Restart now waits for it to end and is judged
// then, when the runtime sees the replacement running: it is stale.
func TestAnExitThatArrivesDuringARestartIsJudgedOnceItEnds(t *testing.T) {
	engine := newFakeEngine()
	replaced := atomic.Bool{}
	restartStarted := make(chan string, 1)
	restartRelease := make(chan struct{})
	auditStarted := make(chan struct{}, 4)
	auditRelease := make(chan struct{})
	engine.markExited = func(string) bool { return !replaced.Load() }
	engine.agentRestartStarted = restartStarted
	engine.agentRestartRelease = restartRelease
	// The exit's journal is slow, as an fsync is: the Restart ends first.
	engine.auditBlockKind = audit.KindSessionFinished
	engine.auditStarted = auditStarted
	engine.auditRelease = auditRelease
	sup, _ := newCoreForTest(t, engine, "agent-a")

	restarted := make(chan error, 1)
	go func() { restarted <- sup.RestartSession(testRunID, "agent-a") }()
	<-restartStarted
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, true)})
	}()
	select {
	case <-auditStarted:
	case <-exited:
	case <-time.After(2 * time.Second):
		t.Fatal("the exit was neither journaled nor set aside")
	}
	replaced.Store(true)
	close(restartRelease)
	waitFor(t, 2*time.Second, "the restart to show the replacement running", func() bool {
		agent := agentOf(t, sup, "agent-a")
		return agent.Running && agent.Status == "running"
	})
	close(auditRelease)
	if err := <-restarted; err != nil {
		t.Fatalf("RestartSession: %v", err)
	}
	<-exited

	if agent := agentOf(t, sup, "agent-a"); !agent.Running || agent.Status != "running" {
		t.Fatalf("after its restart the agent is shown %s (running=%v) while its replacement runs", agent.Status, agent.Running)
	}
	if reasons := finishedEntries(engine); len(reasons) != 1 || reasons[0] != audit.ReasonProcessExitStale {
		t.Fatalf("the stopped process's exit is journaled as %v, want one %s", reasons, audit.ReasonProcessExitStale)
	}
	// The replacement's prompts are taken in, as a running agent's are.
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "after-restart")})
	if ids := pendingIDs(sup); len(ids) != 1 || ids[0] != "after-restart" {
		t.Fatalf("the replacement's prompt was not taken in: pending %v", ids)
	}
}

// A replacement that dies while its Start is still in progress is shown
// exited once the Start ends: nothing runs when its exit is judged. Its exit
// was applied at once, and the Start's end then showed the agent running.
func TestAReplacementThatDiesDuringItsStartIsShownExited(t *testing.T) {
	engine := newFakeEngine()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
	startStarted := make(chan string, 1)
	startRelease := make(chan struct{})
	engine.set(func(f *fakeEngine) {
		f.agentStartStarted = startStarted
		f.agentStartRelease = startRelease
	})

	started := make(chan error, 1)
	go func() { started <- sup.StartSession(testRunID, "agent-a") }()
	<-startStarted
	died := exitEvent("agent-a", 3, true)
	died.ID = "exit-of-the-replacement"
	sup.Handle(session.AdapterEvent{Event: died})
	close(startRelease)
	if err := <-started; err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if agent := agentOf(t, sup, "agent-a"); agent.Running || agent.Status != "failed" || agent.ExitCode == nil || *agent.ExitCode != 3 {
		t.Fatalf("the replacement that died is shown %s (running=%v, exit %v), want failed with its exit code", agent.Status, agent.Running, agent.ExitCode)
	}
	if reasons := finishedEntries(engine); len(reasons) != 2 || reasons[1] != "process_exit" {
		t.Fatalf("the exits are journaled as %v, want the replacement's current", reasons)
	}
}

// A Restart that fails still judges the exits that arrived while it ran: the
// stopped process's exit is current when nothing replaced it, and the agent is
// shown down, which a Start then accepts.
func TestAFailedRestartStillJudgesTheExitsThatArrivedMeanwhile(t *testing.T) {
	engine := newFakeEngine()
	restartStarted := make(chan string, 1)
	restartRelease := make(chan struct{})
	engine.agentRestartStarted = restartStarted
	engine.agentRestartRelease = restartRelease
	engine.agentRestartErr = errors.New("injected: the replacement could not be started")
	sup, _ := newCoreForTest(t, engine, "agent-a")

	restarted := make(chan error, 1)
	go func() { restarted <- sup.RestartSession(testRunID, "agent-a") }()
	<-restartStarted
	sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
	close(restartRelease)
	if err := <-restarted; err == nil {
		t.Fatal("the restart was meant to fail")
	}

	if agent := agentOf(t, sup, "agent-a"); agent.Running {
		t.Fatalf("after a failed restart whose process exited the agent is shown %s and running", agent.Status)
	}
	if reasons := finishedEntries(engine); len(reasons) != 1 || reasons[0] != "process_exit" {
		t.Fatalf("the exit is journaled as %v, want it current", reasons)
	}
	if err := sup.StartSession(testRunID, "agent-a"); err != nil {
		t.Fatalf("a Start after the failed restart: %v", err)
	}
}
