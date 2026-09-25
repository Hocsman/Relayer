package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/policy"
)

// handWrittenPoliciesDocument is a policy a person wrote: a profile named, a
// guardrail switched off explicitly, a relative workspace root, and a rule in
// flow style.
const handWrittenPoliciesDocument = `# Relayer configuration (hand-written)
version: 1
backend: pty
policies:
  profile: custom
  default_action: ask
  dry_run: false
  rate_limit_per_minute: 30
  max_consecutive_auto_decisions: 5
  guardrails:
    block_destructive: true
    block_outside_workspace: false # not yet
    workspace_root: ./workspace
    blocked_patterns:
      - (?i)terraform\s+destroy
  rules:
    - name: deny-reviewer-rm
      match:
        agent_ids: [reviewer]
        command_regex: (?i)^rm\b
      action: deny
agents:
  - id: reviewer
    name: Reviewer
    command: [reviewer]
intercept_patterns:
  - pattern: (?i)\[[yn]/[yn]\]
    description: yes/no confirmation
`

func writeHandWrittenPolicies(t *testing.T, document string) (string, Result) {
	t.Helper()
	directory := t.TempDir()
	if err := os.Mkdir(filepath.Join(directory, "workspace"), 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExisting(path)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	return path, loaded
}

func settingsSave(t *testing.T, path string, loaded Result, change func(*policy.Settings)) Result {
	t.Helper()
	settings := policy.SettingsFrom(loaded.Policies)
	change(&settings)
	requested, err := policy.ApplySettings(loaded.Policies, settings, filepath.Dir(path))
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	updated, _, err := UpdateFullConfiguration(path, loaded.Revision, FullConfigurationUpdate{Policies: &requested})
	if err != nil {
		t.Fatalf("UpdateFullConfiguration: %v", err)
	}
	// Compared as they are written: an absent and an empty list are the same.
	if !reflect.DeepEqual(configuredPoliciesFrom(updated.Policies), configuredPoliciesFrom(requested)) {
		t.Fatalf("the saved policy loads back as\n %+v\nwant\n %+v", updated.Policies, requested)
	}
	return updated
}

// A security save that toggles dry-run changes the dry_run line and nothing
// else. The policies block was rebuilt from the effective policy: it dropped
// "profile: custom" and every guardrail set to false, wrote the relative
// workspace root as an absolute path naming the user's home directory, and
// lost the comments and flow style of the whole block.
func TestASecuritySaveChangesOnlyWhatItChanged(t *testing.T) {
	path, loaded := writeHandWrittenPolicies(t, handWrittenPoliciesDocument)
	settingsSave(t, path, loaded, func(settings *policy.Settings) { settings.DryRun = true })
	want := strings.Replace(handWrittenPoliciesDocument, "  dry_run: false\n", "  dry_run: true\n", 1)
	if got := readText(t, path); got != want {
		t.Fatalf("the security save rewrote more than dry_run:\n%s\nwant:\n%s", got, want)
	}
}

// A security save that changes nothing writes nothing.
func TestASecuritySaveThatChangesNothingWritesNothing(t *testing.T) {
	path, loaded := writeHandWrittenPolicies(t, handWrittenPoliciesDocument)
	updated := settingsSave(t, path, loaded, func(*policy.Settings) {})
	if got := readText(t, path); got != handWrittenPoliciesDocument || updated.Revision != loaded.Revision {
		t.Fatalf("an unchanged security save rewrote the file:\n%s", got)
	}
}

// A field the save changes is written where it was, and a new one is added to
// its section; the profile, the guardrails set to false and the relative root
// stay as written.
func TestASecuritySaveWritesAChangedFieldInPlace(t *testing.T) {
	path, loaded := writeHandWrittenPolicies(t, handWrittenPoliciesDocument)
	settingsSave(t, path, loaded, func(settings *policy.Settings) {
		settings.RateLimitPerMinute = 0
		settings.BlockExfiltration = true
	})
	want := strings.Replace(handWrittenPoliciesDocument, "  rate_limit_per_minute: 30\n", "  rate_limit_per_minute: 0\n", 1)
	want = strings.Replace(want, "      - (?i)terraform\\s+destroy\n", "      - (?i)terraform\\s+destroy\n    block_exfiltration: true\n", 1)
	if got := readText(t, path); got != want {
		t.Fatalf("the changed fields were not written in place:\n%s\nwant:\n%s", got, want)
	}
}

// Choosing a preset in the editor names it in the file when the file names a
// profile, so the file does not keep saying "strict" over a permissive
// policy; the policy loads back as the editor asked either way.
func TestChoosingAPresetNamesItWhereTheFileNamesAProfile(t *testing.T) {
	document := strings.Replace(handWrittenPoliciesDocument, "  profile: custom\n", "  profile: strict\n", 1)
	path, loaded := writeHandWrittenPolicies(t, document)
	settingsSave(t, path, loaded, func(settings *policy.Settings) {
		*settings = policy.PresetSettings()[string(policy.ProfilePermissive)]
	})
	if got := readText(t, path); !strings.Contains(got, "  profile: permissive\n") {
		t.Fatalf("the file does not name the preset chosen:\n%s", got)
	}
}
