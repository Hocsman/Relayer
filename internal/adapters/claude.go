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

// indexFoldASCII returns the byte offset in s of the first occurrence of needle,
// ignoring ASCII case, or -1. needle must be lower-case ASCII.
//
// The offset is always an offset in s itself. That is why this is not
// strings.Index(strings.ToLower(s), needle): ToLower can change the byte length
// of a rune (U+023A is two bytes, its lower case three), so an index taken from
// the lowered copy points somewhere else in the original, past its end once
// enough such runes precede the needle. The match comes from the agent's own
// output, so that was a way for an agent to make slicing it panic.
func indexFoldASCII(s, needle string) int {
	if needle == "" {
		return 0
	}
	for start := 0; start+len(needle) <= len(s); start++ {
		matched := true
		for offset := 0; offset < len(needle); offset++ {
			c := s[start+offset]
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			if c != needle[offset] {
				matched = false
				break
			}
		}
		if matched {
			return start
		}
	}
	return -1
}

func extractClaudeBashCommand(match string) string {
	runIdx, runEnd := -1, -1
	for _, header := range []string{"run shell command", "run\x1b[1cshell\x1b[1ccommand"} {
		if idx := indexFoldASCII(match, header); idx >= 0 {
			runIdx, runEnd = idx, idx+len(header)
			break
		}
	}

	// In real Claude Code (2.1.286+), the command is displayed AFTER "Run shell command",
	// bounded by separator lines or the prompt question.
	if runIdx >= 0 {
		after := match[runEnd:]
		proceedIdx := indexFoldASCII(after, "do you want to pro")
		if proceedIdx >= 0 {
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

// claudeObservedRules lists the prompts Relayer recognizes. The create-file and
// edit-file rules are RiskUnknown, not RiskLow, on purpose: a policy allows only
// a RiskLow prompt on its own, and what Claude Code is asked to write
// decides what runs next (.claude/settings.json, .vscode/tasks.json,
// .github/workflows, .husky hooks, package.json scripts, shell start-up files),
// which the guardrails only partly cover and Relayer has no path allowlist for.
// Until one exists these prompts are for a person to answer.
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
			Expression:  `(?is)(?:Bash\s*command.*?)?Run\s*shell\s*command.*?Do\s*you\s*want\s*to\s*proc?eed\?.*?1\.?\s*Yes.*?[0-9]\.?\s*No.*?Esc\s*to\s*cancel.*?Tab\s*to\s*amend(?:\s*·?[^a-zA-Z0-9\r\n]*|.*?[0-9]\.?\s*Yes)*`,
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
			Expression:  `(?is)Create\s*file.*?Do\s*you\s*want\s*to\s*create\s*([^\s?][^\r\n?]*?)\?.*?1\.?\s*Yes.*?[0-9]\.?\s*No.*?Esc\s*to\s*cancel.*?Tab\s*to\s*amend(?:\s*·?[^a-zA-Z0-9\r\n]*|.*?[0-9]\.?\s*Yes)*`,
		},
		eventType: EventConfirmation,
		risk:      RiskUnknown,
		fixture:   "claude-2.1.285-write-file",
		version:   "2.1.285",
	},
	{
		pattern: Pattern{
			Name:        claudeEditFilePattern,
			Description: "Claude Code asks to edit a file",
			Expression:  `(?is)Edit\s*file.*?Do\s*you\s*want\s*to\s*make\s*this\s*edit\s*to\s*([^\s?][^\r\n?]*?)\?.*?1\.?\s*Yes.*?[0-9]\.?\s*No.*?Esc\s*to\s*cancel.*?Tab\s*to\s*amend(?:\s*·?[^a-zA-Z0-9\r\n]*|.*?[0-9]\.?\s*Yes)*`,
		},
		eventType: EventConfirmation,
		risk:      RiskUnknown,
		fixture:   "claude-2.1.285-edit-file",
		version:   "2.1.285",
	},
}

// ClaudeAdapter recognizes only prompts backed by anonymized Claude Code
// observations: the 2.1.59 workspace-trust and environment-key prompts, the
// 2.1.285 Bash, create-file and edit-file prompts, and the Bash and create-file
// layouts of 2.1.286, which rest on test strings. Configured
// intercept_patterns retain their configured order and take priority,
// preserving the semantics of existing configurations.
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
		tail := normalized[match[1]:]
		if strings.TrimSpace(tail) != "" && !tailIsFurniture(nil, tail) {
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

// EncodeDecision preserves exact manual input compatibility. Automatic allow
// and deny remain unsupported for every Claude Code prompt, the Bash, create
// and edit prompts included: no answer to them has been typed into a real
// Claude Code with its effect checked, and an Enter takes whichever option the
// menu has highlighted, which the prompt text Relayer reads does not show.
// The unsupported decisions make a policy decision on these prompts a question
// for a person instead of a byte written into the agent.
func (a *ClaudeAdapter) EncodeDecision(event Event, decision Decision, manualInput string) ([]byte, error) {
	if a == nil || a.detector == nil {
		return nil, fmt.Errorf("adapter for Claude Code is not initialized")
	}
	return a.detector.EncodeDecision(event, decision, manualInput)
}
