package server

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/config"
)

// agentsProbeSecret is a credential in an agent's environment, where the
// documentation sends API keys.
const agentsProbeSecret = "sk-ant-WEB-PROFILES-SECRET-0001"

// handWrittenAgents is a configuration a person wrote: one agent of each kind
// the web editor cannot show as it is, and one it can.
const handWrittenAgents = `# Relayer configuration (hand-written)
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
    block_outside_workspace: false
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
  # the main coding agent
  - id: claude
    name: Claude
    command: [claude, --model, opus]
    cwd: ./ws
    env:
      ANTHROPIC_API_KEY: ` + agentsProbeSecret + ` # secret
      RELAYER_ROLE: coder
  - id: reviewer
    name: Repository reviewer
    command: ["./bin/reviewer", "--mode", "read only", ""]
    cwd: ./ws
    adapter: generic
    backend: pty
  - id: scripted
    name: Scripted
    shell: 'prepare-input | exec ./agent --mode "$RELAYER_MODE"'
    env:
      RELAYER_MODE: fast
  - id: codex
    name: Codex
    command: [codex]
intercept_patterns:
  - pattern: (?i)\[[yn]/[yn]\]
    description: yes/no confirmation
notifications:
  enabled: true
  bell: false
  desktop: false
  min_severity: info
`

// handWrittenController writes the configuration into a directory of its own,
// points the user's directories there, and returns a controller on it that
// runs nothing.
func handWrittenController(t *testing.T, yaml string) (*Controller, string) {
	t.Helper()
	root := t.TempDir()
	for _, name := range []string{"APPDATA", "LOCALAPPDATA", "HOME", "USERPROFILE", "XDG_CONFIG_HOME"} {
		t.Setenv(name, root)
	}
	dir := filepath.Join(root, "cfg")
	for _, sub := range []string{"ws", "workspace", "bin"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	ctrl, err := NewController(path, io.Discard)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	return ctrl, path
}

// interfaceProfiles is the interface's profilesForSave
// (frontend/src/lib/agentProfiles.ts), field for field and JSON name for JSON
// name: a preserved profile is sent with no argv and "preserve".
func interfaceProfiles(profiles []AgentProfile) []map[string]any {
	sent := make([]map[string]any, 0, len(profiles))
	for _, profile := range profiles {
		argv := []string{}
		if !profile.PreserveOnSave {
			argv = append(argv, profile.Argv...)
		}
		sent = append(sent, map[string]any{
			"id": profile.ID, "name": profile.Name, "presetID": profile.PresetID, "cwd": profile.Cwd,
			"backend": profile.Backend, "adapter": profile.Adapter, "argv": argv,
			"preserve": profile.PreserveOnSave,
		})
	}
	return sent
}

// decodedAsTheGatewayDoes encodes payload and decodes it into T, as the
// websocket dispatcher decodes a request.
func decodedAsTheGatewayDoes[T any](t *testing.T, payload any) T {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	var decoded T
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatalf("decode %T: %v", decoded, err)
	}
	return decoded
}

func loadAgents(t *testing.T, path string) []agent.Spec {
	t.Helper()
	loaded, err := config.LoadExisting(path)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	return loaded.Agents
}

// The editor is shown nothing it could not send back as it is: an agent with
// environment variables or a shell script is read-only, as
// docs/configuration.md says, and no existing command, environment value or
// shell body reaches the browser, an operator's included. Every profile names a
// catalogue entry and the adapter the agent runs with, so the form validates:
// the gateway named no entry for a custom command or a shell agent and showed
// a blank adapter as blank, which the form refuses, so a hand-written
// configuration could not be saved from the web interface at all.
func TestTheWebProfileViewShowsNothingItCannotRoundTrip(t *testing.T) {
	ctrl, _ := handWrittenController(t, handWrittenAgents)
	view, err := ctrl.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	encoded, err := json.Marshal(view.Profiles)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{agentsProbeSecret, "ANTHROPIC_API_KEY", "RELAYER_ROLE", "RELAYER_MODE", "prepare-input", "opus", "bin/reviewer", "read only"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("the operator's profiles carry %q:\n%s", forbidden, encoded)
		}
	}
	want := map[string]struct {
		locked   bool
		reason   string
		preset   string
		adapter  string
		argCount int
	}{
		"claude":   {true, "advanced_environment", "custom", "claude", 0},
		"reviewer": {false, "", "custom", "generic", 3},
		"scripted": {true, "advanced_shell", "custom", "generic", 0},
		"codex":    {false, "", "codex-cli", "codex", 0},
	}
	for _, profile := range view.Profiles {
		expected, known := want[profile.ID]
		if !known {
			t.Fatalf("unexpected profile %q", profile.ID)
		}
		if profile.Locked != expected.locked || profile.ReadOnlyReason != expected.reason ||
			profile.PresetID != expected.preset || profile.Adapter != expected.adapter ||
			profile.ArgumentCount != expected.argCount || !profile.PreserveOnSave || len(profile.Argv) != 0 {
			t.Errorf("profile %s = %+v, want %+v, preserved and with no argv", profile.ID, profile, expected)
		}
	}
}

// A save of the Agents tab keeps every agent it did not change, whichever of
// the gateway's three save paths it takes: each agent's environment, a shell
// script, a blank adapter and a relative working directory come back as they
// were. The gateway rebuilt each agent from the form alone: it dropped every
// environment variable, API keys included, turned the shell agent into
// `command: [scripted]`, and wrote the catalogue's adapter over a blank one.
func TestAWebSaveKeepsEveryAgentItDidNotChange(t *testing.T) {
	for _, path := range []string{"saveAgentProfiles", "saveFullSettings", "saveAgentProfilesAndRestart"} {
		t.Run(path, func(t *testing.T) {
			ctrl, configPath := handWrittenController(t, handWrittenAgents)
			before := loadAgents(t, configPath)
			view, err := ctrl.GetAgentProfiles()
			if err != nil {
				t.Fatalf("GetAgentProfiles: %v", err)
			}
			request := map[string]any{"expectedRevision": view.Revision, "profiles": interfaceProfiles(view.Profiles)}
			switch path {
			case "saveAgentProfiles":
				_, err = ctrl.SaveAgentProfiles("", decodedAsTheGatewayDoes[SaveAgentProfilesRequest](t, request))
			case "saveFullSettings":
				_, err = ctrl.SaveFullSettings("", decodedAsTheGatewayDoes[SaveFullSettingsRequest](t, request))
			case "saveAgentProfilesAndRestart":
				// The write "Save and restart" makes before it restarts; the
				// restart itself is TestAWebRestartKeepsEachAgentsEnvironment's.
				ctrl.mu.Lock()
				err = ctrl.saveRestartConfigurationLocked(decodedAsTheGatewayDoes[SaveAgentProfilesAndRestartRequest](t, request))
				ctrl.mu.Unlock()
			}
			if err != nil {
				t.Fatalf("%s: %v", path, err)
			}
			if after := loadAgents(t, configPath); !reflect.DeepEqual(after, before) {
				t.Fatalf("%s changed agents it was sent back unchanged:\n got %#v\nwant %#v", path, after, before)
			}
		})
	}
}

// Renaming one agent from the web interface changes that agent's name line
// and nothing else in the file: the other agents keep their comments, flow
// sequences, quoting, and the backend they inherit. Every save rewrote every
// agent, pinning each to "backend: pty".
func TestAWebRenameLeavesEverythingElseAsWritten(t *testing.T) {
	ctrl, configPath := handWrittenController(t, handWrittenAgents)
	view, err := ctrl.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	profiles := interfaceProfiles(view.Profiles)
	profiles[1]["name"] = "Reviewer, renamed"
	request := map[string]any{"expectedRevision": view.Revision, "profiles": profiles}
	if _, err := ctrl.SaveAgentProfiles("", decodedAsTheGatewayDoes[SaveAgentProfilesRequest](t, request)); err != nil {
		t.Fatalf("SaveAgentProfiles: %v", err)
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if want := strings.Replace(handWrittenAgents, "name: Repository reviewer", "name: Reviewer, renamed", 1); string(after) != want {
		t.Fatalf("the rename rewrote more than the name:\n%s\nwant:\n%s", after, want)
	}
}

// "Restart the agents" with nothing edited writes nothing: the file keeps its
// bytes and its revision, and the run simply restarts.
func TestAWebRestartWithNothingEditedWritesNothing(t *testing.T) {
	ctrl, configPath := handWrittenController(t, handWrittenAgents)
	view, err := ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}
	request := decodedAsTheGatewayDoes[SaveAgentProfilesAndRestartRequest](t, map[string]any{
		"expectedRevision": view.Revision,
		"profiles":         interfaceProfiles(view.Profiles),
	})
	ctrl.mu.Lock()
	err = ctrl.saveRestartConfigurationLocked(request)
	ctrl.mu.Unlock()
	if err != nil {
		t.Fatalf("saveRestartConfigurationLocked: %v", err)
	}
	if after, _ := os.ReadFile(configPath); string(after) != handWrittenAgents {
		t.Fatalf("a restart with nothing edited rewrote the file:\n%s", after)
	}
}

// A read-only agent can only be changed in the YAML: a save that leaves it
// out, sends it as a new agent or with a command, or preserves an agent the
// file does not have, is refused and writes nothing.
func TestAWebSaveCannotDropOrReplaceAReadOnlyAgent(t *testing.T) {
	for _, test := range []struct {
		name  string
		alter func([]map[string]any) []map[string]any
	}{
		{"left out", func(profiles []map[string]any) []map[string]any { return profiles[1:] }},
		{"sent as a new agent", func(profiles []map[string]any) []map[string]any {
			profiles[0]["preserve"] = false
			profiles[0]["presetID"] = "claude-code"
			profiles[0]["argv"] = []string{"claude"}
			return profiles
		}},
		{"sent with a command", func(profiles []map[string]any) []map[string]any {
			profiles[2]["argv"] = []string{"sh", "-c", "true"}
			return profiles
		}},
		{"an agent the file does not have, preserved", func(profiles []map[string]any) []map[string]any {
			return append(profiles, map[string]any{"id": "ghost", "name": "Ghost", "presetID": "custom", "backend": "pty", "preserve": true})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctrl, configPath := handWrittenController(t, handWrittenAgents)
			view, err := ctrl.GetAgentProfiles()
			if err != nil {
				t.Fatalf("GetAgentProfiles: %v", err)
			}
			request := map[string]any{"expectedRevision": view.Revision, "profiles": test.alter(interfaceProfiles(view.Profiles))}
			if _, err := ctrl.SaveAgentProfiles("", decodedAsTheGatewayDoes[SaveAgentProfilesRequest](t, request)); err == nil {
				t.Fatal("the save was taken")
			} else if strings.Contains(err.Error(), agentsProbeSecret) {
				t.Fatalf("the refusal quotes the agent's environment: %v", err)
			}
			if _, err := ctrl.SaveFullSettings("", decodedAsTheGatewayDoes[SaveFullSettingsRequest](t, request)); err == nil {
				t.Fatal("the settings save was taken")
			}
			after, err := os.ReadFile(configPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(after) != handWrittenAgents {
				t.Fatalf("a refused save changed the file:\n%s", after)
			}
		})
	}
}

// The rule for a file edited while the editor was open: the save is refused as
// stale, whatever it sends, so it can neither drop an environment variable
// added in the YAML nor bring back one removed there; the value a save keeps
// is the file's at the moment it is written. Once reloaded, the editor saves,
// and the file's environment survives it.
func TestAnEnvironmentEditedWhileTheWebEditorWasOpenIsNeitherLostNorRevived(t *testing.T) {
	for _, test := range []struct {
		name, from, to string
	}{
		{"a value added", "      RELAYER_ROLE: coder\n", "      RELAYER_ROLE: coder\n      ADDED_IN_YAML: yes\n"},
		{"a value removed", "      RELAYER_ROLE: coder\n", ""},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctrl, configPath := handWrittenController(t, handWrittenAgents)
			stale, err := ctrl.GetAgentProfiles()
			if err != nil {
				t.Fatalf("GetAgentProfiles: %v", err)
			}
			edited := strings.Replace(handWrittenAgents, test.from, test.to, 1)
			if err := os.WriteFile(configPath, []byte(edited), 0o600); err != nil {
				t.Fatal(err)
			}
			request := map[string]any{"expectedRevision": stale.Revision, "profiles": interfaceProfiles(stale.Profiles)}
			if _, err := ctrl.SaveAgentProfiles("", decodedAsTheGatewayDoes[SaveAgentProfilesRequest](t, request)); !errors.Is(err, errStaleRevision) {
				t.Fatalf("a save prepared before the edit: %v, want errStaleRevision", err)
			}
			if after, _ := os.ReadFile(configPath); string(after) != edited {
				t.Fatalf("a stale save changed the file:\n%s", after)
			}

			want := loadAgents(t, configPath)
			fresh, err := ctrl.GetAgentProfiles()
			if err != nil {
				t.Fatalf("GetAgentProfiles: %v", err)
			}
			request = map[string]any{"expectedRevision": fresh.Revision, "profiles": interfaceProfiles(fresh.Profiles)}
			if _, err := ctrl.SaveAgentProfiles("", decodedAsTheGatewayDoes[SaveAgentProfilesRequest](t, request)); err != nil {
				t.Fatalf("a save after reloading: %v", err)
			}
			if after := loadAgents(t, configPath); !reflect.DeepEqual(after[0].Env, want[0].Env) {
				t.Fatalf("environment after the save = %v, want the file's %v", after[0].Env, want[0].Env)
			}
		})
	}
}

// A viewer's profiles carry no environment, no shell script, no command and
// no path, locked agents included.
func TestAWebViewerSeesNoAgentsEnvironmentScriptOrPath(t *testing.T) {
	ctrl, configPath := handWrittenController(t, handWrittenAgents)
	view, err := ctrl.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	encoded, err := json.Marshal(profilesForRole(view, RoleViewer))
	if err != nil {
		t.Fatal(err)
	}
	// The test's own directory, which every path of the configuration is in.
	testDirectory := filepath.Base(filepath.Dir(filepath.Dir(filepath.Dir(configPath))))
	for _, forbidden := range []string{agentsProbeSecret, "ANTHROPIC_API_KEY", "RELAYER_MODE", "prepare-input", "opus", "bin/reviewer", testDirectory} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("a viewer's profiles carry %q:\n%s", forbidden, encoded)
		}
	}
}

// "Restart the agents", with nothing edited, restarts each agent as the file
// configures it. The gateway rewrote every agent from the form first, so the
// agent came back without its environment, here the mode it runs in, and
// exited at once; an API key in the environment was lost the same way.
func TestAWebRestartKeepsEachAgentsEnvironment(t *testing.T) {
	g := startWebRun(t, webRun{agents: []webAgent{{
		id: "worker", name: "Worker", mode: webAgentListen, adapter: "generic",
		env: map[string]string{"WORKER_API_TOKEN": agentsProbeSecret},
	}}})
	g.awaitScreen("worker", "agent ready", 30*time.Second)
	before := loadAgents(t, g.ctrl.configPath)
	previousRun := g.runID()

	view, err := g.ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}
	request := decodedAsTheGatewayDoes[SaveAgentProfilesAndRestartRequest](t, map[string]any{
		"expectedRunID":    previousRun,
		"expectedRevision": view.Revision,
		"profiles":         interfaceProfiles(view.Profiles),
	})
	result, err := g.ctrl.SaveAgentProfilesAndRestart(request)
	if err != nil {
		t.Fatalf("SaveAgentProfilesAndRestart: %v", err)
	}
	if result.Outcome != "restarted" {
		t.Fatalf("outcome = %q, want restarted", result.Outcome)
	}
	if after := loadAgents(t, g.ctrl.configPath); !reflect.DeepEqual(after, before) {
		t.Fatalf("the restart rewrote the agent:\n got %#v\nwant %#v", after, before)
	}
	g.forgetScreens()
	g.awaitScreen("worker", "agent ready", 30*time.Second)
	// The listening agent runs until it is stopped; one without its mode
	// exits at once.
	time.Sleep(time.Second)
	if agent := g.agent("worker"); !agent.Running {
		t.Fatalf("the restarted agent is not running: %+v; screen:\n%s", agent, g.screen("worker"))
	}
}
