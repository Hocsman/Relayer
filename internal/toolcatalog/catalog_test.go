package toolcatalog

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
)

func TestDescriptorsExposeMinimalDeclarativeInventory(t *testing.T) {
	want := []Descriptor{
		{ID: ClaudeCode, Name: "Claude Code", Executables: []string{"claude"}, DefaultAdapter: agent.AdapterGeneric},
		{ID: CodexCLI, Name: "Codex CLI", Executables: []string{"codex"}, DefaultAdapter: agent.AdapterGeneric},
		{ID: MimoCode, Name: "MiMo Code", Executables: []string{"mimo"}, DefaultAdapter: agent.AdapterGeneric},
		{ID: Custom, Name: "Custom CLI", DefaultAdapter: agent.AdapterGeneric, RequiresExecutable: true},
	}

	got := Descriptors()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("Descriptors() = %#v, want %#v", got, want)
	}

	got[0].Name = "mutated"
	got[0].Executables[0] = "mutated"
	again := Descriptors()
	if !reflect.DeepEqual(again, want) {
		t.Fatalf("catalogue changed through returned descriptor: %#v", again)
	}
}

func TestLookupNormalizesIDAndReturnsDefensiveCopy(t *testing.T) {
	descriptor, ok := Lookup(ProfileID("  CLAUDE-CODE "))
	if !ok || descriptor.ID != ClaudeCode || !reflect.DeepEqual(descriptor.Executables, []string{"claude"}) {
		t.Fatalf("Lookup() = %#v, %t", descriptor, ok)
	}
	descriptor.Executables[0] = "mutated"

	again, ok := Lookup(ClaudeCode)
	if !ok || !reflect.DeepEqual(again.Executables, []string{"claude"}) {
		t.Fatalf("Lookup() retained caller mutation: %#v, %t", again, ok)
	}
	if _, ok := Lookup("missing"); ok {
		t.Fatal("unknown profile unexpectedly found")
	}
}

func TestResolvePreservesExactArgvWithoutInventingToolOptions(t *testing.T) {
	tests := []struct {
		name    string
		request LaunchRequest
		want    agent.Spec
	}{
		{
			name: "claude default executable",
			request: LaunchRequest{
				ProfileID: ClaudeCode,
				AgentID:   " reviewer ",
				Name:      " Claude reviewer ",
				Args:      []string{"--print", "value with spaces", "", "$(literal)"},
				Cwd:       "/workspace",
				Backend:   " auto ",
			},
			want: agent.Spec{
				ID:      "reviewer",
				Name:    "Claude reviewer",
				Command: []string{"claude", "--print", "value with spaces", "", "$(literal)"},
				Cwd:     "/workspace",
				Adapter: agent.AdapterGeneric,
				Backend: agent.BackendAuto,
			},
		},
		{
			name: "codex explicit executable and adapter",
			request: LaunchRequest{
				ProfileID:  CodexCLI,
				AgentID:    "coder",
				Name:       "Coder",
				Executable: "/opt/tools/codex-preview",
				Args:       []string{"review", "--untrusted-argument"},
				Adapter:    " custom-adapter ",
				Backend:    agent.BackendPTY,
			},
			want: agent.Spec{
				ID:      "coder",
				Name:    "Coder",
				Command: []string{"/opt/tools/codex-preview", "review", "--untrusted-argument"},
				Adapter: "custom-adapter",
				Backend: agent.BackendPTY,
			},
		},
		{
			name:    "mimo verified executable name only",
			request: LaunchRequest{ProfileID: MimoCode, AgentID: "mimo", Name: "MiMo"},
			want: agent.Spec{
				ID: "mimo", Name: "MiMo", Command: []string{"mimo"}, Adapter: agent.AdapterGeneric,
			},
		},
		{
			name: "custom requires caller argv",
			request: LaunchRequest{
				ProfileID: Custom, AgentID: "local", Name: "Local", Executable: "./bin/local-agent",
				Args: []string{"--model-selected-by-caller", "opaque-value"},
			},
			want: agent.Spec{
				ID: "local", Name: "Local",
				Command: []string{"./bin/local-agent", "--model-selected-by-caller", "opaque-value"},
				Adapter: agent.AdapterGeneric,
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := Resolve(test.request)
			if err != nil {
				t.Fatalf("Resolve() error = %v", err)
			}
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Resolve() = %#v, want %#v", got, test.want)
			}
			if got.Shell != "" || got.Env != nil {
				t.Fatalf("catalogue invented shell or environment: %#v", got)
			}
		})
	}
}

func TestResolveCopiesArguments(t *testing.T) {
	arguments := []string{"one", "two"}
	spec, err := Resolve(LaunchRequest{
		ProfileID:  Custom,
		AgentID:    "agent",
		Name:       "Agent",
		Executable: "tool",
		Args:       arguments,
	})
	if err != nil {
		t.Fatal(err)
	}
	arguments[0] = "mutated"
	if !reflect.DeepEqual(spec.Command, []string{"tool", "one", "two"}) {
		t.Fatalf("resolved command retained caller storage: %#v", spec.Command)
	}
}

func TestResolveRejectsInvalidRequests(t *testing.T) {
	tests := []struct {
		name    string
		request LaunchRequest
		part    string
	}{
		{name: "unknown profile", request: LaunchRequest{ProfileID: "missing", AgentID: "a", Name: "A"}, part: "unknown tool profile"},
		{name: "blank id", request: LaunchRequest{ProfileID: ClaudeCode, Name: "A"}, part: "agent id"},
		{name: "blank name", request: LaunchRequest{ProfileID: ClaudeCode, AgentID: "a"}, part: "agent name"},
		{name: "custom executable omitted", request: LaunchRequest{ProfileID: Custom, AgentID: "a", Name: "A"}, part: "requires an explicit executable"},
		{name: "custom executable whitespace", request: LaunchRequest{ProfileID: Custom, AgentID: "a", Name: "A", Executable: "  "}, part: "requires an explicit executable"},
		{name: "nul executable", request: LaunchRequest{ProfileID: ClaudeCode, AgentID: "a", Name: "A", Executable: "bad\x00tool"}, part: "executable contains"},
		{name: "nul argument", request: LaunchRequest{ProfileID: ClaudeCode, AgentID: "a", Name: "A", Args: []string{"bad\x00arg"}}, part: "argument 0"},
		{name: "nul cwd", request: LaunchRequest{ProfileID: ClaudeCode, AgentID: "a", Name: "A", Cwd: "bad\x00dir"}, part: "working directory"},
		{name: "nul adapter", request: LaunchRequest{ProfileID: ClaudeCode, AgentID: "a", Name: "A", Adapter: "bad\x00adapter"}, part: "adapter"},
		{name: "nul backend", request: LaunchRequest{ProfileID: ClaudeCode, AgentID: "a", Name: "A", Backend: "bad\x00backend"}, part: "backend"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := Resolve(test.request)
			if err == nil || !strings.Contains(err.Error(), test.part) {
				t.Fatalf("Resolve() error = %v, want containing %q", err, test.part)
			}
		})
	}
}

func TestResolvedSpecUsesCanonicalAgentValidation(t *testing.T) {
	spec, err := Resolve(LaunchRequest{
		ProfileID: ClaudeCode,
		AgentID:   "agent",
		Name:      "Agent",
		Args:      []string{"--literal"},
	})
	if err != nil {
		t.Fatal(err)
	}
	validated, err := agent.ValidateSpec(spec, t.TempDir(), agent.BackendPTY)
	if err != nil {
		t.Fatalf("agent.ValidateSpec(resolved) error = %v", err)
	}
	if validated.Backend != agent.BackendPTY || !reflect.DeepEqual(validated.Command, spec.Command) {
		t.Fatalf("validated spec = %#v", validated)
	}
}
