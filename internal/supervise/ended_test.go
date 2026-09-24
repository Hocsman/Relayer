package supervise_test

import (
	"errors"
	"reflect"
	"testing"

	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// A front end is told when a session's process ended, and the web gateway
// announces the process's finished recording on it. The core reported only
// the status, and the gateway announced a recording only when Relayer lost a
// tmux session, never when a process exited. The report follows the status
// that shows the agent exited, so a front end reading the core then sees it
// exited. The exit of a process a replacement already superseded reports
// nothing: the session's process is the replacement, whose recording goes on.
// A second copy of an exit reports nothing more.
func TestTheEndOfASessionsProcessIsReported(t *testing.T) {
	t.Run("an exit", func(t *testing.T) {
		sup, sink := newCoreForTest(t, newFakeEngine(), "agent-a")
		var atReport []supervise.Agent
		sink.mu.Lock()
		sink.probe = func() {
			calls := sink.snapshot()
			if last := calls[len(calls)-1]; last.kind == "lifecycle" && last.phase == supervise.PhaseEnded {
				agent, _ := sup.Agent("agent-a")
				atReport = append(atReport, agent)
			}
		}
		sink.mu.Unlock()

		exit := exitEvent("agent-a", 0, false)
		sup.Handle(session.AdapterEvent{Event: exit})
		sup.Handle(session.AdapterEvent{Event: exit})

		if got, want := trace(sink.snapshot()), []string{"refresh", "status:exited", "lifecycle:ended"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("sink = %v, want %v", got, want)
		}
		if len(atReport) != 1 || atReport[0].Running || atReport[0].Status != "exited" {
			t.Fatalf("the agent when its end was reported = %#v, want it exited, reported once", atReport)
		}
		for _, call := range sink.snapshot() {
			if call.kind == "lifecycle" && call.sessionID != "agent-a" {
				t.Fatalf("the end was reported for %q, want agent-a", call.sessionID)
			}
		}
	})
	t.Run("the exit of a replaced process", func(t *testing.T) {
		engine := newFakeEngine()
		engine.staleExits = true
		sup, sink := newCoreForTest(t, engine, "agent-a")
		sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 1, true)})
		for _, call := range sink.snapshot() {
			if call.kind == "lifecycle" {
				t.Fatalf("the exit of a replaced process reported %v", trace(sink.snapshot()))
			}
		}
	})
	t.Run("a lost terminal", func(t *testing.T) {
		sup, sink := newCoreForTest(t, newFakeEngine(), "agent-a")
		sup.Handle(session.Exited{SessionID: "agent-a", Err: errors.New("the pane is gone")})
		if got, want := trace(sink.snapshot()), []string{"status:failed", "lifecycle:ended"}; !reflect.DeepEqual(got, want) {
			t.Fatalf("sink = %v, want %v", got, want)
		}
	})
}
