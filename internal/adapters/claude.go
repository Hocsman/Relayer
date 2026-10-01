package adapters

import (
	"fmt"
	"regexp"
	"strings"
)

const (
	// ClaudeID identifies the experimental Claude Code adapter. Its vendor
	// rules are intentionally limited to anonymized prompts observed with the
	// version documented alongside the fixtures.
	ClaudeID = "claude"

	claudeWorkspaceTrustPattern = "relayer.vendor.claude.2_1_59.workspace_trust.4bf978bb"
	claudeEnvironmentKeyPattern = "relayer.vendor.claude.2_1_59.environment_key.55c4a992"
	claudeBashCommandPattern    = "relayer.vendor.claude.2_1_285.bash_command.7a3c1f90"
	claudeWriteFilePattern      = "relayer.vendor.claude.2_1_285.write_file.8b4e2a11"
	claudeEditFilePattern       = "relayer.vendor.claude.2_1_285.edit_file.9c5f3b22"
)

type claudeObservedRule struct {
	pattern   Pattern
	eventType EventType
	risk      RiskLevel
	fixture   string
	version   string
}

var (
	claudeWriteTargetRegex = regexp.MustCompile(`(?is)Do\s*you\s*want\s*to\s*create\s*([^\s?][^\r\n?]*?)\?`)
	claudeEditTargetRegex  = regexp.MustCompile(`(?is)Do\s*you\s*want\s*to\s*make\s*this\s*edit\s*to\s*([^\s?][^\r\n?]*?)\?`)
)

func extractClaudeBashCommand(match string) string {
	lower := strings.ToLower(match)
	runIdx := strings.Index(lower, "run shell command")
	if runIdx < 0 {
		runIdx = strings.Index(lower, "run\x1b[1cshell\x1b[1ccommand")
	}

	// In real Claude Code (2.1.286+), the command is displayed AFTER "Run shell command",
	// bounded by separator lines or the prompt question.
	if runIdx >= 0 {
		after := match[runIdx+len("run shell command"):]
		if proceedIdx := strings.Index(strings.ToLower(after), "do you want to proceed?"); proceedIdx >= 0 {
			after = after[:proceedIdx]
		}
		var candidateLines []string
		for _, rawLine := range strings.Split(after, "\n") {
			trimmed := strings.TrimSpace(rawLine)
			if trimmed == "" || strings.HasPrefix(trimmed, "╌") || strings.HasPrefix(trimmed, "-") ||
				strings.HasPrefix(trimmed, "═") || strings.HasPrefix(trimmed, "│") {
				continue
			}
			candidateLines = append(candidateLines, trimmed)
		}
		if len(candidateLines) > 0 {
			return strings.Join(candidateLines, "\n")
		}
	}

	// Fallback for earlier synthetic fixtures where the command was printed before "Run shell command":
	section := match
	if runIdx >= 0 {
		section = match[:runIdx]
	}
	var cmdLines []string
	lines := strings.Split(section, "\n")
	for _, rawLine := range lines {
		trimmed := strings.TrimSpace(rawLine)
		if trimmed == "" {
			continue
		}
		low := strings.ToLower(trimmed)
		if strings.Contains(low, "bash command") || strings.HasPrefix(low, "tip:") {
			continue
		}
		cmdLines = append(cmdLines, trimmed)
	}
	return strings.Join(cmdLines, "\n")
}

func extractClaudeTargetFile(match string) string {
	if m := claudeWriteTargetRegex.FindStringSubmatch(match); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	if m := claudeEditTargetRegex.FindStringSubmatch(match); len(m) > 1 {
		return strings.TrimSpace(m[1])
	}
	return ""
}

var claudeObservedRules = []claudeObservedRule{
	{
		pattern: Pattern{
			Name:        claudeWorkspaceTrustPattern,
			Description: "Claude Code asks for permission to access the folder",
			// Claude Code renders spaces partly with cursor-forward ANSI
			// sequences. Processor removes those sequences, so every boundary
			// deliberately accepts either whitespace or no byte at all.
			// Matches both numbered ("1. Yes... 2. No...") and unnumbered Ink menu ("❯ No, exit   Yes...").
			Expression: `(?is)Quick\s*safety\s*check:\s*Is\s*this\s*a\s*project\s*you\s*created\s*or\s*one\s*you\s*trust\?.*?(?:Yes,\s*I\s*trust\s*this\s*folder.*?No,\s*exit|No,\s*exit.*?Yes,\s*I\s*trust\s*this\s*folder).*?Enter\s*to\s*confirm.*?Esc\s*to\s*cancel`,
		},
		eventType: EventPermission,
		risk:      RiskHigh,
		fixture:   "claude-2.1.59-workspace-trust",
		version:   "2.1.59",
	},
	{
		pattern: Pattern{
			Name:        claudeEnvironmentKeyPattern,
			Description: "Claude Code asks whether to use an environment key",
			// The expression begins after the displayed environment value. As a
			// result Event.Match cannot contain the key, including in memory.
			// Matches both numbered ("1. Yes 2. No") and unnumbered Ink selection ("Yes ❯ No").
			Expression: `(?is)Do\s*you\s*want\s*to\s*use\s*this\s*API\s*key\?.*?Yes.*?No\s*\(\s*recommended\s*\).*?Enter\s*to\s*confirm.*?Esc\s*to\s*cancel`,
			Sensitive:  true,
		},
		eventType: EventCredential,
		risk:      RiskHigh,
		fixture:   "claude-2.1.59-environment-api-key",
		version:   "2.1.59",
	},
	{
		pattern: Pattern{
			Name:        claudeBashCommandPattern,
			Description: "Claude Code asks to run a shell command",
			Expression:  `(?is)Bash\s*command.*?Run\s*shell\s*command.*?Do\s*you\s*want\s*to\s*proceed\?.*?1\.\s*Yes.*?[0-9]\.\s*No.*?Esc\s*to\s*cancel.*?Tab\s*to\s*amend`,
		},
		eventType: EventPermission,
		risk:      RiskHigh,
		fixture:   "claude-2.1.285-bash-command",
		version:   "2.1.285",
	},
	{
		pattern: Pattern{
			Name:        claudeWriteFilePattern,
			Description: "Claude Code asks to create a file",
			Expression:  `(?is)Create\s*file.*?Do\s*you\s*want\s*to\s*create\s*([^\s?][^\r\n?]*?)\?.*?1\.\s*Yes.*?[0-9]\.\s*No.*?Esc\s*to\s*cancel.*?Tab\s*to\s*amend`,
		},
		eventType: EventConfirmation,
		risk:      RiskLow,
		fixture:   "claude-2.1.285-write-file",
		version:   "2.1.285",
	},
	{
		pattern: Pattern{
			Name:        claudeEditFilePattern,
			Description: "Claude Code asks to edit a file",
			Expression:  `(?is)Edit\s*file.*?Do\s*you\s*want\s*to\s*make\s*this\s*edit\s*to\s*([^\s?][^\r\n?]*?)\?.*?1\.\s*Yes.*?[0-9]\.\s*No.*?Esc\s*to\s*cancel.*?Tab\s*to\s*amend`,
		},
		eventType: EventConfirmation,
		risk:      RiskLow,
		fixture:   "claude-2.1.285-edit-file",
		version:   "2.1.285",
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
			event.Metadata["observed_cli_version"] = rule.version
			if patternName == claudeBashCommandPattern {
				event.Command = extractClaudeBashCommand(event.Match)
			} else if patternName == claudeWriteFilePattern || patternName == claudeEditFilePattern {
				if targetFile := extractClaudeTargetFile(event.Match); targetFile != "" {
					event.Metadata["target_file"] = targetFile
				}
			}
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
		event.ID = state.occurrenceIDFor(event.Signature, event.Sequence)
		events[index] = event.Clone()
		if state != nil && state.pending != nil && state.pending.Sequence == event.Sequence {
			pending := event.Clone()
			state.pending = &pending
		}
	}
	return events, nil
}

// EncodeDecision encodes decisions for verified Claude Code prompts.
// Prompts backed by empirical 2.1.285 observations support verified automatic
// allow (Enter), deny (Esc), and menu-aware manual inputs. Other prompts remain
// conservative: automatic decisions are unsupported to prevent wrong menu selection.
func (a *ClaudeAdapter) EncodeDecision(event Event, decision Decision, manualInput string) ([]byte, error) {
	if a == nil || a.detector == nil {
		return nil, fmt.Errorf("adapter for Claude Code is not initialized")
	}
	patternName := event.Metadata["pattern"]
	switch patternName {
	case claudeBashCommandPattern, claudeWriteFilePattern, claudeEditFilePattern:
		if !event.Actionable() {
			return nil, fmt.Errorf("%w for type %q", ErrDecisionUnsupported, event.Type)
		}
		switch decision {
		case DecisionAllow:
			return []byte("\r"), nil
		case DecisionDeny:
			return []byte{0x1b}, nil
		case DecisionManual:
			if strings.IndexByte(manualInput, 0) >= 0 {
				return nil, fmt.Errorf("invalid manual input: NUL byte")
			}
			lower := strings.ToLower(strings.TrimSpace(manualInput))
			switch lower {
			case "y", "yes", "1":
				return []byte("1\r"), nil
			case "esc", "cancel":
				return []byte{0x1b}, nil
			case "n", "no":
				if patternName == claudeBashCommandPattern {
					return []byte("4\r"), nil
				}
				return []byte("3\r"), nil
			default:
				return []byte(manualInput + "\r"), nil
			}
		default:
			return nil, fmt.Errorf("%w: %q", ErrDecisionUnsupported, decision)
		}
	default:
		return a.detector.EncodeDecision(event, decision, manualInput)
	}
}
