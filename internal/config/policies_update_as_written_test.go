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
	// As both front ends save: with the preset the editor shows as chosen.
	updated, _, err := UpdateFullConfiguration(path, loaded.Revision, FullConfigurationUpdate{Policies: &requested, PolicyPreset: settings.Profile})
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

// A workspace root the file gives as an absolute or rooted path keeps its text
// through a save that does not change it, as a relative one does. The loader
// kept such a root as written, but the editor's save cleans it, so the two
// differed and a save that only toggled dry-run rewrote the root: on Windows
// "/srv/ws" became "\srv\ws", which Linux does not read as that directory, so
// in a configuration shared with it every path an agent named was outside the
// workspace; a trailing separator or a forward-slash Windows path was
// rewritten too.
func TestASecuritySaveKeepsAnAbsoluteWorkspaceRootAsWritten(t *testing.T) {
	directory := t.TempDir()
	workspace := filepath.Join(directory, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct{ name, root string }{
		{"rooted", "/srv/ws"},
		{"doubled and trailing separators", "/srv//ws/"},
		{"forward slashes", filepath.ToSlash(workspace)},
		{"trailing separator", workspace + string(filepath.Separator)},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := strings.Replace(handWrittenPoliciesDocument,
				"    block_outside_workspace: false # not yet\n    workspace_root: ./workspace\n",
				"    block_outside_workspace: true\n    workspace_root: '"+test.root+"'\n", 1)
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			loaded, err := LoadExisting(path)
			if err != nil {
				t.Fatalf("LoadExisting: %v", err)
			}
			settingsSave(t, path, loaded, func(settings *policy.Settings) { settings.DryRun = true })
			want := strings.Replace(document, "  dry_run: false\n", "  dry_run: true\n", 1)
			if got := readText(t, path); got != want {
				t.Fatalf("the security save rewrote the workspace root:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// Turning the workspace guardrail on in a file that names no root writes the
// guardrail and nothing else: the root it then uses is the configuration's
// directory, which the file leaves implicit. The save wrote that directory
// as an absolute workspace_root beside the guardrail, pinning the root to
// where the file was, and naming the user's home directory.
func TestTurningTheWorkspaceGuardrailOnWritesOnlyTheGuardrail(t *testing.T) {
	document := strings.Replace(handWrittenPoliciesDocument, "    workspace_root: ./workspace\n", "", 1)
	path, loaded := writeHandWrittenPolicies(t, document)
	updated := settingsSave(t, path, loaded, func(settings *policy.Settings) { settings.BlockOutsideWorkspace = true })
	want := strings.Replace(document, "block_outside_workspace: false # not yet\n", "block_outside_workspace: true # not yet\n", 1)
	if got := readText(t, path); got != want {
		t.Fatalf("turning the guardrail on wrote more than the guardrail:\n%s\nwant:\n%s", got, want)
	}
	if root := updated.Policies.Guardrails.WorkspaceRoot; root != filepath.Dir(path) {
		t.Fatalf("workspace root = %q, want the configuration's directory", root)
	}
}

// A preset chosen in the editor and then adjusted is still named in the file,
// with the adjusted field written beside it, and the profile line keeps its
// comments. Only a preset the policy matches exactly was tried, so choosing
// "strict" and then setting the rate limit to 5 removed the profile line and
// its comments, and wrote every field of strict out, the guardrails, the
// limits and the workspace root as an absolute path.
func TestAPresetChosenThenAdjustedIsNamedWhereTheFileNamesAProfile(t *testing.T) {
	document := strings.Replace(handWrittenPoliciesDocument, `policies:
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
`, `policies:
  # head
  profile: developer-friendly # base
  dry_run: false
`, 1)
	path, loaded := writeHandWrittenPolicies(t, document)
	settingsSave(t, path, loaded, func(settings *policy.Settings) {
		// The interface's presetSettings: the preset's values, the user's
		// workspace root and dry-run kept; then one field adjusted.
		root, dryRun := settings.WorkspaceRoot, settings.DryRun
		*settings = policy.PresetSettings()[string(policy.ProfileStrict)]
		settings.WorkspaceRoot, settings.DryRun = root, dryRun
		settings.RateLimitPerMinute = 5
	})
	want := strings.Replace(document, "  profile: developer-friendly # base\n  dry_run: false\n",
		"  profile: strict # base\n  dry_run: false\n  rate_limit_per_minute: 5\n", 1)
	if got := readText(t, path); got != want {
		t.Fatalf("the adjusted preset was not named in the file:\n%s\nwant:\n%s", got, want)
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

// handWrittenPoliciesBlock is the policies block of handWrittenPoliciesDocument.
const handWrittenPoliciesBlock = `policies:
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
`

// choosePreset is the interface's presetSettings: the preset's values, with
// the user's workspace root and dry-run kept.
func choosePreset(profile policy.Profile) func(*policy.Settings) {
	return func(settings *policy.Settings) {
		root, dryRun := settings.WorkspaceRoot, settings.DryRun
		*settings = policy.PresetSettings()[string(profile)]
		settings.WorkspaceRoot, settings.DryRun = root, dryRun
	}
}

// requireRootFollowsTheFile checks that the file at path names no absolute
// workspace root: a copy of it in another directory guards that directory.
func requireRootFollowsTheFile(t *testing.T, path string) {
	t.Helper()
	text := readText(t, path)
	if strings.Contains(text, filepath.Dir(path)) {
		t.Fatalf("the file names its own directory as an absolute path:\n%s", text)
	}
	moved := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(moved, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExisting(moved)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	if root := loaded.Policies.Guardrails.WorkspaceRoot; root != filepath.Dir(moved) {
		t.Fatalf("the moved file's workspace root = %q, want its new directory %q", root, filepath.Dir(moved))
	}
}

// Switching from strict to permissive keeps a workspace root the file leaves
// implicit relative. Strict guards the configuration's directory when the
// file names no root; permissive turns the guardrail off, and the editor
// keeps the root it shows, so the save had to write it, and wrote it as an
// absolute path naming the user's home directory, pinned to where the file
// was. It is now written ".", which keeps following the file.
func TestSwitchingFromStrictToPermissiveKeepsTheWorkspaceRootRelative(t *testing.T) {
	// As the editor writes strict in a file that names no profile.
	const spelledOut = `policies:
  default_action: ask
  max_consecutive_auto_decisions: 1
  rate_limit_per_minute: 10
  guardrails:
    block_destructive: true
    block_exfiltration: true
    block_sensitive_paths: true
    block_outside_workspace: true # the configuration's directory
`
	for _, test := range []struct {
		name, written string
		change        func(*policy.Settings)
		want          string
	}{
		{
			name:    "the file names the profile",
			written: "policies:\n  profile: strict # base\n",
			change:  choosePreset(policy.ProfilePermissive),
			want:    "policies:\n  profile: permissive # base\n  guardrails:\n    workspace_root: .\n",
		},
		{
			name:    "the file spells the preset out",
			written: spelledOut,
			change:  choosePreset(policy.ProfilePermissive),
			want: `policies:
  default_action: allow
  max_consecutive_auto_decisions: 50
  rate_limit_per_minute: 120
  guardrails:
    block_destructive: true
    block_exfiltration: true
    block_sensitive_paths: true
    block_outside_workspace: false # the configuration's directory
    workspace_root: .
`,
		},
		{
			name:    "the workspace guardrail is turned off",
			written: spelledOut,
			change:  func(settings *policy.Settings) { settings.BlockOutsideWorkspace = false },
			want: `policies:
  default_action: ask
  max_consecutive_auto_decisions: 1
  rate_limit_per_minute: 10
  guardrails:
    block_destructive: true
    block_exfiltration: true
    block_sensitive_paths: true
    block_outside_workspace: false # the configuration's directory
    workspace_root: .
`,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			document := strings.Replace(handWrittenPoliciesDocument, handWrittenPoliciesBlock, test.written, 1)
			if document == handWrittenPoliciesDocument {
				t.Fatal("the document has no policies block to replace")
			}
			path, loaded := writeHandWrittenPolicies(t, document)
			if root := loaded.Policies.Guardrails.WorkspaceRoot; root != filepath.Dir(path) {
				t.Fatalf("the workspace root before the save = %q, want the configuration's directory", root)
			}
			updated := settingsSave(t, path, loaded, test.change)
			want := strings.Replace(document, test.written, test.want, 1)
			if got := readText(t, path); got != want {
				t.Fatalf("the save did not keep the workspace root relative:\n%s\nwant:\n%s", got, want)
			}
			if root := updated.Policies.Guardrails.WorkspaceRoot; root != filepath.Dir(path) {
				t.Fatalf("workspace root = %q, want the configuration's directory", root)
			}
			requireRootFollowsTheFile(t, path)
		})
	}
}

// A policies block the file does not have yet is written whole, and a
// workspace root that is the configuration's directory is written "." in it
// too: choosing strict in a file without the block wrote that directory as
// an absolute path, which the next switch to permissive then kept.
func TestAPoliciesBlockWrittenWholeKeepsTheWorkspaceRootRelative(t *testing.T) {
	document := strings.Replace(handWrittenPoliciesDocument, handWrittenPoliciesBlock, "", 1)
	if document == handWrittenPoliciesDocument {
		t.Fatal("the document has no policies block to remove")
	}
	path, loaded := writeHandWrittenPolicies(t, document)
	loaded = settingsSave(t, path, loaded, choosePreset(policy.ProfileStrict))
	if !loaded.Policies.Guardrails.BlockOutsideWorkspace || loaded.Policies.Guardrails.WorkspaceRoot != filepath.Dir(path) {
		t.Fatalf("strict loads back as %+v, want the configuration's directory guarded", loaded.Policies.Guardrails)
	}
	requireRootFollowsTheFile(t, path)

	settingsSave(t, path, loaded, choosePreset(policy.ProfilePermissive))
	requireRootFollowsTheFile(t, path)
}
