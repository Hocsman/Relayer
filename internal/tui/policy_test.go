package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

type automaticDecisionCall struct {
	sessionID string
	eventID   string
	decision  adapters.Decision
}

type policyTestBackend struct {
	*fakeBackend

	policyMu           sync.Mutex
	pending            map[string]*adapters.Event
	automaticCalls     []automaticDecisionCall
	automaticErr       error
	replacementOnError *adapters.Event
}

func newPolicyTestBackend() *policyTestBackend {
	return &policyTestBackend{
		fakeBackend: newFakeBackend(),
		pending:     make(map[string]*adapters.Event),
	}
}

func (b *policyTestBackend) setPending(event adapters.Event) {
	b.policyMu.Lock()
	copy := event.Clone()
	b.pending[strings.ToLower(event.SessionID)] = &copy
	b.policyMu.Unlock()
}

func (b *policyTestBackend) PendingEvent(_ context.Context, sessionID string) (*adapters.Event, error) {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	pending := b.pending[strings.ToLower(sessionID)]
	if pending == nil {
		return nil, nil
	}
	copy := pending.Clone()
	return &copy, nil
}

func (b *policyTestBackend) SendAutomaticDecision(
	sessionID string,
	event adapters.Event,
	decision adapters.Decision,
) error {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	b.automaticCalls = append(b.automaticCalls, automaticDecisionCall{
		sessionID: sessionID,
		eventID:   event.ID,
		decision:  decision,
	})
	pending := b.pending[strings.ToLower(sessionID)]
	if pending == nil || pending.ID != event.ID {
		return adapters.ErrEventMismatch
	}
	if b.automaticErr != nil {
		if b.replacementOnError != nil {
			copy := b.replacementOnError.Clone()
			b.pending[strings.ToLower(sessionID)] = &copy
		}
		return b.automaticErr
	}
	delete(b.pending, strings.ToLower(sessionID))
	return nil
}

func (b *policyTestBackend) automaticSnapshot() []automaticDecisionCall {
	b.policyMu.Lock()
	defer b.policyMu.Unlock()
	return append([]automaticDecisionCall(nil), b.automaticCalls...)
}

func newPolicyModel(t *testing.T, backend Backend, configuration policy.Config) *Model {
	t.Helper()
	engine, err := policy.New(configuration)
	if err != nil {
		t.Fatal(err)
	}
	application, err := NewModelWithPolicy(
		backend,
		make(chan session.Event, 16),
		[]Pane{
			{ID: "agent-a", Name: "Agent A", Backend: "pty", Adapter: adapters.GenericID},
			{ID: "agent-b", Name: "Agent B", Backend: "tmux", Adapter: adapters.GenericID},
		},
		120,
		36,
		nil,
		engine,
	)
	if err != nil {
		t.Fatal(err)
	}
	return application
}

func automaticEvent(sessionID, eventID string) adapters.Event {
	event := testAdapterEvent(sessionID, "confirmation", "safe policy summary", false).Event
	event.ID = eventID
	event.Risk = adapters.RiskLow
	event.Match = "match-secret-marker"
	event.Metadata["private"] = "metadata-secret-marker"
	return event
}

func TestPolicyAutomaticDecisionsAreCASKeyedBySessionAndOccurrence(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	application := newPolicyModel(t, backend, policy.Config{DefaultAction: policy.ActionAllow})
	first := automaticEvent("agent-a", "shared-occurrence-id")
	second := automaticEvent("agent-b", "shared-occurrence-id")
	backend.setPending(first)
	backend.setPending(second)

	application, firstCommand := updateModel(t, application, session.AdapterEvent{Event: first})
	application, duplicate := updateModel(t, application, session.AdapterEvent{Event: first})
	if duplicate != nil {
		t.Fatal("duplicate occurrence scheduled another automatic decision")
	}
	application, secondCommand := updateModel(t, application, session.AdapterEvent{Event: second})
	if firstCommand == nil || secondCommand == nil {
		t.Fatal("independent sessions did not schedule independent automatic decisions")
	}

	application, _ = updateModel(t, application, executeCommand(t, secondCommand))
	application, _ = updateModel(t, application, executeCommand(t, firstCommand))
	calls := backend.automaticSnapshot()
	if len(calls) != 2 || calls[0] != (automaticDecisionCall{"agent-b", second.ID, adapters.DecisionAllow}) ||
		calls[1] != (automaticDecisionCall{"agent-a", first.ID, adapters.DecisionAllow}) {
		t.Fatalf("automatic calls = %#v", calls)
	}
	if !application.eventResolved(first.SessionID, first.ID) || !application.eventResolved(second.SessionID, second.ID) {
		t.Fatalf("composite resolved keys missing: %#v", application.resolvedEventIDs)
	}
	visible := strings.Join(application.logs, "\n") + application.View()
	if !strings.Contains(visible, "summary=safe policy summary") || !strings.Contains(visible, "automatic=true") {
		t.Fatalf("non-sensitive summary missing from policy audit: %q", visible)
	}
	for _, secret := range []string{first.Match, first.Metadata["private"], second.Match, second.Metadata["private"]} {
		if strings.Contains(visible, secret) {
			t.Fatalf("policy UI leaked %q", secret)
		}
	}
}

func TestPolicySuccessiveSameSignatureOccurrencesAreSequencedPerSession(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	application := newPolicyModel(t, backend, policy.Config{DefaultAction: policy.ActionDeny})
	first := automaticEvent("agent-a", "occurrence-1")
	second := automaticEvent("agent-a", "occurrence-2")
	second.Signature = first.Signature
	backend.setPending(first)

	application, firstCommand := updateModel(t, application, session.AdapterEvent{Event: first})
	firstResult := executeCommand(t, firstCommand)
	backend.setPending(second)
	application, deferredCommand := updateModel(t, application, session.AdapterEvent{Event: second})
	if deferredCommand != nil {
		t.Fatal("second occurrence bypassed per-session automatic sequencing")
	}
	application, secondCommand := updateModel(t, application, firstResult)
	if secondCommand == nil {
		t.Fatal("second occurrence was not resumed after first completion")
	}
	application, _ = updateModel(t, application, executeCommand(t, secondCommand))
	calls := backend.automaticSnapshot()
	if len(calls) != 2 || calls[0].eventID != first.ID || calls[1].eventID != second.ID ||
		calls[0].decision != adapters.DecisionDeny || calls[1].decision != adapters.DecisionDeny {
		t.Fatalf("successive automatic calls = %#v", calls)
	}
}

func TestPolicyUnsupportedDecisionFallsBackToExactPendingHumanPrompt(t *testing.T) {
	for _, action := range []policy.Action{policy.ActionAllow, policy.ActionDeny} {
		t.Run(string(action), func(t *testing.T) {
			backend := newPolicyTestBackend()
			backend.automaticErr = adapters.ErrDecisionUnsupported
			t.Cleanup(backend.cancel)
			application := newPolicyModel(t, backend, policy.Config{DefaultAction: action})
			event := automaticEvent("agent-a", "unsupported-"+string(action))
			backend.setPending(event)

			application, command := updateModel(t, application, session.AdapterEvent{Event: event})
			application, focusCommand := updateModel(t, application, executeCommand(t, command))
			_ = focusCommand
			if !application.panes[0].blocked || application.panes[0].prompt.ID != event.ID ||
				application.inputTarget != event.SessionID || application.panes[0].policyTag != "AUTO → ASK" {
				t.Fatalf("unsupported fallback state = blocked %t prompt %q target %q tag %q",
					application.panes[0].blocked,
					application.panes[0].prompt.ID,
					application.inputTarget,
					application.panes[0].policyTag,
				)
			}
			calls := backend.automaticSnapshot()
			wantDecision := adapters.DecisionAllow
			if action == policy.ActionDeny {
				wantDecision = adapters.DecisionDeny
			}
			if len(calls) != 1 || calls[0].decision != wantDecision {
				t.Fatalf("unsupported %s calls = %#v", action, calls)
			}
			logs := strings.Join(application.logs, "\n")
			if !strings.Contains(logs, "status=fallback_unsupported") || strings.Contains(logs, adapters.ErrDecisionUnsupported.Error()) {
				t.Fatalf("unsafe/absent fallback audit: %q", logs)
			}
		})
	}
}

func TestPolicyFailureUsesCurrentPendingAndNeverAutomaticallyRetriesIt(t *testing.T) {
	backend := newPolicyTestBackend()
	backend.automaticErr = errors.New("delivery-secret-marker")
	t.Cleanup(backend.cancel)
	application := newPolicyModel(t, backend, policy.Config{DefaultAction: policy.ActionAllow})
	first := automaticEvent("agent-a", "ambiguous-first")
	current := automaticEvent("agent-a", "current-after-ambiguous-delivery")
	backend.replacementOnError = &current
	backend.setPending(first)

	application, command := updateModel(t, application, session.AdapterEvent{Event: first})
	application, _ = updateModel(t, application, executeCommand(t, command))
	if !application.panes[0].blocked || !application.panes[0].policyFrozen ||
		application.panes[0].prompt.ID != current.ID || application.inputTarget != "" {
		t.Fatalf("ambiguous failure did not freeze current event: pane=%#v target=%q", application.panes[0], application.inputTarget)
	}
	if calls := backend.automaticSnapshot(); len(calls) != 1 {
		t.Fatalf("ambiguous delivery retried automatically: %#v", calls)
	}
	visible := strings.Join(application.logs, "\n") + application.View()
	if strings.Contains(visible, backend.automaticErr.Error()) {
		t.Fatal("raw delivery error leaked into policy UI")
	}
	if !strings.Contains(visible, "status=delivery_uncertain") {
		t.Fatalf("ambiguous delivery was not identified safely: %q", visible)
	}
	if !strings.Contains(visible, "LIVRAISON INCERTAINE") || !strings.Contains(visible, "aucune nouvelle réponse envoyée") {
		t.Fatalf("ambiguous delivery did not explain the frozen state: %q", visible)
	}
	backend.setPending(current)
	application, retry := updateModel(t, application, session.AdapterEvent{Event: current})
	if retry != nil || len(backend.automaticSnapshot()) != 1 || application.inputTarget != "" {
		t.Fatalf("frozen policy retried: command=%v calls=%#v target=%q", retry, backend.automaticSnapshot(), application.inputTarget)
	}
}

func TestPolicyProcessExitPurgesInFlightAndLateCompletionCannotResurrect(t *testing.T) {
	backend := newPolicyTestBackend()
	backend.automaticErr = errors.New("late-secret-delivery-error")
	t.Cleanup(backend.cancel)
	application := newPolicyModel(t, backend, policy.Config{DefaultAction: policy.ActionAllow})
	event := automaticEvent("agent-a", "exit-race")
	backend.setPending(event)

	application, command := updateModel(t, application, session.AdapterEvent{Event: event})
	lateResult := executeCommand(t, command)
	exit := automaticEvent("agent-a", "process-exit")
	exit.Type = adapters.EventProcessExit
	exit.Summary = "processus terminé"
	exit.Match = ""
	application, _ = updateModel(t, application, session.AdapterEvent{Event: exit})
	logsAfterExit := len(application.logs)
	application, followup := updateModel(t, application, lateResult)
	if followup != nil || !application.panes[0].exited || application.panes[0].blocked ||
		application.inputTarget != "" || len(application.automaticInFlight) != 0 || len(application.deferredEvents) != 0 {
		t.Fatalf("late result resurrected state: pane %#v target %q attempts %#v deferred %#v",
			application.panes[0], application.inputTarget, application.automaticInFlight, application.deferredEvents)
	}
	if len(application.logs) != logsAfterExit || strings.Contains(strings.Join(application.logs, "\n"), backend.automaticErr.Error()) {
		t.Fatal("late automatic result changed or leaked into logs")
	}
}

func TestPolicyDryRunAndCredentialAlwaysRemainHumanAndSecretSafe(t *testing.T) {
	t.Run("dry run", func(t *testing.T) {
		backend := newPolicyTestBackend()
		t.Cleanup(backend.cancel)
		application := newPolicyModel(t, backend, policy.Config{DefaultAction: policy.ActionAllow, DryRun: true})
		event := automaticEvent("agent-a", "dry-run")
		backend.setPending(event)
		application, _ = updateModel(t, application, session.AdapterEvent{Event: event})
		if !application.panes[0].blocked || application.inputTarget != event.SessionID || len(backend.automaticSnapshot()) != 0 {
			t.Fatalf("dry-run state = blocked %t target %q calls %#v", application.panes[0].blocked, application.inputTarget, backend.automaticSnapshot())
		}
		if !strings.Contains(application.View(), "DRY RUN") || !strings.Contains(strings.Join(application.logs, "\n"), "proposed=allow") {
			t.Fatalf("dry-run metadata missing: view/logs %q / %#v", application.View(), application.logs)
		}
	})

	t.Run("dry run preserves risk safety reason", func(t *testing.T) {
		backend := newPolicyTestBackend()
		t.Cleanup(backend.cancel)
		application := newPolicyModel(t, backend, policy.Config{DefaultAction: policy.ActionAllow, DryRun: true})
		event := automaticEvent("agent-a", "dry-run-unknown-risk")
		event.Risk = adapters.RiskUnknown
		backend.setPending(event)
		application, _ = updateModel(t, application, session.AdapterEvent{Event: event})
		visible := strings.Join(application.logs, "\n") + application.View()
		if !application.panes[0].blocked || len(backend.automaticSnapshot()) != 0 ||
			!strings.Contains(visible, "reason="+policy.ReasonRisk) || !strings.Contains(visible, "DRY RUN") {
			t.Fatalf("dry-run risk guard missing: blocked=%t calls=%#v visible=%q",
				application.panes[0].blocked, backend.automaticSnapshot(), visible)
		}
	})

	t.Run("credential flag is defensive", func(t *testing.T) {
		backend := newPolicyTestBackend()
		t.Cleanup(backend.cancel)
		application := newPolicyModel(t, backend, policy.Config{DefaultAction: policy.ActionAllow})
		event := automaticEvent("agent-a", "credential")
		event.Type = adapters.EventCredential
		event.Sensitive = false
		event.Risk = adapters.RiskHigh
		event.Summary = "credential-summary-secret"
		event.Match = "credential-match-secret"
		event.Metadata["private"] = "credential-metadata-secret"
		backend.setPending(event)
		application, _ = updateModel(t, application, session.AdapterEvent{Event: event})
		if !application.panes[0].blocked || application.input.EchoMode != textinput.EchoPassword || len(backend.automaticSnapshot()) != 0 {
			t.Fatalf("credential safety state = blocked %t echo %v calls %#v", application.panes[0].blocked, application.input.EchoMode, backend.automaticSnapshot())
		}
		visible := strings.Join(application.logs, "\n") + application.View()
		for _, secret := range []string{event.Summary, event.Match, event.Metadata["private"]} {
			if strings.Contains(visible, secret) {
				t.Fatalf("credential value %q leaked", secret)
			}
		}
		if !strings.Contains(visible, "summary=sensitive_event") {
			t.Fatalf("credential summary was not replaced with its safe marker: %q", visible)
		}
	})
}

func TestPolicySummaryNormalizesControlsAndTruncatesByRune(t *testing.T) {
	backend := newPolicyTestBackend()
	t.Cleanup(backend.cancel)
	application := newPolicyModel(t, backend, policy.Config{DefaultAction: policy.ActionAsk})
	event := automaticEvent("agent-a", "summary-audit")
	event.Summary = "  review\tgenerated\nfile\x00 " + strings.Repeat("é", 90)
	backend.setPending(event)

	application, _ = updateModel(t, application, session.AdapterEvent{Event: event})
	want := safeEventSummary(event)
	if got := len([]rune(want)); got != 80 {
		t.Fatalf("safe summary rune length = %d, want 80 (%q)", got, want)
	}
	visible := strings.Join(application.logs, "\n") + application.View()
	if !strings.Contains(visible, "summary="+want) {
		t.Fatalf("normalized summary missing from supervisor: %q", visible)
	}
	for _, control := range []string{"\x00", "\t"} {
		if strings.Contains(visible, control) {
			t.Fatalf("control %q survived in supervisor output", control)
		}
	}
}

type backendWithoutDecision struct {
	Backend
}

func TestManualDecisionNeverFallsBackToRawInput(t *testing.T) {
	raw := newFakeBackend()
	t.Cleanup(raw.cancel)
	backend := backendWithoutDecision{Backend: raw}
	event := automaticEvent("agent-a", "manual-cas-required")

	message, ok := executeCommand(t, deliverInput(backend, event.SessionID, "Y", event)).(inputDeliveredMsg)
	if !ok {
		t.Fatalf("manual delivery returned %T", message)
	}
	if !errors.Is(message.Err, errDecisionBackendUnavailable) {
		t.Fatalf("manual delivery error = %v, want safe capability error", message.Err)
	}
	if calls := raw.inputSnapshot(); len(calls) != 0 {
		t.Fatalf("manual decision bypassed event CAS through raw input: %#v", calls)
	}
}

var _ AutomaticDecisionBackend = (*policyTestBackend)(nil)
var _ EventSnapshotBackend = (*policyTestBackend)(nil)
var _ DecisionBackend = (*fakeBackend)(nil)
var _ tea.Model = (*Model)(nil)
