package tui

import (
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func TestAdapterEventDeduplicatesRepeatedLiveOccurrenceID(t *testing.T) {
	application, _, _ := newModelHarness(t)
	event := testAdapterEvent("agent-a", "confirmation", "synthetic confirmation", false)
	application, _ = updateModel(t, application, event)
	logsAfterFirst := len(application.logs)
	if !application.panes[0].blocked || application.panes[0].prompt.ID != event.Event.ID ||
		application.inputTarget != "agent-a" || len(application.pending) != 1 {
		t.Fatalf("first event state = blocked %t prompt %q target %q pending %#v",
			application.panes[0].blocked,
			application.panes[0].prompt.ID,
			application.inputTarget,
			application.pending,
		)
	}

	application, _ = updateModel(t, application, event)
	if len(application.logs) != logsAfterFirst || len(application.pending) != 1 ||
		application.panes[0].prompt.ID != event.Event.ID || application.inputTarget != "agent-a" {
		t.Fatalf("duplicate live occurrence changed state: logs %d/%d pending %#v prompt %q target %q",
			len(application.logs),
			logsAfterFirst,
			application.pending,
			application.panes[0].prompt.ID,
			application.inputTarget,
		)
	}
}

func TestAdapterEventAcceptsNewOccurrenceWithSameSignatureAndIgnoresDelayedResolvedEvent(t *testing.T) {
	application, _, _ := newModelHarness(t)
	first := testAdapterEvent("agent-a", "confirmation", "first occurrence", false)
	application, _ = updateModel(t, application, first)
	application.input.SetValue("Y")
	application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	application, _ = updateModel(t, application, executeCommand(t, delivery))
	if !application.eventResolved(first.Event.SessionID, first.Event.ID) || application.panes[0].blocked || application.inputTarget != "" {
		t.Fatalf("resolved first occurrence state = resolved %t blocked %t target %q",
			application.eventResolved(first.Event.SessionID, first.Event.ID),
			application.panes[0].blocked,
			application.inputTarget,
		)
	}

	logsBeforeDelayed := len(application.logs)
	application, _ = updateModel(t, application, first)
	if application.panes[0].blocked || application.inputTarget != "" ||
		len(application.pending) != 0 || len(application.logs) != logsBeforeDelayed {
		t.Fatalf("delayed resolved event reactivated state: blocked %t target %q pending %#v logs %d/%d",
			application.panes[0].blocked,
			application.inputTarget,
			application.pending,
			len(application.logs),
			logsBeforeDelayed,
		)
	}

	second := testAdapterEvent("agent-a", "confirmation", "second occurrence", false)
	if second.Event.Signature != first.Event.Signature || second.Event.ID == first.Event.ID {
		t.Fatalf("test occurrences = first %#v second %#v", first.Event, second.Event)
	}
	application, _ = updateModel(t, application, second)
	if !application.panes[0].blocked || application.panes[0].prompt.ID != second.Event.ID ||
		application.inputTarget != "agent-a" || len(application.pending) != 1 {
		t.Fatalf("second occurrence was not accepted: blocked %t prompt %q target %q pending %#v",
			application.panes[0].blocked,
			application.panes[0].prompt.ID,
			application.inputTarget,
			application.pending,
		)
	}
}

func TestAdapterProcessExitClearsPendingStateAndIsIdempotent(t *testing.T) {
	application, _, _ := newModelHarness(t)
	pending := testAdapterEvent("agent-a", "password", "sensitive request", true)
	application, _ = updateModel(t, application, pending)
	application.input.SetValue("must-be-cleared")

	exit := testAdapterEvent("agent-a", "process-exit", "processus synthétique terminé", false)
	exit.Event.Type = adapters.EventProcessExit
	exit.Event.Match = ""
	exit.Event.Sensitive = false
	exit.Event.Risk = adapters.RiskUnknown
	application, _ = updateModel(t, application, exit)
	if !application.panes[0].exited || application.panes[0].blocked ||
		application.panes[0].prompt.ID != "" || application.inputTarget != "" ||
		application.input.Value() != "" || len(application.pending) != 0 {
		t.Fatalf("process_exit state = exited %t blocked %t prompt %q target %q input %q pending %#v",
			application.panes[0].exited,
			application.panes[0].blocked,
			application.panes[0].prompt.ID,
			application.inputTarget,
			application.input.Value(),
			application.pending,
		)
	}
	if !application.eventResolved(pending.Event.SessionID, pending.Event.ID) {
		t.Fatalf("process_exit did not resolve pending occurrence %q", pending.Event.ID)
	}
	logsAfterExit := len(application.logs)
	if !strings.Contains(strings.Join(application.logs, "\n"), exit.Event.Summary) {
		t.Fatalf("process_exit summary missing from logs: %#v", application.logs)
	}
	application, _ = updateModel(t, application, exit)
	if len(application.logs) != logsAfterExit {
		t.Fatalf("duplicate process_exit appended logs: %d -> %d", logsAfterExit, len(application.logs))
	}
	application, _ = updateModel(t, application, session.Exited{SessionID: "agent-a"})
	if len(application.logs) != logsAfterExit {
		t.Fatalf("legacy Exited duplicated process_exit logs: %d -> %d", logsAfterExit, len(application.logs))
	}
}

func TestSensitiveAdapterEventNeverRendersOrLogsSummaryMatchOrManualInput(t *testing.T) {
	application, _, _ := newModelHarness(t)
	const (
		secretSummary = "summary-secret-marker"
		secretMatch   = "match-secret-marker"
		secretInput   = "manual-secret-marker"
	)
	event := testAdapterEvent("agent-a", "password", secretSummary, true)
	event.Event.Match = secretMatch
	event.Event.Metadata["private"] = secretMatch
	application, _ = updateModel(t, application, event)
	application.input.SetValue(secretInput)

	logs := strings.Join(application.logs, "\n")
	view := application.View()
	for _, secret := range []string{secretSummary, secretMatch, secretInput} {
		if strings.Contains(logs, secret) {
			t.Fatalf("sensitive value %q leaked into logs: %q", secret, logs)
		}
		if strings.Contains(view, secret) {
			t.Fatalf("sensitive value %q leaked into View", secret)
		}
	}
	if !strings.Contains(logs, "saisie sensible requise") {
		t.Fatalf("sensitive event did not use generic safe log text: %q", logs)
	}
}
