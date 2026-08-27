package tui

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

type tuiAuditSink struct {
	mu       sync.Mutex
	lines    [][]byte
	attempts int
	failAt   int
}

func (s *tuiAuditSink) WriteLine(line []byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempts++
	if s.failAt > 0 && s.attempts >= s.failAt {
		return errors.New("audit-secret-write-error")
	}
	s.lines = append(s.lines, append([]byte(nil), line...))
	return nil
}

func (s *tuiAuditSink) Close() error { return nil }

func (s *tuiAuditSink) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.lines)
}

func (s *tuiAuditSink) entries(t *testing.T) []audit.Entry {
	t.Helper()
	s.mu.Lock()
	lines := make([][]byte, len(s.lines))
	for index := range s.lines {
		lines[index] = append([]byte(nil), s.lines[index]...)
	}
	s.mu.Unlock()
	result := make([]audit.Entry, len(lines))
	for index := range lines {
		if err := json.Unmarshal(lines[index], &result[index]); err != nil {
			t.Fatalf("decode audit line %d: %v", index, err)
		}
	}
	return result
}

func (s *tuiAuditSink) raw() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var result strings.Builder
	for _, line := range s.lines {
		result.Write(line)
	}
	return result.String()
}

func newTUIAuditRecorder(t *testing.T, sink audit.LineSink) *audit.Recorder {
	t.Helper()
	configuration := audit.DefaultConfig()
	configuration.Mode = audit.ModeDetailed
	var identifiers atomic.Uint64
	recorder, err := audit.NewRecorder(configuration, sink, nil, func() (string, error) {
		return fmt.Sprintf("tui-audit-%d", identifiers.Add(1)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	return recorder
}

func newAuditedModel(
	t *testing.T,
	backend Backend,
	configuration policy.Config,
	panes []Pane,
	sink audit.LineSink,
) *Model {
	t.Helper()
	engine, err := policy.New(configuration)
	if err != nil {
		t.Fatal(err)
	}
	application, err := NewModelWithPolicyAndAudit(
		backend,
		make(chan session.Event, 16),
		panes,
		120,
		36,
		nil,
		engine,
		newTUIAuditRecorder(t, sink),
	)
	if err != nil {
		t.Fatal(err)
	}
	return application
}

func auditedPanes() []Pane {
	return []Pane{
		{ID: "agent-a", Name: "Agent A", Backend: "pty", Adapter: adapters.GenericID},
		{ID: "agent-b", Name: "Agent B", Backend: "tmux", Adapter: adapters.GenericID},
	}
}

func TestAuditAutomaticDecisionIsDurableOrderedAndDeduplicated(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(
		t,
		backend,
		policy.Config{DefaultAction: policy.ActionAllow},
		auditedPanes(),
		sink,
	)
	event := automaticEvent("agent-a", "audit-auto-1")
	backend.setPending(event)

	application, command := updateModel(t, application, session.AdapterEvent{Event: event})
	if command == nil {
		t.Fatal("automatic event did not return a delivery command")
	}
	if calls := backend.automaticSnapshot(); len(calls) != 0 {
		t.Fatalf("backend called before Bubble Tea command execution: %#v", calls)
	}
	entries := sink.entries(t)
	wantKinds := []audit.Kind{audit.KindEventDetected, audit.KindPolicyEvaluated, audit.KindDecision}
	if len(entries) != len(wantKinds) {
		t.Fatalf("pre-delivery entries = %#v", entries)
	}
	for index, want := range wantKinds {
		if entries[index].Kind != want || entries[index].Sequence != uint64(index+1) {
			t.Fatalf("entry %d = %#v, want %s", index, entries[index], want)
		}
	}
	if entries[1].Decision != audit.DecisionAllow || entries[1].DecisionBy != audit.DecisionByPolicy ||
		entries[2].Decision != audit.DecisionAllow || entries[2].Outcome != audit.OutcomeInFlight {
		t.Fatalf("policy/decision audit = %#v", entries)
	}
	application, duplicate := updateModel(t, application, session.AdapterEvent{Event: event})
	if duplicate != nil || sink.count() != 3 {
		t.Fatal("duplicate occurrence produced another audit record or command")
	}

	_, _ = updateModel(t, application, executeCommand(t, command))
	entries = sink.entries(t)
	if len(entries) != 4 || entries[3].Kind != audit.KindDelivery ||
		entries[3].Outcome != audit.OutcomeSucceeded || entries[3].Decision != audit.DecisionAllow {
		t.Fatalf("delivery audit = %#v", entries)
	}
	if calls := backend.automaticSnapshot(); len(calls) != 1 || calls[0].eventID != event.ID {
		t.Fatalf("automatic backend calls = %#v", calls)
	}
	raw := sink.raw()
	for _, secret := range []string{event.Match, event.Metadata["private"]} {
		if strings.Contains(raw, secret) {
			t.Fatalf("audit leaked event secret %q: %s", secret, raw)
		}
	}
}

func TestAuditFailureBeforeAutomaticSendFreezesWithoutBackendIO(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{failAt: 3}
	application := newAuditedModel(
		t,
		backend,
		policy.Config{DefaultAction: policy.ActionAllow},
		auditedPanes(),
		sink,
	)
	event := automaticEvent("agent-a", "audit-fail-before-auto")
	backend.setPending(event)

	application, command := updateModel(t, application, session.AdapterEvent{Event: event})
	if command != nil {
		t.Fatal("audit decision failure returned a backend command")
	}
	if calls := backend.automaticSnapshot(); len(calls) != 0 {
		t.Fatalf("backend calls after pre-send audit failure = %#v", calls)
	}
	if !application.auditUnavailable || !strings.Contains(application.View(), "AUDIT INDISPONIBLE") {
		t.Fatalf("audit failure did not freeze and surface the UI: %q", application.View())
	}
}

func TestAuditGateStopsPreviouslyScheduledDecisionAfterGlobalFailure(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{failAt: 4}
	application := newAuditedModel(
		t,
		backend,
		policy.Config{DefaultAction: policy.ActionAllow},
		auditedPanes(),
		sink,
	)
	first := automaticEvent("agent-a", "scheduled-before-audit-failure")
	second := automaticEvent("agent-b", "triggers-audit-failure")
	backend.setPending(first)
	backend.setPending(second)

	application, scheduled := updateModel(t, application, session.AdapterEvent{Event: first})
	if scheduled == nil {
		t.Fatal("first automatic decision was not scheduled")
	}
	application, rejected := updateModel(t, application, session.AdapterEvent{Event: second})
	if rejected != nil || !application.auditUnavailable {
		t.Fatal("second event did not trip the configured audit failure")
	}
	result, ok := executeCommand(t, scheduled).(automaticDecisionFinishedMsg)
	if !ok || !errors.Is(result.Err, errAuditUnavailable) {
		t.Fatalf("closed audit gate result = %#v", result)
	}
	if calls := backend.automaticSnapshot(); len(calls) != 0 {
		t.Fatalf("closed audit gate still called backend: %#v", calls)
	}
}

func TestDeliveryGateLinearizesCloseWithoutWaitingForInFlightAttach(t *testing.T) {
	gate := newDeliveryGate()
	if !gate.beginOperation() {
		t.Fatal("new gate rejected first operation")
	}
	closed := make(chan struct{})
	go func() {
		gate.close()
		close(closed)
	}()
	<-closed
	if gate.beginOperation() {
		t.Fatal("operation started after the gate's close linearization point")
	}
	// close must not wait for a potentially long-lived tea.ExecProcess attach.
	gate.endOperation()
}

func TestAuditManualDecisionNeverPersistsInput(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(
		t,
		backend,
		policy.Config{DefaultAction: policy.ActionAsk},
		auditedPanes(),
		sink,
	)
	event := testAdapterEvent("agent-a", "confirmation", "continue safely", false).Event

	application, _ = updateModel(t, application, session.AdapterEvent{Event: event})
	const manualSecret = "manual-input-secret-sentinel"
	application.input.SetValue(manualSecret)
	application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if delivery == nil {
		t.Fatal("manual decision did not return a delivery command")
	}
	entries := sink.entries(t)
	if len(entries) != 3 || entries[2].Kind != audit.KindDecision ||
		entries[2].Decision != audit.DecisionAsk || entries[2].DecisionBy != audit.DecisionByHuman {
		t.Fatalf("manual decision audit = %#v", entries)
	}
	if strings.Contains(sink.raw(), manualSecret) {
		t.Fatal("manual input reached the synchronized audit record")
	}

	_, _ = updateModel(t, application, executeCommand(t, delivery))
	if calls := backend.inputSnapshot(); len(calls) != 1 || calls[0].value != manualSecret {
		t.Fatalf("manual backend calls = %#v", calls)
	}
	entries = sink.entries(t)
	if len(entries) != 4 || entries[3].Kind != audit.KindDelivery ||
		entries[3].Decision != audit.DecisionAsk || entries[3].DecisionBy != audit.DecisionByHuman {
		t.Fatalf("manual delivery audit = %#v", entries)
	}
	if strings.Contains(sink.raw(), manualSecret) {
		t.Fatal("manual input leaked after delivery audit")
	}
}

func TestAuditEventSummaryShowsSafeTextAndMasksSensitiveText(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(
		t,
		backend,
		policy.Config{DefaultAction: policy.ActionAsk},
		auditedPanes(),
		sink,
	)
	normal := testAdapterEvent("agent-a", "confirmation", "safe visible summary", false).Event
	credential := testAdapterEvent("agent-b", "password", "credential-summary-secret", true).Event

	application, _ = updateModel(t, application, session.AdapterEvent{Event: normal})
	_, _ = updateModel(t, application, session.AdapterEvent{Event: credential})
	entries := sink.entries(t)
	if len(entries) != 4 || entries[0].Kind != audit.KindEventDetected ||
		entries[0].Summary != "safe visible summary" || entries[2].Kind != audit.KindEventDetected ||
		entries[2].Summary != "sensitive_event" || !entries[2].Sensitive {
		t.Fatalf("summary audit entries = %#v", entries)
	}
	if strings.Contains(sink.raw(), "credential-summary-secret") {
		t.Fatal("sensitive event summary leaked into audit")
	}
}

func TestAuditFailureBeforeManualSendClearsSecretAndSkipsBackend(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{failAt: 3}
	application := newAuditedModel(
		t,
		backend,
		policy.Config{DefaultAction: policy.ActionAsk},
		auditedPanes(),
		sink,
	)
	event := testAdapterEvent("agent-a", "confirmation", "manual audit failure", false).Event
	application, _ = updateModel(t, application, session.AdapterEvent{Event: event})
	application.input.SetValue("must-be-erased")

	application, command := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil || !application.auditUnavailable || application.input.Value() != "" {
		t.Fatalf("pre-send failure state = command=%t unavailable=%t input=%q", command != nil, application.auditUnavailable, application.input.Value())
	}
	if calls := backend.inputSnapshot(); len(calls) != 0 {
		t.Fatalf("backend called after failed manual decision audit: %#v", calls)
	}
}

func TestAuditFailureAfterAutomaticSendFreezesWithoutRetry(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{failAt: 4}
	application := newAuditedModel(
		t,
		backend,
		policy.Config{DefaultAction: policy.ActionAllow},
		auditedPanes(),
		sink,
	)
	event := automaticEvent("agent-a", "post-send-audit-failure")
	backend.setPending(event)
	application, command := updateModel(t, application, session.AdapterEvent{Event: event})
	result := executeCommand(t, command)
	if calls := backend.automaticSnapshot(); len(calls) != 1 {
		t.Fatalf("backend calls before result reduction = %#v", calls)
	}

	application, retry := updateModel(t, application, result)
	if retry != nil || !application.auditUnavailable {
		t.Fatalf("post-send audit failure scheduled a retry or did not freeze: retry=%t unavailable=%t", retry != nil, application.auditUnavailable)
	}
	if calls := backend.automaticSnapshot(); len(calls) != 1 {
		t.Fatalf("post-send audit failure retried backend: %#v", calls)
	}
}

type observedAuditAttachBackend struct {
	*fakeAttachBackend
	sink         *tuiAuditSink
	countAtBuild int
}

func (b *observedAuditAttachBackend) AttachCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	b.countAtBuild = b.sink.count()
	return b.fakeAttachBackend.AttachCommand(ctx, id)
}

func TestAuditAttachIsPairedAroundResyncAndFailsClosed(t *testing.T) {
	events := make(chan session.Event, 4)
	sink := &tuiAuditSink{}
	base := newFakeAttachBackend(events)
	backend := &observedAuditAttachBackend{fakeAttachBackend: base, sink: sink}
	t.Cleanup(backend.cancel)
	application := newAuditedModel(
		t,
		backend,
		policy.Config{DefaultAction: policy.ActionAsk},
		[]Pane{{ID: "tmux-agent", Name: "Tmux Agent", Backend: "tmux", Adapter: adapters.GenericID}},
		sink,
	)
	application.execProcess = func(_ *exec.Cmd, callback tea.ExecCallback) tea.Cmd {
		return func() tea.Msg { return callback(nil) }
	}

	application, attach := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if attach == nil || backend.countAtBuild != 1 {
		t.Fatalf("AttachCommand observed %d synchronized records before construction", backend.countAtBuild)
	}
	entries := sink.entries(t)
	if len(entries) != 1 || entries[0].Kind != audit.KindAttachStarted || entries[0].Outcome != audit.OutcomeStarted {
		t.Fatalf("attach start audit = %#v", entries)
	}
	returned := executeCommand(t, attach)
	application, resync := updateModel(t, application, returned)
	if sink.count() != 1 || resync == nil {
		t.Fatal("successful client return emitted a premature finish or skipped resync")
	}
	application, duplicateResync := updateModel(t, application, returned)
	if duplicateResync != nil || sink.count() != 1 {
		t.Fatal("duplicate attach callback scheduled another resync or audit record")
	}
	resynced := executeCommand(t, resync)
	application, _ = updateModel(t, application, resynced)
	entries = sink.entries(t)
	if len(entries) != 2 || entries[1].Kind != audit.KindAttachFinished ||
		entries[1].Outcome != audit.OutcomeSucceeded || entries[1].Reason != "detach_resynced" {
		t.Fatalf("attach finish audit = %#v", entries)
	}
	_, duplicateFinish := updateModel(t, application, resynced)
	if duplicateFinish != nil || sink.count() != 2 {
		t.Fatal("duplicate resync completion produced another terminal audit entry")
	}

	// A new model with a failing first audit write must not even construct the
	// backend command.
	failingSink := &tuiAuditSink{failAt: 1}
	failingBase := newFakeAttachBackend(make(chan session.Event, 1))
	failingBackend := &observedAuditAttachBackend{fakeAttachBackend: failingBase, sink: failingSink}
	t.Cleanup(failingBackend.cancel)
	failing := newAuditedModel(
		t,
		failingBackend,
		policy.Config{DefaultAction: policy.ActionAsk},
		[]Pane{{ID: "tmux-agent", Name: "Tmux Agent", Backend: "tmux", Adapter: adapters.GenericID}},
		failingSink,
	)
	failing, command := updateModel(t, failing, tea.KeyMsg{Type: tea.KeyEnter})
	attachCalls, _ := failingBackend.attachSnapshot()
	if command != nil || len(attachCalls) != 0 || !failing.auditUnavailable {
		t.Fatalf("failed attach audit = command=%t calls=%#v unavailable=%t", command != nil, attachCalls, failing.auditUnavailable)
	}
}

func TestAuditResyncFailureRecordsOneFinishAndBackendError(t *testing.T) {
	sink := &tuiAuditSink{}
	backend := newFakeAttachBackend(make(chan session.Event, 1))
	backend.resyncErr = errors.New("resync-error-secret-sentinel")
	t.Cleanup(backend.cancel)
	application := newAuditedModel(
		t,
		backend,
		policy.Config{DefaultAction: policy.ActionAsk},
		[]Pane{{ID: "tmux-agent", Name: "Tmux Agent", Backend: "tmux", Adapter: adapters.GenericID}},
		sink,
	)
	application.execProcess = func(_ *exec.Cmd, callback tea.ExecCallback) tea.Cmd {
		return func() tea.Msg { return callback(nil) }
	}

	application, attach := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	application, resync := updateModel(t, application, executeCommand(t, attach))
	_, _ = updateModel(t, application, executeCommand(t, resync))
	entries := sink.entries(t)
	if len(entries) != 3 || entries[0].Kind != audit.KindAttachStarted ||
		entries[1].Kind != audit.KindAttachFinished || entries[1].Outcome != audit.OutcomeFailed ||
		entries[1].Reason != "detach_resync_failed" || entries[2].Kind != audit.KindBackendError ||
		entries[2].Reason != "detach_resync_failed" {
		t.Fatalf("failed resync audit = %#v", entries)
	}
	if strings.Contains(sink.raw(), backend.resyncErr.Error()) {
		t.Fatal("raw resync error leaked into audit")
	}
}

func TestAuditAttachConstructionFailureRecordsFinishAndBackendError(t *testing.T) {
	sink := &tuiAuditSink{}
	backend := newFakeAttachBackend(make(chan session.Event, 1))
	backend.attachErr = errors.New("attach-error-secret-sentinel")
	t.Cleanup(backend.cancel)
	application := newAuditedModel(
		t,
		backend,
		policy.Config{DefaultAction: policy.ActionAsk},
		[]Pane{{ID: "tmux-agent", Name: "Tmux Agent", Backend: "tmux", Adapter: adapters.GenericID}},
		sink,
	)

	application, command := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil || application.attachPending != "" {
		t.Fatalf("failed AttachCommand returned command=%t pending=%q", command != nil, application.attachPending)
	}
	entries := sink.entries(t)
	if len(entries) != 3 || entries[0].Kind != audit.KindAttachStarted ||
		entries[1].Kind != audit.KindAttachFinished || entries[1].Outcome != audit.OutcomeFailed ||
		entries[1].Reason != "attach_command_failed" || entries[2].Kind != audit.KindBackendError ||
		entries[2].Reason != "attach_command_failed" {
		t.Fatalf("failed attach construction audit = %#v", entries)
	}
	if strings.Contains(sink.raw(), backend.attachErr.Error()) {
		t.Fatal("raw attach construction error leaked into audit")
	}
}

func TestAuditRecordsProcessExitAndSafeBackendErrors(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	sink := &tuiAuditSink{}
	application := newAuditedModel(
		t,
		backend,
		policy.Config{DefaultAction: policy.ActionAsk},
		auditedPanes(),
		sink,
	)
	application, _ = updateModel(t, application, session.Error{
		SessionID: "agent-a",
		Err:       errors.New("backend-error-secret-sentinel"),
	})
	application, _ = updateModel(t, application, resizeFinishedMsg{
		Generation: 1,
		Failures: []resizeFailure{{
			SessionID: "agent-a",
			Name:      "Agent A",
			Err:       errors.New("resize-error-secret-sentinel"),
		}},
	})
	exitCode := 17
	exit := adapters.NewProcessExitEvent("agent-b", "agent-b", adapters.GenericID, 1, &exitCode, true)
	application, _ = updateModel(t, application, session.AdapterEvent{Event: exit})

	entries := sink.entries(t)
	if len(entries) != 4 || entries[0].Kind != audit.KindBackendError ||
		entries[0].Reason != "backend_event" || entries[1].Kind != audit.KindBackendError ||
		entries[1].Reason != "resize_failed" || entries[2].Kind != audit.KindEventDetected ||
		entries[2].EventType != adapters.EventProcessExit || entries[2].Metadata["exit_code"] != "17" ||
		entries[3].Kind != audit.KindSessionFinished || entries[3].Outcome != audit.OutcomeFailed ||
		entries[3].Reason != "process_exit" || entries[3].EventID != exit.ID {
		t.Fatalf("backend/exit audit = %#v", entries)
	}
	if strings.Contains(sink.raw(), "backend-error-secret-sentinel") ||
		strings.Contains(sink.raw(), "resize-error-secret-sentinel") {
		t.Fatal("raw backend error leaked into audit")
	}
	_, _ = updateModel(t, application, session.Exited{SessionID: "agent-b"})
	if sink.count() != 4 {
		t.Fatal("legacy Exited duplicated canonical process_exit audit records")
	}
	finished := 0
	for _, entry := range entries {
		if entry.Kind == audit.KindSessionFinished {
			finished++
		}
	}
	if finished != 1 {
		t.Fatalf("session_finished records = %d, want exactly one", finished)
	}
}
