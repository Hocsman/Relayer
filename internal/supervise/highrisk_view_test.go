package supervise_test

import (
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
)

// A high-risk prompt is not a secret: its view shows the bounded, redacted
// command and is not marked sensitive, so a front end answers it in an
// ordinary field under an honest label. Only the journal keeps the constant
// label: the sanitizer marks a high-risk entry sensitive there, as it always
// has.
func TestAHighRiskPromptShowsItsCommandAndKeepsItsJournalLabel(t *testing.T) {
	engine := newFakeEngine()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	event := promptEvent("agent-a", "prompt-risky")
	event.Type = adapters.EventPermission
	event.Risk = adapters.RiskHigh
	event.Summary = "Run command: npm test -- password=hunter2"
	sup.Handle(session.AdapterEvent{Event: event})

	pending := sup.State().Pending
	if len(pending) != 1 {
		t.Fatalf("pending = %v, want the prompt", pendingIDs(sup))
	}
	view := pending[0]
	if view.Sensitive {
		t.Fatal("a high-risk prompt is marked sensitive: it is shown, not masked")
	}
	if !strings.Contains(view.Summary, "npm test") {
		t.Fatalf("summary = %q, want the command shown", view.Summary)
	}
	if strings.Contains(view.Summary, "hunter2") {
		t.Fatalf("summary = %q, want the credential shape redacted", view.Summary)
	}

	entries := engine.auditSnapshot()
	journaled := 0
	for _, entry := range entries {
		if entry.EventID != "prompt-risky" {
			continue
		}
		journaled++
		kept := audit.SanitizeEntry(entry, audit.ModeDetailed)
		if !kept.Sensitive || kept.Summary != "sensitive_event" {
			t.Fatalf("journaled %#v: the journal does not keep its constant label", kept)
		}
	}
	if journaled == 0 {
		t.Fatal("the prompt was not journaled")
	}
}
