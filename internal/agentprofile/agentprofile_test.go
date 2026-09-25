package agentprofile

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

const envSecret = "sk-ant-AGENTPROFILE-SECRET-0001"

// fileAgents is one agent of each kind a hand-written configuration holds,
// as the loader returns them: a catalogue CLI with environment variables, a
// custom command, a shell script and a catalogue CLI with no adapter.
func fileAgents(directory string) []agent.Spec {
	return []agent.Spec{
		{ID: "claude", Name: "Claude", Command: []string{"claude", "--model", "opus"}, Cwd: directory,
			Env: map[string]string{"ANTHROPIC_API_KEY": envSecret}, Backend: agent.BackendPTY},
		{ID: "reviewer", Name: "Reviewer", Command: []string{"./bin/reviewer", "--mode", "read only"}, Cwd: directory,
			Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY},
		{ID: "scripted", Name: "Scripted", Shell: `prepare-input | exec ./agent --mode "$RELAYER_MODE"`,
			Env: map[string]string{"RELAYER_MODE": "fast"}, Backend: agent.BackendPTY},
		{ID: "codex", Name: "Codex", Command: []string{"codex"}, Backend: agent.BackendPTY},
	}
}

// sentBack is what an editor sends for profiles it did not change: the
// interface's profilesForSave, which sends no argv for a preserved profile.
func sentBack(profiles []Profile) []Input {
	inputs := make([]Input, len(profiles))
	for index, profile := range profiles {
		inputs[index] = Input{
			ID: profile.ID, Name: profile.Name, PresetID: profile.PresetID, Cwd: profile.Cwd,
			Backend: profile.Backend, Adapter: profile.Adapter, Preserve: profile.PreserveOnSave,
		}
	}
	return inputs
}

func views(specs []agent.Spec) []Profile {
	profiles := make([]Profile, len(specs))
	for index, spec := range specs {
		profiles[index] = View(spec)
	}
	return profiles
}

// The editor is shown no environment value, no shell body and no argv, and an
// agent it cannot represent is read-only. Every profile names a catalogue entry
// and its effective adapter, so a form built from the view validates.
func TestTheViewShowsNoValueTheEditorCannotRoundTrip(t *testing.T) {
	directory := t.TempDir()
	profiles := views(fileAgents(directory))

	encoded, err := json.Marshal(profiles)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{envSecret, "ANTHROPIC_API_KEY", "RELAYER_MODE", "prepare-input", "--model", "opus", "./bin/reviewer", "read only"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Errorf("the view carries %q: %s", forbidden, encoded)
		}
	}
	want := []struct {
		locked   bool
		reason   string
		preset   toolcatalog.ProfileID
		adapter  string
		argCount int
	}{
		{true, ReasonEnvironment, toolcatalog.Custom, "claude", 0},
		{false, "", toolcatalog.Custom, agent.AdapterGeneric, 2},
		{true, ReasonShell, toolcatalog.Custom, agent.AdapterGeneric, 0},
		{false, "", toolcatalog.CodexCLI, "codex", 0},
	}
	for index, profile := range profiles {
		expected := want[index]
		if profile.Locked != expected.locked || profile.ReadOnlyReason != expected.reason ||
			profile.PresetID != string(expected.preset) || profile.Adapter != expected.adapter ||
			profile.ArgumentCount != expected.argCount || !profile.PreserveOnSave || len(profile.Argv) != 0 {
			t.Errorf("profile %d = %+v, want %+v", index, profile, expected)
		}
	}
}

// A save of the view as it was shown gives back the file's agents exactly.
func TestAnUnchangedSaveResolvesToTheFilesAgents(t *testing.T) {
	directory := t.TempDir()
	current := fileAgents(directory)
	specs, err := Resolve(sentBack(views(current)), current, directory, agent.BackendPTY)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !reflect.DeepEqual(specs, current) {
		t.Fatalf("resolved agents differ from the file's:\n got %#v\nwant %#v", specs, current)
	}
}

// A preserved agent takes its name, working directory and backend from the
// form and keeps the rest; a locked one ignores the form altogether.
func TestAPreservedAgentTakesOnlyWhatTheFormShows(t *testing.T) {
	directory := t.TempDir()
	current := fileAgents(directory)
	inputs := sentBack(views(current))
	for index := range inputs {
		inputs[index].Name = "Renamed " + inputs[index].ID
		inputs[index].Backend = agent.BackendTmux
	}
	specs, err := Resolve(inputs, current, directory, agent.BackendPTY)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	for index, spec := range specs {
		want := current[index]
		if LockedReason(want) == "" {
			want.Name = "Renamed " + want.ID
			want.Backend = agent.BackendTmux
		}
		if !reflect.DeepEqual(spec, want) {
			t.Errorf("agent %d = %#v, want %#v", index, spec, want)
		}
	}
}

// A read-only agent cannot be dropped, replaced or reused by a new agent, and
// Preserve cannot conjure an agent the file does not have.
func TestASaveCannotDropReplaceOrInventAnAgent(t *testing.T) {
	directory := t.TempDir()
	current := fileAgents(directory)
	unchanged := sentBack(views(current))
	replacement := Input{ID: "claude", Name: "Claude", PresetID: string(toolcatalog.ClaudeCode), Cwd: directory,
		Backend: agent.BackendPTY, Adapter: "claude", Argv: []string{"claude"}}

	for _, test := range []struct {
		name   string
		inputs []Input
	}{
		{"a locked agent left out", unchanged[1:]},
		{"a locked agent sent as a new one", append([]Input{replacement}, unchanged[1:]...)},
		{"a locked agent sent with a command", func() []Input {
			inputs := append([]Input(nil), unchanged...)
			inputs[2].Argv = []string{"sh"}
			return inputs
		}()},
		{"an agent the file does not have, preserved", append(append([]Input(nil), unchanged...),
			Input{ID: "ghost", Name: "Ghost", PresetID: "custom", Backend: agent.BackendPTY, Preserve: true})},
		{"a new agent reusing a locked identifier in another case", append(append([]Input(nil), unchanged[:3]...),
			Input{ID: "CLAUDE", Name: "Again", PresetID: "custom", Backend: agent.BackendPTY, Adapter: agent.AdapterGeneric, Argv: []string{"runner"}},
		)},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := Resolve(test.inputs, current, directory, agent.BackendPTY); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Resolve error = %v, want ErrInvalid", err)
			}
		})
	}
}

// An agent removed and added again under its identifier is a new agent: it
// has what the form gave it and nothing of the one it replaces.
func TestANewAgentInheritsNothingFromTheIdentifierItReuses(t *testing.T) {
	directory := t.TempDir()
	current := []agent.Spec{{ID: "worker", Name: "Worker", Command: []string{"runner", "--flag"}, Cwd: directory,
		Adapter: agent.AdapterGeneric, Backend: agent.BackendTmux}}
	specs, err := Resolve([]Input{{ID: "worker", Name: "Worker", PresetID: "custom", Backend: agent.BackendPTY,
		Adapter: agent.AdapterGeneric, Argv: []string{"other"}}}, current, directory, agent.BackendPTY)
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	want := agent.Spec{ID: "worker", Name: "Worker", Command: []string{"other"}, Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY}
	if len(specs) != 1 || !reflect.DeepEqual(specs[0], want) {
		t.Fatalf("resolved = %#v, want %#v", specs, want)
	}
}

// A refusal names the agent by its position, never by a value it carries.
func TestARefusalQuotesNoValue(t *testing.T) {
	directory := t.TempDir()
	_, err := Resolve([]Input{{ID: "keyed", Name: "Keyed", PresetID: "custom", Backend: agent.BackendPTY,
		Argv: []string{"runner", "--api-key", envSecret}}}, nil, directory, agent.BackendPTY)
	if !errors.Is(err, ErrInvalid) || strings.Contains(err.Error(), envSecret) || !strings.Contains(err.Error(), "agent 1") {
		t.Fatalf("Resolve error = %v, want ErrInvalid naming agent 1 and no value", err)
	}
}
