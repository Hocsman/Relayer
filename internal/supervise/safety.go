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
	// maxDisplayToolNameRunes and maxDisplayToolValueRunes are the bounds the
	// adapters' tool-call parser already applies to a name and to a parameter
	// value; the display form keeps them whatever the parser becomes.
	maxDisplayToolNameRunes  = 64
	maxDisplayToolValueRunes = 256
	// redactedToolValue replaces a parameter value whose redaction could not
	// be told apart from its name.
	redactedToolValue = "[REDACTED]"
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

// displayToolCall is the only form of an event's MCP tool call that may be
// shown: none for an event whose text must not be shown, and otherwise its
// names and parameter values redacted and bounded. A parameter value is
// agent-controlled terminal text, which the web gateway sends to every client,
// viewers included; the adapters' parser bounds it but redacts nothing, and a
// token handed to a tool reached every screen as the agent printed it.
func displayToolCall(event adapters.Event) *adapters.ToolCall {
	if event.ToolCall == nil || requiresSecretHandling(event) {
		return nil
	}
	call := event.ToolCall
	shown := &adapters.ToolCall{
		Server:          boundedDisplayText(audit.Redact(call.Server), maxDisplayToolNameRunes, ""),
		Tool:            boundedDisplayText(audit.Redact(call.Tool), maxDisplayToolNameRunes, ""),
		Risk:            call.Risk,
		ParamsTruncated: call.ParamsTruncated,
	}
	if len(call.Params) > 0 {
		shown.Params = make([]adapters.ToolCallParam, 0, len(call.Params))
		for _, param := range call.Params {
			shown.Params = append(shown.Params, displayToolCallParam(param))
		}
	}
	return shown
}

// displayToolCallParam is one parameter of a tool call as it may be shown. Its
// value is redacted as the assignment it is: a password reads as no secret on
// its own, only under its name, so the value is redacted with its name before
// it, and the name taken off again. A redaction that took the name with it
// leaves the value redacted whole.
func displayToolCallParam(param adapters.ToolCallParam) adapters.ToolCallParam {
	assignment := param.Name + "="
	value, named := strings.CutPrefix(audit.Redact(assignment+param.Value), assignment)
	if !named {
		value = redactedToolValue
	}
	shown, cut := boundDisplayText(value, maxDisplayToolValueRunes, "")
	return adapters.ToolCallParam{
		Name:      boundedDisplayText(audit.Redact(param.Name), maxDisplayToolNameRunes, ""),
		Value:     shown,
		Truncated: param.Truncated || cut,
	}
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
	text, _ := boundDisplayText(value, limit, fallback)
	return text
}

// boundDisplayText is boundedDisplayText, and whether the limit cut the text.
func boundDisplayText(value string, limit int, fallback string) (string, bool) {
	value = strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' || character == '\t' || unicode.IsControl(character) {
			return ' '
		}
		return character
	}, value)
	value = strings.Join(strings.Fields(value), " ")
	if value == "" {
		return fallback, false
	}
	if utf8.RuneCountInString(value) <= limit {
		return value, false
	}
	runes := []rune(value)
	return string(runes[:limit-1]) + "…", true
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
		"guardrail_pattern_blocked", "consecutive_auto_limit", "rate_limit_exceeded",
		ReasonRepeatAfterDelivery, ReasonOperatorAttached:
		return value
	default:
		return "unknown"
	}
}
