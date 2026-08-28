package tui

import (
	"testing"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
)

func blockedOnPrompt(t *testing.T, backend *policyTestBackend, sink *tuiAuditSink) (*Model, adapters.Event) {
	t.Helper()
	application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)
	prompt := automaticEvent("agent-a", "occurrence-1")
	prompt.Risk = adapters.RiskUnknown
	backend.setPending(prompt)
	application, command := updateModel(t, application, session.AdapterEvent{Event: prompt})
	application, _ = updateModel(t, application, executeCommand(t, command))
	if !application.panes[0].blocked || application.inputTarget != "agent-a" {
		t.Fatal("the pane did not block on the prompt")
	}
	return application, prompt
}

// The adapters encode allow and deny, and the audit models a decision made by a
// person, but no operator surface could ask for either: every human answer went
// through the free-text field and was recorded as an ask. A refusal made by a
// human was therefore impossible to produce.
func TestOperatorCanExpressAllowAndDeny(t *testing.T) {
	for _, test := range []struct {
		name     string
		key      tea.KeyType
		decision adapters.Decision
		audited  audit.Decision
	}{
		{name: "allow", key: tea.KeyF2, decision: adapters.DecisionAllow, audited: audit.DecisionAllow},
		{name: "deny", key: tea.KeyF3, decision: adapters.DecisionDeny, audited: audit.DecisionDeny},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := newPolicyTestBackend()
			t.Cleanup(backend.cancel)
			sink := &tuiAuditSink{}
			application, prompt := blockedOnPrompt(t, backend, sink)

			application, command := updateModel(t, application, tea.KeyMsg{Type: test.key})
			if command == nil {
				t.Fatal("the key produced no delivery command")
			}
			application, _ = updateModel(t, application, executeCommand(t, command))

			calls := backend.automaticSnapshot()
			if len(calls) != 1 || calls[0].eventID != prompt.ID || calls[0].decision != test.decision {
				t.Fatalf("backend calls = %#v, want one %s for the pending occurrence", calls, test.decision)
			}
			if application.panes[0].blocked {
				t.Fatal("the pane stayed blocked after the decision was delivered")
			}

			var decision, delivery *audit.Entry
			entries := sink.entries(t)
			for index := range entries {
				switch entries[index].Kind {
				case audit.KindDecision:
					decision = &entries[index]
				case audit.KindDelivery:
					delivery = &entries[index]
				}
			}
			if decision == nil || decision.Decision != test.audited || decision.DecisionBy != audit.DecisionByHuman {
				t.Fatalf("decision record = %#v, want %s by a human", decision, test.audited)
			}
			if delivery == nil || delivery.Decision != test.audited ||
				delivery.DecisionBy != audit.DecisionByHuman || delivery.Outcome != audit.OutcomeSucceeded {
				t.Fatalf("delivery record = %#v, want %s by a human", delivery, test.audited)
			}
		})
	}
}

// Only the codex adapter encodes allow and deny today. An adapter that cannot
// represent the answer must leave the occurrence pending rather than have
// terminal bytes invented on the supervisor's behalf.
func TestUnsupportedSemanticDecisionKeepsThePromptPending(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application, prompt := blockedOnPrompt(t, backend, sink)
	backend.automaticErr = adapters.ErrDecisionUnsupported

	application, command := updateModel(t, application, tea.KeyMsg{Type: tea.KeyF3})
	if command == nil {
		t.Fatal("the key produced no delivery command")
	}
	application, _ = updateModel(t, application, executeCommand(t, command))

	if !application.panes[0].blocked || application.panes[0].prompt.ID != prompt.ID {
		t.Fatalf("the prompt was lost after an unsupported decision: blocked=%t prompt=%q",
			application.panes[0].blocked, application.panes[0].prompt.ID)
	}
	// The attempt is still auditable: it happened, and it failed.
	failed := false
	for _, entry := range sink.entries(t) {
		if entry.Kind == audit.KindDelivery && entry.Outcome == audit.OutcomeFailed &&
			entry.Decision == audit.DecisionDeny && entry.DecisionBy == audit.DecisionByHuman {
			failed = true
		}
	}
	if !failed {
		t.Fatalf("the refused attempt was not recorded: %#v", sink.entries(t))
	}
}

// The keys must do nothing when no prompt is waiting.
func TestSemanticDecisionKeysAreInertWithoutAPendingPrompt(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)

	for _, key := range []tea.KeyType{tea.KeyF2, tea.KeyF3} {
		if _, command := updateModel(t, application, tea.KeyMsg{Type: key}); command != nil {
			t.Fatalf("key %v produced a command with no prompt pending", key)
		}
	}
	if calls := backend.automaticSnapshot(); len(calls) != 0 {
		t.Fatalf("backend was called with no prompt pending: %#v", calls)
	}
	if sink.count() != 0 {
		t.Fatalf("audit records were written with no prompt pending: %#v", sink.entries(t))
	}
}
