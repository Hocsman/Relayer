package supervise_test

import (
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
)

// These tests pin the reason a withdrawal is journaled with. The agent takes
// a question back by its own output; a backend's resynchronisation, after a
// native attach, finds a question gone that somebody most likely answered
// inside the terminal. The core journaled both as the agent's; only the TUI,
// which journaled resyncs itself, told them apart.

// withdrawnEntries returns the event_withdrawn entries of one prompt.
func withdrawnEntries(engine *fakeEngine, eventID string) []audit.Entry {
	return engine.auditFor(audit.KindEventWithdrawn, eventID)
}

// A pending prompt a resync no longer finds is journaled withdrawn for the
// reason resync, and shown gone as any withdrawn prompt is.
func TestAResyncWithdrawalIsJournaledAsResync(t *testing.T) {
	engine := newFakeEngine()
	sup, sink := newCoreForTest(t, engine, "agent-a")
	prompt := promptEvent("agent-a", "prompt-1")
	sup.Handle(session.AdapterEvent{Event: prompt})
	sink.reset()

	sup.Handle(session.AdapterEventWithdrawn{Event: prompt, Reason: session.WithdrawnByResync})
	withdrawn := withdrawnEntries(engine, "prompt-1")
	if len(withdrawn) != 1 {
		t.Fatalf("event_withdrawn entries = %#v, want one", withdrawn)
	}
	if entry := withdrawn[0]; entry.Reason != "resync" || entry.Outcome != audit.OutcomeCancelled ||
		entry.DecisionBy != audit.DecisionBySystem || entry.SessionID != "agent-a" {
		t.Fatalf("the withdrawal is journaled as %#v, want the resync's", entry)
	}
	if ids := pendingIDs(sup); len(ids) != 0 {
		t.Fatalf("pending after the withdrawal = %v", ids)
	}
	if got, want := trace(sink.snapshot()), []string{"prompt:delivered", "status:running", "refresh"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("sink = %v, want %v", got, want)
	}
}

// A withdrawal that arrives while its prompt is still being taken in leaves a
// tombstone, and the withdrawal the tombstone journals, after the entries that
// took the prompt in, keeps the reason it came with. A second withdrawal of
// the same prompt meanwhile changes neither the reason nor the one entry.
func TestATombstoneKeepsItsWithdrawalsReason(t *testing.T) {
	engine := newFakeEngine()
	journalStarted := make(chan struct{}, 1)
	journalRelease := make(chan struct{})
	releaseJournal := releaser(t, journalRelease)
	engine.auditBlockKind = audit.KindEventDetected
	engine.auditBlockEventID = "prompt-1"
	engine.auditStarted = journalStarted
	engine.auditRelease = journalRelease
	sup, _ := newCoreForTest(t, engine, "agent-a")
	prompt := promptEvent("agent-a", "prompt-1")

	handled := make(chan struct{})
	go func() {
		sup.Handle(session.AdapterEvent{Event: prompt})
		close(handled)
	}()
	select {
	case <-journalStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the prompt's detection was never journaled")
	}
	sup.Handle(session.AdapterEventWithdrawn{Event: prompt, Reason: session.WithdrawnByResync})
	sup.Handle(session.AdapterEventWithdrawn{Event: prompt})
	releaseJournal()
	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("the prompt was never taken in")
	}

	var journal []string
	for _, entry := range engine.auditSnapshot() {
		if entry.EventID == "prompt-1" {
			journal = append(journal, string(entry.Kind)+":"+entry.Reason)
		}
	}
	want := []string{"event_detected:event_detected", "policy_evaluated:default_action", "event_withdrawn:resync"}
	if !reflect.DeepEqual(journal, want) {
		t.Fatalf("the prompt's journal = %v, want %v", journal, want)
	}
	if ids := pendingIDs(sup); len(ids) != 0 {
		t.Fatalf("pending = %v, want the withdrawn prompt gone", ids)
	}
}

// Only a backend's resync reason is its own. Any other reason, an empty one
// included, is the agent's withdrawal, and a backend's text never reaches the
// journal as it is.
func TestAnUnknownWithdrawalReasonIsTheAgents(t *testing.T) {
	for _, reason := range []string{"", "answered_in_tmux", "resync\nforged entry"} {
		t.Run(strconv.Quote(reason), func(t *testing.T) {
			engine := newFakeEngine()
			sup, _ := newCoreForTest(t, engine, "agent-a")
			prompt := promptEvent("agent-a", "prompt-1")
			sup.Handle(session.AdapterEvent{Event: prompt})
			sup.Handle(session.AdapterEventWithdrawn{Event: prompt, Reason: reason})

			withdrawn := withdrawnEntries(engine, "prompt-1")
			if len(withdrawn) != 1 || withdrawn[0].Reason != "agent_withdrew_occurrence" {
				t.Fatalf("the withdrawal is journaled as %#v, want the agent's", withdrawn)
			}
			if ids := pendingIDs(sup); len(ids) != 0 {
				t.Fatalf("pending after the withdrawal = %v", ids)
			}
		})
	}
}
