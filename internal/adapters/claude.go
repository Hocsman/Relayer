package adapters

import (
	"fmt"
	"strings"
)

const (
	// ClaudeID identifies the experimental Claude Code adapter. Its vendor
	// rules are intentionally limited to anonymized prompts observed with the
	// version documented alongside the fixtures.
	ClaudeID = "claude"

	claudeWorkspaceTrustPattern = "relayer.vendor.claude.2_1_59.workspace_trust.4bf978bb"
	claudeEnvironmentKeyPattern = "relayer.vendor.claude.2_1_59.environment_key.55c4a992"
)

type claudeObservedRule struct {
	pattern   Pattern
	eventType EventType
	risk      RiskLevel
	fixture   string
}

var claudeObservedRules = []claudeObservedRule{
	{
		pattern: Pattern{
			Name:        claudeWorkspaceTrustPattern,
			Description: "Claude Code asks for permission to access the folder",
			// Claude Code renders spaces partly with cursor-forward ANSI
			// sequences. Processor removes those sequences, so every boundary
			// deliberately accepts either whitespace or no byte at all.
			Expression: `(?is)Quick\s*safety\s*check:\s*Is\s*this\s*a\s*project\s*you\s*created\s*or\s*one\s*you\s*trust\?.*?1\.\s*Yes,\s*I\s*trust\s*this\s*folder\s*2\.\s*No,\s*exit\s*Enter\s*to\s*confirm\s*Esc\s*to\s*cancel`,
		},
		eventType: EventPermission,
		risk:      RiskHigh,
		fixture:   "claude-2.1.59-workspace-trust",
	},
	{
		pattern: Pattern{
			Name:        claudeEnvironmentKeyPattern,
			Description: "Claude Code asks whether to use an environment key",
			// The expression begins after the displayed environment value. As a
			// result Event.Match cannot contain the key, including in memory.
			Expression: `(?is)Do\s*you\s*want\s*to\s*use\s*this\s*API\s*key\?\s*1\.\s*Yes\s*2\.\s*No\s*\(\s*recommended\s*\)\s*Enter\s*to\s*confirm\s*Esc\s*to\s*cancel`,
			Sensitive:  true,
		},
		eventType: EventCredential,
		risk:      RiskHigh,
		fixture:   "claude-2.1.59-environment-api-key",
	},
}

// ClaudeAdapter recognizes only prompts backed by the anonymized Claude Code
// 2.1.59 fixtures. Configured intercept_patterns retain their configured order
// and take priority, preserving the semantics of existing configurations.
//
// The adapter remains experimental: no automatic allow or deny byte sequence
// is claimed because the highlighted TUI selection can change independently
// of the prompt text visible to Relayer.
type ClaudeAdapter struct {
	detector        *GenericRegexAdapter
	rules           map[string]claudeObservedRule
	configuredNames map[string]struct{}
}

// NewClaudeAdapter validates both the observed rules and every configured
// intercept_pattern before a session starts.
func NewClaudeAdapter(patterns []Pattern) (*ClaudeAdapter, error) {
	combined := make([]Pattern, 0, len(claudeObservedRules)+len(patterns))
	rules := make(map[string]claudeObservedRule, len(claudeObservedRules))
	configuredNames := make(map[string]struct{}, len(patterns))
	combined = append(combined, patterns...)
	for _, pattern := range patterns {
		configuredNames[pattern.Name] = struct{}{}
	}
	for _, rule := range claudeObservedRules {
		combined = append(combined, rule.pattern)
		rules[rule.pattern.Name] = rule
	}
	detector, err := NewGenericRegexAdapter(combined)
	if err != nil {
		return nil, fmt.Errorf("initialize the Claude Code adapter: %w", err)
	}
	return &ClaudeAdapter{detector: detector, rules: rules, configuredNames: configuredNames}, nil
}

func (*ClaudeAdapter) ID() string { return ClaudeID }

func (a *ClaudeAdapter) snapshotFingerprintSource(normalized, active string, inCodeFence bool) string {
	if ignoredContext(active, inCodeFence) {
		return active
	}
	latestEnd := -1
	source := active
	for _, pattern := range a.detector.patterns {
		if _, configured := a.configuredNames[pattern.Name]; configured {
			continue
		}
		if _, observed := a.rules[pattern.Name]; !observed {
			continue
		}
		matches := pattern.regex.FindAllStringIndex(normalized, -1)
		if len(matches) == 0 {
			continue
		}
		match := matches[len(matches)-1]
		// A completed prompt may remain in tmux scrollback after the user has
		// answered it directly. Only the rule whose verified footer still
		// reaches the active end of the pane can describe the current prompt.
		// Otherwise returning the historical block would keep a stale pending
		// occurrence alive without giving Detect a chance to clear it.
		if strings.TrimSpace(normalized[match[1]:]) != "" {
			continue
		}
		if match[1] > latestEnd {
			latestEnd = match[1]
			matchSource := normalized[match[0]:match[1]]
			if pattern.Name == claudeWorkspaceTrustPattern {
				// The workspace line contains no credential and distinguishes two
				// successive trust prompts after a direct tmux response. It remains
				// only in this private in-memory fingerprint, never Event or audit
				// metadata. The credential rule deliberately starts after its value.
				if start := strings.LastIndex(normalized[:match[0]], "Accessing"); start >= 0 {
					matchSource = normalized[start:match[1]]
				}
			}
			source = pattern.Name + "\x00" + compactFingerprintSource(matchSource)
		}
	}
	return source
}

func (*ClaudeAdapter) snapshotOccurrenceAware(event Event) bool {
	return event.Metadata["fixture"] != ""
}

// Detect delegates bounded stream handling and legacy regex behavior to the
// generic adapter, then enriches only rules tied to real Claude Code fixtures.
func (a *ClaudeAdapter) Detect(state *DetectionState, chunk []byte) ([]Event, error) {
	if a == nil || a.detector == nil {
		return nil, fmt.Errorf("adapter for Claude Code is not initialized")
	}
	events, err := a.detector.Detect(state, chunk)
	if err != nil || len(events) == 0 {
		return events, err
	}
	for index := range events {
		event := events[index].Clone()
		patternName := event.Metadata["pattern"]
		signatureMatch := event.Match
		_, configured := a.configuredNames[patternName]
		if rule, observed := a.rules[patternName]; !configured && observed && event.Summary == rule.pattern.Description {
			event.Type = rule.eventType
			event.Risk = rule.risk
			event.Metadata["fixture"] = rule.fixture
			event.Metadata["observed_cli_version"] = "2.1.59"
			// ANSI cursor movement and tmux capture render equivalent spacing
			// differently. The fixture rule is the stable semantic signature;
			// Processor's private prompt fingerprint distinguishes occurrences.
			signatureMatch = patternName
		}
		event.Adapter = ClaudeID
		event.Signature = stableSignature(
			event.SessionID,
			ClaudeID,
			event.Type,
			patternName,
			signatureMatch,
		)
		event.ID = occurrenceID(event.Signature, event.Sequence)
		events[index] = event.Clone()
		if state != nil && state.pending != nil && state.pending.Sequence == event.Sequence {
			pending := event.Clone()
			state.pending = &pending
		}
	}
	return events, nil
}

// EncodeDecision preserves exact manual input compatibility. Automatic allow
// and deny remain unsupported until selection-independent bytes are verified.
func (a *ClaudeAdapter) EncodeDecision(event Event, decision Decision, manualInput string) ([]byte, error) {
	if a == nil || a.detector == nil {
		return nil, fmt.Errorf("adapter for Claude Code is not initialized")
	}
	return a.detector.EncodeDecision(event, decision, manualInput)
}
