package main

import (
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/config"
)

// interfaceInputs is the interface's profilesForSave
// (frontend/src/lib/agentProfiles.ts): a preserved profile is sent with no
// argv and Preserve.
func interfaceInputs(profiles []AgentProfile) []AgentProfileInput {
	inputs := make([]AgentProfileInput, len(profiles))
	for index, profile := range profiles {
		inputs[index] = AgentProfileInput{
			ID: profile.ID, Name: profile.Name, PresetID: profile.PresetID, Cwd: profile.Cwd,
			Backend: profile.Backend, Adapter: profile.Adapter, Preserve: profile.PreserveOnSave,
		}
		if !profile.PreserveOnSave {
			inputs[index].Argv = append([]string(nil), profile.Argv...)
		}
	}
	return inputs
}

// handWrittenDesktopAgents is a configuration a person wrote: comments, flow
// sequences, quoted arguments, agents that inherit the file's backend and an
// agent whose adapter is left to its executable.
const handWrittenDesktopAgents = `# Relayer configuration (hand-written)
version: 1
backend: pty
agents:
  # the main coding agent
  - id: claude
    name: Claude
    command: [claude, --model, opus]
    cwd: ./ws
    env:
      ANTHROPIC_API_KEY: sk-ant-DESKTOP-AS-WRITTEN-0001 # secret
  - id: reviewer
    name: Repository reviewer
    command: ["./bin/reviewer", "--mode", "read only", ""]
    cwd: ./ws
    adapter: generic
  - id: scripted
    name: Scripted
    shell: 'prepare-input | exec ./agent --mode "$RELAYER_MODE"'
    env:
      RELAYER_MODE: fast
  - id: codex
    name: Codex
    command: [codex] # inherits the backend
intercept_patterns:
  - pattern: (?i)\[[yn]/[yn]\]
    description: yes/no confirmation
`

// Renaming one agent on the desktop changes that agent's name line and
// nothing else in the file. The desktop kept every agent's data, but each save
// rebuilt every entry: the other agents lost their comments and flow
// sequences, the shell script's quoting changed, and every agent was pinned
// to "backend: pty".
func TestRenamingOneAgentOnTheDesktopLeavesEverythingElseAsWritten(t *testing.T) {
	application, path := profileTestApp(t, nil)
	if err := os.Mkdir(filepath.Join(filepath.Dir(path), "ws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(handWrittenDesktopAgents), 0o600); err != nil {
		t.Fatal(err)
	}
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	inputs := interfaceInputs(view.Profiles)
	inputs[1].Name = "Reviewer, renamed"
	if _, err := saveAgentProfilesForTest(application, SaveAgentProfilesRequest{
		ExpectedRevision: view.Revision,
		Profiles:         inputs,
	}); err != nil {
		t.Fatalf("SaveAgentProfiles: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(handWrittenDesktopAgents, "name: Repository reviewer", "name: Reviewer, renamed", 1); string(after) != want {
		t.Fatalf("the rename rewrote more than the name:\n%s\nwant:\n%s", after, want)
	}
}

// A security save on the desktop changes the fields it changed and keeps the
// rest of the policies block as written: "profile: custom", the guardrail set
// to false and the relative workspace root, which the block's rebuild
// dropped, dropped and wrote as an absolute path naming the user's home
// directory. One that changes nothing writes nothing.
func TestADesktopSecuritySaveKeepsThePolicyAsWritten(t *testing.T) {
	document := strings.Replace(handWrittenDesktopAgents, "backend: pty\nagents:\n", `backend: pty
policies:
  profile: custom
  default_action: ask
  dry_run: false
  rate_limit_per_minute: 30
  guardrails:
    block_destructive: true
    block_outside_workspace: false
    workspace_root: ./workspace
  rules:
    - name: deny-reviewer-rm
      match:
        agent_ids: [reviewer]
        command_regex: (?i)^rm\b
      action: deny
agents:
`, 1)
	for _, test := range []struct {
		name   string
		change func(*SecuritySettings)
		want   string
	}{
		{"dry-run toggled", func(security *SecuritySettings) { security.DryRun = true },
			strings.Replace(document, "  dry_run: false\n", "  dry_run: true\n", 1)},
		{"nothing changed", func(*SecuritySettings) {}, document},
	} {
		t.Run(test.name, func(t *testing.T) {
			application, path := profileTestApp(t, nil)
			for _, sub := range []string{"ws", "workspace"} {
				if err := os.Mkdir(filepath.Join(filepath.Dir(path), sub), 0o700); err != nil {
					t.Fatal(err)
				}
			}
			if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
				t.Fatal(err)
			}
			view, err := application.GetFullSettings()
			if err != nil {
				t.Fatalf("GetFullSettings: %v", err)
			}
			security := view.Security
			test.change(&security)
			if _, err := application.SaveFullSettings(activeRunIDForTest(application), SaveFullSettingsRequest{
				ExpectedRevision: view.Revision,
				Security:         &security,
			}); err != nil {
				t.Fatalf("SaveFullSettings: %v", err)
			}
			if after, _ := os.ReadFile(path); string(after) != test.want {
				t.Fatalf("the security save rewrote the policy:\n%s\nwant:\n%s", after, test.want)
			}
		})
	}
}

// A preset chosen on the desktop and then adjusted is named in the file where
// it names a profile, with the adjusted field beside it, as on the gateway.
// The writer was not told which preset was chosen and recognized only one the
// policy matched exactly, so the profile line and its comments were dropped
// and every field of the preset was written out.
func TestADesktopPresetChosenThenAdjustedIsNamedInTheFile(t *testing.T) {
	document := strings.Replace(handWrittenDesktopAgents, "backend: pty\nagents:\n",
		"backend: pty\npolicies:\n  # head\n  profile: developer-friendly # base\n  dry_run: false\nagents:\n", 1)
	application, path := profileTestApp(t, nil)
	if err := os.Mkdir(filepath.Join(filepath.Dir(path), "ws"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	view, err := application.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}
	// The interface's presetSettings: the preset's values, the user's
	// workspace root and dry-run kept; then one field adjusted.
	security := view.SecurityPresets["strict"]
	security.Profile = "strict"
	security.WorkspaceRoot, security.DryRun = view.Security.WorkspaceRoot, view.Security.DryRun
	security.RateLimitPerMinute = 5
	if _, err := application.SaveFullSettings(activeRunIDForTest(application), SaveFullSettingsRequest{
		ExpectedRevision: view.Revision,
		Security:         &security,
	}); err != nil {
		t.Fatalf("SaveFullSettings: %v", err)
	}
	want := strings.Replace(document, "  profile: developer-friendly # base\n  dry_run: false\n",
		"  profile: strict # base\n  dry_run: false\n  rate_limit_per_minute: 5\n", 1)
	if after, _ := os.ReadFile(path); string(after) != want {
		t.Fatalf("the adjusted preset was not named in the file:\n%s\nwant:\n%s", after, want)
	}
}

// A settings save that writes nothing leaves the revision the editor holds
// good, as the gateway's does: the settings are loaded twice, one save sends
// the tabs back unchanged, and a save prepared from the other load is then
// taken. The desktop issued a new token after every settings save, written or
// not, so the next save of any editor loaded before it was refused as stale.
func TestADesktopSaveThatChangesNothingLeavesTheRevisionAsItWas(t *testing.T) {
	for _, test := range []struct {
		name    string
		request func(FullSettingsView) SaveFullSettingsRequest
	}{
		{"unchanged security", func(view FullSettingsView) SaveFullSettingsRequest {
			security := view.Security
			return SaveFullSettingsRequest{ExpectedRevision: view.Revision, Security: &security}
		}},
		{"unchanged agents", func(view FullSettingsView) SaveFullSettingsRequest {
			return SaveFullSettingsRequest{ExpectedRevision: view.Revision, Profiles: interfaceInputs(view.Profiles)}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			application, path := profileTestApp(t, nil)
			if err := os.Mkdir(filepath.Join(filepath.Dir(path), "ws"), 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, []byte(handWrittenDesktopAgents), 0o600); err != nil {
				t.Fatal(err)
			}
			first, err := application.GetFullSettings()
			if err != nil {
				t.Fatalf("GetFullSettings: %v", err)
			}
			second, err := application.GetFullSettings()
			if err != nil {
				t.Fatalf("GetFullSettings: %v", err)
			}
			saved, err := application.SaveFullSettings(activeRunIDForTest(application), test.request(second))
			if err != nil {
				t.Fatalf("SaveFullSettings: %v", err)
			}
			if written, _ := os.ReadFile(path); string(written) != handWrittenDesktopAgents {
				t.Fatalf("a save that changed nothing wrote the file:\n%s", written)
			}
			if saved.Revision != first.Revision {
				t.Fatalf("a save that changed nothing issued revision %q, want %q", saved.Revision, first.Revision)
			}
			inputs := interfaceInputs(first.Profiles)
			inputs[1].Name = "Reviewer, renamed"
			if _, err := saveAgentProfilesForTest(application, SaveAgentProfilesRequest{
				ExpectedRevision: first.Revision,
				Profiles:         inputs,
			}); err != nil {
				t.Fatalf("a save prepared before a save that wrote nothing: %v", err)
			}
		})
	}
}

// The rule for a file edited while the editor was open, the same as the web
// gateway's: the save is refused as stale, whatever it sends, so it can neither
// drop an environment variable added in the YAML nor bring back one removed
// there; the environment a save keeps is the file's when it is written. Once
// reloaded, the editor saves, and the file's environment survives it.
func TestAnEnvironmentEditedWhileTheDesktopEditorWasOpenIsNeitherLostNorRevived(t *testing.T) {
	for _, test := range []struct {
		name string
		edit map[string]string
	}{
		{"a value added", map[string]string{"API_TOKEN": "fixture-token", "ADDED_IN_YAML": "yes"}},
		{"a value removed", map[string]string{"ADDED_ELSEWHERE": "kept"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			seeded := agent.Spec{ID: "keyed", Name: "Keyed", Command: []string{"runner"}, Cwd: directory,
				Env: map[string]string{"API_TOKEN": "fixture-token"}, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY}
			application, path := profileTestApp(t, []agent.Spec{seeded})
			stale, err := application.GetAgentProfiles()
			if err != nil {
				t.Fatalf("GetAgentProfiles: %v", err)
			}

			// Another editor changes the environment meanwhile.
			current, err := config.LoadExisting(path)
			if err != nil {
				t.Fatal(err)
			}
			edited := seeded
			edited.Env = test.edit
			if _, _, err := config.ReplaceAgents(path, current.Revision, []agent.Spec{edited}); err != nil {
				t.Fatalf("edit the environment: %v", err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			_, err = saveAgentProfilesForTest(application, SaveAgentProfilesRequest{
				ExpectedRevision: stale.Revision,
				Profiles:         interfaceInputs(stale.Profiles),
			})
			if !errors.Is(err, errProfilesStale) {
				t.Fatalf("a save prepared before the edit: %v, want errProfilesStale", err)
			}
			if after, _ := os.ReadFile(path); string(after) != string(before) {
				t.Fatalf("a stale save changed the file:\n%s", after)
			}

			fresh, err := application.GetAgentProfiles()
			if err != nil {
				t.Fatalf("GetAgentProfiles: %v", err)
			}
			fresh.Profiles[0].Name = "Renamed"
			if _, err := saveAgentProfilesForTest(application, SaveAgentProfilesRequest{
				ExpectedRevision: fresh.Revision,
				Profiles:         interfaceInputs(fresh.Profiles),
			}); err != nil {
				t.Fatalf("a save after reloading: %v", err)
			}
			reloaded, err := config.LoadExisting(path)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(reloaded.Agents[0].Env, test.edit) {
				t.Fatalf("environment after the save = %v, want the file's %v", reloaded.Agents[0].Env, test.edit)
			}
		})
	}
}
