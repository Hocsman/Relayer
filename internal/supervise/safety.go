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
	// maxDisplayCommandLines and maxDisplayCommandLineRunes bound the command a
	// prompt asks about: enough for a one-liner or a short script, not for a
	// payload that would push the controls off the screen.
	maxDisplayCommandLines     = 8
	maxDisplayCommandLineRunes = 200
	// maxCommandInputBytes bounds what is read of a command before it is split
	// and redacted: the adapters' detection window is 16 KiB, and the extractor
	// of a Claude Code command has no bound of its own.
	maxCommandInputBytes = 16 << 10
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
// or journaled: the adapter marked it sensitive, or it asks for a credential.
// High risk is not a secret: a high-risk prompt keeps an honest label, shows
// its summary bounded and redacted and takes a normal, visible answer. Only
// the journal still holds such an event under its constant label
// (audit.SanitizeEntry marks it sensitive there).
func requiresSecretHandling(event adapters.Event) bool {
	return event.Sensitive || event.Type == adapters.EventCredential
}

// safeEventSummary is the only form of an event's summary that may be shown
// or journaled.
func safeEventSummary(event adapters.Event) string {
	if requiresSecretHandling(event) {
		return "Sensitive input required"
	}
	return boundedDisplayText(audit.Redact(event.Summary), maxDisplaySummaryRunes, "Event detected")
}

// displayCommand is the only form of the command an event asks about that may
// be shown. It is none for an event whose text must not be shown and for one
// that is not a shell-command prompt: the generic adapter's Event.Command is a
// quoted fragment of the question ("config.yaml", "yes") that has lost the words
// that would let a redaction see what it is. Otherwise it is the command with
// each line redacted as the journal redacts values, line breaks kept, and lines
// and their lengths bounded. The adapters read it from the agent's own screen, so
// it is agent-controlled text: display data, never an instruction, and never
// journaled (the journal records a high-risk entry under a constant label).
//
// Each line is cleaned and then redacted on its own: a keyword at the end of one
// line must not mask the first word of the next, and a control character is a
// space before the redaction looks, as in the journal, or "password\x00is\x00x"
// would pass it. The redaction drops no text after a masked value
// (audit.RedactValues): a line that stops at "[REDACTED]" reads as a whole one.
// Line breaks are kept because flattening "rm -rf a" and "ls" onto one line reads
// as one command with arguments. A format character, such as a bidi override or a
// zero-width space, shows as U+FFFD: it reorders or hides what a person reads.
func displayCommand(event adapters.Event) string {
	if requiresSecretHandling(event) || !event.IsShellCommandPrompt() || strings.TrimSpace(event.Command) == "" {
		return ""
	}
	text := event.Command
	if len(text) > maxCommandInputBytes {
		text = strings.ToValidUTF8(text[:maxCommandInputBytes], "")
	}
	text = strings.NewReplacer("\r\n", "\n", "\r", "\n").Replace(text)
	lines := make([]string, 0, maxDisplayCommandLines)
	cut := false
	for _, line := range strings.Split(text, "\n") {
		line = displayCommandLine(line)
		if line == "" && len(lines) == 0 {
			continue
		}
		if len(lines) == maxDisplayCommandLines {
			if line != "" {
				cut = true
				break
			}
			continue
		}
		if utf8.RuneCountInString(line) > maxDisplayCommandLineRunes {
			line = string([]rune(line)[:maxDisplayCommandLineRunes-1]) + "…"
		}
		lines = append(lines, line)
	}
	for len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	if cut {
		lines = append(lines, "…")
	}
	return strings.Join(lines, "\n")
}

// displayCommandLine is one line of a command as it may be shown: control
// characters as spaces, then redacted, trailing space off, then format
// characters as a visible mark.
func displayCommandLine(line string) string {
	line = strings.Map(func(character rune) rune {
		if unicode.IsControl(character) {
			return ' '
		}
		return character
	}, line)
	line = strings.TrimRightFunc(audit.RedactValues(line), unicode.IsSpace)
	return strings.Map(func(character rune) rune {
		if unicode.Is(unicode.Cf, character) {
			return unicode.ReplacementChar
		}
		return character
	}, line)
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
		ReasonRepeatAfterDelivery, ReasonOperatorAttached, ReasonTypedAtTerminal:
		return value
	default:
		return "unknown"
	}
}
