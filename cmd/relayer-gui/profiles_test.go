package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

type profileDetectorFixture struct {
	installed map[string]string
}

func (fixture profileDetectorFixture) Detect(_ context.Context, candidates []string) (toolcatalog.Detection, error) {
	for _, candidate := range candidates {
		if path, found := fixture.installed[candidate]; found {
			return toolcatalog.Detection{
				Status:     toolcatalog.InstallInstalled,
				Executable: candidate,
				Path:       path,
			}, nil
		}
	}
	return toolcatalog.Detection{Status: toolcatalog.InstallNotInstalled}, nil
}

func TestGetAgentProfilesReturnsSafeCatalogAndLocksAdvancedSpecs(t *testing.T) {
	directory := t.TempDir()
	specs := []agent.Spec{
		{ID: "claude", Name: "Claude Code", Command: []string{"claude", "--safe"}, Cwd: directory, Adapter: agent.AdapterGeneric, Backend: agent.BackendAuto},
		{ID: "advanced", Name: "Advanced", Command: []string{"runner"}, Cwd: directory, Env: map[string]string{"API_TOKEN": "fixture-env-secret"}, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY},
	}
	application, _ := profileTestApp(t, specs)
	application.profileDetector = profileDetectorFixture{installed: map[string]string{
		"claude": "/private/hidden/home/bin/claude",
	}}

	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	if len(view.Catalog) != 4 || view.Catalog[0].ID != string(toolcatalog.ClaudeCode) || !view.Catalog[0].Installed {
		t.Fatalf("catalog = %#v", view.Catalog)
	}
	if view.Catalog[1].Installed || view.Catalog[3].InstallStatus != string(toolcatalog.InstallUnknown) {
		t.Fatalf("catalog installation states = %#v", view.Catalog)
	}
	if len(view.Profiles) != 2 || view.Profiles[0].PresetID != string(toolcatalog.ClaudeCode) {
		t.Fatalf("profiles = %#v", view.Profiles)
	}
	if len(view.Profiles[0].Argv) != 0 || !view.Profiles[0].PreserveOnSave || view.Profiles[0].ExecutableLabel != "claude" {
		t.Fatalf("existing command was exposed: %#v", view.Profiles[0])
	}
	if view.Profiles[1].Locked != true || view.Profiles[1].ReadOnlyReason != "advanced_environment" ||
		len(view.Profiles[1].Argv) != 0 || !view.Profiles[1].PreserveOnSave {
		t.Fatalf("advanced profile = %#v", view.Profiles[1])
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	for _, forbidden := range []string{"fixture-env-secret", "API_TOKEN", "/private/hidden/home", "--safe", "runner"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("safe profile DTO contains %q: %s", forbidden, encoded)
		}
	}

	view.Profiles[0].Name = "mutated"
	view.Catalog[0].DefaultArgv[0] = "mutated"
	fresh, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("fresh GetAgentProfiles: %v", err)
	}
	if fresh.Profiles[0].Name != "Claude Code" || fresh.Catalog[0].DefaultArgv[0] != "claude" {
		t.Fatalf("profile view aliases caller data: %#v", fresh)
	}
}

func TestSaveAgentProfilesWritesLiteralArgvAndRequiresNextLaunch(t *testing.T) {
	application, path := profileTestApp(t, nil)
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	request := SaveAgentProfilesRequest{
		ExpectedRevision: view.Revision,
		Profiles: []AgentProfileInput{
			{ID: "claude", Name: "Claude Code", PresetID: "claude-code", Cwd: filepath.Dir(path), Backend: "auto", Argv: []string{"claude"}},
			{ID: "codex", Name: "Codex CLI", PresetID: "codex-cli", Cwd: filepath.Dir(path), Backend: "pty", Argv: []string{"codex", "review", "$(literal)", ""}},
			{ID: "mimo", Name: "MiMo Code", PresetID: "mimo-code", Cwd: filepath.Dir(path), Backend: "tmux", Argv: []string{"mimo"}},
		},
	}
	updated, err := saveAgentProfilesForTest(application, request)
	if err != nil {
		t.Fatalf("SaveAgentProfiles: %v", err)
	}
	if !updated.RestartRequired || updated.Revision == view.Revision {
		t.Fatalf("updated view = %#v", updated)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load updated config: %v", err)
	}
	if len(loaded.Agents) != 3 || loaded.Agents[0].ID != "claude" || loaded.Agents[2].ID != "mimo" {
		t.Fatalf("saved agents = %#v", loaded.Agents)
	}
	if got := loaded.Agents[1].Command; len(got) != 4 || got[2] != "$(literal)" || got[3] != "" {
		t.Fatalf("literal argv = %#v", got)
	}
	if loaded.Agents[0].Adapter != agent.AdapterGeneric || loaded.Agents[2].Backend != agent.BackendTmux {
		t.Fatalf("resolved profile defaults = %#v", loaded.Agents)
	}

	request.ExpectedRevision = updated.Revision
	unchanged, err := saveAgentProfilesForTest(application, request)
	if err != nil {
		t.Fatalf("SaveAgentProfiles unchanged: %v", err)
	}
	if unchanged.Revision != updated.Revision {
		t.Fatalf("unchanged save rotated revision %q -> %q", updated.Revision, unchanged.Revision)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before stale save: %v", err)
	}
	request.ExpectedRevision = view.Revision
	if _, err := saveAgentProfilesForTest(application, request); !errors.Is(err, errProfilesStale) {
		t.Fatalf("stale SaveAgentProfiles error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after stale save: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("stale profile save mutated configuration")
	}
}

func TestSaveAgentProfilesRejectsSecretsAndCardinalityWithoutMutation(t *testing.T) {
	application, path := profileTestApp(t, nil)
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	secret := "fixture-manual-secret"
	for _, test := range []struct {
		name     string
		profiles []AgentProfileInput
	}{
		{name: "empty", profiles: nil},
		{name: "too many", profiles: profileInputs(9, filepath.Dir(path))},
		{name: "api key flag", profiles: []AgentProfileInput{{ID: "custom", Name: "Custom", PresetID: "custom", Cwd: filepath.Dir(path), Backend: "pty", Argv: []string{"runner", "--api-key", secret}}}},
		{name: "auth flag", profiles: []AgentProfileInput{{ID: "custom", Name: "Custom", PresetID: "custom", Cwd: filepath.Dir(path), Backend: "pty", Argv: []string{"runner", "--auth", secret}}}},
		{name: "credential URL", profiles: []AgentProfileInput{{ID: "custom", Name: "Custom", PresetID: "custom", Cwd: filepath.Dir(path), Backend: "pty", Argv: []string{"runner", "https://user:" + secret + "@example.invalid"}}}},
		{name: "JWT", profiles: []AgentProfileInput{{ID: "custom", Name: "Custom", PresetID: "custom", Cwd: filepath.Dir(path), Backend: "pty", Argv: []string{"runner", "eyJhbGciOiJIUzI1NiJ9.e30.dBjftJeZ4CVP-mB92K27uhbUJU1p1r_wW1gFWFOEjXk"}}}},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, saveErr := saveAgentProfilesForTest(application, SaveAgentProfilesRequest{ExpectedRevision: view.Revision, Profiles: test.profiles})
			if !errors.Is(saveErr, errProfilesInvalid) {
				t.Fatalf("SaveAgentProfiles error = %v", saveErr)
			}
			if strings.Contains(fmt.Sprint(saveErr), secret) {
				t.Fatalf("error contains secret: %v", saveErr)
			}
			payload, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatalf("read after rejected save: %v", readErr)
			}
			if string(payload) != string(before) {
				t.Fatal("rejected profile save mutated configuration")
			}
		})
	}
}

func TestSaveAgentProfilesPreservesLockedSpecInsideGo(t *testing.T) {
	directory := t.TempDir()
	locked := agent.Spec{
		ID: "advanced", Name: "Advanced", Command: []string{"runner"}, Cwd: directory,
		Env: map[string]string{"API_TOKEN": "fixture-secret"}, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY,
	}
	application, path := profileTestApp(t, []agent.Spec{locked})
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	updated, err := saveAgentProfilesForTest(application, SaveAgentProfilesRequest{
		ExpectedRevision: view.Revision,
		Profiles: []AgentProfileInput{
			{ID: "claude", Name: "Claude", PresetID: "claude-code", Cwd: directory, Backend: "auto", Argv: []string{"claude"}},
			{ID: "advanced", Preserve: true},
		},
	})
	if err != nil {
		t.Fatalf("SaveAgentProfiles: %v", err)
	}
	if len(updated.Profiles) != 2 || !updated.Profiles[1].Locked || len(updated.Profiles[1].Argv) != 0 {
		t.Fatalf("updated locked profile = %#v", updated.Profiles)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(loaded.Agents) != 2 || loaded.Agents[1].Env["API_TOKEN"] != "fixture-secret" {
		t.Fatalf("locked spec was not preserved: %#v", loaded.Agents)
	}
}

func TestSaveAgentProfilesEditsMetadataWithoutExposingExistingArgv(t *testing.T) {
	directory := t.TempDir()
	otherDirectory := filepath.Join(directory, "other")
	if err := os.Mkdir(otherDirectory, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	command := []string{"runner", "--auth", "fixture-opaque-value"}
	application, path := profileTestApp(t, []agent.Spec{{
		ID: "agent", Name: "Original", Command: command, Cwd: directory, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY,
	}})
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	if len(view.Profiles) != 1 || len(view.Profiles[0].Argv) != 0 || !view.Profiles[0].PreserveOnSave {
		t.Fatalf("existing argv was not masked: %#v", view.Profiles)
	}
	updated, err := saveAgentProfilesForTest(application, SaveAgentProfilesRequest{
		ExpectedRevision: view.Revision,
		Profiles: []AgentProfileInput{{
			ID: "agent", Name: "Renamed", PresetID: "custom", Cwd: otherDirectory, Backend: "tmux", Preserve: true,
		}},
	})
	if err != nil {
		t.Fatalf("SaveAgentProfiles: %v", err)
	}
	if len(updated.Profiles[0].Argv) != 0 || !updated.Profiles[0].PreserveOnSave {
		t.Fatalf("saved argv was exposed: %#v", updated.Profiles[0])
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if loaded.Agents[0].Name != "Renamed" || loaded.Agents[0].Cwd != otherDirectory || loaded.Agents[0].Backend != agent.BackendTmux {
		t.Fatalf("metadata not updated: %#v", loaded.Agents[0])
	}
	if fmt.Sprint(loaded.Agents[0].Command) != fmt.Sprint(command) {
		t.Fatalf("opaque command changed: %#v", loaded.Agents[0].Command)
	}
}

func TestSaveAgentProfilesDoesNotRestartActiveEngine(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	application.profilesMu.Lock()
	application.configPath = path
	application.activeConfigRevision = loaded.Revision
	application.profileTokenGenerator = profileTokenSequence()
	application.profilesMu.Unlock()
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	_, err = saveAgentProfilesForTest(application, SaveAgentProfilesRequest{
		ExpectedRevision: view.Revision,
		Profiles:         []AgentProfileInput{{ID: "claude", Name: "Claude", PresetID: "claude-code", Cwd: directory, Backend: "auto", Argv: []string{"claude"}}},
	})
	if err != nil {
		t.Fatalf("SaveAgentProfiles: %v", err)
	}
	engine.mu.Lock()
	closeCalls := engine.closeCalls
	engine.mu.Unlock()
	if closeCalls != 0 {
		t.Fatalf("profile save restarted or closed active engine %d time(s)", closeCalls)
	}
}

func TestSaveAgentProfilesTokenFailureDoesNotPublish(t *testing.T) {
	application, path := profileTestApp(t, nil)
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	application.profileTokenGenerator = func() (string, error) {
		return "", errors.New("fixture token failure")
	}
	_, err = saveAgentProfilesForTest(application, SaveAgentProfilesRequest{
		ExpectedRevision: view.Revision,
		Profiles: []AgentProfileInput{{
			ID: "agent", Name: "Agent", PresetID: "custom", Cwd: filepath.Dir(path), Backend: "pty", Argv: []string{"runner"},
		}},
	})
	if !errors.Is(err, errProfilesSave) {
		t.Fatalf("SaveAgentProfiles error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("token failure published a profile update")
	}
}

func profileTestApp(t *testing.T, specs []agent.Spec) (*App, string) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load default config: %v", err)
	}
	if specs != nil {
		loaded, _, err = config.ReplaceAgents(path, loaded.Revision, specs)
		if err != nil {
			t.Fatalf("seed agents: %v", err)
		}
	}
	application := NewApp()
	application.ctx = context.Background()
	application.configPath = path
	application.activeConfigRevision = loaded.Revision
	application.profileTokenGenerator = profileTokenSequence()
	return application, path
}

func saveAgentProfilesForTest(application *App, request SaveAgentProfilesRequest) (AgentProfilesView, error) {
	return application.SaveAgentProfiles(activeRunIDForTest(application), request)
}

func profileTokenSequence() func() (string, error) {
	index := 0
	return func() (string, error) {
		index++
		return fmt.Sprintf("opaque-revision-%d", index), nil
	}
}

func profileInputs(count int, cwd string) []AgentProfileInput {
	result := make([]AgentProfileInput, count)
	for index := range result {
		result[index] = AgentProfileInput{
			ID:       fmt.Sprintf("agent-%d", index+1),
			Name:     fmt.Sprintf("Agent %d", index+1),
			PresetID: "custom",
			Cwd:      cwd,
			Backend:  "pty",
			Argv:     []string{"runner"},
		}
	}
	return result
}
