package tui

import (
	"errors"
	"strconv"
	"strings"
	"sync"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
)

var errAuditUnavailable = errors.New("local audit unavailable")

// deliveryGate closes the small gap between returning an asynchronous
// Bubble Tea command and that command actually calling a backend. A later
// audit failure can therefore still prevent an approved-but-not-started write.
type deliveryGate struct {
	mu        sync.Mutex
	available bool
	inFlight  int
}

func newDeliveryGate() *deliveryGate {
	return &deliveryGate{available: true}
}

// beginOperation is the linearization point between an audit failure and a
// backend call. Once it returns true the operation is already considered in
// flight; close prevents every later operation without a check/call TOCTOU.
func (g *deliveryGate) beginOperation() bool {
	if g == nil {
		return true
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if !g.available {
		return false
	}
	g.inFlight++
	return true
}

func (g *deliveryGate) endOperation() {
	if g == nil {
		return
	}
	g.mu.Lock()
	if g.inFlight > 0 {
		g.inFlight--
	}
	g.mu.Unlock()
}

func (g *deliveryGate) close() {
	if g != nil {
		g.mu.Lock()
		g.available = false
		g.mu.Unlock()
	}
}

func newDisabledAuditRecorder() (*audit.Recorder, error) {
	configuration := audit.DefaultConfig()
	configuration.Enabled = false
	configuration.Mode = audit.ModeOff
	return audit.NewRecorder(configuration, nil, nil, nil)
}

// recordAudit is the only persistence boundary used by Update. Record is
// synchronous: returning true means the JSONL file was synchronized before
// the caller is allowed to create a backend command.
func (m *Model) recordAudit(paneIndex int, entry audit.Entry) bool {
	if m.auditUnavailable {
		return false
	}
	if m.auditor == nil {
		m.freezeAudit(paneIndex)
		return false
	}
	if err := m.auditor.Record(entry); err != nil {
		m.freezeAudit(paneIndex)
		return false
	}
	return true
}

func (m *Model) freezeAudit(paneIndex int) {
	if m.auditUnavailable {
		return
	}
	m.auditUnavailable = true
	m.auditGate.close()

	// An audit failure is global: commands for every session share the same
	// recorder and must stop. Already running attachments may still return and
	// resynchronize, but no new decision or attachment is started.
	for index := range m.panes {
		if m.panes[index].exited {
			continue
		}
		m.panes[index].policyFrozen = true
		m.panes[index].policyTag = "AUDIT UNAVAILABLE"
	}
	if paneIndex >= 0 && paneIndex < len(m.panes) && !m.panes[paneIndex].exited {
		m.panes[paneIndex].blocked = m.panes[paneIndex].blocked || m.panes[paneIndex].prompt.ID != ""
	}

	m.automaticInFlight = make(map[eventKey]automaticAttempt)
	m.automaticBySession = make(map[string]eventKey)
	m.deferredEvents = make(map[string]adapters.Event)
	m.pending = nil
	m.inputTarget = ""
	m.writePending = false
	m.lineInputTarget = ""
	m.lineWritePending = ""
	m.lineDeferredEvents = make(map[string]adapters.Event)
	m.input.Reset()
	m.input.Blur()
	setInputInterceptionStyle(&m.input, false)
	m.appendLog("Local audit unavailable: no new decision or attachment will be sent")
}

func (m *Model) eventAuditEntry(paneIndex int, kind audit.Kind, event adapters.Event) audit.Entry {
	entry := audit.Entry{
		Kind:       kind,
		SessionID:  strings.TrimSpace(event.SessionID),
		AgentID:    strings.TrimSpace(event.AgentID),
		Adapter:    strings.ToLower(strings.TrimSpace(event.Adapter)),
		EventID:    strings.TrimSpace(event.ID),
		EventType:  event.Type,
		Risk:       event.Risk,
		Summary:    event.Summary,
		Sensitive:  requiresSecretHandling(event),
		DecisionBy: audit.DecisionBySystem,
	}
	if paneIndex >= 0 && paneIndex < len(m.panes) {
		pane := m.panes[paneIndex]
		entry.Backend = pane.backend
		if entry.SessionID == "" {
			entry.SessionID = pane.sessionID
		}
		if entry.AgentID == "" {
			entry.AgentID = pane.sessionID
		}
		if entry.Adapter == "" {
			entry.Adapter = pane.adapter
		}
	}
	if entry.Risk == "" {
		entry.Risk = adapters.RiskUnknown
	}
	if event.Type == adapters.EventProcessExit {
		entry.Metadata = safeProcessExitMetadata(event.Metadata)
	}
	return entry
}

func safeProcessExitMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	result := make(map[string]string, 2)
	if metadata["failed"] == "true" {
		result["failed"] = "true"
	}
	if code := strings.TrimSpace(metadata["exit_code"]); code != "" {
		if _, err := strconv.Atoi(code); err == nil {
			result["exit_code"] = code
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

func (m *Model) recordEventDetected(paneIndex int, event adapters.Event) bool {
	entry := m.eventAuditEntry(paneIndex, audit.KindEventDetected, event)
	entry.Outcome = audit.OutcomeDetected
	entry.Reason = "event_detected"
	return m.recordAudit(paneIndex, entry)
}

func (m *Model) recordProcessExit(paneIndex int, event adapters.Event) bool {
	if !m.recordEventDetected(paneIndex, event) {
		return false
	}
	entry := m.eventAuditEntry(paneIndex, audit.KindSessionFinished, event)
	entry.Outcome = audit.OutcomeFinished
	if event.Metadata["failed"] == "true" {
		entry.Outcome = audit.OutcomeFailed
	}
	entry.Reason = "process_exit"
	return m.recordAudit(paneIndex, entry)
}

func (m *Model) recordPolicyEvaluation(
	paneIndex int,
	event adapters.Event,
	evaluation policy.Evaluation,
) bool {
	entry := m.eventAuditEntry(paneIndex, audit.KindPolicyEvaluated, event)
	entry.DecisionBy = audit.DecisionByPolicy
	entry.Rule = evaluation.RuleName
	entry.Reason = safeReason(evaluation.Reason)
	entry.Decision = decisionForPolicyAction(evaluation.Action)
	entry.Outcome = audit.OutcomeAsk
	if evaluation.DryRun || m.policyConfig.DryRun {
		entry.Outcome = audit.OutcomeDryRun
	} else if _, automatic := automaticDecision(evaluation); automatic {
		entry.Outcome = audit.OutcomeInFlight
	}
	mode := "enforce"
	if evaluation.DryRun || m.policyConfig.DryRun {
		mode = "dry_run"
	}
	entry.Metadata = map[string]string{
		"automatic":        strconv.FormatBool(evaluation.Automatic),
		"effective_action": safeAction(evaluation.Action),
		"mode":             mode,
		"proposed_action":  safeAction(evaluation.ProposedAction),
	}
	return m.recordAudit(paneIndex, entry)
}

func decisionForPolicyAction(action policy.Action) audit.Decision {
	switch action {
	case policy.ActionAllow:
		return audit.DecisionAllow
	case policy.ActionAsk:
		return audit.DecisionAsk
	case policy.ActionDeny:
		return audit.DecisionDeny
	default:
		return ""
	}
}

func decisionForAdapter(value adapters.Decision) audit.Decision {
	switch value {
	case adapters.DecisionAllow:
		return audit.DecisionAllow
	case adapters.DecisionDeny:
		return audit.DecisionDeny
	default:
		return audit.DecisionUnknown
	}
}

func (m *Model) recordDecision(
	paneIndex int,
	event adapters.Event,
	decision audit.Decision,
	actor audit.DecisionBy,
) bool {
	entry := m.eventAuditEntry(paneIndex, audit.KindDecision, event)
	entry.Decision = decision
	entry.DecisionBy = actor
	entry.Outcome = audit.OutcomeInFlight
	entry.Reason = "decision_selected"
	// Never even hand free-form prompt text to the recorder on a human-input
	// code path. The manual value is intentionally not a function argument.
	if actor == audit.DecisionByHuman {
		entry.Summary = ""
		entry.Metadata = nil
	}
	return m.recordAudit(paneIndex, entry)
}

func (m *Model) recordDelivery(
	paneIndex int,
	event adapters.Event,
	decision audit.Decision,
	actor audit.DecisionBy,
	outcome audit.Outcome,
	reason string,
) bool {
	entry := m.eventAuditEntry(paneIndex, audit.KindDelivery, event)
	entry.Decision = decision
	entry.DecisionBy = actor
	entry.Outcome = outcome
	entry.Reason = reason
	entry.Summary = ""
	entry.Metadata = nil
	return m.recordAudit(paneIndex, entry)
}

// recordOperatorInput persists only static lifecycle metadata. The submitted
// line and its length are intentionally not accepted as arguments.
func (m *Model) recordOperatorInput(
	paneIndex int,
	outcome audit.Outcome,
	reason string,
) bool {
	entry := audit.Entry{
		Kind:       audit.KindOperatorInput,
		DecisionBy: audit.DecisionByHuman,
		Outcome:    outcome,
		Reason:     reason,
	}
	if paneIndex >= 0 && paneIndex < len(m.panes) {
		pane := m.panes[paneIndex]
		entry.SessionID = pane.sessionID
		entry.AgentID = pane.sessionID
		entry.Backend = pane.backend
		entry.Adapter = pane.adapter
	}
	return m.recordAudit(paneIndex, entry)
}

func (m *Model) recordBackendError(paneIndex int, reason string) bool {
	entry := audit.Entry{
		Kind:       audit.KindBackendError,
		DecisionBy: audit.DecisionBySystem,
		Outcome:    audit.OutcomeFailed,
		Reason:     reason,
	}
	if paneIndex >= 0 && paneIndex < len(m.panes) {
		pane := m.panes[paneIndex]
		entry.SessionID = pane.sessionID
		entry.AgentID = pane.sessionID
		entry.Backend = pane.backend
		entry.Adapter = pane.adapter
	}
	return m.recordAudit(paneIndex, entry)
}

func (m *Model) recordAttach(
	paneIndex int,
	kind audit.Kind,
	outcome audit.Outcome,
	reason string,
) bool {
	entry := audit.Entry{
		Kind:       kind,
		DecisionBy: audit.DecisionByHuman,
		Outcome:    outcome,
		Reason:     reason,
	}
	if paneIndex >= 0 && paneIndex < len(m.panes) {
		pane := m.panes[paneIndex]
		entry.SessionID = pane.sessionID
		entry.AgentID = pane.sessionID
		entry.Backend = pane.backend
		entry.Adapter = pane.adapter
	}
	return m.recordAudit(paneIndex, entry)
}

func (m *Model) recordAttachFailure(paneIndex int, reason string) bool {
	if !m.recordAttach(paneIndex, audit.KindAttachFinished, audit.OutcomeFailed, reason) {
		return false
	}
	return m.recordBackendError(paneIndex, reason)
}

func automaticDeliveryAudit(err error) (audit.Outcome, string) {
	switch automaticFailureStatus(err) {
	case "unsupported":
		return audit.OutcomeFallbackUnsupported, "decision_unsupported"
	case "stale":
		return audit.OutcomeFallbackStale, "event_stale"
	case "delivery_uncertain":
		return audit.OutcomeFallbackDeliveryUncertain, "delivery_uncertain"
	default:
		return audit.OutcomeUnknown, "unknown"
	}
}

// recordEventWithdrawn notes that an occurrence stopped awaiting a human
// without a decision being delivered.
//
// Snapshot reconciliation withdraws a pending occurrence when the replayed
// screen no longer shows it, which is legitimate — the operator may have
// answered directly in tmux while attached. It is also the one path where a
// supervision gate opens with nobody recording it, so the audit needs to be
// able to distinguish "answered" from "stopped being asked". The record carries
// the occurrence identity only; the matched text is never part of the model.
func (m *Model) recordEventWithdrawn(paneIndex int, event adapters.Event) {
	if m.auditUnavailable || !event.Actionable() {
		return
	}
	entry := m.eventAuditEntry(paneIndex, audit.KindEventWithdrawn, event)
	entry.Outcome = audit.OutcomeCancelled
	entry.Reason = "resync_withdrew_occurrence"
	m.recordAudit(paneIndex, entry)
}
