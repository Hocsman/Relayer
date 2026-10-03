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
		product string
		input   string
		want    string
	}{
		{"claude", "claude 2.1.285 (Claude Code)", "2.1.285"},
		{"claude", "2.1.285 (Claude Code)", "2.1.285"},
		{"aider", "aider 0.86.2", "0.86.2"},
		{"goose", "goose version 1.52.0", "1.52.0"},
		{"interpreter", "interpreter 0.4.3", "0.4.3"},
		{"codex", "codex-cli 0.148.0-alpha.21", "0.148.0-alpha.21"},
		// A line that names no product carries no version, however
		// version-shaped it is.
		{"claude", "v2.1.59", ""},
		{"aider", "version V0.86.2-rc1", ""},
		{"claude", "no version here", ""},
		{"claude", "", ""},
		{"", "claude 2.1.285", ""},
		// Noisy output: a launcher's own version, a banner, then the
		// product's line. The first number in the stream is not the
		// agent's version.
		{"claude", "npm warn node v99.0.0\nhelper banner 9.9.9\nclaude 2.1.285 (Claude Code)", "2.1.285"},
		{"codex", "codex-cli 0.148.0\nwarning: telemetry endpoint 10.0.0.1 unreachable", "0.148.0"},
		// The product's line without a version on it is not a version.
		{"claude", "claude\n2.1.285", ""},
	}

	for _, tc := range tests {
		got := ParseVersion(tc.input, tc.product)
		if got != tc.want {
			t.Errorf("ParseVersion(%q, %q) = %q, want %q", tc.input, tc.product, got, tc.want)
		}
	}
}

func TestVendorExecutable(t *testing.T) {
	for adapterID, want := range map[string]string{
		AiderID: "aider", ClaudeID: "claude", CodexID: "codex",
		GooseID: "goose", OpenInterpreterID: "interpreter",
	} {
		if got := VendorExecutable(adapterID); got != want {
			t.Errorf("VendorExecutable(%q) = %q, want %q", adapterID, got, want)
		}
	}
	for _, adapterID := range []string{GenericID, "custom", "", "unknown"} {
		if got := VendorExecutable(adapterID); got != "" {
			t.Errorf("VendorExecutable(%q) = %q, want no executable", adapterID, got)
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
