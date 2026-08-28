package config

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
)

func TestReplaceAgentsPreservesConfigurationAndPublishesAtomically(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if _, err := LoadOrCreate(path); err != nil {
		t.Fatalf("create default config: %v", err)
	}
	revision, err := FileRevision(path)
	if err != nil {
		t.Fatalf("FileRevision: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	beforeInfo, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat before: %v", err)
	}

	specs := []agent.Spec{
		{ID: "claude", Name: "Claude Code", Command: []string{"claude", "--safe argument"}, Cwd: directory, Adapter: agent.AdapterGeneric, Backend: agent.BackendAuto},
		{ID: "codex", Name: "Codex CLI", Command: []string{"codex"}, Cwd: directory, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY},
	}
	updated, nextRevision, err := ReplaceAgents(path, revision, specs)
	if err != nil {
		t.Fatalf("ReplaceAgents: %v", err)
	}
	if nextRevision == revision || nextRevision == "" {
		t.Fatalf("next revision = %q, previous %q", nextRevision, revision)
	}
	if len(updated.Agents) != 2 || updated.Agents[0].ID != "claude" || updated.Agents[1].ID != "codex" {
		t.Fatalf("updated agents = %#v", updated.Agents)
	}
	if got := updated.Agents[0].Command; len(got) != 2 || got[1] != "--safe argument" {
		t.Fatalf("literal argv = %#v", got)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	for _, preserved := range []string{"policies:", "audit:", "intercept_patterns:", "sessions:"} {
		if !strings.Contains(string(before), preserved) || !strings.Contains(string(after), preserved) {
			t.Fatalf("field %q was not preserved", preserved)
		}
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat updated config: %v", err)
	}
	if info.Mode().Perm() != beforeInfo.Mode().Perm() {
		t.Fatalf("mode = %o, want preserved mode %o", info.Mode().Perm(), beforeInfo.Mode().Perm())
	}
}

func TestReplaceAgentsRejectsStaleRevisionWithoutMutation(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	if _, err := LoadOrCreate(path); err != nil {
		t.Fatalf("create default config: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	_, _, err = ReplaceAgents(path, strings.Repeat("0", 64), []agent.Spec{{
		ID: "agent", Name: "Agent", Command: []string{"agent"}, Backend: agent.BackendPTY,
	}})
	if !errors.Is(err, ErrRevisionMismatch) {
		t.Fatalf("ReplaceAgents error = %v, want revision mismatch", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("stale update mutated configuration")
	}
}

func TestReplaceAgentsRejectsLegacyAndInvalidSpecsWithoutMutation(t *testing.T) {
	directory := t.TempDir()
	legacyPath := filepath.Join(directory, "legacy.yaml")
	legacy := "- pattern: overwrite\\?\\s*\\[Y/n\\]\n  description: overwrite\n"
	if err := os.WriteFile(legacyPath, []byte(legacy), 0o600); err != nil {
		t.Fatalf("write legacy: %v", err)
	}
	revision, err := FileRevision(legacyPath)
	if err != nil {
		t.Fatalf("legacy revision: %v", err)
	}
	if _, _, err := ReplaceAgents(legacyPath, revision, nil); err == nil || !strings.Contains(err.Error(), "legacy configuration") {
		t.Fatalf("legacy ReplaceAgents error = %v", err)
	}
	payload, err := os.ReadFile(legacyPath)
	if err != nil || string(payload) != legacy {
		t.Fatalf("legacy file mutated: %v", err)
	}

	path := filepath.Join(directory, "config.yaml")
	if _, err := LoadOrCreate(path); err != nil {
		t.Fatalf("create default: %v", err)
	}
	revision, err = FileRevision(path)
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	tooMany := make([]agent.Spec, maxAgents+1)
	for index := range tooMany {
		tooMany[index] = agent.Spec{ID: "agent-" + string(rune('a'+index)), Name: "Agent", Command: []string{"runner"}, Backend: agent.BackendPTY}
	}
	if _, _, err := ReplaceAgents(path, revision, tooMany); err == nil {
		t.Fatal("too many agents were accepted")
	}
	duplicate := []agent.Spec{
		{ID: "Agent", Name: "First", Command: []string{"runner"}, Backend: agent.BackendPTY},
		{ID: "agent", Name: "Second", Command: []string{"runner"}, Backend: agent.BackendPTY},
	}
	if _, _, err := ReplaceAgents(path, revision, duplicate); err == nil {
		t.Fatal("duplicate agent IDs were accepted")
	}
}

func TestReplaceAgentsPreservesAdvancedAgentDataWhenPassedBackFromCore(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	document := versionOneDocument(`
  - id: advanced
    name: Advanced shell
    shell: 'exec ./runner'
    cwd: .
    env:
      API_TOKEN: fixture-secret
    adapter: generic
    backend: pty`)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loaded, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	revision, err := FileRevision(path)
	if err != nil {
		t.Fatalf("revision: %v", err)
	}
	updated, _, err := ReplaceAgents(path, revision, loaded.Agents)
	if err != nil {
		t.Fatalf("ReplaceAgents: %v", err)
	}
	if len(updated.Agents) != 1 || updated.Agents[0].Shell != "exec ./runner" || updated.Agents[0].Env["API_TOKEN"] != "fixture-secret" {
		t.Fatalf("advanced spec not preserved: %#v", updated.Agents)
	}
}

func TestReplaceAgentsRejectsRemovedPolicyAgent(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	document := "version: 1\n" +
		"backend: pty\n" +
		"policies:\n" +
		"  default_action: ask\n" +
		"  dry_run: false\n" +
		"  rules:\n" +
		"    - name: reviewer-only\n" +
		"      match:\n" +
		"        agent_ids: [reviewer]\n" +
		"      action: ask\n" +
		"agents:\n" +
		"  - id: reviewer\n" +
		"    name: Reviewer\n" +
		"    command: [reviewer]\n" +
		"intercept_patterns:\n" +
		"  - pattern: '(?i)continue'\n" +
		"    description: Continue confirmation\n"
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	revision, err := FileRevision(path)
	if err != nil {
		t.Fatalf("FileRevision: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read before: %v", err)
	}
	_, _, err = ReplaceAgents(path, revision, []agent.Spec{{
		ID: "builder", Name: "Builder", Command: []string{"builder"}, Backend: agent.BackendPTY,
	}})
	if err == nil || !strings.Contains(err.Error(), "references a missing agent") {
		t.Fatalf("ReplaceAgents error = %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after: %v", err)
	}
	if string(after) != string(before) {
		t.Fatal("invalid policy update mutated configuration")
	}
}

func TestReplaceAgentsReportsCommitUncertainAfterDirectorySyncFailure(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	loaded, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("Load default: %v", err)
	}
	originalSync := syncConfigurationDirectory
	syncConfigurationDirectory = func(string) error { return errors.New("fixture sync failure") }
	t.Cleanup(func() { syncConfigurationDirectory = originalSync })

	updated, revision, err := ReplaceAgents(path, loaded.Revision, []agent.Spec{{
		ID: "agent", Name: "Agent", Command: []string{"runner"}, Backend: agent.BackendPTY,
	}})
	if !errors.Is(err, ErrCommitUncertain) {
		t.Fatalf("ReplaceAgents error = %v, want uncertain commit", err)
	}
	if revision == "" || revision == loaded.Revision || len(updated.Agents) != 1 || updated.Agents[0].ID != "agent" {
		t.Fatalf("post-commit state was not returned: revision=%q updated=%#v", revision, updated)
	}
	fresh, loadErr := LoadOrCreate(path)
	if loadErr != nil {
		t.Fatalf("Load committed config: %v", loadErr)
	}
	if fresh.Revision != revision || fresh.Agents[0].ID != "agent" {
		t.Fatalf("committed config = %#v revision=%q", fresh.Agents, fresh.Revision)
	}
}

func TestReplaceAgentsPreservesPortableWorkingDirectories(t *testing.T) {
	directory := t.TempDir()
	subdirectory := filepath.Join(directory, "workspace")
	if err := os.Mkdir(subdirectory, 0o700); err != nil {
		t.Fatalf("mkdir workspace: %v", err)
	}
	path := filepath.Join(directory, "config.yaml")
	document := versionOneDocument(`
  - id: existing
    name: Existing
    command: [runner]
    cwd: .
    adapter: generic
    backend: pty`)
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	loaded, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	requested := []agent.Spec{
		loaded.Agents[0],
		{ID: "new", Name: "New", Command: []string{"runner"}, Cwd: "workspace", Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY},
	}
	if _, _, err := ReplaceAgents(path, loaded.Revision, requested); err != nil {
		t.Fatalf("ReplaceAgents: %v", err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read updated config: %v", err)
	}
	text := string(payload)
	if !strings.Contains(text, "cwd: .\n") || !strings.Contains(text, "cwd: workspace\n") {
		t.Fatalf("portable cwd values were not preserved:\n%s", text)
	}
	if strings.Contains(text, directory) {
		t.Fatalf("absolute personal path leaked into config:\n%s", text)
	}
}
