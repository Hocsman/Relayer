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
