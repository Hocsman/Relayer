package main

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

func requiresSecretHandling(event adapters.Event) bool {
	return event.Sensitive || event.Type == adapters.EventCredential || event.Risk == adapters.RiskHigh
}

func safeEventSummary(event adapters.Event) string {
	if requiresSecretHandling(event) {
		return "Sensitive input required"
	}
	return boundedDisplayText(audit.Redact(event.Summary), maxDisplaySummaryRunes, "Event detected")
}

func safeRuleName(value string) string {
	return boundedDisplayText(audit.Redact(value), 64, "")
}

func safeDisplayError(err error) string {
	if err == nil {
		return "Unknown error"
	}
	return boundedDisplayText(audit.Redact(err.Error()), maxDisplayErrorRunes, "Operation failed")
}

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

func safeReason(value string) string {
	switch value {
	case "default_action", "rule_match", "invalid_event", "non_actionable",
		"sensitive_event", "risk_not_low", "dry_run", "engine_unavailable",
		"event_detected", "process_exit", "decision_selected",
		"delivery_applied", "fallback_unsupported", "fallback_stale",
		"delivery_uncertain", "audit_unavailable", "runtime_stopped":
		return value
	default:
		return "unknown"
	}
}
