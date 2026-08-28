package tui

import (
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
)

// Pressing Enter on an empty field must not answer the prompt.
//
// The generic adapter encodes a manual decision as the typed text plus a
// carriage return, so an empty submission delivers a bare carriage return —
// whatever the prompt treats as its default. Combined with the supervisor field
// taking focus by itself when a prompt arrives, a reflex keystroke could answer
// a prompt the operator had not read, and it was recorded as a human decision.
func TestEmptySubmitDoesNotAnswerThePrompt(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)

	prompt := automaticEvent("agent-a", "occurrence-1")
	prompt.Risk = adapters.RiskUnknown
	backend.setPending(prompt)
	application, command := updateModel(t, application, session.AdapterEvent{Event: prompt})
	application, _ = updateModel(t, application, executeCommand(t, command))
	if !application.panes[0].blocked || application.inputTarget != "agent-a" {
		t.Fatal("the pane did not block and focus the supervisor field")
	}
	before := sink.count()

	for _, empty := range []string{"", "   ", "\t"} {
		application.input.SetValue(empty)
		if cmd := application.submitInput(); cmd != nil {
			t.Fatalf("empty submission %q produced a delivery command", empty)
		}
		// The prompt is still waiting for a real answer.
		if !application.panes[0].blocked || application.inputTarget != "agent-a" {
			t.Fatalf("empty submission %q cleared the pending prompt", empty)
		}
		if sink.count() != before {
			t.Fatalf("empty submission %q was recorded as a decision", empty)
		}
	}

	// A typed answer still works and is recorded.
	application.input.SetValue("y")
	if cmd := application.submitInput(); cmd == nil {
		t.Fatal("a typed answer produced no delivery command")
	}
	if application.panes[0].blocked {
		t.Fatal("the pane stayed blocked after a real answer")
	}
	recorded := false
	for _, entry := range sink.entries(t) {
		if entry.Kind == audit.KindDecision && entry.DecisionBy == audit.DecisionByHuman {
			recorded = true
		}
	}
	if !recorded {
		t.Fatal("the typed answer was not recorded as a human decision")
	}
}
