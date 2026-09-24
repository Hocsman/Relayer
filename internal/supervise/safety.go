package supervise

import (
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
)

const (
	maxDisplaySummaryRunes = 120
	maxDisplayErrorRunes   = 180
)

// requiresSecretHandling reports whether an event's text must never be shown
// or journaled: the adapter marked it sensitive, it asks for a credential, or
// its risk is high.
func requiresSecretHandling(event adapters.Event) bool {
	return event.Sensitive || event.Type == adapters.EventCredential || event.Risk == adapters.RiskHigh
}

// safeEventSummary is the only form of an event's summary that may be shown
// or journaled.
func safeEventSummary(event adapters.Event) string {
	if requiresSecretHandling(event) {
		return "Sensitive input required"
	}
	return boundedDisplayText(audit.Redact(event.Summary), maxDisplaySummaryRunes, "Event detected")
}

// safeRuleName bounds and redacts a policy rule name for display.
func safeRuleName(value string) string {
	return boundedDisplayText(audit.Redact(value), 64, "")
}

// SafeDisplayError bounds and redacts an error for display.
func SafeDisplayError(err error) string {
	if err == nil {
		return "Unknown error"
	}
	return boundedDisplayText(audit.Redact(err.Error()), maxDisplayErrorRunes, "Operation failed")
}

// boundedDisplayText flattens a text to one line of at most limit runes, or
// returns fallback when nothing printable is left.
func boundedDisplayText(value string, limit int, fallback string) string {
	value = strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' || character == '\t' || unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return fallback
	}
	if utf8.RuneCountInString(value) <= limit {
		return value
	}
	runes := []rune(value)
	return string(runes[:limit-1]) + "…"
}

// safeReason passes a reason code through only when it is one the core
// itself produces, and replaces anything else with "unknown".
func safeReason(value string) string {
	switch value {
	case "default_action", "rule_match", "invalid_event", "non_actionable",
		"sensitive_event", "risk_not_low", "dry_run", "engine_unavailable",
		"event_detected", "process_exit", "process_exit_stale", "decision_selected",
		"delivery_applied", "fallback_unsupported", "fallback_stale",
		"delivery_uncertain", "audit_unavailable", "runtime_stopped",
		"agent_withdrew_occurrence", "resync",
		"sensitive_path_blocked", "outside_workspace_blocked",
		"destructive_command_blocked", "exfiltration_attempt_blocked",
		"guardrail_pattern_blocked", "consecutive_auto_limit", "rate_limit_exceeded":
		return value
	default:
		return "unknown"
	}
}
