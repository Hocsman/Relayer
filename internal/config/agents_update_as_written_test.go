package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
)

// handWrittenAgentsDocument is a configuration a person wrote: comments, flow
// sequences, quoted arguments, agents that inherit the file's backend and an
// agent whose adapter is left to its executable.
const handWrittenAgentsDocument = `# Relayer configuration (hand-written)
version: 1
backend: pty
agents:
  # the main coding agent
  - id: claude
    name: Claude
    command: [claude, --model, opus]
    cwd: ./ws
    env:
      ANTHROPIC_API_KEY: sk-ant-AS-WRITTEN-0001 # secret
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
    command: [codex] # inherits the backend
intercept_patterns:
  - pattern: (?i)\[[yn]/[yn]\]
    description: yes/no confirmation
`

func writeHandWrittenAgents(t *testing.T) (string, Result) {
	t.Helper()
	directory := t.TempDir()
	for _, sub := range []string{"ws", "other"} {
		if err := os.Mkdir(filepath.Join(directory, sub), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	path := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(path, []byte(handWrittenAgentsDocument), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExisting(path)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	return path, loaded
}

func readText(t *testing.T, path string) string {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(payload)
}

// A save that changes one agent changes that agent's lines and nothing else:
// the other agents keep their comments, flow sequences, quoting, and the
// backend and adapter they leave to the file and their executable. Each save
// rebuilt every entry: every comment went, every command became a block list,
// and every agent was pinned to "backend: pty", so a later change of the
// file's backend no longer reached it.
func TestSavingOneAgentLeavesTheOthersAsWritten(t *testing.T) {
	for _, writer := range []string{"ReplaceAgents", "UpdateFullConfiguration"} {
		t.Run(writer, func(t *testing.T) {
			path, loaded := writeHandWrittenAgents(t)
			specs := append([]agent.Spec(nil), loaded.Agents...)
			specs[1].Name = "Reviewer, renamed"
			var err error
			if writer == "ReplaceAgents" {
				_, _, err = ReplaceAgents(path, loaded.Revision, specs)
			} else {
				_, _, err = UpdateFullConfiguration(path, loaded.Revision, FullConfigurationUpdate{Agents: specs, UpdateAgents: true})
			}
			if err != nil {
				t.Fatalf("%s: %v", writer, err)
			}
			want := strings.Replace(handWrittenAgentsDocument, "name: Repository reviewer", "name: Reviewer, renamed", 1)
			if got := readText(t, path); got != want {
				t.Fatalf("the save rewrote more than the renamed agent:\n%s\nwant:\n%s", got, want)
			}
		})
	}
}

// A field that changes is written where it was; the fields of the same agent
// that did not change keep their text, a relative working directory
// included.
func TestAChangedAgentKeepsTheFieldsItDidNotChange(t *testing.T) {
	path, loaded := writeHandWrittenAgents(t)
	directory := filepath.Dir(path)
	specs := append([]agent.Spec(nil), loaded.Agents...)
	specs[1].Cwd = filepath.Join(directory, "other")
	specs[1].Backend = agent.BackendTmux
	requested := append([]agent.Spec(nil), specs...)
	requested[1].Cwd = "other"
	if _, _, err := ReplaceAgents(path, loaded.Revision, requested); err != nil {
		t.Fatalf("ReplaceAgents: %v", err)
	}
	want := strings.Replace(handWrittenAgentsDocument,
		"    cwd: ./ws\n    adapter: generic\n    backend: pty\n",
		"    cwd: other\n    adapter: generic\n    backend: tmux\n", 1)
	if got := readText(t, path); got != want {
		t.Fatalf("the changed agent lost what did not change:\n%s\nwant:\n%s", got, want)
	}
}

// The fields the editors never change are written in place too when a caller
// of the configuration package changes them: a shell agent given a command
// keeps its place, and an environment variable that changes leaves the others
// and their comments as they were.
func TestFieldsTheEditorsNeverChangeAreWrittenInPlace(t *testing.T) {
	path, loaded := writeHandWrittenAgents(t)
	specs := append([]agent.Spec(nil), loaded.Agents...)
	specs[0].Env = map[string]string{"ANTHROPIC_API_KEY": "sk-ant-AS-WRITTEN-0001", "RELAYER_ROLE": "reviewer"}
	specs[2].Shell = ""
	specs[2].Command = []string{"./agent", "--mode", "fast"}
	specs[2].Env = nil
	if _, _, err := ReplaceAgents(path, loaded.Revision, specs); err != nil {
		t.Fatalf("ReplaceAgents: %v", err)
	}
	want := strings.Replace(handWrittenAgentsDocument, "      RELAYER_ROLE: coder\n", "      RELAYER_ROLE: reviewer\n", 1)
	want = strings.Replace(want,
		"    shell: 'prepare-input | exec ./agent --mode \"$RELAYER_MODE\"'\n    env:\n      RELAYER_MODE: fast\n",
		"    command:\n      - ./agent\n      - --mode\n      - fast\n", 1)
	if got := readText(t, path); got != want {
		t.Fatalf("the changed fields were not written in place:\n%s\nwant:\n%s", got, want)
	}
}

// A save that changes nothing writes nothing: the file keeps its bytes and
// its revision. The web gateway's "Restart the agents" sends every agent
// even when none was edited, and each one rewrote the whole file.
func TestAnAgentsSaveThatChangesNothingWritesNothing(t *testing.T) {
	for _, writer := range []string{"ReplaceAgents", "UpdateFullConfiguration"} {
		t.Run(writer, func(t *testing.T) {
			path, loaded := writeHandWrittenAgents(t)
			var revision string
			var err error
			if writer == "ReplaceAgents" {
				_, revision, err = ReplaceAgents(path, loaded.Revision, loaded.Agents)
			} else {
				_, revision, err = UpdateFullConfiguration(path, loaded.Revision, FullConfigurationUpdate{Agents: loaded.Agents, UpdateAgents: true})
			}
			if err != nil {
				t.Fatalf("%s: %v", writer, err)
			}
			if got := readText(t, path); got != handWrittenAgentsDocument || revision != loaded.Revision {
				t.Fatalf("an unchanged save rewrote the file (revision %q, was %q):\n%s", revision, loaded.Revision, got)
			}
		})
	}
}

// An agent that replaces another under a new ID is written with the text a
// working directory already has in the file when it names the same directory.
// The editor shows working directories resolved, so a relative one came back
// absolute, and the user's home directory with it.
func TestANewAgentAtADirectoryTheFileNamesKeepsItsText(t *testing.T) {
	path, loaded := writeHandWrittenAgents(t)
	directory := filepath.Dir(path)
	specs := append([]agent.Spec(nil), loaded.Agents[:3]...)
	specs = append(specs, agent.Spec{ID: "codex-two", Name: "Codex", Command: []string{"codex"},
		Cwd: filepath.Join(directory, "ws"), Adapter: "codex", Backend: agent.BackendPTY})
	if _, _, err := ReplaceAgents(path, loaded.Revision, specs); err != nil {
		t.Fatalf("ReplaceAgents: %v", err)
	}
	got := readText(t, path)
	if strings.Contains(got, directory) {
		t.Fatalf("the new agent's working directory was written absolute:\n%s", got)
	}
	if !strings.Contains(got, "  - id: codex-two\n    name: Codex\n    command:\n      - codex\n    cwd: ./ws\n") {
		t.Fatalf("the new agent was not written with the file's text for its directory:\n%s", got)
	}
}
