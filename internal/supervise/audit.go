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
// is shaped here, from display-safe fields only.

// eventAuditEntry is the common shape of an entry about one adapter event.
func eventAuditEntry(kind audit.Kind, event adapters.Event, backend string) audit.Entry {
	entry := audit.Entry{
		Kind:       kind,
		SessionID:  strings.TrimSpace(event.SessionID),
		AgentID:    strings.TrimSpace(event.AgentID),
		Backend:    strings.ToLower(strings.TrimSpace(backend)),
		Adapter:    strings.ToLower(strings.TrimSpace(event.Adapter)),
		EventID:    strings.TrimSpace(event.ID),
		EventType:  event.Type,
		Risk:       event.Risk,
		Summary:    safeEventSummary(event),
		Sensitive:  requiresSecretHandling(event),
		DecisionBy: audit.DecisionBySystem,
	}
	if event.Type == adapters.EventProcessExit {
		entry.Metadata = safeExitMetadata(event.Metadata)
	}
	return entry
}

// eventDetectedEntry journals that a prompt or an exit was taken in.
func eventDetectedEntry(event adapters.Event, backend string) audit.Entry {
	entry := eventAuditEntry(audit.KindEventDetected, event, backend)
	entry.Outcome = audit.OutcomeDetected
	entry.Reason = "event_detected"
	return entry
}

// eventWithdrawnEntry journals that the agent took its question back.
func eventWithdrawnEntry(event adapters.Event, backend, reason string) audit.Entry {
	entry := eventAuditEntry(audit.KindEventWithdrawn, event, backend)
	entry.Outcome = audit.OutcomeCancelled
	entry.Reason = safeReason(reason)
	return entry
}

// safeExitMetadata keeps only the exit facts the journal may hold: whether the
// process failed and its numeric exit code.
func safeExitMetadata(metadata map[string]string) map[string]string {
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

// policyAuditEntry journals the policy's evaluation of a prompt.
func policyAuditEntry(event adapters.Event, backend string, evaluation policy.Evaluation) audit.Entry {
	entry := eventAuditEntry(audit.KindPolicyEvaluated, event, backend)
	entry.DecisionBy = audit.DecisionByPolicy
	entry.Rule = evaluation.RuleName
	entry.Reason = safeReason(evaluation.Reason)
	entry.Decision = auditDecisionForPolicy(evaluation.Action)
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

// decisionAuditEntry journals a decision before it is delivered.
func decisionAuditEntry(
	event adapters.Event,
	backend string,
	decision audit.Decision,
	actor audit.DecisionBy,
) audit.Entry {
	entry := eventAuditEntry(audit.KindDecision, event, backend)
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

// policyDecisionAuditEntry journals the policy's decision before it is
// delivered, under the rule that made it, named as the policy's evaluation
// entry names it.
func policyDecisionAuditEntry(event adapters.Event, backend string, evaluation policy.Evaluation) audit.Entry {
	entry := decisionAuditEntry(event, backend, auditDecisionForPolicy(evaluation.Action), audit.DecisionByPolicy)
	entry.Rule = evaluation.RuleName
	return entry
}

// deliveryAuditEntry journals the terminal outcome of a decision's delivery.
func deliveryAuditEntry(
	event adapters.Event,
	backend string,
	decision audit.Decision,
	actor audit.DecisionBy,
	outcome audit.Outcome,
	reason string,
) audit.Entry {
	entry := eventAuditEntry(audit.KindDelivery, event, backend)
	entry.Decision = decision
	entry.DecisionBy = actor
	entry.Outcome = outcome
	entry.Reason = safeReason(reason)
	entry.Summary = ""
	entry.Metadata = nil
	return entry
}

// operatorInputAuditEntry deliberately has no free-form input, summary,
// decision, event or metadata field. It records only the lifecycle of an
// operator-initiated line at the already-known session boundary, and who sent
// it: the actor's identity, when its front end named one, and nothing else of
// the actor, whose role and connection would need the metadata this shape
// does not have.
func operatorInputAuditEntry(agent Agent, actor Actor, outcome audit.Outcome, reason string) audit.Entry {
	return audit.Entry{
		Kind:       audit.KindOperatorInput,
		SessionID:  strings.TrimSpace(agent.SessionID),
		AgentID:    strings.TrimSpace(agent.AgentID),
		Backend:    strings.ToLower(strings.TrimSpace(agent.Backend)),
		Adapter:    strings.ToLower(strings.TrimSpace(agent.Adapter)),
		DecisionBy: audit.DecisionByHuman,
		Operator:   strings.TrimSpace(actor.Identity),
		Outcome:    outcome,
		Reason:     reason,
	}
}

// auditDecisionForPolicy names a policy action in the journal's vocabulary.
func auditDecisionForPolicy(action policy.Action) audit.Decision {
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

// adapterDecisionForPolicy is the answer an automatic decision sends, and
// false for an action no adapter can encode on its own.
func adapterDecisionForPolicy(action policy.Action) (adapters.Decision, bool) {
	switch action {
	case policy.ActionAllow:
		return adapters.DecisionAllow, true
	case policy.ActionDeny:
		return adapters.DecisionDeny, true
	default:
		return "", false
	}
}

// humanAuditDecision names the answer a human actually gave. A free-text reply
// stays "ask": the journal records that a human was consulted and answered, not
// that the answer permitted anything — only the adapter knows what the bytes
// mean.
func humanAuditDecision(decision adapters.Decision) audit.Decision {
	switch decision {
	case adapters.DecisionAllow:
		return audit.DecisionAllow
	case adapters.DecisionDeny:
		return audit.DecisionDeny
	default:
		return audit.DecisionAsk
	}
}
