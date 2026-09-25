package supervise_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// These tests pin FreezeSession: a front end's word that it no longer knows
// what a session's terminal holds, such as a screen it could not read back
// after a native attach. Keeping the hand held did not stop a person's answer,
// and the rule that did would have lived in one front end.

// terminalStateUncertain is the failure a front end's freeze shows.
const terminalStateUncertain = "The terminal state is uncertain. The session is frozen until its agent is started again."

// A frozen session takes nothing that writes to it: no answer, chosen or typed,
// no line, no keystrokes, and no automatic answer. A Stop is taken: it writes
// nothing, and it is how the operator acts on the freeze.
func TestAFrozenSessionTakesNothingButAStop(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
	sup, _ := newCoreForTest(t, engine, "agent-a")
	if err := sup.FreezeSession(testRunID, "agent-a"); err != nil {
		t.Fatalf("FreezeSession: %v", err)
	}
	if agent := agentOf(t, sup, "agent-a"); !agent.InputFrozen || !agent.Running {
		t.Fatalf("the agent after the freeze = %#v, want it running and frozen", agent)
	}

	// Handle schedules the policy's answer before it returns: a prompt still
	// pending then is one the policy will not answer.
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	if shown := viewOf(sup, "prompt-1"); shown == nil || shown.DeliveryStatus != "pending" {
		t.Fatalf("the prompt on a frozen session is shown as %#v, want it pending and unanswered", shown)
	}
	if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", alice); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("a typed answer = %v, want ErrDeliveryUncertain", err)
	}
	if err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", alice); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("a chosen answer = %v, want ErrDeliveryUncertain", err)
	}
	if err := sup.SubmitLine(testRunID, "agent-a", "hello", alice); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("a line = %v, want ErrDeliveryUncertain", err)
	}
	sup.SetHolder("agent-a", "conn-1")
	if release, err := sup.Admit("agent-a", "conn-1"); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		if release != nil {
			release()
		}
		t.Fatalf("the holder's keystrokes = %v, want ErrDeliveryUncertain", err)
	}
	sup.SetHolder("agent-a", "")

	if err := sup.StopSession(testRunID, "agent-a"); err != nil {
		t.Fatalf("a Stop of a frozen session = %v, want it taken", err)
	}
	if stops, _, _ := engine.lifecycleCalls(); stops != 1 {
		t.Fatalf("stops reaching the runtime = %d, want 1", stops)
	}
	sup.BeginDrain()
	sup.Wait()
	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("a frozen session was answered: %#v", calls)
	}
	if lines := engine.lineSnapshot(); len(lines) != 0 {
		t.Fatalf("a frozen session received lines: %#v", lines)
	}
	for _, entry := range engine.auditSnapshot() {
		if entry.Kind == audit.KindDecision || entry.Kind == audit.KindDelivery || entry.Kind == audit.KindOperatorInput {
			t.Fatalf("a refused write was journaled: %#v", entry)
		}
	}
}

// The freeze belongs to the process whose terminal the front end could not
// vouch for: a new process ends it, whether a Restart starts it or a Start
// after a Stop. The session is then written to as any other.
func TestAFrontEndFreezeEndsWithARestart(t *testing.T) {
	for _, test := range []struct {
		name    string
		replace func(sup *supervise.Supervisor) error
	}{
		{name: "a restart", replace: func(sup *supervise.Supervisor) error {
			return sup.RestartSession(testRunID, "agent-a")
		}},
		{name: "a stop, then a start", replace: func(sup *supervise.Supervisor) error {
			if err := sup.StopSession(testRunID, "agent-a"); err != nil {
				return err
			}
			return sup.StartSession(testRunID, "agent-a")
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluation = automaticAllow()
			sup, _ := newCoreForTest(t, engine, "agent-a")
			if err := sup.FreezeSession(testRunID, "agent-a"); err != nil {
				t.Fatalf("FreezeSession: %v", err)
			}
			if err := test.replace(sup); err != nil {
				t.Fatalf("replacing the frozen process: %v", err)
			}
			if agent := agentOf(t, sup, "agent-a"); agent.InputFrozen || !agent.Running {
				t.Fatalf("the agent once replaced = %#v, want it running and writable", agent)
			}

			sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
			waitFor(t, 2*time.Second, "the replacement's prompt to be answered", func() bool {
				deliveries := engine.auditFor(audit.KindDelivery, "prompt-1")
				return len(deliveries) == 1 && deliveries[0].Outcome == audit.OutcomeApplied
			})
			waitForTheSessionToBeFree(t, sup, "agent-a")
		})
	}
}

// An agent that is not running has no terminal to vouch for: freezing it does
// nothing, shows nothing and leaves its next process writable.
func TestFreezingAStoppedAgentDoesNothing(t *testing.T) {
	for _, test := range []struct {
		name string
		end  func(t *testing.T, sup *supervise.Supervisor)
	}{
		{name: "stopped", end: func(t *testing.T, sup *supervise.Supervisor) {
			if err := sup.StopSession(testRunID, "agent-a"); err != nil {
				t.Fatalf("StopSession: %v", err)
			}
		}},
		{name: "exited", end: func(t *testing.T, sup *supervise.Supervisor) {
			sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			sup, sink := newCoreForTest(t, newFakeEngine(), "agent-a")
			test.end(t, sup)
			if agent := agentOf(t, sup, "agent-a"); agent.Running {
				t.Fatalf("the agent is still running: %#v", agent)
			}
			sink.reset()

			if err := sup.FreezeSession(testRunID, "agent-a"); err != nil {
				t.Fatalf("freezing an agent that is not running = %v, want nothing done", err)
			}
			if agent := agentOf(t, sup, "agent-a"); agent.InputFrozen {
				t.Fatalf("the agent was frozen: %#v", agent)
			}
			if calls := sink.snapshot(); len(calls) != 0 {
				t.Fatalf("freezing an agent that is not running showed %v", trace(calls))
			}
			if err := sup.StartSession(testRunID, "agent-a"); err != nil {
				t.Fatalf("StartSession: %v", err)
			}
			if err := sup.SubmitLine(testRunID, "agent-a", "hello", desktop); err != nil {
				t.Fatalf("a line to the next process = %v", err)
			}
		})
	}
}

// A freeze is an operation on one run like any other: refused once the run
// drains, since a drain freezes nothing, refused for another run, and refused
// for an agent the run does not have. None of them freezes anything.
func TestFreezeSessionIsRefusedOnAStaleRunAndDuringADrain(t *testing.T) {
	sup, sink := newCoreForTest(t, newFakeEngine(), "agent-a")
	for _, test := range []struct {
		name      string
		runID     string
		sessionID string
		want      error
	}{
		{name: "another run", runID: "run-other", sessionID: "agent-a", want: supervise.ErrRunStale},
		{name: "no run", runID: " ", sessionID: "agent-a", want: supervise.ErrRunStale},
		{name: "an unknown agent", runID: testRunID, sessionID: "agent-z", want: supervise.ErrAgentUnknown},
	} {
		if err := sup.FreezeSession(test.runID, test.sessionID); !errors.Is(err, test.want) {
			t.Errorf("freezing for %s = %v, want %v", test.name, err, test.want)
		}
	}
	sup.BeginDrain()
	if err := sup.FreezeSession(testRunID, "agent-a"); !errors.Is(err, supervise.ErrRuntimeStopped) {
		t.Errorf("freezing during a drain = %v, want ErrRuntimeStopped", err)
	}
	if agent := agentOf(t, sup, "agent-a"); agent.InputFrozen {
		t.Fatalf("a refused freeze froze the agent: %#v", agent)
	}
	if calls := sink.snapshot(); len(calls) != 0 {
		t.Fatalf("a refused freeze showed %v", trace(calls))
	}
}

// A freeze writes nothing, so nothing is journaled. It is shown once, by a
// fixed code and message under the agent's own ID, however the caller spelt
// it; the prompts it leaves are not shown again, and a second freeze of the
// same session shows nothing more.
func TestFreezeSessionJournalsNothingAndShowsOneSafeError(t *testing.T) {
	engine := newFakeEngine()
	sup, sink := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	before := viewOf(sup, "prompt-1")
	journaled := len(engine.auditSnapshot())
	sink.reset()

	for range 2 {
		if err := sup.FreezeSession(testRunID, " AGENT-A "); err != nil {
			t.Fatalf("FreezeSession: %v", err)
		}
	}
	if entries := engine.auditSnapshot(); len(entries) != journaled {
		t.Fatalf("the freeze was journaled: %#v", entries[journaled:])
	}
	calls := sink.snapshot()
	if len(calls) != 1 || calls[0].kind != "error" {
		t.Fatalf("the freeze showed %v, want one failure", trace(calls))
	}
	failure := calls[0].failure
	if failure.Timestamp == "" {
		t.Fatalf("the failure has no time: %#v", failure)
	}
	failure.Timestamp = ""
	want := supervise.SafeError{RunID: testRunID, Code: "terminal_state_uncertain", Message: terminalStateUncertain, SessionID: "agent-a"}
	if failure != want {
		t.Fatalf("the failure shown = %#v, want %#v", failure, want)
	}
	if after := viewOf(sup, "prompt-1"); after == nil || !reflect.DeepEqual(*after, *before) {
		t.Fatalf("the pending prompt changed with the freeze: %#v, was %#v", after, before)
	}
}
