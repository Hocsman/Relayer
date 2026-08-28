package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
)

func TestLoadVersionOneAcceptsBackendSelectorsAndAgentsInheritGlobalBackend(t *testing.T) {
	for _, backend := range []string{agent.BackendPTY, agent.BackendTmux, agent.BackendAuto} {
		t.Run(backend, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := strings.Replace(
				versionOneDocument("\n  - id: inherited\n    name: Inherited backend\n    command: [runner]"),
				"backend: pty",
				"backend: "+backend,
				1,
			)
			writeConfigTestFile(t, path, []byte(content))

			result, err := LoadOrCreate(path)
			if err != nil {
				t.Fatalf("Load returned an error: %v", err)
			}
			if result.Backend != backend {
				t.Fatalf("global backend = %q, want %q", result.Backend, backend)
			}
			if len(result.Agents) != 1 || result.Agents[0].Backend != backend {
				t.Fatalf("agent backends = %#v, want inherited %q", result.Agents, backend)
			}
		})
	}
}

func TestLoadVersionOneAcceptsExplicitAgentBackendSelectors(t *testing.T) {
	for _, backend := range []string{agent.BackendPTY, agent.BackendTmux, agent.BackendAuto} {
		t.Run(backend, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := versionOneDocument("\n  - id: explicit\n    name: Explicit backend\n    command: [runner]\n    backend: " + backend)
			writeConfigTestFile(t, path, []byte(content))

			result, err := LoadOrCreate(path)
			if err != nil {
				t.Fatalf("Load returned an error: %v", err)
			}
			if got := result.Agents[0].Backend; got != backend {
				t.Fatalf("agent backend = %q, want %q", got, backend)
			}
		})
	}
}

func TestLoadVersionOneDefaultsSessionPolicyWhenSessionsAreOmitted(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfigTestFile(t, path, []byte(versionOneDocument("[]")))

	result, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	if result.Sessions.PersistOnExit {
		t.Fatal("PersistOnExit = true, want the compatibility default false")
	}
	if !result.Sessions.CleanupOnSuccess {
		t.Fatal("CleanupOnSuccess = false, want the compatibility default true")
	}
}

func TestLoadVersionOneMergesPartialSessionPolicyWithDefaults(t *testing.T) {
	tests := []struct {
		name             string
		sessions         string
		persistOnExit    bool
		cleanupOnSuccess bool
	}{
		{name: "empty mapping", sessions: "{}", persistOnExit: false, cleanupOnSuccess: true},
		{name: "persist only", sessions: "\n  persist_on_exit: true", persistOnExit: true, cleanupOnSuccess: true},
		{name: "cleanup only", sessions: "\n  cleanup_on_success: false", persistOnExit: false, cleanupOnSuccess: false},
		{name: "both disabled", sessions: "\n  persist_on_exit: false\n  cleanup_on_success: false", persistOnExit: false, cleanupOnSuccess: false},
		{name: "both enabled", sessions: "\n  persist_on_exit: true\n  cleanup_on_success: true", persistOnExit: true, cleanupOnSuccess: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := strings.Replace(
				versionOneDocument("[]"),
				"agents: []",
				"sessions: "+test.sessions+"\nagents: []",
				1,
			)
			writeConfigTestFile(t, path, []byte(content))

			result, err := LoadOrCreate(path)
			if err != nil {
				t.Fatalf("Load returned an error: %v\n%s", err, content)
			}
			if result.Sessions.PersistOnExit != test.persistOnExit || result.Sessions.CleanupOnSuccess != test.cleanupOnSuccess {
				t.Fatalf("session policy = %#v, want persist_on_exit=%t cleanup_on_success=%t", result.Sessions, test.persistOnExit, test.cleanupOnSuccess)
			}
		})
	}
}

func TestLoadVersionOneRejectsInvalidSessionPolicyShapesAndTypes(t *testing.T) {
	tests := []struct {
		name     string
		sessions string
	}{
		{name: "null", sessions: "null"},
		{name: "scalar", sessions: "true"},
		{name: "sequence", sessions: "[]"},
		{name: "quoted persist boolean", sessions: "\n  persist_on_exit: \"true\""},
		{name: "numeric cleanup boolean", sessions: "\n  cleanup_on_success: 1"},
		{name: "unknown field", sessions: "\n  cleanup_successfully: true"},
		{name: "duplicate field", sessions: "\n  persist_on_exit: true\n  persist_on_exit: false"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := strings.Replace(
				versionOneDocument("[]"),
				"agents: []",
				"sessions: "+test.sessions+"\nagents: []",
				1,
			)
			writeConfigTestFile(t, path, []byte(content))

			if result, err := LoadOrCreate(path); err == nil {
				t.Fatalf("Load accepted invalid sessions YAML and returned %#v\n%s", result, content)
			}
		})
	}
}

func TestLoadVersionOneRejectsUnknownOrNonCanonicalBackend(t *testing.T) {
	for _, backend := range []string{"", "pipe", "TMUX"} {
		t.Run(strings.ReplaceAll(backend, " ", "_"), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := strings.Replace(versionOneDocument("[]"), "backend: pty", "backend: '"+backend+"'", 1)
			writeConfigTestFile(t, path, []byte(content))

			if result, err := LoadOrCreate(path); err == nil {
				t.Fatalf("Load accepted backend %q and returned %#v", backend, result)
			}
		})
	}
}

func TestLegacyDocumentsUsePTYAndDefaultSessionPolicy(t *testing.T) {
	for _, content := range []string{
		"- pattern: valid\n  description: Legacy direct\n",
		"intercept_patterns:\n  - pattern: valid\n    description: Legacy wrapper\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		writeConfigTestFile(t, path, []byte(content))

		result, err := LoadOrCreate(path)
		if err != nil {
			t.Fatalf("Load legacy document: %v", err)
		}
		if result.Backend != agent.BackendPTY || result.Sessions.PersistOnExit || !result.Sessions.CleanupOnSuccess {
			t.Fatalf("legacy backend/session defaults = backend %q policy %#v", result.Backend, result.Sessions)
		}
	}
}

func TestGeneratedConfigPublishesExplicitSessionDefaults(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	result, err := LoadOrCreate(path)
	if err != nil {
		t.Fatalf("Load missing config: %v", err)
	}
	if !result.Created {
		t.Fatal("missing config was not reported as created")
	}
	if result.Sessions.PersistOnExit || !result.Sessions.CleanupOnSuccess {
		t.Fatalf("generated session defaults = %#v", result.Sessions)
	}

	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	for _, field := range []string{
		"sessions:\n",
		"persist_on_exit: false\n",
		"cleanup_on_success: true\n",
	} {
		if !strings.Contains(string(payload), field) {
			t.Fatalf("generated config does not contain %q:\n%s", strings.TrimSpace(field), payload)
		}
	}
}
