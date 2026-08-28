// Package policy evaluates immutable, side-effect-free automation rules for
// semantic agent events. It deliberately does not encode or deliver a
// decision: callers must fall back to a human whenever an adapter cannot
// represent the proposed action or a delivery fails.
package policy

import (
	"fmt"
	"regexp"
	"strings"

	"github.com/Hocsman/Relayer/internal/adapters"
)

// Action is the outcome selected by the policy engine.
type Action string

const (
	ActionAllow Action = "allow"
	ActionAsk   Action = "ask"
	ActionDeny  Action = "deny"
)

// Static evaluation reasons are safe to expose in logs. They never contain
// event text, regex matches, metadata, or other terminal output.
const (
	ReasonDefault       = "default_action"
	ReasonRule          = "rule_match"
	ReasonInvalidEvent  = "invalid_event"
	ReasonNonActionable = "non_actionable"
	ReasonSensitive     = "sensitive_event"
	ReasonRisk          = "risk_not_low"
	ReasonDryRun        = "dry_run"
	ReasonNoEngine      = "engine_unavailable"
)

// Config describes the ordered rules evaluated by an Engine.
type Config struct {
	DefaultAction Action
	DryRun        bool
	Rules         []Rule
}

// Rule applies Action when every populated Match field accepts an event.
// Rules are evaluated in order and the first match wins.
type Rule struct {
	Name   string
	Match  Match
	Action Action
}

// Match combines fields with AND semantics. Values inside each list use OR
// semantics. TextRegex is evaluated against Summary + "\n" + Match but that
// text is never retained by Engine or copied into Evaluation.
type Match struct {
	EventTypes []adapters.EventType
	TextRegex  string
	AgentIDs   []string
	RiskLevels []adapters.RiskLevel
	Sensitive  *bool
}

// Evaluation separates the configured proposal from the effective action.
// Sensitive events and dry-run configurations retain ProposedAction for a
// safe audit record while forcing ActionAsk and disabling automation.
type Evaluation struct {
	Action         Action
	ProposedAction Action
	RuleName       string
	Reason         string
	EventID        string
	Automatic      bool
	DryRun         bool
}

type compiledRule struct {
	rule      Rule
	text      *regexp.Regexp
	agentIDs  []string
	sensitive *bool
}

// Engine is immutable after construction and safe for concurrent evaluation.
type Engine struct {
	config Config
	rules  []compiledRule
}

// DefaultConfig preserves Relayer's human-in-the-loop behavior.
func DefaultConfig() Config {
	return Config{DefaultAction: ActionAsk}
}

// New validates and defensively copies a policy configuration. Regexes are
// compiled once so Evaluate remains deterministic and allocation-light.
func New(config Config) (*Engine, error) {
	if !validAction(config.DefaultAction) {
		return nil, fmt.Errorf("invalid default policy action %q", config.DefaultAction)
	}

	cloned := cloneConfig(config)
	compiled := make([]compiledRule, 0, len(cloned.Rules))
	names := make([]string, 0, len(cloned.Rules))
	for index := range cloned.Rules {
		rule := &cloned.Rules[index]
		name := strings.TrimSpace(rule.Name)
		if name == "" {
			return nil, fmt.Errorf("policy rule %d has a blank name", index+1)
		}
		if containsNUL(name) {
			return nil, fmt.Errorf("policy rule %d name contains a NUL byte", index+1)
		}
		for _, existing := range names {
			if strings.EqualFold(existing, name) {
				return nil, fmt.Errorf("policy rule %d duplicates name %q", index+1, name)
			}
		}
		names = append(names, name)
		rule.Name = name

		if !validAction(rule.Action) {
			return nil, fmt.Errorf("policy rule %q has invalid action %q", name, rule.Action)
		}
		if !hasMatcher(rule.Match) {
			return nil, fmt.Errorf("policy rule %q has no matcher", name)
		}

		for _, eventType := range rule.Match.EventTypes {
			if eventType != adapters.EventConfirmation && eventType != adapters.EventPermission &&
				eventType != adapters.EventCredential {
				return nil, fmt.Errorf("policy rule %q has invalid event type %q", name, eventType)
			}
		}
		for _, risk := range rule.Match.RiskLevels {
			if !validRisk(risk) {
				return nil, fmt.Errorf("policy rule %q has invalid risk level %q", name, risk)
			}
		}

		agentIDs := make([]string, len(rule.Match.AgentIDs))
		for agentIndex, agentID := range rule.Match.AgentIDs {
			agentID = strings.TrimSpace(agentID)
			if agentID == "" {
				return nil, fmt.Errorf("policy rule %q has a blank agent id", name)
			}
			if containsNUL(agentID) {
				return nil, fmt.Errorf("policy rule %q agent id contains a NUL byte", name)
			}
			agentIDs[agentIndex] = agentID
		}

		var expression *regexp.Regexp
		if rule.Match.TextRegex != "" {
			if containsNUL(rule.Match.TextRegex) {
				return nil, fmt.Errorf("policy rule %q text regex contains a NUL byte", name)
			}
			var err error
			expression, err = regexp.Compile(rule.Match.TextRegex)
			if err != nil {
				return nil, fmt.Errorf("policy rule %q has invalid text regex: %w", name, err)
			}
		}

		compiled = append(compiled, compiledRule{
			rule:      cloneRule(*rule),
			text:      expression,
			agentIDs:  agentIDs,
			sensitive: cloneBool(rule.Match.Sensitive),
		})
	}

	return &Engine{config: cloned, rules: compiled}, nil
}

// Evaluate applies the first matching rule without mutating either the engine
// or event. Invalid and non-actionable events can never become automatic.
func (e *Engine) Evaluate(event adapters.Event) Evaluation {
	result := Evaluation{
		Action:         ActionAsk,
		ProposedAction: ActionAsk,
		EventID:        event.ID,
		Reason:         ReasonNoEngine,
	}
	if e == nil {
		return result
	}
	result.DryRun = e.config.DryRun
	if strings.TrimSpace(event.ID) == "" || strings.TrimSpace(event.SessionID) == "" ||
		strings.TrimSpace(event.AgentID) == "" || strings.TrimSpace(event.Adapter) == "" ||
		!validRisk(event.Risk) {
		result.Reason = ReasonInvalidEvent
		return result
	}
	if !event.Actionable() {
		result.Reason = ReasonNonActionable
		return result
	}

	proposed := e.config.DefaultAction
	reason := ReasonDefault
	for _, rule := range e.rules {
		if rule.matches(event) {
			proposed = rule.rule.Action
			result.RuleName = rule.rule.Name
			reason = ReasonRule
			break
		}
	}
	result.ProposedAction = proposed

	if event.Sensitive || event.Type == adapters.EventCredential {
		result.Reason = ReasonSensitive
		return result
	}
	if proposed == ActionAllow && event.Risk != adapters.RiskLow {
		result.Reason = ReasonRisk
		return result
	}
	if e.config.DryRun {
		result.Reason = ReasonDryRun
		return result
	}

	result.Action = proposed
	result.Automatic = proposed == ActionAllow || proposed == ActionDeny
	result.Reason = reason
	return result
}

// Config returns a deep copy that cannot mutate the engine.
func (e *Engine) Config() Config {
	if e == nil {
		return DefaultConfig()
	}
	return cloneConfig(e.config)
}

func (r compiledRule) matches(event adapters.Event) bool {
	if len(r.rule.Match.EventTypes) > 0 && !containsEventType(r.rule.Match.EventTypes, event.Type) {
		return false
	}
	if len(r.agentIDs) > 0 && !containsAgentID(r.agentIDs, event.AgentID) {
		return false
	}
	if len(r.rule.Match.RiskLevels) > 0 && !containsRisk(r.rule.Match.RiskLevels, event.Risk) {
		return false
	}
	eventSensitive := event.Sensitive || event.Type == adapters.EventCredential
	if r.sensitive != nil && *r.sensitive != eventSensitive {
		return false
	}
	return r.text == nil || r.text.MatchString(event.Summary+"\n"+event.Match)
}

func validAction(action Action) bool {
	return action == ActionAllow || action == ActionAsk || action == ActionDeny
}

func validRisk(risk adapters.RiskLevel) bool {
	switch risk {
	case adapters.RiskLow, adapters.RiskUnknown, adapters.RiskHigh:
		return true
	default:
		return false
	}
}

func hasMatcher(match Match) bool {
	return len(match.EventTypes) > 0 || match.TextRegex != "" || len(match.AgentIDs) > 0 ||
		len(match.RiskLevels) > 0 || match.Sensitive != nil
}

func containsEventType(values []adapters.EventType, target adapters.EventType) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsAgentID(values []string, target string) bool {
	target = strings.TrimSpace(target)
	for _, value := range values {
		if strings.EqualFold(value, target) {
			return true
		}
	}
	return false
}

func containsRisk(values []adapters.RiskLevel, target adapters.RiskLevel) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneConfig(config Config) Config {
	cloned := config
	if config.Rules != nil {
		cloned.Rules = make([]Rule, len(config.Rules))
		for index, rule := range config.Rules {
			cloned.Rules[index] = cloneRule(rule)
		}
	}
	return cloned
}

func cloneRule(rule Rule) Rule {
	cloned := rule
	cloned.Match.EventTypes = cloneSlice(rule.Match.EventTypes)
	cloned.Match.AgentIDs = cloneSlice(rule.Match.AgentIDs)
	cloned.Match.RiskLevels = cloneSlice(rule.Match.RiskLevels)
	cloned.Match.Sensitive = cloneBool(rule.Match.Sensitive)
	return cloned
}

func cloneSlice[T any](values []T) []T {
	if values == nil {
		return nil
	}
	return append(make([]T, 0, len(values)), values...)
}

func cloneBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	cloned := *value
	return &cloned
}

func containsNUL(value string) bool {
	return strings.IndexByte(value, 0) >= 0
}
