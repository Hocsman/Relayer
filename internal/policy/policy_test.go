package policy

import (
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
)

func TestEvaluateMatrix(t *testing.T) {
	base := actionableEvent()
	falseValue := false
	trueValue := true

	tests := []struct {
		name   string
		config Config
		mutate func(*adapters.Event)
		want   Evaluation
	}{
		{
			name:   "default ask",
			config: DefaultConfig(),
			want:   evaluation(ActionAsk, ActionAsk, "", ReasonDefault, false, false),
		},
		{
			name:   "automatic default allow",
			config: Config{DefaultAction: ActionAllow},
			want:   evaluation(ActionAllow, ActionAllow, "", ReasonDefault, true, false),
		},
		{
			name: "first matching rule wins",
			config: Config{DefaultAction: ActionAsk, Rules: []Rule{
				{Name: "first", Match: Match{TextRegex: `(?i)overwrite`}, Action: ActionAllow},
				{Name: "second", Match: Match{AgentIDs: []string{"agent-a"}}, Action: ActionDeny},
			}},
			want: evaluation(ActionAllow, ActionAllow, "first", ReasonRule, true, false),
		},
		{
			name: "matcher fields use and and list values use or",
			config: Config{DefaultAction: ActionAsk, Rules: []Rule{{
				Name: "compound",
				Match: Match{
					EventTypes: []adapters.EventType{adapters.EventCredential, adapters.EventConfirmation},
					TextRegex:  `Overwrite target\?\n\[Y/n\]`,
					AgentIDs:   []string{"another", "AGENT-A"},
					RiskLevels: []adapters.RiskLevel{adapters.RiskLow, adapters.RiskUnknown},
					Sensitive:  &falseValue,
				},
				Action: ActionDeny,
			}}},
			want: evaluation(ActionDeny, ActionDeny, "compound", ReasonRule, true, false),
		},
		{
			name: "one nonmatching field falls back to default",
			config: Config{DefaultAction: ActionDeny, Rules: []Rule{{
				Name: "wrong risk",
				Match: Match{
					AgentIDs:   []string{"agent-a"},
					RiskLevels: []adapters.RiskLevel{adapters.RiskHigh},
				},
				Action: ActionAllow,
			}}},
			want: evaluation(ActionDeny, ActionDeny, "", ReasonDefault, true, false),
		},
		{
			name: "event type mismatch falls back to default",
			config: Config{DefaultAction: ActionDeny, Rules: []Rule{{
				Name: "credential-only", Match: Match{EventTypes: []adapters.EventType{adapters.EventCredential}}, Action: ActionAllow,
			}}},
			want: evaluation(ActionDeny, ActionDeny, "", ReasonDefault, true, false),
		},
		{
			name: "agent mismatch falls back to default",
			config: Config{DefaultAction: ActionDeny, Rules: []Rule{{
				Name: "other-agent", Match: Match{AgentIDs: []string{"agent-b"}}, Action: ActionAllow,
			}}},
			want: evaluation(ActionDeny, ActionDeny, "", ReasonDefault, true, false),
		},
		{
			name: "sensitive mismatch falls back to default",
			config: Config{DefaultAction: ActionDeny, Rules: []Rule{{
				Name: "sensitive-only", Match: Match{Sensitive: &trueValue}, Action: ActionAllow,
			}}},
			want: evaluation(ActionDeny, ActionDeny, "", ReasonDefault, true, false),
		},
		{
			name: "text matcher ignores metadata",
			config: Config{DefaultAction: ActionAsk, Rules: []Rule{{
				Name: "metadata-is-not-text", Match: Match{TextRegex: "metadata-secret"}, Action: ActionAllow,
			}}},
			want: evaluation(ActionAsk, ActionAsk, "", ReasonDefault, false, false),
		},
		{
			name: "matched ask is not automatic",
			config: Config{DefaultAction: ActionAllow, Rules: []Rule{{
				Name: "human", Match: Match{AgentIDs: []string{"agent-a"}}, Action: ActionAsk,
			}}},
			want: evaluation(ActionAsk, ActionAsk, "human", ReasonRule, false, false),
		},
		{
			name:   "blank event id is invalid",
			config: Config{DefaultAction: ActionAllow},
			mutate: func(event *adapters.Event) { event.ID = " \t" },
			want:   evaluation(ActionAsk, ActionAsk, "", ReasonInvalidEvent, false, false),
		},
		{
			name:   "blank session id is invalid",
			config: Config{DefaultAction: ActionDeny},
			mutate: func(event *adapters.Event) { event.SessionID = "" },
			want:   evaluation(ActionAsk, ActionAsk, "", ReasonInvalidEvent, false, false),
		},
		{
			name:   "blank agent id is invalid",
			config: Config{DefaultAction: ActionDeny},
			mutate: func(event *adapters.Event) { event.AgentID = " \n" },
			want:   evaluation(ActionAsk, ActionAsk, "", ReasonInvalidEvent, false, false),
		},
		{
			name:   "blank adapter is invalid",
			config: Config{DefaultAction: ActionDeny},
			mutate: func(event *adapters.Event) { event.Adapter = "" },
			want:   evaluation(ActionAsk, ActionAsk, "", ReasonInvalidEvent, false, false),
		},
		{
			name:   "unknown risk enum is invalid",
			config: Config{DefaultAction: ActionDeny},
			mutate: func(event *adapters.Event) { event.Risk = adapters.RiskLevel("critical") },
			want:   evaluation(ActionAsk, ActionAsk, "", ReasonInvalidEvent, false, false),
		},
		{
			name:   "process exit is not actionable",
			config: Config{DefaultAction: ActionAllow},
			mutate: func(event *adapters.Event) { event.Type = adapters.EventProcessExit },
			want:   evaluation(ActionAsk, ActionAsk, "", ReasonNonActionable, false, false),
		},
		{
			name: "credential forces effective ask after matching",
			config: Config{DefaultAction: ActionAsk, Rules: []Rule{{
				Name:   "credential-rule",
				Match:  Match{EventTypes: []adapters.EventType{adapters.EventCredential}, Sensitive: &trueValue},
				Action: ActionAllow,
			}}},
			mutate: func(event *adapters.Event) {
				event.Type = adapters.EventCredential
				event.Sensitive = false
			},
			want: evaluation(ActionAsk, ActionAllow, "credential-rule", ReasonSensitive, false, false),
		},
		{
			name: "sensitive confirmation forces effective ask",
			config: Config{DefaultAction: ActionAsk, Rules: []Rule{{
				Name: "sensitive-rule", Match: Match{Sensitive: &trueValue}, Action: ActionDeny,
			}}},
			mutate: func(event *adapters.Event) { event.Sensitive = true },
			want:   evaluation(ActionAsk, ActionDeny, "sensitive-rule", ReasonSensitive, false, false),
		},
		{
			name: "dry run retains allow proposal",
			config: Config{DefaultAction: ActionAsk, DryRun: true, Rules: []Rule{{
				Name: "would-allow", Match: Match{AgentIDs: []string{"agent-a"}}, Action: ActionAllow,
			}}},
			want: evaluation(ActionAsk, ActionAllow, "would-allow", ReasonDryRun, false, true),
		},
		{
			name:   "allow requires explicit low risk",
			config: Config{DefaultAction: ActionAllow},
			mutate: func(event *adapters.Event) { event.Risk = adapters.RiskUnknown },
			want:   evaluation(ActionAsk, ActionAllow, "", ReasonRisk, false, false),
		},
		{
			name:   "allow high risk remains human",
			config: Config{DefaultAction: ActionAllow},
			mutate: func(event *adapters.Event) { event.Risk = adapters.RiskHigh },
			want:   evaluation(ActionAsk, ActionAllow, "", ReasonRisk, false, false),
		},
		{
			name:   "deny unknown risk remains automatic",
			config: Config{DefaultAction: ActionDeny},
			mutate: func(event *adapters.Event) { event.Risk = adapters.RiskUnknown },
			want:   evaluation(ActionDeny, ActionDeny, "", ReasonDefault, true, false),
		},
		{
			name:   "risk gate precedes dry run reason",
			config: Config{DefaultAction: ActionAllow, DryRun: true},
			mutate: func(event *adapters.Event) { event.Risk = adapters.RiskUnknown },
			want:   evaluation(ActionAsk, ActionAllow, "", ReasonRisk, false, true),
		},
		{
			name:   "dry run retains deny default",
			config: Config{DefaultAction: ActionDeny, DryRun: true},
			want:   evaluation(ActionAsk, ActionDeny, "", ReasonDryRun, false, true),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, err := New(test.config)
			if err != nil {
				t.Fatal(err)
			}
			event := base.Clone()
			if test.mutate != nil {
				test.mutate(&event)
			}
			got := engine.Evaluate(event)
			test.want.EventID = event.ID
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("Evaluate() = %#v, want %#v", got, test.want)
			}
		})
	}
}

func TestNewValidationMatrix(t *testing.T) {
	validRule := func() Rule {
		return Rule{Name: "valid", Match: Match{AgentIDs: []string{"agent-a"}}, Action: ActionAllow}
	}
	tests := []struct {
		name   string
		config Config
		needle string
	}{
		{name: "blank default", config: Config{}, needle: "default"},
		{name: "unknown default", config: Config{DefaultAction: Action("ALLOW")}, needle: "default"},
		{name: "blank rule name", config: Config{DefaultAction: ActionAsk, Rules: []Rule{{Match: Match{AgentIDs: []string{"a"}}, Action: ActionAllow}}}, needle: "blank name"},
		{name: "nul rule name", config: Config{DefaultAction: ActionAsk, Rules: []Rule{{Name: "bad\x00name", Match: Match{AgentIDs: []string{"a"}}, Action: ActionAllow}}}, needle: "NUL"},
		{name: "duplicate names case insensitive", config: Config{DefaultAction: ActionAsk, Rules: []Rule{
			validRule(), {Name: " VALID ", Match: Match{TextRegex: "continue"}, Action: ActionDeny},
		}}, needle: "duplicates"},
		{name: "duplicate names use unicode equal fold", config: Config{DefaultAction: ActionAsk, Rules: []Rule{
			{Name: "K", Match: Match{TextRegex: "first"}, Action: ActionAllow},
			{Name: "K", Match: Match{TextRegex: "second"}, Action: ActionDeny},
		}}, needle: "duplicates"},
		{name: "invalid rule action", config: Config{DefaultAction: ActionAsk, Rules: []Rule{{Name: "bad", Match: Match{TextRegex: "x"}, Action: Action("approve")}}}, needle: "invalid action"},
		{name: "no matcher", config: Config{DefaultAction: ActionAsk, Rules: []Rule{{Name: "empty", Action: ActionAsk}}}, needle: "no matcher"},
		{name: "process exit matcher", config: Config{DefaultAction: ActionAsk, Rules: []Rule{{Name: "exit", Match: Match{EventTypes: []adapters.EventType{adapters.EventProcessExit}}, Action: ActionAsk}}}, needle: "event type"},
		{name: "unknown event matcher", config: Config{DefaultAction: ActionAsk, Rules: []Rule{{Name: "unknown", Match: Match{EventTypes: []adapters.EventType{"tool_call"}}, Action: ActionAsk}}}, needle: "event type"},
		{name: "unknown risk", config: Config{DefaultAction: ActionAsk, Rules: []Rule{{Name: "risk", Match: Match{RiskLevels: []adapters.RiskLevel{"critical"}}, Action: ActionAsk}}}, needle: "risk level"},
		{name: "blank agent id", config: Config{DefaultAction: ActionAsk, Rules: []Rule{{Name: "agent", Match: Match{AgentIDs: []string{" "}}, Action: ActionAsk}}}, needle: "blank agent"},
		{name: "nul agent id", config: Config{DefaultAction: ActionAsk, Rules: []Rule{{Name: "agent", Match: Match{AgentIDs: []string{"agent\x00a"}}, Action: ActionAsk}}}, needle: "NUL"},
		{name: "nul regex", config: Config{DefaultAction: ActionAsk, Rules: []Rule{{Name: "regex", Match: Match{TextRegex: "a\x00b"}, Action: ActionAsk}}}, needle: "NUL"},
		{name: "invalid regex", config: Config{DefaultAction: ActionAsk, Rules: []Rule{{Name: "regex", Match: Match{TextRegex: "["}, Action: ActionAsk}}}, needle: "text regex"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			engine, err := New(test.config)
			if err == nil || engine != nil || !strings.Contains(err.Error(), test.needle) {
				t.Fatalf("New() = engine %#v, error %v; want error containing %q", engine, err, test.needle)
			}
		})
	}
}

func TestNewAcceptsEverySupportedActionEventAndRisk(t *testing.T) {
	for _, action := range []Action{ActionAllow, ActionAsk, ActionDeny} {
		for _, eventType := range []adapters.EventType{adapters.EventConfirmation, adapters.EventCredential} {
			for _, risk := range []adapters.RiskLevel{adapters.RiskLow, adapters.RiskUnknown, adapters.RiskHigh} {
				name := fmt.Sprintf("%s/%s/%s", action, eventType, risk)
				t.Run(name, func(t *testing.T) {
					_, err := New(Config{DefaultAction: action, Rules: []Rule{{
						Name: name,
						Match: Match{
							EventTypes: []adapters.EventType{eventType},
							RiskLevels: []adapters.RiskLevel{risk},
						},
						Action: action,
					}}})
					if err != nil {
						t.Fatal(err)
					}
				})
			}
		}
	}
}

func TestConfigAndInputsAreDefensivelyCopied(t *testing.T) {
	falseValue := false
	config := Config{DefaultAction: ActionAsk, Rules: []Rule{{
		Name: "original",
		Match: Match{
			EventTypes: []adapters.EventType{adapters.EventConfirmation},
			TextRegex:  "Overwrite",
			AgentIDs:   []string{"agent-a"},
			RiskLevels: []adapters.RiskLevel{adapters.RiskLow},
			Sensitive:  &falseValue,
		},
		Action: ActionAllow,
	}}}
	engine, err := New(config)
	if err != nil {
		t.Fatal(err)
	}

	config.DefaultAction = ActionDeny
	config.Rules[0].Name = "mutated"
	config.Rules[0].Action = ActionDeny
	config.Rules[0].Match.EventTypes[0] = adapters.EventCredential
	config.Rules[0].Match.AgentIDs[0] = "other"
	config.Rules[0].Match.RiskLevels[0] = adapters.RiskHigh
	*config.Rules[0].Match.Sensitive = true

	if got := engine.Evaluate(actionableEvent()); got.Action != ActionAllow || got.RuleName != "original" {
		t.Fatalf("input mutation changed engine: %#v", got)
	}

	first := engine.Config()
	first.DefaultAction = ActionDeny
	first.Rules[0].Name = "returned mutation"
	first.Rules[0].Match.AgentIDs[0] = "returned mutation"
	*first.Rules[0].Match.Sensitive = true
	second := engine.Config()
	if second.DefaultAction != ActionAsk || second.Rules[0].Name != "original" ||
		second.Rules[0].Match.AgentIDs[0] != "agent-a" || *second.Rules[0].Match.Sensitive {
		t.Fatalf("Config() exposed engine storage: %#v", second)
	}
}

func TestEvaluateIsConcurrentAndDoesNotRetainSecretText(t *testing.T) {
	const secret = "secret-marker-never-returned"
	engine, err := New(Config{DefaultAction: ActionAsk, Rules: []Rule{{
		Name: "safe-rule", Match: Match{TextRegex: "secret-marker"}, Action: ActionAllow,
	}}})
	if err != nil {
		t.Fatal(err)
	}
	event := actionableEvent()
	event.Summary = secret
	event.Match = "second-" + secret
	want := evaluation(ActionAllow, ActionAllow, "safe-rule", ReasonRule, true, false)
	want.EventID = event.ID

	const workers = 64
	const iterations = 100
	errors := make(chan string, workers)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				got := engine.Evaluate(event)
				if !reflect.DeepEqual(got, want) {
					errors <- fmt.Sprintf("Evaluate() = %#v, want %#v", got, want)
					return
				}
				if strings.Contains(fmt.Sprintf("%#v", got), secret) {
					errors <- "evaluation exposed secret event text"
					return
				}
			}
		}()
	}
	group.Wait()
	close(errors)
	for message := range errors {
		t.Error(message)
	}
}

func TestEquivalentOccurrencesRemainIndependentAndInputIsUnchanged(t *testing.T) {
	engine, err := New(Config{DefaultAction: ActionDeny})
	if err != nil {
		t.Fatal(err)
	}
	first := actionableEvent()
	first.ID = "occurrence-1"
	second := first.Clone()
	second.ID = "occurrence-2"
	firstBefore := first.Clone()
	secondBefore := second.Clone()

	firstEvaluation := engine.Evaluate(first)
	secondEvaluation := engine.Evaluate(second)
	if firstEvaluation.EventID != first.ID || secondEvaluation.EventID != second.ID ||
		firstEvaluation.EventID == secondEvaluation.EventID ||
		firstEvaluation.Action != ActionDeny || secondEvaluation.Action != ActionDeny {
		t.Fatalf("occurrence evaluations = first %#v, second %#v", firstEvaluation, secondEvaluation)
	}
	if !reflect.DeepEqual(first, firstBefore) || !reflect.DeepEqual(second, secondBefore) {
		t.Fatalf("Evaluate mutated input events: first %#v, second %#v", first, second)
	}
}

func TestNilEngineAndDefaultConfigAreSafe(t *testing.T) {
	if got := DefaultConfig(); got.DefaultAction != ActionAsk || got.DryRun || len(got.Rules) != 0 {
		t.Fatalf("DefaultConfig() = %#v", got)
	}
	var engine *Engine
	got := engine.Evaluate(actionableEvent())
	if got.Action != ActionAsk || got.ProposedAction != ActionAsk || got.Automatic || got.Reason != ReasonNoEngine {
		t.Fatalf("nil Engine Evaluate() = %#v", got)
	}
	if got := engine.Config(); !reflect.DeepEqual(got, DefaultConfig()) {
		t.Fatalf("nil Engine Config() = %#v", got)
	}
}

func actionableEvent() adapters.Event {
	return adapters.Event{
		ID:        "evt-test-1",
		Signature: "signature-not-used-for-policy",
		Sequence:  1,
		SessionID: "session-a",
		AgentID:   "agent-a",
		Adapter:   adapters.GenericID,
		Type:      adapters.EventConfirmation,
		Summary:   "Overwrite target?",
		Match:     "[Y/n]",
		Risk:      adapters.RiskLow,
		Metadata:  map[string]string{"ignored": "metadata-secret"},
	}
}

func evaluation(action, proposed Action, ruleName, reason string, automatic, dryRun bool) Evaluation {
	return Evaluation{
		Action:         action,
		ProposedAction: proposed,
		RuleName:       ruleName,
		Reason:         reason,
		Automatic:      automatic,
		DryRun:         dryRun,
	}
}
