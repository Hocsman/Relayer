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
