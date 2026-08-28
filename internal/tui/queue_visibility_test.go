package tui

import (
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
)

// Only the agent being answered is named in the supervisor title, and only four
// agents are visible per page. An operator answering one prompt could not learn
// that others were queued behind it, possibly on a page they were not looking
// at, so a queue could build up unseen.
func TestSupervisorTitleShowsHowManyPromptsAreWaiting(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)

	// Nothing waiting: no count.
	if got := application.View(); strings.Contains(got, "EN ATTENTE") {
		t.Fatalf("an idle supervisor advertised a queue:\n%s", got)
	}

	first := automaticEvent("agent-a", "occurrence-a")
	first.Risk = adapters.RiskUnknown
	backend.setPending(first)
	application, command := updateModel(t, application, session.AdapterEvent{Event: first})
	application, _ = updateModel(t, application, executeCommand(t, command))

	// A single waiting prompt is already the one being answered and named.
	if got := application.View(); strings.Contains(got, "EN ATTENTE") {
		t.Fatalf("a single prompt was counted as a queue:\n%s", got)
	}

	second := automaticEvent("agent-b", "occurrence-b")
	second.Risk = adapters.RiskUnknown
	second.SessionID = "agent-b"
	second.AgentID = "agent-b"
	backend.setPending(second)
	application, command = updateModel(t, application, session.AdapterEvent{Event: second})
	if command != nil {
		application, _ = updateModel(t, application, executeCommand(t, command))
	}

	view := application.View()
	if !strings.Contains(view, "2 EN ATTENTE") {
		t.Fatalf("the queued prompt was invisible:\n%s", view)
	}
}

// A half-composed direct instruction is discarded when a prompt takes the
// shared input field. That is correct — a supervision request outranks a note
// to an agent — but it used to happen with no log line at all, so the operator
// saw their sentence vanish with nothing to explain it.
func TestPreemptedDirectInstructionIsAnnounced(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)

	// Compose a direct instruction on an idle agent.
	application.focus = FocusTarget{Kind: FocusAgent, AgentID: application.panes[0].sessionID}
	if command := application.beginLineInput(0); command != nil {
		application, _ = updateModel(t, application, executeCommand(t, command))
	}
	if application.lineInputTarget == "" {
		t.Fatal("the direct instruction composer did not open")
	}
	application.input.SetValue("deploy the staging branch")

	prompt := automaticEvent("agent-b", "occurrence-b")
	prompt.Risk = adapters.RiskUnknown
	prompt.SessionID = "agent-b"
	prompt.AgentID = "agent-b"
	backend.setPending(prompt)
	application, command := updateModel(t, application, session.AdapterEvent{Event: prompt})
	if command != nil {
		application, _ = updateModel(t, application, executeCommand(t, command))
	}

	if application.lineInputTarget != "" {
		t.Fatal("the composer survived a supervision request")
	}
	view := application.View()
	if !strings.Contains(view, "Consigne directe abandonnée") {
		t.Fatalf("the discarded instruction was not announced:\n%s", view)
	}
	// The text itself must never be logged.
	if strings.Contains(view, "deploy the staging branch") {
		t.Fatalf("the discarded instruction leaked into the log:\n%s", view)
	}
}
