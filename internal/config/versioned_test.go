package config

import (
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
)

func TestLoadVersionOneAcceptsEveryAgentCountFromOneThroughEight(t *testing.T) {
	for count := 1; count <= maxAgents; count++ {
		t.Run(fmt.Sprintf("%d agents", count), func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeConfigTestFile(t, path, []byte(versionOneDocument(commandAgents(count))))

			result, err := Load(path)
			if err != nil {
				t.Fatalf("Load returned an error: %v", err)
			}
			if result.Version != CurrentVersion || result.Legacy || result.Backend != agent.BackendPTY {
				t.Fatalf("version metadata = version %d legacy %t backend %q", result.Version, result.Legacy, result.Backend)
			}
			if len(result.Agents) != count {
				t.Fatalf("loaded %d agents, want %d", len(result.Agents), count)
			}
			for index, configured := range result.Agents {
				if configured.ID != fmt.Sprintf("agent-%d", index+1) || configured.Name != fmt.Sprintf("Agent %d", index+1) {
					t.Fatalf("agent %d identity = %#v", index, configured)
				}
				if configured.Backend != agent.BackendPTY || configured.Adapter != agent.AdapterGeneric {
					t.Fatalf("agent %d defaults = backend %q adapter %q", index, configured.Backend, configured.Adapter)
				}
			}
		})
	}
}

func TestLoadVersionOneAllowsZeroAgentsForApplicationFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	writeConfigTestFile(t, path, []byte(versionOneDocument("[]")))

	result, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	if result.Version != CurrentVersion || result.Legacy || result.Backend != agent.BackendPTY {
		t.Fatalf("metadata = %#v", result)
	}
	if result.Agents == nil || len(result.Agents) != 0 {
		t.Fatalf("agents = %#v, want an explicit empty slice", result.Agents)
	}
}

func TestLoadVersionOnePreservesArgumentsEnvironmentAndResolvesCwd(t *testing.T) {
	directory := t.TempDir()
	workingDirectory := filepath.Join(directory, "project")
	if err := os.Mkdir(workingDirectory, 0o755); err != nil {
		t.Fatalf("creating cwd: %v", err)
	}
	path := filepath.Join(directory, "config.yaml")
	content := versionOneDocument(`
  - id: literal-argv
    name: Literal argv
    command:
      - runner
      - "argument with spaces"
      - "; printf unsafe"
      - "$(touch must-not-run)"
      - ""
    cwd: project
    env:
      MODEL: "llama 3.2"
      EMPTY: ""
    adapter: generic
    backend: pty`)
	writeConfigTestFile(t, path, []byte(content))

	result, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	wantCommand := []string{"runner", "argument with spaces", "; printf unsafe", "$(touch must-not-run)", ""}
	if !reflect.DeepEqual(result.Agents[0].Command, wantCommand) {
		t.Fatalf("command = %#v, want %#v", result.Agents[0].Command, wantCommand)
	}
	if result.Agents[0].Cwd != workingDirectory {
		t.Fatalf("cwd = %q, want %q", result.Agents[0].Cwd, workingDirectory)
	}
	if !reflect.DeepEqual(result.Agents[0].Env, map[string]string{"MODEL": "llama 3.2", "EMPTY": ""}) {
		t.Fatalf("env = %#v", result.Agents[0].Env)
	}
}

func TestLoadVersionOneAcceptsExplicitShell(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := versionOneDocument(`
  - id: shell-agent
    name: Shell agent
    shell: "  printf '%s' 'preserved verbatim'  "`)
	writeConfigTestFile(t, path, []byte(content))

	result, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned an error: %v", err)
	}
	if got, want := result.Agents[0].Shell, "  printf '%s' 'preserved verbatim'  "; got != want {
		t.Fatalf("shell = %q, want %q", got, want)
	}
}

func TestLoadVersionOneRejectsAgentSemanticErrors(t *testing.T) {
	tests := []struct {
		name        string
		agents      string
		wantMessage string
	}{
		{
			name:        "more than eight agents",
			agents:      commandAgents(maxAgents + 1),
			wantMessage: "maximum",
		},
		{
			name: "duplicate ids ignoring case",
			agents: `
  - id: Alpha
    name: First
    command: [runner]
  - id: " alpha "
    name: Second
    command: [runner]`,
			wantMessage: "duplicates",
		},
		{
			name: "empty command",
			agents: `
  - id: empty
    name: Empty command
    command: []`,
			wantMessage: "exactly one",
		},
		{
			name: "blank executable",
			agents: `
  - id: blank
    name: Blank executable
    command: [" "]`,
			wantMessage: "executable",
		},
		{
			name: "command and shell",
			agents: `
  - id: both
    name: Both modes
    command: [runner]
    shell: echo duplicate`,
			wantMessage: "mutuellement exclusifs",
		},
		{
			name: "missing cwd",
			agents: `
  - id: cwd
    name: Missing cwd
    command: [runner]
    cwd: does-not-exist`,
			wantMessage: "working directory",
		},
		{
			name: "invalid environment name",
			agents: `
  - id: env
    name: Invalid env
    command: [runner]
    env:
      BAD-NAME: value`,
			wantMessage: "environment variable name",
		},
		{
			name: "unsupported agent backend",
			agents: `
  - id: backend
    name: Backend
    command: [runner]
    backend: pipe`,
			wantMessage: "unsupported backend",
		},
		{
			name: "unsupported adapter",
			agents: `
  - id: adapter
    name: Adapter
    command: [runner]
    adapter: claude`,
			wantMessage: "unsupported adapter",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeConfigTestFile(t, path, []byte(versionOneDocument(test.agents)))
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("Load error = %v, want substring %q", err, test.wantMessage)
			}
		})
	}
}

func TestLoadVersionOneRejectsCoercedTypesAndUnknownFields(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "string version", content: strings.Replace(versionOneDocument("[]"), "version: 1", `version: "1"`, 1)},
		{name: "boolean backend", content: strings.Replace(versionOneDocument("[]"), "backend: pty", "backend: true", 1)},
		{name: "numeric id", content: versionOneDocument("\n  - id: 12\n    name: Numeric id\n    command: [runner]")},
		{name: "numeric command argument", content: versionOneDocument("\n  - id: args\n    name: Args\n    command: [runner, 12]")},
		{name: "boolean env value", content: versionOneDocument("\n  - id: env\n    name: Env\n    command: [runner]\n    env:\n      DEBUG: true")},
		{name: "scalar command", content: versionOneDocument("\n  - id: scalar\n    name: Scalar\n    command: runner")},
		{name: "unknown root field", content: versionOneDocument("[]") + "unknown: value\n"},
		{name: "unknown agent field", content: versionOneDocument("\n  - id: unknown\n    name: Unknown\n    command: [runner]\n    typo: value")},
		{name: "agent alias", content: versionOneDocument("\n  - &shared\n    id: aliased\n    name: Aliased\n    command: [runner]\n  - *shared")},
		{name: "agent merge", content: versionOneDocument("\n  - id: merged\n    name: Merged\n    <<: {command: [runner]}")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeConfigTestFile(t, path, []byte(test.content))
			if result, err := Load(path); err == nil {
				t.Fatalf("Load accepted invalid YAML and returned %#v", result)
			}
		})
	}
}

func TestLoadVersionOneRequiresKnownVersionBackendAgentsAndPatterns(t *testing.T) {
	valid := versionOneDocument("[]")
	tests := []struct {
		name    string
		content string
	}{
		{name: "unknown version", content: strings.Replace(valid, "version: 1", "version: 2", 1)},
		{name: "unsupported backend", content: strings.Replace(valid, "backend: pty", "backend: pipe", 1)},
		{name: "missing backend", content: strings.Replace(valid, "backend: pty\n", "", 1)},
		{name: "missing agents", content: strings.Replace(valid, "agents: []\n", "", 1)},
		{name: "null agents", content: strings.Replace(valid, "agents: []", "agents: null", 1)},
		{name: "missing patterns", content: strings.Split(valid, "intercept_patterns:")[0]},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeConfigTestFile(t, path, []byte(test.content))
			if result, err := Load(path); err == nil {
				t.Fatalf("Load accepted incomplete v1 YAML and returned %#v", result)
			}
		})
	}
}

func TestLoadLegacyDocumentsExposeCompatibilityMetadata(t *testing.T) {
	for _, content := range []string{
		"- pattern: valid\n  description: Legacy direct\n",
		"intercept_patterns:\n  - pattern: valid\n    description: Legacy wrapper\n",
	} {
		path := filepath.Join(t.TempDir(), "config.yaml")
		writeConfigTestFile(t, path, []byte(content))
		result, err := Load(path)
		if err != nil {
			t.Fatalf("Load legacy document: %v", err)
		}
		if result.Version != 0 || !result.Legacy || result.Backend != agent.BackendPTY || result.Agents != nil {
			t.Fatalf("legacy metadata = %#v", result)
		}
	}
}

func versionOneDocument(agents string) string {
	return "version: 1\n" +
		"backend: pty\n" +
		"agents: " + agents + "\n" +
		"intercept_patterns:\n" +
		"  - pattern: '(?i)continue'\n" +
		"    description: Continue confirmation\n"
}

func commandAgents(count int) string {
	var result strings.Builder
	result.WriteByte('\n')
	for index := 1; index <= count; index++ {
		fmt.Fprintf(
			&result,
			"  - id: agent-%d\n    name: Agent %d\n    command: [runner, \"argument %d\"]\n",
			index,
			index,
			index,
		)
	}
	return strings.TrimSuffix(result.String(), "\n")
}
