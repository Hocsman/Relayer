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
	ReasonDefault          = "default_action"
	ReasonRule             = "rule_match"
	ReasonInvalidEvent     = "invalid_event"
	ReasonNonActionable    = "non_actionable"
	ReasonSensitive        = "sensitive_event"
	ReasonRisk             = "risk_not_low"
	ReasonDryRun           = "dry_run"
	ReasonNoEngine         = "engine_unavailable"
	ReasonConsecutiveLimit = "consecutive_auto_limit"
	ReasonRateLimit        = "rate_limit_exceeded"
	ReasonDestructive      = "destructive_command_blocked"
	ReasonExfiltration     = "exfiltration_attempt_blocked"
	ReasonGuardrailBlocked = "guardrail_pattern_blocked"
)

// GuardrailsConfig defines safety boundaries that prevent autonomous execution
// of high-risk actions even when a policy rule would otherwise allow them.
type GuardrailsConfig struct {
	BlockDestructive      bool
	BlockExfiltration     bool
	BlockSensitivePaths   bool
	BlockOutsideWorkspace bool
	WorkspaceRoot         string
	BlockedPatterns       []string
}

// Config describes the ordered rules evaluated by an Engine.
type Config struct {
	DefaultAction               Action
	DryRun                      bool
	MaxConsecutiveAutoDecisions int
	RateLimitPerMinute          int
	Guardrails                  GuardrailsConfig
	Rules                       []Rule
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
	EventTypes   []adapters.EventType
	TextRegex    string
	AgentIDs     []string
	RiskLevels   []adapters.RiskLevel
	Sensitive    *bool
	CommandRegex string
	PathRegex    string
	ReadOnly     *bool
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

var (
	builtInDestructivePatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\brm\s+(-[a-z]*r[a-z]*f|-[a-z]*f[a-z]*r|--recursive\s+--force|--force\s+--recursive)\b`),
		regexp.MustCompile(`(?i)\brm\s+(-[a-z]*[rf][a-z]*\s+)+[/~*]`),
		regexp.MustCompile(`(?i)\b(del|erase)\s+/[sS]\s+[/\\*]`),
		regexp.MustCompile(`(?i)\brmdir\s+/[sS]\b`),
		regexp.MustCompile(`(?i)\bmkfs(\.[a-z0-9]+)?\b`),
		regexp.MustCompile(`(?i)\bdd\s+.*(\bof=/dev/|\bof=\\\\.\\)`),
		regexp.MustCompile(`(?i)\bformat\s+[a-z]:`),
		regexp.MustCompile(`(?i)\b(fdisk|parted|gdisk|diskpart)\b`),
		regexp.MustCompile(`(?i)\bchmod\s+(-[a-z]*R[a-z]*\s+)?(777|000)\s+[/~*]`),
	}

	builtInExfiltrationPatterns = []*regexp.Regexp{
		regexp.MustCompile(`(?i)\b(curl|wget|fetch)\b.*\|\s*(ba|z|k|c)?sh\b`),
		regexp.MustCompile(`(?i)\b(curl|wget)\b.*\|\s*(python[0-9]*|perl|ruby|powershell|pwsh)\b`),
		regexp.MustCompile(`(?i)\bcat\s+.*(\.ssh/(id_|authorized_keys)|credentials|\.aws/credentials|\.env\b)`),
		regexp.MustCompile(`(?i)\b(curl|wget)\s+.*(--data|-d|--upload-file|-T)\s+.*(@?.*(\.ssh|\.aws|\.env|id_rsa))`),
		regexp.MustCompile(`(?i)\bnc\s+.*<.*(\.ssh|\.aws|\.env|id_rsa)`),
	}
)

type compiledRule struct {
	rule      Rule
	text      *regexp.Regexp
	command   *regexp.Regexp
	path      *regexp.Regexp
	readOnly  *bool
	agentIDs  []string
	sensitive *bool
}

// Engine is immutable after construction and safe for concurrent evaluation.
type Engine struct {
	config                Config
	rules                 []compiledRule
	destructivePatterns   []*regexp.Regexp
	exfiltrationPatterns  []*regexp.Regexp
	customBlockedPatterns []*regexp.Regexp
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
	if config.MaxConsecutiveAutoDecisions < 0 {
		return nil, fmt.Errorf("max_consecutive_auto_decisions cannot be negative: %d", config.MaxConsecutiveAutoDecisions)
	}
	if config.RateLimitPerMinute < 0 {
		return nil, fmt.Errorf("rate_limit_per_minute cannot be negative: %d", config.RateLimitPerMinute)
	}

	var customBlocked []*regexp.Regexp
	for index, pattern := range config.Guardrails.BlockedPatterns {
		pattern = strings.TrimSpace(pattern)
		if pattern == "" {
			return nil, fmt.Errorf("guardrail blocked pattern %d is blank", index+1)
		}
		if containsNUL(pattern) {
			return nil, fmt.Errorf("guardrail blocked pattern %d contains a NUL byte", index+1)
		}
		re, err := regexp.Compile(pattern)
		if err != nil {
			return nil, fmt.Errorf("invalid guardrail blocked pattern %q: %w", pattern, err)
		}
		customBlocked = append(customBlocked, re)
	}

	var destructive []*regexp.Regexp
	if config.Guardrails.BlockDestructive {
		destructive = builtInDestructivePatterns
	}
	var exfiltration []*regexp.Regexp
	if config.Guardrails.BlockExfiltration {
		exfiltration = builtInExfiltrationPatterns
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

		var commandExpr *regexp.Regexp
		if rule.Match.CommandRegex != "" {
			if containsNUL(rule.Match.CommandRegex) {
				return nil, fmt.Errorf("policy rule %q command regex contains a NUL byte", name)
			}
			var err error
			commandExpr, err = regexp.Compile(rule.Match.CommandRegex)
			if err != nil {
				return nil, fmt.Errorf("policy rule %q has invalid command regex: %w", name, err)
			}
		}

		var pathExpr *regexp.Regexp
		if rule.Match.PathRegex != "" {
			if containsNUL(rule.Match.PathRegex) {
				return nil, fmt.Errorf("policy rule %q path regex contains a NUL byte", name)
			}
			var err error
			pathExpr, err = regexp.Compile(rule.Match.PathRegex)
			if err != nil {
				return nil, fmt.Errorf("policy rule %q has invalid path regex: %w", name, err)
			}
		}

		compiled = append(compiled, compiledRule{
			rule:      cloneRule(*rule),
			text:      expression,
			command:   commandExpr,
			path:      pathExpr,
			readOnly:  cloneBool(rule.Match.ReadOnly),
			agentIDs:  agentIDs,
			sensitive: cloneBool(rule.Match.Sensitive),
		})
	}

	return &Engine{
		config:                cloned,
		rules:                 compiled,
		destructivePatterns:   destructive,
		exfiltrationPatterns:  exfiltration,
		customBlockedPatterns: customBlocked,
	}, nil
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
	if proposed == ActionAllow {
		if guardReason, blocked := e.checkGuardrails(event); blocked {
			result.Reason = guardReason
			result.Action = ActionAsk
			result.Automatic = false
			return result
		}
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

func (e *Engine) checkGuardrails(event adapters.Event) (string, bool) {
	if e == nil {
		return "", false
	}
	target := strings.Join([]string{event.Summary, event.Match, event.Command}, "\n")
	for _, p := range e.destructivePatterns {
		if p.MatchString(target) {
			return ReasonDestructive, true
		}
	}
	for _, p := range e.exfiltrationPatterns {
		if p.MatchString(target) {
			return ReasonExfiltration, true
		}
	}
	if e.config.Guardrails.BlockSensitivePaths {
		if ContainsSensitivePathReference(target) {
			return ReasonSensitivePath, true
		}
		paths := ExtractPaths(target)
		for _, p := range paths {
			if IsSensitivePath(p) {
				return ReasonSensitivePath, true
			}
		}
	}
	if e.config.Guardrails.BlockOutsideWorkspace && e.config.Guardrails.WorkspaceRoot != "" {
		paths := ExtractPaths(target)
		for _, p := range paths {
			if !IsPathInsideWorkspace(p, e.config.Guardrails.WorkspaceRoot) {
				return ReasonOutsideWorkspace, true
			}
		}
	}
	for _, p := range e.customBlockedPatterns {
		if p.MatchString(target) {
			return ReasonGuardrailBlocked, true
		}
	}
	return "", false
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
	if r.command != nil && !r.command.MatchString(event.Command) {
		return false
	}
	if r.readOnly != nil {
		isRO := IsReadOnlyCommand(event.Command)
		if *r.readOnly != isRO {
			return false
		}
	}
	if r.path != nil {
		targetText := strings.Join([]string{event.Command, event.Summary, event.Match}, "\n")
		paths := ExtractPaths(targetText)
		matched := false
		for _, p := range paths {
			if r.path.MatchString(p) {
				matched = true
				break
			}
		}
		if !matched {
			return false
		}
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
		len(match.RiskLevels) > 0 || match.Sensitive != nil || match.CommandRegex != "" ||
		match.PathRegex != "" || match.ReadOnly != nil
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
	if config.Guardrails.BlockedPatterns != nil {
		cloned.Guardrails.BlockedPatterns = cloneSlice(config.Guardrails.BlockedPatterns)
	}
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
	cloned.Match.ReadOnly = cloneBool(rule.Match.ReadOnly)
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
