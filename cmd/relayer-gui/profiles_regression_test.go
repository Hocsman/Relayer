package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/config"
)

func TestSaveAgentProfilesAcceptsEveryCardinalityFromOneThroughEight(t *testing.T) {
	for count := minimumAgentProfiles; count <= maximumAgentProfiles; count++ {
		t.Run(fmt.Sprintf("%d agents", count), func(t *testing.T) {
			application, path := profileTestApp(t, nil)
			application.profileDetector = profileDetectorFixture{installed: map[string]string{}}
			view, err := application.GetAgentProfiles()
			if err != nil {
				t.Fatalf("GetAgentProfiles: %v", err)
			}
			updated, err := application.SaveAgentProfiles(SaveAgentProfilesRequest{
				ExpectedRevision: view.Revision,
				Profiles:         profileInputs(count, filepath.Dir(path)),
			})
			if err != nil {
				t.Fatalf("SaveAgentProfiles: %v", err)
			}
			if len(updated.Profiles) != count || !updated.RestartRequired {
				t.Fatalf("updated view = %#v", updated)
			}
			loaded, err := config.Load(path)
			if err != nil {
				t.Fatalf("Load saved config: %v", err)
			}
			if len(loaded.Agents) != count {
				t.Fatalf("saved agent count = %d, want %d", len(loaded.Agents), count)
			}
			for index, configured := range loaded.Agents {
				if configured.ID != fmt.Sprintf("agent-%d", index+1) {
					t.Fatalf("saved order = %#v", loaded.Agents)
				}
			}
		})
	}
}

func TestAdvancedAndSensitiveProfilesRemainOpaque(t *testing.T) {
	directory := t.TempDir()
	const (
		shellSecret = "fixture-shell-secret"
		envSecret   = "fixture-environment-secret"
		argvSecret  = "fixture-argument-secret"
		execSecret  = "sk-fixture-executable-secret"
	)
	specs := []agent.Spec{
		{ID: "shell", Name: "Shell", Shell: "printf '" + shellSecret + "'", Cwd: directory, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY},
		{ID: "environment", Name: "Environment", Command: []string{"runner"}, Cwd: directory, Env: map[string]string{"API_TOKEN": envSecret}, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY},
		{ID: "adapter", Name: "Adapter", Command: []string{"runner"}, Cwd: directory, Adapter: "private-adapter", Backend: agent.BackendPTY},
		{ID: "arguments", Name: "Arguments", Command: []string{execSecret, "--auth", argvSecret}, Cwd: directory, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY},
	}
	application, _ := profileTestApp(t, specs)
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	wantReasons := []string{"advanced_shell", "advanced_environment", "advanced_adapter"}
	if len(view.Profiles) != 4 {
		t.Fatalf("profiles = %#v", view.Profiles)
	}
	for index, profile := range view.Profiles[:3] {
		if !profile.Locked || !profile.PreserveOnSave || profile.ReadOnlyReason != wantReasons[index] || len(profile.Argv) != 0 {
			t.Fatalf("opaque profile %d = %#v", index, profile)
		}
	}
	if profile := view.Profiles[3]; profile.Locked || !profile.PreserveOnSave || len(profile.Argv) != 0 || profile.ReadOnlyReason != "" {
		t.Fatalf("existing argv profile was not safely masked: %#v", profile)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal safe view: %v", err)
	}
	for _, forbidden := range []string{shellSecret, envSecret, argvSecret, execSecret, "--auth", "API_TOKEN", "private-adapter"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("profile DTO contains protected value %q: %s", forbidden, encoded)
		}
	}
}

func TestLegacyConfigurationIsExposedAsReadOnly(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	legacy := "- pattern: overwrite\\?\\s*\\[Y/n\\]\n  description: overwrite\n"
	if err := os.WriteFile(path, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy config: %v", err)
	}
	application := NewApp()
	application.ctx = context.Background()
	application.configPath = path
	application.profileTokenGenerator = profileTokenSequence()
	application.profileDetector = profileDetectorFixture{installed: map[string]string{}}

	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	if view.Editable || view.ReadOnlyReason != "legacy_config" {
		t.Fatalf("legacy editability = editable %t reason %q", view.Editable, view.ReadOnlyReason)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read legacy config: %v", err)
	}
	_, saveErr := application.SaveAgentProfiles(SaveAgentProfilesRequest{
		ExpectedRevision: view.Revision,
		Profiles: []AgentProfileInput{{
			ID: "agent", Name: "Agent", PresetID: "custom", Cwd: directory, Backend: "pty", Argv: []string{"runner"},
		}},
	})
	if saveErr == nil {
		t.Fatal("SaveAgentProfiles accepted a legacy configuration")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read legacy config after rejected save: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("rejected legacy save mutated the configuration")
	}
}

func TestSaveAgentProfilesPreservesUnchangedLegacyIdentifier(t *testing.T) {
	directory := t.TempDir()
	application, path := profileTestApp(t, []agent.Spec{{
		ID: "Reviewer.V1", Name: "Reviewer", Command: []string{"reviewer"}, Cwd: directory, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY,
	}})
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	if !view.Editable || len(view.Profiles) != 1 || view.Profiles[0].ID != "Reviewer.V1" {
		t.Fatalf("legacy identifier view = %#v", view)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	updated, err := application.SaveAgentProfiles(SaveAgentProfilesRequest{
		ExpectedRevision: view.Revision,
		Profiles: []AgentProfileInput{{
			ID: "Reviewer.V1", Name: "Reviewer", PresetID: "custom", Cwd: directory, Backend: "pty", Preserve: true,
		}},
	})
	if err != nil {
		t.Fatalf("SaveAgentProfiles unchanged legacy ID: %v", err)
	}
	if updated.Revision != view.Revision || len(updated.Profiles) != 1 || updated.Profiles[0].ID != "Reviewer.V1" {
		t.Fatalf("unchanged legacy ID result = %#v", updated)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("unchanged legacy identifier rewrote the configuration")
	}
}

func TestSaveAgentProfilesRejectsSecretLikePrefixesWithoutMutation(t *testing.T) {
	for _, secretLike := range []string{
		"pk-live-fixture0123456789abcdef",
		"api-prod-fixture0123456789abcdef",
	} {
		t.Run(strings.SplitN(secretLike, "-", 2)[0], func(t *testing.T) {
			application, path := profileTestApp(t, nil)
			view, err := application.GetAgentProfiles()
			if err != nil {
				t.Fatalf("GetAgentProfiles: %v", err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read before: %v", err)
			}
			_, saveErr := application.SaveAgentProfiles(SaveAgentProfilesRequest{
				ExpectedRevision: view.Revision,
				Profiles: []AgentProfileInput{{
					ID: "agent", Name: "Agent", PresetID: "custom", Cwd: filepath.Dir(path), Backend: "pty", Argv: []string{"runner", secretLike},
				}},
			})
			if !errors.Is(saveErr, errProfilesInvalid) {
				t.Fatalf("SaveAgentProfiles error = %v, want secret-like argv rejection", saveErr)
			}
			if strings.Contains(fmt.Sprint(saveErr), secretLike) {
				t.Fatalf("safe error contains secret-like value: %v", saveErr)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read after: %v", err)
			}
			if string(after) != string(before) {
				t.Fatal("secret-like argv rejection mutated configuration")
			}
		})
	}
}

func TestSaveAgentProfilesEnforcesArgvBoundsInGo(t *testing.T) {
	t.Run("accepts 64 argv entries", func(t *testing.T) {
		application, path := profileTestApp(t, nil)
		view, err := application.GetAgentProfiles()
		if err != nil {
			t.Fatalf("GetAgentProfiles: %v", err)
		}
		argv := make([]string, 64)
		argv[0] = "runner"
		for index := 1; index < len(argv); index++ {
			argv[index] = "argument"
		}
		if _, err := application.SaveAgentProfiles(SaveAgentProfilesRequest{
			ExpectedRevision: view.Revision,
			Profiles: []AgentProfileInput{{
				ID: "agent", Name: "Agent", PresetID: "custom", Cwd: filepath.Dir(path), Backend: "pty", Argv: argv,
			}},
		}); err != nil {
			t.Fatalf("SaveAgentProfiles rejected the argv boundary: %v", err)
		}
	})

	for _, test := range []struct {
		name string
		argv []string
	}{
		{
			name: "more than 64 argv entries",
			argv: func() []string {
				argv := make([]string, 65)
				argv[0] = "runner"
				for index := 1; index < len(argv); index++ {
					argv[index] = "argument"
				}
				return argv
			}(),
		},
		{name: "argument longer than 4096", argv: []string{"runner", strings.Repeat("x", 4097)}},
	} {
		t.Run(test.name, func(t *testing.T) {
			application, path := profileTestApp(t, nil)
			view, err := application.GetAgentProfiles()
			if err != nil {
				t.Fatalf("GetAgentProfiles: %v", err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read before: %v", err)
			}
			_, saveErr := application.SaveAgentProfiles(SaveAgentProfilesRequest{
				ExpectedRevision: view.Revision,
				Profiles: []AgentProfileInput{{
					ID: "agent", Name: "Agent", PresetID: "custom", Cwd: filepath.Dir(path), Backend: "pty", Argv: test.argv,
				}},
			})
			if !errors.Is(saveErr, errProfilesInvalid) {
				t.Fatalf("SaveAgentProfiles error = %v, want bounded argv rejection", saveErr)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read after: %v", err)
			}
			if string(after) != string(before) {
				t.Fatal("bounded argv rejection mutated configuration")
			}
		})
	}
}

func TestSaveAgentProfilesCannotReplaceOrRemoveLockedProfile(t *testing.T) {
	for _, test := range []struct {
		name     string
		profiles func(string) []AgentProfileInput
	}{
		{
			name: "remove",
			profiles: func(directory string) []AgentProfileInput {
				return []AgentProfileInput{{ID: "editable", Name: "Editable", PresetID: "custom", Cwd: directory, Backend: "pty", Argv: []string{"runner"}}}
			},
		},
		{
			name: "replace",
			profiles: func(directory string) []AgentProfileInput {
				return []AgentProfileInput{
					{ID: "advanced", Name: "Forged replacement", PresetID: "custom", Cwd: directory, Backend: "pty", Argv: []string{"runner"}},
					{ID: "editable", Name: "Editable", PresetID: "custom", Cwd: directory, Backend: "pty", Argv: []string{"runner"}},
				}
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			application, path := profileTestApp(t, []agent.Spec{
				{ID: "advanced", Name: "Advanced", Command: []string{"runner"}, Cwd: directory, Env: map[string]string{"API_TOKEN": "fixture-secret"}, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY},
				{ID: "editable", Name: "Editable", Command: []string{"runner"}, Cwd: directory, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY},
			})
			view, err := application.GetAgentProfiles()
			if err != nil {
				t.Fatalf("GetAgentProfiles: %v", err)
			}
			before, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read before: %v", err)
			}
			_, saveErr := application.SaveAgentProfiles(SaveAgentProfilesRequest{
				ExpectedRevision: view.Revision,
				Profiles:         test.profiles(directory),
			})
			if !errors.Is(saveErr, errProfilesInvalid) {
				t.Fatalf("SaveAgentProfiles error = %v, want invalid locked-profile mutation", saveErr)
			}
			after, err := os.ReadFile(path)
			if err != nil {
				t.Fatalf("read after: %v", err)
			}
			if string(after) != string(before) {
				t.Fatal("locked-profile mutation changed configuration")
			}
		})
	}
}

func TestSaveAgentProfilesTokenFailurePreservesRevisionAuthorityForRetry(t *testing.T) {
	application, path := profileTestApp(t, nil)
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	application.profilesMu.Lock()
	beforeHash := application.profileRevisionHash
	beforeToken := application.profileRevisionToken
	application.profileTokenGenerator = func() (string, error) {
		return "", errors.New("fixture token failure")
	}
	application.profilesMu.Unlock()
	request := SaveAgentProfilesRequest{
		ExpectedRevision: view.Revision,
		Profiles: []AgentProfileInput{{
			ID: "agent", Name: "Agent", PresetID: "custom", Cwd: filepath.Dir(path), Backend: "pty", Argv: []string{"runner"},
		}},
	}
	if _, saveErr := application.SaveAgentProfiles(request); !errors.Is(saveErr, errProfilesSave) {
		t.Fatalf("SaveAgentProfiles error = %v, want token-generation failure", saveErr)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after failed save: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("token generation failure mutated the configuration")
	}
	application.profilesMu.Lock()
	afterHash := application.profileRevisionHash
	afterToken := application.profileRevisionToken
	application.profileTokenGenerator = func() (string, error) { return "retry-revision", nil }
	application.profilesMu.Unlock()
	if afterHash != beforeHash || afterToken != beforeToken {
		t.Fatalf("token failure changed revision authority: hash %q -> %q, token %q -> %q", beforeHash, afterHash, beforeToken, afterToken)
	}
	updated, err := application.SaveAgentProfiles(request)
	if err != nil {
		t.Fatalf("retry with original authority: %v", err)
	}
	if updated.Revision != "retry-revision" {
		t.Fatalf("retry revision = %q, want freshly generated token", updated.Revision)
	}
}

func TestPreservedProfileKeepsOpaqueArgvAndRelativeWorkingDirectory(t *testing.T) {
	application, path := profileTestApp(t, nil)
	directory := filepath.Dir(path)
	workspace := filepath.Join(directory, "workspace")
	if err := os.Mkdir(workspace, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load default config: %v", err)
	}
	command := []string{"runner", "--auth", "fixture-opaque-value"}
	if _, _, err := config.ReplaceAgents(path, loaded.Revision, []agent.Spec{{
		ID: "agent", Name: "Original", Command: command, Cwd: directory, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY,
	}}); err != nil {
		t.Fatalf("seed opaque profile: %v", err)
	}
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	updated, err := application.SaveAgentProfiles(SaveAgentProfilesRequest{
		ExpectedRevision: view.Revision,
		Profiles: []AgentProfileInput{{
			ID: "agent", Name: "Renamed", PresetID: "custom", Cwd: "workspace", Backend: "tmux", Preserve: true,
		}},
	})
	if err != nil {
		t.Fatalf("SaveAgentProfiles: %v", err)
	}
	if len(updated.Profiles) != 1 || len(updated.Profiles[0].Argv) != 0 || !updated.Profiles[0].PreserveOnSave {
		t.Fatalf("updated DTO exposed opaque argv: %#v", updated.Profiles)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read updated config: %v", err)
	}
	if !strings.Contains(string(payload), "cwd: workspace\n") || strings.Contains(string(payload), "cwd: "+workspace) {
		t.Fatalf("relative working directory was not preserved:\n%s", payload)
	}
	fresh, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load updated config: %v", err)
	}
	if len(fresh.Agents) != 1 || !reflect.DeepEqual(fresh.Agents[0].Command, command) {
		t.Fatalf("preserved argv changed: %#v", fresh.Agents)
	}
	if fresh.Agents[0].Name != "Renamed" || fresh.Agents[0].Cwd != workspace || fresh.Agents[0].Backend != agent.BackendTmux {
		t.Fatalf("preserved metadata = %#v", fresh.Agents[0])
	}
}

func TestConcurrentProfileSavesAcrossAppsHaveOneCASWinner(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load default config: %v", err)
	}

	const contenders = 12
	type contender struct {
		application *App
		request     SaveAgentProfilesRequest
	}
	apps := make([]contender, contenders)
	for index := range apps {
		application := NewApp()
		application.ctx = context.Background()
		application.configPath = path
		application.activeConfigRevision = loaded.Revision
		application.profileTokenGenerator = profileTokenSequence()
		application.profileDetector = profileDetectorFixture{installed: map[string]string{}}
		view, viewErr := application.GetAgentProfiles()
		if viewErr != nil {
			t.Fatalf("GetAgentProfiles(%d): %v", index, viewErr)
		}
		id := fmt.Sprintf("candidate-%02d", index)
		apps[index] = contender{
			application: application,
			request: SaveAgentProfilesRequest{
				ExpectedRevision: view.Revision,
				Profiles: []AgentProfileInput{{
					ID: id, Name: id, PresetID: "custom", Cwd: directory, Backend: "pty", Argv: []string{"runner"},
				}},
			},
		}
	}

	start := make(chan struct{})
	results := make(chan error, contenders)
	var workers sync.WaitGroup
	workers.Add(contenders)
	for _, candidate := range apps {
		candidate := candidate
		go func() {
			defer workers.Done()
			<-start
			_, saveErr := candidate.application.SaveAgentProfiles(candidate.request)
			results <- saveErr
		}()
	}
	close(start)
	workers.Wait()
	close(results)

	successes := 0
	for saveErr := range results {
		if saveErr == nil {
			successes++
			continue
		}
		if !errors.Is(saveErr, errProfilesStale) {
			t.Fatalf("concurrent profile save error = %v, want stale revision", saveErr)
		}
	}
	if successes != 1 {
		t.Fatalf("successful concurrent profile saves = %d, want one", successes)
	}
}

func TestSaveAgentProfilesLeavesActiveRunUntouched(t *testing.T) {
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
	application.profileDetector = profileDetectorFixture{installed: map[string]string{}}
	application.profilesMu.Unlock()
	beforeState, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState before save: %v", err)
	}
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	updated, err := application.SaveAgentProfiles(SaveAgentProfilesRequest{
		ExpectedRevision: view.Revision,
		Profiles: []AgentProfileInput{{
			ID: "saved-agent", Name: "Saved Agent", PresetID: "custom", Cwd: directory, Backend: "pty", Argv: []string{"runner"},
		}},
	})
	if err != nil {
		t.Fatalf("SaveAgentProfiles: %v", err)
	}
	afterState, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState after save: %v", err)
	}
	if !updated.RestartRequired || application.activeConfigRevision != loaded.Revision {
		t.Fatalf("save unexpectedly activated config: view=%#v active=%q", updated, application.activeConfigRevision)
	}
	if !reflect.DeepEqual(afterState, beforeState) {
		t.Fatalf("active run changed during profile save:\nbefore=%#v\nafter=%#v", beforeState, afterState)
	}
	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("profile save invoked active backend: %#v", calls)
	}
	engine.mu.Lock()
	closeCalls := engine.closeCalls
	operations := append([]string(nil), engine.operations...)
	engine.mu.Unlock()
	if closeCalls != 0 || len(operations) != 0 {
		t.Fatalf("profile save hot-restarted engine: close=%d operations=%#v", closeCalls, operations)
	}
}
