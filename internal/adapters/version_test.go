package adapters

import (
	"testing"
)

func TestIsVendorAdapter(t *testing.T) {
	for _, id := range []string{AiderID, ClaudeID, CodexID, GooseID, OpenInterpreterID} {
		if !IsVendorAdapter(id) {
			t.Errorf("expected %q to be vendor adapter", id)
		}
	}

	for _, id := range []string{GenericID, "custom", "", "unknown"} {
		if IsVendorAdapter(id) {
			t.Errorf("expected %q not to be vendor adapter", id)
		}
	}
}

func TestParseVersion(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"claude 2.1.285 (Claude Code)", "2.1.285"},
		{"2.1.285 (Claude Code)", "2.1.285"},
		{"aider 0.86.2", "0.86.2"},
		{"goose version 1.52.0", "1.52.0"},
		{"interpreter 0.4.3", "0.4.3"},
		{"codex-cli 0.148.0-alpha.21", "0.148.0-alpha.21"},
		{"v2.1.59", "2.1.59"},
		{"version V0.86.2-rc1", "0.86.2-rc1"},
		{"no version here", ""},
		{"", ""},
	}

	for _, tc := range tests {
		got := ParseVersion(tc.input)
		if got != tc.want {
			t.Errorf("ParseVersion(%q) = %q, want %q", tc.input, got, tc.want)
		}
	}
}

func TestCheckVersion(t *testing.T) {
	// Generic adapter: never unverified
	unverified, reason := CheckVersion(GenericID, "1.0.0")
	if unverified || reason != "" {
		t.Errorf("expected generic adapter not to be unverified, got %v (%s)", unverified, reason)
	}

	// Claude verified version
	unverified, reason = CheckVersion(ClaudeID, "2.1.285")
	if unverified {
		t.Errorf("expected claude 2.1.285 to be verified, got unverified with reason: %s", reason)
	}

	// Claude verified with 'v' prefix
	unverified, reason = CheckVersion(ClaudeID, "v2.1.59")
	if unverified {
		t.Errorf("expected claude v2.1.59 to be verified, got unverified with reason: %s", reason)
	}

	// Claude unverified version
	unverified, reason = CheckVersion(ClaudeID, "3.0.0")
	if !unverified {
		t.Errorf("expected claude 3.0.0 to be unverified")
	}
	if reason == "" {
		t.Errorf("expected unverified reason for claude 3.0.0")
	}

	// Claude missing version
	unverified, reason = CheckVersion(ClaudeID, "")
	if !unverified {
		t.Errorf("expected missing version to be unverified")
	}
	if reason == "" {
		t.Errorf("expected unverified reason for missing version")
	}

	// Aider verified version
	unverified, _ = CheckVersion(AiderID, "0.86.2")
	if unverified {
		t.Errorf("expected aider 0.86.2 to be verified")
	}

	// Aider unverified version
	unverified, _ = CheckVersion(AiderID, "0.87.0")
	if !unverified {
		t.Errorf("expected aider 0.87.0 to be unverified")
	}
}
