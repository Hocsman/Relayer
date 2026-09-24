package supervise_test

import (
	"errors"
	"testing"

	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
	"github.com/Hocsman/Relayer/internal/terminal"
)

// Every frame about a session names it by its agent's own ID, however the
// caller spelt it: the core finds a session whatever its case, and a front end
// matches a frame to its agent by the exact ID. A failed Start, Restart or
// Stop, and a refused line, went out under the caller's spelling while
// "starting" and "stopping" went out under the agent's own, so a client that
// asked for a start as "AGENT-A" showed the agent starting for good, its Start
// button hidden, once the start had failed.
func TestAFailedOperationIsShownUnderTheAgentsOwnID(t *testing.T) {
	for _, test := range []struct {
		name string
		// fail makes the operation fail on agent-a, asked as AGENT-A.
		fail func(t *testing.T, engine *fakeEngine, sup *supervise.Supervisor) error
	}{
		{name: "a start", fail: func(t *testing.T, engine *fakeEngine, sup *supervise.Supervisor) error {
			sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
			engine.set(func(f *fakeEngine) { f.agentStartErr = errors.New("injected: the process could not be started") })
			return sup.StartSession(testRunID, "AGENT-A")
		}},
		{name: "a restart", fail: func(t *testing.T, engine *fakeEngine, sup *supervise.Supervisor) error {
			engine.set(func(f *fakeEngine) { f.agentRestartErr = errors.New("injected: the process could not be restarted") })
			return sup.RestartSession(testRunID, "AGENT-A")
		}},
		{name: "a stop", fail: func(t *testing.T, engine *fakeEngine, sup *supervise.Supervisor) error {
			engine.set(func(f *fakeEngine) { f.stopErr = errors.New("injected: the process could not be stopped") })
			return sup.StopSession(testRunID, "AGENT-A")
		}},
		{name: "a line", fail: func(t *testing.T, engine *fakeEngine, sup *supervise.Supervisor) error {
			engine.set(func(f *fakeEngine) { f.lineErr = terminal.ErrInvalidLine })
			return sup.SubmitLine(testRunID, "AGENT-A", "hello", alice)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			sup, sink := newCoreForTest(t, engine, "agent-a")
			sink.reset()
			if err := test.fail(t, engine, sup); err == nil {
				t.Fatal("the operation was meant to fail")
			}
			shown := 0
			for _, call := range sink.snapshot() {
				if call.kind != "status" && call.kind != "error" {
					continue
				}
				shown++
				if call.sessionID != "agent-a" {
					t.Errorf("%s shown under %q, want the agent's own ID agent-a", trace([]sinkCall{call}), call.sessionID)
				}
			}
			if shown == 0 {
				t.Fatal("the failure showed no status and no error")
			}
		})
	}
}

// One agent can still be stopped once the journal has failed: nothing more is
// written to any agent then, and stopping one is how an operator acts on it.
// The journal's failure closed the gate a Stop went through, and the Stop was
// refused as if the run were stopping, with a message that said so; only
// stopping the whole run was left. A drain still waits for the Stop.
func TestAnAgentCanBeStoppedOnceTheJournalHasFailed(t *testing.T) {
	engine := newFakeEngine()
	sup, _ := newCoreForTest(t, engine, "agent-a", "agent-b")
	engine.set(func(f *fakeEngine) { f.auditFailAt = f.auditCalls + 1 })
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	if !sup.State().AuditFailed {
		t.Fatal("the journal's failure did not freeze the run")
	}

	if err := sup.StopSession(testRunID, "agent-b"); err != nil {
		t.Fatalf("a stop once the journal has failed = %v", err)
	}
	if stops, _, _ := engine.lifecycleCalls(); stops != 1 {
		t.Fatalf("%d stops reached the runtime, want the one asked for", stops)
	}
	if agent := agentOf(t, sup, "agent-b"); agent.Running {
		t.Fatalf("the stopped agent is shown %#v", agent)
	}
	// Nothing else is taken: an answer and a start are still refused.
	if err := sup.StartSession(testRunID, "agent-b"); !errors.Is(err, supervise.ErrRuntimeStopped) && !errors.Is(err, supervise.ErrAuditUnavailable) {
		t.Fatalf("a start once the journal has failed = %v", err)
	}
	sup.BeginDrain()
	if err := sup.StopSession(testRunID, "agent-a"); !errors.Is(err, supervise.ErrRuntimeStopped) {
		t.Fatalf("a stop during the drain = %v, want ErrRuntimeStopped", err)
	}
}

// A journal that fails shows every prompt still pending failed, with the
// reason, as well as the run's status: a client then offers no answer to any
// of them. The prompts were marked failed only in the state, and every client
// went on offering answers the core refused one by one.
func TestAFailedJournalShowsEveryPendingPromptFailed(t *testing.T) {
	engine := newFakeEngine()
	sup, sink := newCoreForTest(t, engine, "agent-a", "agent-b")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-b", "prompt-2")})
	sink.reset()
	engine.set(func(f *fakeEngine) { f.auditFailAt = f.auditCalls + 1 })
	third := promptEvent("agent-a", "prompt-3")
	third.Sequence = 3
	sup.Handle(session.AdapterEvent{Event: third})

	failed := map[string]bool{}
	for _, call := range sink.snapshot() {
		if call.kind == "prompt" && call.view.DeliveryStatus == "failed" && call.view.Evaluation.Reason == "audit_unavailable" {
			failed[call.view.ID] = true
		}
	}
	if !failed["prompt-1"] || !failed["prompt-2"] {
		t.Fatalf("the journal's failure showed %v, want every pending prompt failed", trace(sink.snapshot()))
	}
}
