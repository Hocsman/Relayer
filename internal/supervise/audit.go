package supervise

import (
	"strconv"
	"strings"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
)

// The builders below are the journal vocabulary of supervision: every entry
// the core writes about a prompt, a decision, its delivery or an operator line
// is shaped here, from display-safe fields only. They are exported while the
// desktop's state machine still calls them from its own package.

// EventAuditEntry is the common shape of an entry about one adapter event.
func EventAuditEntry(kind audit.Kind, event adapters.Event, backend string) audit.Entry {
	entry := audit.Entry{
		Kind:       kind,
		SessionID:  strings.TrimSpace(event.SessionID),
		AgentID:    strings.TrimSpace(event.AgentID),
		Backend:    strings.ToLower(strings.TrimSpace(backend)),
		Adapter:    strings.ToLower(strings.TrimSpace(event.Adapter)),
		EventID:    strings.TrimSpace(event.ID),
		EventType:  event.Type,
		Risk:       event.Risk,
		Summary:    SafeEventSummary(event),
		Sensitive:  RequiresSecretHandling(event),
		DecisionBy: audit.DecisionBySystem,
	}
	if event.Type == adapters.EventProcessExit {
		entry.Metadata = SafeExitMetadata(event.Metadata)
	}
	return entry
}

// EventDetectedEntry journals that a prompt or an exit was taken in.
func EventDetectedEntry(event adapters.Event, backend string) audit.Entry {
	entry := EventAuditEntry(audit.KindEventDetected, event, backend)
	entry.Outcome = audit.OutcomeDetected
	entry.Reason = "event_detected"
	return entry
}

// EventWithdrawnEntry journals that the agent took its question back.
func EventWithdrawnEntry(event adapters.Event, backend, reason string) audit.Entry {
	entry := EventAuditEntry(audit.KindEventWithdrawn, event, backend)
	entry.Outcome = audit.OutcomeCancelled
	entry.Reason = SafeReason(reason)
	return entry
}

// SafeExitMetadata keeps only the exit facts the journal may hold: whether the
// process failed and its numeric exit code.
func SafeExitMetadata(metadata map[string]string) map[string]string {
	if len(metadata) == 0 {
		return nil
	}
	result := make(map[string]string, 2)
	if metadata["failed"] == "true" {
		result["failed"] = "true"
	}
	if value := strings.TrimSpace(metadata["exit_code"]); value != "" {
		if _, err := strconv.Atoi(value); err == nil {
			result["exit_code"] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
}

// PolicyAuditEntry journals the policy's evaluation of a prompt.
func PolicyAuditEntry(event adapters.Event, backend string, evaluation policy.Evaluation) audit.Entry {
	entry := EventAuditEntry(audit.KindPolicyEvaluated, event, backend)
	entry.DecisionBy = audit.DecisionByPolicy
	entry.Rule = evaluation.RuleName
	entry.Reason = SafeReason(evaluation.Reason)
	entry.Decision = AuditDecisionForPolicy(evaluation.Action)
	entry.Outcome = audit.OutcomeAsk
	if evaluation.DryRun {
		entry.Outcome = audit.OutcomeDryRun
	} else if evaluation.Automatic {
		entry.Outcome = audit.OutcomeInFlight
	}
	entry.Metadata = map[string]string{
		"automatic":        strconv.FormatBool(evaluation.Automatic),
		"effective_action": string(evaluation.Action),
		"mode":             map[bool]string{true: "dry_run", false: "enforce"}[evaluation.DryRun],
		"proposed_action":  string(evaluation.ProposedAction),
	}
	return entry
}

// DecisionAuditEntry journals a decision before it is delivered.
func DecisionAuditEntry(
	event adapters.Event,
	backend string,
	decision audit.Decision,
	actor audit.DecisionBy,
) audit.Entry {
	entry := EventAuditEntry(audit.KindDecision, event, backend)
	entry.Decision = decision
	entry.DecisionBy = actor
	entry.Outcome = audit.OutcomeInFlight
	entry.Reason = "decision_selected"
	if actor == audit.DecisionByHuman {
		entry.Summary = ""
		entry.Metadata = nil
	}
	return entry
}

// DeliveryAuditEntry journals the terminal outcome of a decision's delivery.
func DeliveryAuditEntry(
	event adapters.Event,
	backend string,
	decision audit.Decision,
	actor audit.DecisionBy,
	outcome audit.Outcome,
	reason string,
) audit.Entry {
	entry := EventAuditEntry(audit.KindDelivery, event, backend)
	entry.Decision = decision
	entry.DecisionBy = actor
	entry.Outcome = outcome
	entry.Reason = SafeReason(reason)
	entry.Summary = ""
	entry.Metadata = nil
	return entry
}

// OperatorInputAuditEntry deliberately has no free-form input, summary,
// decision, event or metadata field. It records only the lifecycle of an
// operator-initiated line at the already-known session boundary.
func OperatorInputAuditEntry(agent AgentSpec, outcome audit.Outcome, reason string) audit.Entry {
	return audit.Entry{
		Kind:       audit.KindOperatorInput,
		SessionID:  strings.TrimSpace(agent.SessionID),
		AgentID:    strings.TrimSpace(agent.AgentID),
		Backend:    strings.ToLower(strings.TrimSpace(agent.Backend)),
		Adapter:    strings.ToLower(strings.TrimSpace(agent.Adapter)),
		DecisionBy: audit.DecisionByHuman,
		Outcome:    outcome,
		Reason:     reason,
	}
}

// AuditDecisionForPolicy names a policy action in the journal's vocabulary.
func AuditDecisionForPolicy(action policy.Action) audit.Decision {
	switch action {
	case policy.ActionAllow:
		return audit.DecisionAllow
	case policy.ActionDeny:
		return audit.DecisionDeny
	case policy.ActionAsk:
		return audit.DecisionAsk
	default:
		return audit.DecisionUnknown
	}
}

// AdapterDecisionForPolicy is the answer an automatic decision sends, and
// false for an action no adapter can encode on its own.
func AdapterDecisionForPolicy(action policy.Action) (adapters.Decision, bool) {
	switch action {
	case policy.ActionAllow:
		return adapters.DecisionAllow, true
	case policy.ActionDeny:
		return adapters.DecisionDeny, true
	default:
		return "", false
	}
}

// HumanAuditDecision names the answer a human actually gave. A free-text reply
// stays "ask": the journal records that a human was consulted and answered, not
// that the answer permitted anything — only the adapter knows what the bytes
// mean.
func HumanAuditDecision(decision adapters.Decision) audit.Decision {
	switch decision {
	case adapters.DecisionAllow:
		return audit.DecisionAllow
	case adapters.DecisionDeny:
		return audit.DecisionDeny
	default:
		return audit.DecisionAsk
	}
}
