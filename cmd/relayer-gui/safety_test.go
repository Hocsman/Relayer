package main

import (
	"testing"
)

func TestSafeReason(t *testing.T) {
	allowedReasons := []string{
		"default_action",
		"rule_match",
		"invalid_event",
		"non_actionable",
		"sensitive_event",
		"risk_not_low",
		"dry_run",
		"engine_unavailable",
		"event_detected",
		"process_exit",
		"decision_selected",
		"delivery_applied",
		"fallback_unsupported",
		"fallback_stale",
		"delivery_uncertain",
		"audit_unavailable",
		"runtime_stopped",
		"agent_withdrew_occurrence",
		"resync",
		"sensitive_path_blocked",
		"outside_workspace_blocked",
		"destructive_command_blocked",
		"exfiltration_attempt_blocked",
		"guardrail_pattern_blocked",
		"consecutive_auto_limit",
		"rate_limit_exceeded",
	}

	for _, reason := range allowedReasons {
		if got := safeReason(reason); got != reason {
			t.Errorf("safeReason(%q) = %q, want %q", reason, got, reason)
		}
	}

	// Unknown or arbitrary text must be sanitized to "unknown"
	unknowns := []string{
		"arbitrary_injection",
		"secret_token=12345",
		"",
		"some other reason",
	}
	for _, unk := range unknowns {
		if got := safeReason(unk); got != "unknown" {
			t.Errorf("safeReason(%q) = %q, want %q", unk, got, "unknown")
		}
	}
}
