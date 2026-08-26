package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/policy"
)

func TestLoadPoliciesUsesSafeDefaultsWhenAbsentOrLegacy(t *testing.T) {
	tests := []struct {
		name    string
		content string
	}{
		{name: "versioned policies absent", content: versionOneDocument("[]")},
		{name: "versioned policies empty", content: versionOnePolicyDocument("policies: {}")},
		{
			name:    "legacy direct list",
			content: "- pattern: valid\n  description: Legacy direct\n",
		},
		{
			name:    "legacy wrapped list",
			content: "intercept_patterns:\n  - pattern: valid\n    description: Legacy wrapped\n",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			writeConfigTestFile(t, path, []byte(test.content))

			result, err := Load(path)
			if err != nil {
				t.Fatalf("Load returned an error: %v", err)
			}
			if !reflect.DeepEqual(result.Policies, policy.DefaultConfig()) {
				t.Fatalf("policies = %#v, want safe defaults %#v", result.Policies, policy.DefaultConfig())
			}
			if result.Policies.DefaultAction != policy.ActionAsk || result.Policies.DryRun || len(result.Policies.Rules) != 0 {
				t.Fatalf("unsafe policy defaults: %#v", result.Policies)
			}
		})
	}
}

func TestLoadVersionOnePoliciesDecodesAllFieldsAndPreservesFirstRule(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := versionOnePolicyDocument(`policies:
  default_action: deny
  dry_run: false
  rules:
    - name: first-confirmation
      match:
        event_types: [confirmation, credential]
        text_regex: '(?i)overwrite'
        agent_ids: [agent-a, agent-b]
        risk_levels: [low, unknown, high]
        sensitive: false
      action: allow
    - name: second-agent-rule
      match:
        agent_ids: [agent-a]
      action: deny`)
	writeConfigTestFile(t, path, []byte(content))

	result, err := Load(path)
	if err != nil {
		t.Fatalf("Load returned an error: %v\n%s", err, content)
	}
	if result.Policies.DefaultAction != policy.ActionDeny || result.Policies.DryRun {
		t.Fatalf("policy header = %#v", result.Policies)
	}
	if len(result.Policies.Rules) != 2 {
		t.Fatalf("rules = %#v, want two ordered rules", result.Policies.Rules)
	}

	first := result.Policies.Rules[0]
	if first.Name != "first-confirmation" || first.Action != policy.ActionAllow ||
		first.Match.TextRegex != `(?i)overwrite` || first.Match.Sensitive == nil || *first.Match.Sensitive {
		t.Fatalf("first rule scalar fields = %#v", first)
	}
	if want := []adapters.EventType{adapters.EventConfirmation, adapters.EventCredential}; !reflect.DeepEqual(first.Match.EventTypes, want) {
		t.Fatalf("first event types = %#v, want %#v", first.Match.EventTypes, want)
	}
	if want := []string{"agent-a", "agent-b"}; !reflect.DeepEqual(first.Match.AgentIDs, want) {
		t.Fatalf("first agent ids = %#v, want %#v", first.Match.AgentIDs, want)
	}
	if want := []adapters.RiskLevel{adapters.RiskLow, adapters.RiskUnknown, adapters.RiskHigh}; !reflect.DeepEqual(first.Match.RiskLevels, want) {
		t.Fatalf("first risk levels = %#v, want %#v", first.Match.RiskLevels, want)
	}
	if second := result.Policies.Rules[1]; second.Name != "second-agent-rule" || second.Action != policy.ActionDeny {
		t.Fatalf("second rule = %#v", second)
	}

	engine, err := policy.New(result.Policies)
	if err != nil {
		t.Fatalf("policy.New with loaded config: %v", err)
	}
	evaluation := engine.Evaluate(adapters.Event{
		ID:        "evt-config-policy",
		SessionID: "agent-a",
		AgentID:   "agent-a",
		Adapter:   adapters.GenericID,
		Type:      adapters.EventConfirmation,
		Summary:   "Overwrite file?",
		Risk:      adapters.RiskLow,
	})
	if evaluation.RuleName != "first-confirmation" || evaluation.Action != policy.ActionAllow {
		t.Fatalf("first matching loaded rule was not preserved: %#v", evaluation)
	}
}

func TestLoadVersionOnePoliciesRejectsWrongTypesUnknownAndMissingFields(t *testing.T) {
	tests := []struct {
		name     string
		policies string
	}{
		{name: "null policies", policies: "policies: null"},
		{name: "policies sequence", policies: "policies: []"},
		{name: "boolean default action", policies: "policies:\n  default_action: true"},
		{name: "string dry run", policies: "policies:\n  dry_run: 'false'"},
		{name: "rules mapping", policies: "policies:\n  rules: {}"},
		{name: "null rule", policies: "policies:\n  rules: [null]"},
		{name: "scalar rule", policies: "policies:\n  rules: [invalid]"},
		{name: "boolean name", policies: validPolicyRule("name: true", "match:\n        agent_ids: [agent-a]", "action: ask")},
		{name: "scalar match", policies: validPolicyRule("name: typed", "match: invalid", "action: ask")},
		{name: "boolean action", policies: validPolicyRule("name: typed", "match:\n        agent_ids: [agent-a]", "action: true")},
		{name: "scalar event types", policies: validPolicyRule("name: typed", "match:\n        event_types: confirmation", "action: ask")},
		{name: "numeric event type", policies: validPolicyRule("name: typed", "match:\n        event_types: [confirmation, 7]", "action: ask")},
		{name: "boolean text regex", policies: validPolicyRule("name: typed", "match:\n        text_regex: true", "action: ask")},
		{name: "scalar agent ids", policies: validPolicyRule("name: typed", "match:\n        agent_ids: agent-a", "action: ask")},
		{name: "numeric agent id", policies: validPolicyRule("name: typed", "match:\n        agent_ids: [agent-a, 7]", "action: ask")},
		{name: "scalar risk levels", policies: validPolicyRule("name: typed", "match:\n        risk_levels: low", "action: ask")},
		{name: "numeric risk level", policies: validPolicyRule("name: typed", "match:\n        risk_levels: [low, 7]", "action: ask")},
		{name: "string sensitive", policies: validPolicyRule("name: typed", "match:\n        sensitive: 'false'", "action: ask")},
		{name: "unknown policy field", policies: "policies:\n  default_action: ask\n  typo: true"},
		{name: "unknown rule field", policies: validPolicyRule("name: typed\n      typo: true", "match:\n        agent_ids: [agent-a]", "action: ask")},
		{name: "unknown match field", policies: validPolicyRule("name: typed", "match:\n        agent_ids: [agent-a]\n        typo: true", "action: ask")},
		{name: "missing rule name", policies: validPolicyRule("", "match:\n        agent_ids: [agent-a]", "action: ask")},
		{name: "missing rule match", policies: validPolicyRule("name: missing-match", "", "action: ask")},
		{name: "missing rule action", policies: validPolicyRule("name: missing-action", "match:\n        agent_ids: [agent-a]", "")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := versionOnePolicyDocument(test.policies)
			original := []byte(content)
			writeConfigTestFile(t, path, original)

			if result, err := Load(path); err == nil {
				t.Fatalf("Load accepted invalid policies and returned %#v\n%s", result, content)
			}
			assertConfigFileBytes(t, path, original)
		})
	}
}

func TestLoadVersionOnePoliciesRejectsInvalidSemanticsAndEmptyMatcherLists(t *testing.T) {
	tests := []struct {
		name        string
		policies    string
		wantMessage string
	}{
		{name: "invalid default action", policies: "policies:\n  default_action: approve", wantMessage: "default"},
		{name: "invalid rule action", policies: validPolicyRule("name: bad-action", "match:\n        agent_ids: [agent-a]", "action: approve"), wantMessage: "invalid action"},
		{name: "invalid regex", policies: validPolicyRule("name: bad-regex", "match:\n        text_regex: '['", "action: ask"), wantMessage: "regex"},
		{name: "blank regex", policies: validPolicyRule("name: blank-regex", "match:\n        text_regex: '   '", "action: ask"), wantMessage: "ne peut pas être vide"},
		{name: "invalid event type", policies: validPolicyRule("name: bad-type", "match:\n        event_types: [process_exit]", "action: ask"), wantMessage: "event type"},
		{name: "invalid risk", policies: validPolicyRule("name: bad-risk", "match:\n        risk_levels: [critical]", "action: ask"), wantMessage: "risk level"},
		{
			name: "duplicate rule name case insensitive",
			policies: `policies:
  rules:
    - name: Duplicate
      match:
        agent_ids: [agent-a]
      action: ask
    - name: ' duplicate '
      match:
        agent_ids: [agent-b]
      action: deny`,
			wantMessage: "duplicates",
		},
		{name: "empty event type list", policies: validPolicyRule("name: empty-types", "match:\n        event_types: []", "action: ask"), wantMessage: "ne peut pas être vide"},
		{name: "empty agent id list", policies: validPolicyRule("name: empty-agents", "match:\n        agent_ids: []", "action: ask"), wantMessage: "ne peut pas être vide"},
		{name: "empty risk list", policies: validPolicyRule("name: empty-risks", "match:\n        risk_levels: []", "action: ask"), wantMessage: "ne peut pas être vide"},
		{name: "empty match object", policies: validPolicyRule("name: no-matcher", "match: {}", "action: ask"), wantMessage: "no matcher"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			content := versionOnePolicyDocument(test.policies)
			original := []byte(content)
			writeConfigTestFile(t, path, original)

			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), test.wantMessage) {
				t.Fatalf("Load error = %v, want substring %q\n%s", err, test.wantMessage, content)
			}
			assertConfigFileBytes(t, path, original)
		})
	}
}

func TestGeneratedConfigPublishesSafePolicyDefaultsAndDoesNotOverwrite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yaml")
	result, err := Load(path)
	if err != nil {
		t.Fatalf("Load missing config: %v", err)
	}
	if !result.Created || result.Policies.DefaultAction != policy.ActionAsk || result.Policies.DryRun || len(result.Policies.Rules) != 0 {
		t.Fatalf("generated policy defaults = created %t, policies %#v", result.Created, result.Policies)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read generated config: %v", err)
	}
	for _, field := range []string{
		"policies:\n",
		"default_action: ask\n",
		"dry_run: false\n",
		"rules: []\n",
	} {
		if !strings.Contains(string(payload), field) {
			t.Fatalf("generated config does not contain %q:\n%s", strings.TrimSpace(field), payload)
		}
	}

	original := []byte(versionOnePolicyDocument("policies:\n  default_action: deny\n  dry_run: true\n  rules: []"))
	writeConfigTestFile(t, path, original)
	created, err := createDefault(path)
	if err != nil {
		t.Fatalf("createDefault for existing config: %v", err)
	}
	if created {
		t.Fatal("createDefault reported overwriting an existing policy config")
	}
	assertConfigFileBytes(t, path, original)
}

func versionOnePolicyDocument(policies string) string {
	policies = strings.TrimSuffix(policies, "\n")
	return strings.Replace(versionOneDocument("[]"), "agents: []\n", policies+"\nagents: []\n", 1)
}

func validPolicyRule(name, match, action string) string {
	fields := make([]string, 0, 3)
	for _, field := range []string{name, match, action} {
		if field != "" {
			fields = append(fields, "      "+strings.ReplaceAll(field, "\n", "\n      "))
		}
	}
	return "policies:\n  rules:\n    -\n" + strings.Join(fields, "\n")
}
