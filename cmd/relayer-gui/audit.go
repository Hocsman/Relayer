package main

import (
	"strconv"
	"strings"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
)

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
// operator-initiated line at the already-known session boundary.
func operatorInputAuditEntry(agent AgentState, outcome audit.Outcome, reason string) audit.Entry {
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
