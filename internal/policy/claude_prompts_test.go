package policy

import (
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
)

// claudePrompts are prompts the Claude adapter recognizes for a tool the agent
// wants to run. The strings follow the layouts in the adapter's tests, not its
// stored fixtures, and none is recorded as observed.
var claudePrompts = map[string]string{
	"bash": "Bash command\n" +
		"Run shell command ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
		"echo PROBE_BASH_OK > bash_probe.txt\n" +
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
		"Do you want to proceed?\n" +
		"❯ 1. Yes\n  2. Yes, and always allow access to folder\n  3. No\n\n" +
		"Esc to cancel · Tab to amend",
	"create file": "Create file\n.github/workflows/ci.yml\n" +
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n  1 on: push\n" +
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
		"Do you want to create .github/workflows/ci.yml?\n❯ 1. Yes\n  2. No\n\n" +
		"Esc to cancel · Tab to amend",
	"edit file": "Edit file\npackage.json\n" +
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n- \"test\": \"go test\"\n+ \"postinstall\": \"sh x.sh\"\n" +
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
		"Do you want to make this edit to package.json?\n❯ 1. Yes\n  2. No\n\n" +
		"Esc to cancel · Tab to amend",
}

// What Claude Code is asked to write decides what runs next, so a policy must
// not answer such a prompt on its own, whatever it is configured to allow. The
// refusal is the risk level of the event, which holds even when the adapter
// can encode an automatic answer: before this the create and edit prompts were
// RiskLow, and only the missing encoder kept a permissive profile from
// approving a write to a CI workflow or a package.json script.
func TestAPolicyNeverAnswersAClaudeToolPromptOnItsOwn(t *testing.T) {
	workspace := t.TempDir()
	allowClaude := Config{DefaultAction: ActionAsk, Rules: []Rule{{
		Name:   "allow claude",
		Match:  Match{AgentIDs: []string{"agent-claude"}},
		Action: ActionAllow,
	}}}
	allowConfirmations := Config{DefaultAction: ActionAsk, Rules: []Rule{{
		Name:   "allow confirmations",
		Match:  Match{EventTypes: []adapters.EventType{adapters.EventConfirmation}},
		Action: ActionAllow,
	}}}
	configs := map[string]Config{
		"permissive profile":                      ProfileConfig(ProfilePermissive, workspace),
		"default action allow, no guardrails":     {DefaultAction: ActionAllow},
		"an allow rule for the claude agent":      allowClaude,
		"an allow rule for every confirmation":    allowConfirmations,
		"developer-friendly profile":              ProfileConfig(ProfileDeveloperFriendly, workspace),
		"strict profile":                          ProfileConfig(ProfileStrict, workspace),
		"default action allow with the guardrail": {DefaultAction: ActionAllow, Guardrails: GuardrailsConfig{BlockSensitivePaths: true, BlockOutsideWorkspace: true, WorkspaceRoot: workspace}},
	}

	adapter, err := adapters.NewClaudeAdapter(nil)
	if err != nil {
		t.Fatal(err)
	}
	for promptName, prompt := range claudePrompts {
		state := adapters.NewDetectionState("session-claude-"+promptName, "agent-claude", adapters.ClaudeID)
		events, err := adapter.Detect(state, []byte(prompt))
		if err != nil || len(events) != 1 {
			t.Fatalf("%s: detected %#v, %v", promptName, events, err)
		}
		event := events[0]
		if event.Risk == adapters.RiskLow {
			t.Fatalf("%s is risk %q: a policy would answer it by itself", promptName, event.Risk)
		}
		for configName, config := range configs {
			engine, err := New(config)
			if err != nil {
				t.Fatalf("%s: %v", configName, err)
			}
			evaluation := engine.Evaluate(event)
			if evaluation.Action != ActionAsk || evaluation.Automatic {
				t.Fatalf("%s under %s: evaluation = %#v, want a question for a person", promptName, configName, evaluation)
			}
		}
	}
}
