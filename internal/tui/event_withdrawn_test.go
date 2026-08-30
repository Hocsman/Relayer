package tui

import (
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
)

// A snapshot that no longer shows the prompt withdraws the pending occurrence.
// That is legitimate — the operator may have answered directly inside tmux —
// but it opens the supervision gate without a decision being delivered, and it
// used to leave no trace: the pane simply stopped being blocked.
//
// The audit has to be able to tell "answered" from "stopped being asked".
func TestWithdrawnOccurrenceIsAudited(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)

	blocked := automaticEvent("agent-a", "occurrence-1")
	blocked.Risk = adapters.RiskUnknown
	blocked.Match = "Overwrite file? [Y/n]"
	backend.setPending(blocked)
	application, command := updateModel(t, application, session.AdapterEvent{Event: blocked})
	application, _ = updateModel(t, application, executeCommand(t, command))
	if !application.panes[0].blocked {
		t.Fatal("the pane did not block on the prompt")
	}

	// The replayed screen no longer shows it.
	application.reconcileEvent("agent-a", nil)
	if application.panes[0].blocked {
		t.Fatal("the pane stayed blocked after the occurrence was withdrawn")
	}

	entries := sink.entries(t)
	var withdrawn *audit.Entry
	for index := range entries {
		if entries[index].Kind == audit.KindEventWithdrawn {
			withdrawn = &entries[index]
		}
	}
	if withdrawn == nil {
		t.Fatalf("the withdrawal left no audit record: %#v", entries)
	}
	if withdrawn.EventID != blocked.ID {
		t.Fatalf("record names occurrence %q, want %q", withdrawn.EventID, blocked.ID)
	}
	if withdrawn.SessionID != "agent-a" || withdrawn.Outcome != audit.OutcomeCancelled {
		t.Fatalf("withdrawal record = %#v", *withdrawn)
	}
	// A decision was never delivered, so the record must not look like one.
	if withdrawn.Decision != "" {
		t.Fatalf("withdrawal recorded a decision: %#v", *withdrawn)
	}
	// The matched terminal text must never reach the journal.
	if strings.Contains(sink.raw(), "Overwrite file?") {
		t.Fatalf("the matched text leaked into the audit journal: %s", sink.raw())
	}
}

// Replacement by a different occurrence is also a withdrawal of the first.
func TestReplacedOccurrenceIsAuditedAsWithdrawn(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)

	first := automaticEvent("agent-a", "occurrence-1")
	first.Risk = adapters.RiskUnknown
	backend.setPending(first)
	application, command := updateModel(t, application, session.AdapterEvent{Event: first})
	application, _ = updateModel(t, application, executeCommand(t, command))
	if !application.panes[0].blocked {
		t.Fatal("the pane did not block on the first prompt")
	}

	second := first
	second.ID = "occurrence-2"
	second.Signature = "sig-2"
	application.reconcileEvent("agent-a", &second)

	withdrawnIDs := []string{}
	for _, entry := range sink.entries(t) {
		if entry.Kind == audit.KindEventWithdrawn {
			withdrawnIDs = append(withdrawnIDs, entry.EventID)
		}
	}
	if len(withdrawnIDs) != 1 || withdrawnIDs[0] != first.ID {
		t.Fatalf("withdrawal records = %v, want exactly the replaced occurrence", withdrawnIDs)
	}
}

// Reconciling a pane that was not blocked must not invent a record.
func TestUnblockedPaneRecordsNoWithdrawal(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)

	application.reconcileEvent("agent-a", nil)
	for _, entry := range sink.entries(t) {
		if entry.Kind == audit.KindEventWithdrawn {
			t.Fatalf("a withdrawal was recorded for a pane that was never blocked: %#v", entry)
		}
	}
}

// The native PTY backend has no snapshot to reconcile from, so the core tells
// the interface directly when the agent takes its question back. The pane must
// unblock and the withdrawal must be audited as what it is.
func TestAgentWithdrawnOccurrenceUnblocksAndIsAudited(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)

	blocked := automaticEvent("agent-a", "occurrence-1")
	blocked.Risk = adapters.RiskUnknown
	blocked.Match = "Do you want to continue? [y/n]"
	backend.setPending(blocked)
	application, command := updateModel(t, application, session.AdapterEvent{Event: blocked})
	application, _ = updateModel(t, application, executeCommand(t, command))
	if !application.panes[0].blocked {
		t.Fatal("the pane did not block on the prompt")
	}

	application, _ = updateModel(t, application, session.AdapterEventWithdrawn{Event: blocked})
	if application.panes[0].blocked {
		t.Fatal("the pane stayed blocked after the agent withdrew its question")
	}
	if len(application.pending) != 0 {
		t.Fatalf("the action queue still holds the withdrawn occurrence: %v", application.pending)
	}

	var withdrawn *audit.Entry
	entries := sink.entries(t)
	for index := range entries {
		if entries[index].Kind == audit.KindEventWithdrawn {
			withdrawn = &entries[index]
		}
	}
	if withdrawn == nil {
		t.Fatalf("the withdrawal left no audit record: %#v", entries)
	}
	// A resync is not what happened, and an audit that says so is wrong.
	if withdrawn.Reason != "agent_withdrew_occurrence" {
		t.Fatalf("withdrawal reason = %q, want agent_withdrew_occurrence", withdrawn.Reason)
	}
	if withdrawn.Decision != "" {
		t.Fatalf("withdrawal recorded a decision: %#v", *withdrawn)
	}
	if strings.Contains(sink.raw(), "Do you want to continue?") {
		t.Fatalf("the matched text leaked into the audit journal: %s", sink.raw())
	}
}

// A withdrawal for an occurrence that is no longer the one on offer changes
// nothing: there is no question to stop asking, and inventing a record would
// describe a gate that never opened.
func TestAWithdrawalForAnotherOccurrenceIsIgnored(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)

	blocked := automaticEvent("agent-a", "occurrence-1")
	blocked.Risk = adapters.RiskUnknown
	backend.setPending(blocked)
	application, command := updateModel(t, application, session.AdapterEvent{Event: blocked})
	application, _ = updateModel(t, application, executeCommand(t, command))

	stale := blocked
	stale.ID = "occurrence-0"
	application, _ = updateModel(t, application, session.AdapterEventWithdrawn{Event: stale})
	if !application.panes[0].blocked {
		t.Fatal("a stale withdrawal unblocked the pane")
	}
	for _, entry := range sink.entries(t) {
		if entry.Kind == audit.KindEventWithdrawn {
			t.Fatalf("a stale withdrawal was recorded: %#v", entry)
		}
	}
}
