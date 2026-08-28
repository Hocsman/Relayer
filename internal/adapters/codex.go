package adapters

import (
	"fmt"
	"strings"
)

const (
	// CodexID is experimental and intentionally limited to interactions backed
	// by the anonymized captures in testdata/codex.
	CodexID = "codex"

	codexInteractionMetadata = "interaction"
	codexCommandApproval     = "command_approval"
	codexDirectoryTrust      = "directory_trust"
)

type codexPrompt struct {
	interaction string
	summary     string
	match       string
	risk        RiskLevel
	markers     []string
	allows      []string
	denies      []string
	footers     []string
}

var codexPrompts = []codexPrompt{
	{
		interaction: codexCommandApproval,
		summary:     "Codex asks to run a command (y=allow, esc=deny)",
		match:       "Would you like to run the following command?",
		risk:        RiskUnknown,
		markers:     []string{"Would you like to run the following command?"},
		allows:      []string{"Yes, proceed (y)"},
		denies:      []string{"No, and tell Codex what to do differently (esc)"},
		footers:     []string{"Press enter to confirm or esc to cancel"},
	},
	{
		interaction: codexDirectoryTrust,
		summary:     "Codex asks to trust the directory (1=continue, 2=quit)",
		match:       "Do you trust the contents of this directory?",
		risk:        RiskHigh,
		// The compact variants are the same captured screen after ANSI cursor
		// positioning is stripped from the raw PTY stream. tmux snapshots retain
		// the spaced variants.
		markers: []string{
			"Do you trust the contents of this directory?",
			"Doyoutrustthecontentsofthisdirectory?",
		},
		allows:  []string{"1. Yes, continue"},
		denies:  []string{"2. No, quit", "2.No,quit"},
		footers: []string{"Press enter to continue"},
	},
}

// CodexAdapter recognizes only prompts captured from the CLI version recorded
// in testdata/codex. Configured intercept_patterns remain a complete fallback;
// this wrapper does not silently replace or weaken them.
type CodexAdapter struct {
	generic *GenericRegexAdapter
}

// NewCodexAdapter validates and copies the legacy intercept_patterns used by
// the generic fallback.
func NewCodexAdapter(patterns []Pattern) (*CodexAdapter, error) {
	generic, err := NewGenericRegexAdapter(patterns)
	if err != nil {
		return nil, err
	}
	return &CodexAdapter{generic: generic}, nil
}

func (*CodexAdapter) ID() string { return CodexID }

func (*CodexAdapter) snapshotFingerprintSource(normalized, active string, inCodeFence bool) string {
	prompt, found := detectCodexPrompt(normalized, active, inCodeFence)
	if !found {
		return active
	}
	if block, complete := codexPromptBlock(normalized, active, prompt); complete {
		return prompt.interaction + "\x00" + compactFingerprintSource(block)
	}
	return active
}

func (*CodexAdapter) snapshotOccurrenceAware(event Event) bool {
	return event.Metadata[codexInteractionMetadata] != ""
}

// Detect gives configured intercept_patterns priority so enabling the Codex
// adapter cannot change an existing policy's event semantics. Verified Codex
// prompts are considered only when no configured pattern matches. Probes are
// value copies, so a chunk is committed to the bounded DetectionState exactly
// once.
func (a *CodexAdapter) Detect(state *DetectionState, chunk []byte) ([]Event, error) {
	if state == nil {
		return nil, fmt.Errorf("nil detection state")
	}
	if len(chunk) == 0 {
		return nil, nil
	}
	if state.pending != nil {
		return nil, nil
	}

	genericProbe := *state
	genericEvents, err := a.generic.Detect(&genericProbe, chunk)
	if err != nil {
		return nil, err
	}
	if len(genericEvents) > 0 {
		*state = genericProbe
		return a.rewriteGenericEvents(state, genericEvents), nil
	}

	vendorProbe := *state
	start, end, ok := vendorProbe.appendDetectionText(chunk)
	if ok {
		activeLine := vendorProbe.detectionText[start:end]
		if prompt, found := detectCodexPrompt(vendorProbe.detectionText, activeLine, vendorProbe.inCodeFence); found {
			// Commit the already-probed normalized chunk only after a complete,
			// structurally verified vendor prompt has been found.
			state.appendDetectionText(chunk)
			candidate := Event{
				SessionID: state.SessionID,
				AgentID:   state.AgentID,
				Adapter:   CodexID,
				Type:      EventPermission,
				Summary:   prompt.summary,
				Match:     prompt.match,
				Risk:      prompt.risk,
				Metadata: map[string]string{
					codexInteractionMetadata: prompt.interaction,
				},
			}
			candidate.Signature = stableSignature(
				state.SessionID,
				CodexID,
				EventPermission,
				prompt.interaction,
				prompt.match,
			)
			return []Event{state.replacePending(candidate)}, nil
		}
	}

	events, err := a.generic.Detect(state, chunk)
	if err != nil || len(events) == 0 {
		return events, err
	}
	return a.rewriteGenericEvents(state, events), nil
}

func (a *CodexAdapter) rewriteGenericEvents(state *DetectionState, events []Event) []Event {
	// A fallback event still belongs to the Codex session. Preserve every
	// legacy semantic field while routing future encoding back through this
	// adapter. No terminal content is added to metadata.
	for index := range events {
		event := events[index].Clone()
		event.Adapter = CodexID
		pattern := event.Metadata["pattern"]
		event.Signature = stableSignature(
			state.SessionID,
			CodexID,
			event.Type,
			"generic:"+pattern,
			event.Match,
		)
		event.ID = occurrenceID(event.Signature, event.Sequence)
		events[index] = event
		stored := event.Clone()
		state.pending = &stored
	}
	return events
}

func detectCodexPrompt(window, activeLine string, inCodeFence bool) (codexPrompt, bool) {
	if ignoredContext(activeLine, inCodeFence) {
		return codexPrompt{}, false
	}
	for _, prompt := range codexPrompts {
		footerOffset, footer := lastCodexVariant(activeLine, prompt.footers)
		if footerOffset < 0 || quotedMatch(activeLine, footerOffset, footerOffset+len(footer)) {
			continue
		}
		if _, complete := codexPromptBlock(window, activeLine, prompt); complete {
			return prompt, true
		}
	}
	return codexPrompt{}, false
}

func codexPromptBlock(window, activeLine string, prompt codexPrompt) (string, bool) {
	activeOffset := strings.LastIndex(window, activeLine)
	if activeOffset < 0 || strings.TrimSpace(window[activeOffset+len(activeLine):]) != "" {
		return "", false
	}
	footerOffset, footer := lastCodexVariant(activeLine, prompt.footers)
	if footerOffset < 0 || strings.TrimSpace(activeLine[footerOffset+len(footer):]) != "" ||
		quotedMatch(activeLine, footerOffset, footerOffset+len(footer)) {
		return "", false
	}
	footerStart := activeOffset + footerOffset
	footerEnd := footerStart + len(footer)

	// Never join a historical prompt header/options to the footer of a newer,
	// unsupported interaction. A previously completed Codex footer is a hard
	// lower bound for the current candidate block.
	blockStart := latestCodexFooterEnd(window[:footerStart])
	for _, marker := range prompt.markers {
		// Displayed command text is untrusted and can repeat the question
		// verbatim. Try every marker candidate from newest to oldest rather than
		// letting the last occurrence shadow the real prompt header.
		for searchEnd := footerStart; searchEnd > blockStart; {
			markerOffset := strings.LastIndex(window[blockStart:searchEnd], marker)
			if markerOffset < 0 {
				break
			}
			markerOffset += blockStart
			body := window[markerOffset:footerStart]
			allowOffset := firstCodexVariant(body, prompt.allows)
			denyOffset := firstCodexVariant(body, prompt.denies)
			if allowOffset >= 0 && denyOffset >= 0 && allowOffset < denyOffset {
				return window[markerOffset:footerEnd], true
			}
			searchEnd = markerOffset
		}
	}
	return "", false
}

func latestCodexFooterEnd(value string) int {
	latest := 0
	for _, candidate := range codexPrompts {
		for _, footer := range candidate.footers {
			if offset := strings.LastIndex(value, footer); offset >= 0 && offset+len(footer) > latest {
				latest = offset + len(footer)
			}
		}
	}
	return latest
}

func compactFingerprintSource(value string) string {
	return strings.Map(func(character rune) rune {
		switch character {
		case ' ', '\t', '\n', '\r', '\v', '\f':
			return -1
		default:
			return character
		}
	}, value)
}

func firstCodexVariant(value string, variants []string) int {
	first := -1
	for _, variant := range variants {
		if offset := strings.Index(value, variant); offset >= 0 && (first < 0 || offset < first) {
			first = offset
		}
	}
	return first
}

func lastCodexVariant(value string, variants []string) (int, string) {
	last := -1
	matched := ""
	for _, variant := range variants {
		if offset := strings.LastIndex(value, variant); offset > last {
			last = offset
			matched = variant
		}
	}
	return last, matched
}

// EncodeDecision supports only byte sequences verified against the captured
// CLI. Generic fallback events retain the established manual-input behavior.
func (a *CodexAdapter) EncodeDecision(event Event, decision Decision, manualInput string) ([]byte, error) {
	if !event.Actionable() {
		return nil, fmt.Errorf("%w for type %q", ErrDecisionUnsupported, event.Type)
	}
	interaction := event.Metadata[codexInteractionMetadata]
	if interaction == "" {
		return a.generic.EncodeDecision(event, decision, manualInput)
	}
	if strings.IndexByte(manualInput, 0) >= 0 {
		return nil, fmt.Errorf("invalid manual input: NUL byte")
	}
	if decision != DecisionManual && manualInput != "" {
		return nil, fmt.Errorf("an automatic Codex decision cannot carry manual input")
	}

	switch interaction {
	case codexCommandApproval:
		switch decision {
		case DecisionAllow:
			return []byte{'y'}, nil
		case DecisionDeny:
			return []byte{0x1b}, nil
		case DecisionManual:
			switch manualInput {
			case "y":
				return []byte{'y'}, nil
			case "esc":
				return []byte{0x1b}, nil
			}
		}
	case codexDirectoryTrust:
		switch decision {
		case DecisionDeny:
			return []byte{'2'}, nil
		case DecisionManual:
			switch manualInput {
			case "1":
				return []byte{'\r'}, nil
			case "2":
				return []byte{'2'}, nil
			}
		}
	default:
		return nil, fmt.Errorf("%w: Codex interaction %q", ErrDecisionUnsupported, interaction)
	}
	return nil, fmt.Errorf("%w: %q for Codex interaction %q", ErrDecisionUnsupported, decision, interaction)
}
