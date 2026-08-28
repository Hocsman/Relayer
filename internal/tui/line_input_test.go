package tui

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	tea "github.com/charmbracelet/bubbletea"
)

func lineInputKey() tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{'i'}}
}

func openLineInput(t *testing.T, application *Model) *Model {
	t.Helper()
	application, _ = updateModel(t, application, lineInputKey())
	if application.lineInputTarget == "" || !application.input.Focused() {
		t.Fatalf("line composer did not open: target=%q focused=%t", application.lineInputTarget, application.input.Focused())
	}
	return application
}

func TestLineInputOpenCancelAndExactAgentDelivery(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	application = openLineInput(t, application)
	if application.lineInputTarget != "agent-a" || application.focus.AgentID != "agent-a" {
		t.Fatalf("first composer target/focus = %q/%#v", application.lineInputTarget, application.focus)
	}
	application.input.SetValue("discarded-value")
	application, command := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEsc})
	if command != nil || application.lineInputTarget != "" || application.input.Value() != "" || application.input.Focused() {
		t.Fatalf("cancel state = command=%v target=%q value=%q focused=%t",
			command != nil, application.lineInputTarget, application.input.Value(), application.input.Focused())
	}
	if calls := backend.lineSnapshot(); len(calls) != 0 {
		t.Fatalf("cancel reached backend: %#v", calls)
	}

	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlRight})
	application = openLineInput(t, application)
	const line = "review the synthetic change"
	application.input.SetValue(line)
	application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if delivery == nil || application.lineInputTarget != "" || application.lineWritePending != "agent-b" ||
		application.input.Value() != "" {
		t.Fatalf("submitted state = command=%v target=%q pending=%q value=%q",
			delivery != nil, application.lineInputTarget, application.lineWritePending, application.input.Value())
	}
	message, ok := executeCommand(t, delivery).(lineInputDeliveredMsg)
	if !ok {
		t.Fatalf("line command returned %T", message)
	}
	if strings.Contains(fmt.Sprintf("%#v", message), line) {
		t.Fatal("line value survived in the Bubble Tea result message")
	}
	if calls := backend.lineSnapshot(); len(calls) != 1 || calls[0] != (inputCall{id: "agent-b", value: line}) {
		t.Fatalf("line calls = %#v", calls)
	}
	application, _ = updateModel(t, application, message)
	if application.lineWritePending != "" || application.panes[1].policyFrozen {
		t.Fatalf("completed line state = pending=%q frozen=%t", application.lineWritePending, application.panes[1].policyFrozen)
	}
}

func TestLineInputAuditIsOrderedAndNeverReceivesValueOrLength(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)
	application = openLineInput(t, application)
	const secret = "operator-line-secret-marker"
	application.input.SetValue(secret)
	application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if application.input.Value() != "" || strings.Contains(application.View()+strings.Join(application.logs, "\n"), secret) {
		t.Fatal("operator line remained visible after submission")
	}
	entries := sink.entries(t)
	if len(entries) != 1 || entries[0].Kind != audit.KindOperatorInput ||
		entries[0].Outcome != audit.OutcomeInFlight || entries[0].DecisionBy != audit.DecisionByHuman ||
		entries[0].Summary != "" || entries[0].Metadata != nil {
		t.Fatalf("pre-I/O operator audit = %#v", entries)
	}
	if strings.Contains(sink.raw(), secret) {
		t.Fatal("operator audit contains the submitted value")
	}

	message := executeCommand(t, delivery)
	if strings.Contains(fmt.Sprintf("%#v", message), secret) {
		t.Fatal("operator result message contains the submitted value")
	}
	application, _ = updateModel(t, application, message)
	entries = sink.entries(t)
	if len(entries) != 2 || entries[1].Kind != audit.KindOperatorInput ||
		entries[1].Outcome != audit.OutcomeApplied || entries[1].Reason != "operator_input_applied" ||
		entries[1].Sequence <= entries[0].Sequence {
		t.Fatalf("terminal operator audit = %#v", entries)
	}
	visible := sink.raw() + application.View() + strings.Join(application.logs, "\n")
	if strings.Contains(visible, secret) {
		t.Fatal("submitted operator line leaked after delivery")
	}
}

func TestLineInputAuditFailureFailsClosedAroundDelivery(t *testing.T) {
	t.Run("before I/O", func(t *testing.T) {
		backend := newFakeBackend()
		t.Cleanup(backend.cancel)
		sink := &tuiAuditSink{failAt: 1}
		application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)
		application = openLineInput(t, application)
		application.input.SetValue("pre-audit-secret")

		application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
		if delivery != nil || !application.auditUnavailable || application.lineWritePending != "" ||
			application.input.Value() != "" || len(backend.lineSnapshot()) != 0 {
			t.Fatalf("pre-audit failure state: command=%t unavailable=%t pending=%q calls=%#v",
				delivery != nil, application.auditUnavailable, application.lineWritePending, backend.lineSnapshot())
		}
	})

	t.Run("after I/O", func(t *testing.T) {
		backend := newFakeBackend()
		t.Cleanup(backend.cancel)
		sink := &tuiAuditSink{failAt: 2}
		application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)
		application = openLineInput(t, application)
		application.input.SetValue("post-audit-secret")

		application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
		result := executeCommand(t, delivery)
		application, retry := updateModel(t, application, result)
		if retry != nil || !application.auditUnavailable || application.lineWritePending != "" ||
			len(backend.lineSnapshot()) != 1 {
			t.Fatalf("terminal-audit failure state: retry=%t unavailable=%t pending=%q calls=%#v",
				retry != nil, application.auditUnavailable, application.lineWritePending, backend.lineSnapshot())
		}
		application, retry = updateModel(t, application, lineInputKey())
		if retry != nil || application.lineInputTarget != "" || len(backend.lineSnapshot()) != 1 {
			t.Fatal("audit failure admitted an operator-input retry")
		}
	})
}

func TestLineInputExistingPromptAndEveryUnsafeStateBlockOpening(t *testing.T) {
	t.Run("pending prompt", func(t *testing.T) {
		application, backend, _ := newModelHarness(t)
		application, _ = updateModel(t, application, testAdapterEvent("agent-a", "confirmation", "answer first", false))
		application.focus = FocusTarget{Kind: FocusAgent, AgentID: "agent-b"}
		application, command := updateModel(t, application, lineInputKey())
		if command != nil || application.lineInputTarget != "" || len(backend.lineSnapshot()) != 0 {
			t.Fatalf("pending prompt admitted line input: command=%v target=%q", command != nil, application.lineInputTarget)
		}
	})

	tests := []struct {
		name  string
		setup func(*Model)
	}{
		{name: "exited", setup: func(m *Model) { m.panes[0].exited = true }},
		{name: "policy frozen", setup: func(m *Model) { m.panes[0].policyFrozen = true }},
		{name: "audit unavailable", setup: func(m *Model) { m.auditUnavailable = true }},
		{name: "manual write", setup: func(m *Model) { m.writePending = true }},
		{name: "line write", setup: func(m *Model) { m.lineWritePending = "agent-b" }},
		{name: "attach", setup: func(m *Model) { m.attachPending = "agent-b" }},
		{name: "automatic", setup: func(m *Model) {
			key := semanticEventKey("agent-a", "event-auto")
			m.automaticBySession[key.sessionID] = key
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			application, backend, _ := newModelHarness(t)
			test.setup(application)
			application, command := updateModel(t, application, lineInputKey())
			if command != nil || application.lineInputTarget != "" || application.input.Value() != "" ||
				len(backend.lineSnapshot()) != 0 {
				t.Fatalf("unsafe state admitted line: command=%v target=%q calls=%#v",
					command != nil, application.lineInputTarget, backend.lineSnapshot())
			}
		})
	}
}

func TestLineInputPromptEventBeforeResultDefersPolicyUntilTerminalAudit(t *testing.T) {
	backend := newPolicyTestBackend()
	backend.lineError = terminal.ErrEventPending
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(
		t,
		backend,
		policy.Config{DefaultAction: policy.ActionAllow},
		auditedPanes(),
		sink,
	)
	application = openLineInput(t, application)
	application.input.SetValue("ordinary instruction")
	application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	event := automaticEvent("agent-a", "line-race-prompt")
	backend.setPending(event)

	application, premature := updateModel(t, application, session.AdapterEvent{Event: event})
	if premature != nil || len(backend.automaticSnapshot()) != 0 || len(application.automaticBySession) != 0 ||
		application.panes[0].blocked {
		t.Fatalf("prompt bypassed line terminal result: command=%v calls=%#v automatic=%#v pane=%#v",
			premature != nil, backend.automaticSnapshot(), application.automaticBySession, application.panes[0])
	}
	entries := sink.entries(t)
	if len(entries) != 1 || entries[0].Kind != audit.KindOperatorInput || entries[0].Outcome != audit.OutcomeInFlight {
		t.Fatalf("audit before line result = %#v", entries)
	}

	lineResult := executeCommand(t, delivery)
	application, automatic := updateModel(t, application, lineResult)
	if automatic == nil || len(backend.automaticSnapshot()) != 0 {
		t.Fatalf("deferred prompt did not schedule policy after line result: command=%v calls=%#v",
			automatic != nil, backend.automaticSnapshot())
	}
	entries = sink.entries(t)
	if len(entries) < 5 || entries[1].Kind != audit.KindOperatorInput ||
		entries[1].Outcome != audit.OutcomeFallbackStale || entries[2].Kind != audit.KindEventDetected ||
		entries[3].Kind != audit.KindPolicyEvaluated || entries[4].Kind != audit.KindDecision {
		t.Fatalf("line/prompt audit order = %#v", entries)
	}
	updateModel(t, application, executeCommand(t, automatic))
	if calls := backend.automaticSnapshot(); len(calls) != 1 || calls[0].eventID != event.ID {
		t.Fatalf("deferred automatic calls = %#v", calls)
	}
}

func TestLineInputTransportErrorFreezesWithoutRetryOrLeak(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	backend.lineError = errors.New("transport-error-secret-marker")
	application = openLineInput(t, application)
	const secret = "submitted-secret-marker"
	application.input.SetValue(secret)
	application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	result := executeCommand(t, delivery)
	application, retry := updateModel(t, application, result)
	if retry != nil || !application.panes[0].policyFrozen || !application.panes[0].blocked ||
		application.panes[0].policyTag != "DELIVERY UNCERTAIN" || application.lineWritePending != "" {
		t.Fatalf("uncertain line state = retry=%v pane=%#v pending=%q",
			retry != nil, application.panes[0], application.lineWritePending)
	}
	application.focus = FocusTarget{Kind: FocusAgent, AgentID: "agent-a"}
	application, retry = updateModel(t, application, lineInputKey())
	if retry != nil || application.lineInputTarget != "" || len(backend.lineSnapshot()) != 1 {
		t.Fatalf("frozen line retried: command=%v target=%q calls=%#v",
			retry != nil, application.lineInputTarget, backend.lineSnapshot())
	}
	visible := application.View() + strings.Join(application.logs, "\n") + fmt.Sprintf("%#v", result)
	if strings.Contains(visible, secret) || strings.Contains(visible, backend.lineError.Error()) {
		t.Fatal("line value or raw transport error leaked into UI state")
	}
}

func TestLineInputExitAndLateResultCannotResurrectState(t *testing.T) {
	for _, test := range []struct {
		name string
		exit tea.Msg
	}{
		{name: "legacy exit", exit: session.Exited{SessionID: "agent-a"}},
		{name: "process exit", exit: session.AdapterEvent{Event: adapters.NewProcessExitEvent(
			"agent-a", "agent-a", adapters.GenericID, 99, nil, false,
		)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			application, _, _ := newModelHarness(t)
			application = openLineInput(t, application)
			application.input.SetValue("ephemeral-secret")
			application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
			application, _ = updateModel(t, application, test.exit)
			if application.lineInputTarget != "" || application.lineWritePending != "" ||
				application.input.Value() != "" || !application.panes[0].exited {
				t.Fatalf("exit cleanup = target=%q pending=%q value=%q pane=%#v",
					application.lineInputTarget, application.lineWritePending, application.input.Value(), application.panes[0])
			}
			before := strings.Join(application.logs, "\n")
			application, late := updateModel(t, application, executeCommand(t, delivery))
			if late != nil || !application.panes[0].exited || application.lineWritePending != "" ||
				strings.Join(application.logs, "\n") != before {
				t.Fatal("late line result resurrected exited state")
			}
		})
	}
}

func TestLineInputShutdownClearsComposerAndClosesInFlightAudit(t *testing.T) {
	for _, test := range []struct {
		name    string
		message tea.Msg
	}{
		{name: "ctrl-c", message: tea.KeyMsg{Type: tea.KeyCtrlC}},
		{name: "backend stopped", message: backendStoppedMsg{}},
	} {
		t.Run(test.name, func(t *testing.T) {
			backend := newFakeBackend()
			t.Cleanup(backend.cancel)
			sink := &tuiAuditSink{}
			application := newAuditedModel(t, backend, policy.DefaultConfig(), auditedPanes(), sink)
			application = openLineInput(t, application)
			const secret = "shutdown-line-secret-marker"
			application.input.SetValue(secret)
			application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
			if delivery == nil || application.lineWritePending == "" {
				t.Fatal("line was not admitted before shutdown")
			}

			application, quit := updateModel(t, application, test.message)
			if quit == nil || application.lineInputTarget != "" || application.lineWritePending != "" ||
				application.input.Value() != "" || len(application.lineDeferredEvents) != 0 {
				t.Fatalf("shutdown line state = quit=%v target=%q pending=%q value=%q deferred=%#v",
					quit != nil, application.lineInputTarget, application.lineWritePending,
					application.input.Value(), application.lineDeferredEvents)
			}
			entries := sink.entries(t)
			if len(entries) != 2 || entries[0].Kind != audit.KindOperatorInput ||
				entries[0].Outcome != audit.OutcomeInFlight || entries[1].Kind != audit.KindOperatorInput ||
				entries[1].Outcome != audit.OutcomeFallbackDeliveryUncertain ||
				entries[1].Reason != "operator_input_shutdown" {
				t.Fatalf("shutdown audit = %#v", entries)
			}
			if strings.Contains(sink.raw()+application.View()+strings.Join(application.logs, "\n"), secret) {
				t.Fatal("shutdown retained the operator line")
			}
		})
	}
}

func TestLineInputKeyboardModeDoesNotRegressNavigation(t *testing.T) {
	application, _, _ := newModelHarness(t)
	application = openLineInput(t, application)
	application.input.SetValue("cursor")
	application.input.CursorEnd()
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyLeft})
	if application.focus.AgentID != "agent-a" || application.lineInputTarget != "agent-a" {
		t.Fatalf("editing line changed pane focus: focus=%#v target=%q", application.focus, application.lineInputTarget)
	}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlRight})
	if application.focus.AgentID != "agent-a" || application.lineInputTarget != "agent-a" {
		t.Fatalf("navigation escaped active composer: focus=%#v target=%q", application.focus, application.lineInputTarget)
	}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyEsc})
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlRight})
	if application.focus.AgentID != "agent-b" {
		t.Fatalf("navigation after cancel = %#v", application.focus)
	}
}

type backendWithoutLineInput struct{ Backend }

func TestLineInputCapabilityNeverFallsBackToRawInput(t *testing.T) {
	raw := newFakeBackend()
	t.Cleanup(raw.cancel)
	application, err := NewModel(
		backendWithoutLineInput{Backend: raw},
		make(chan session.Event, 1),
		[]Pane{{ID: "agent-a", Name: "Agent A", Backend: "pty"}},
		100,
		30,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	application, command := updateModel(t, application, lineInputKey())
	if command != nil || application.lineInputTarget != "" || len(raw.inputSnapshot()) != 0 || len(raw.lineSnapshot()) != 0 {
		t.Fatalf("missing capability used fallback: command=%v target=%q raw=%#v line=%#v",
			command != nil, application.lineInputTarget, raw.inputSnapshot(), raw.lineSnapshot())
	}
}

var _ LineInputBackend = (*fakeBackend)(nil)
