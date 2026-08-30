package adapters

import (
	"fmt"
	"regexp"
	"strings"
)

const GenericID = "generic"

// Pattern is the backward-compatible representation of intercept_patterns.
type Pattern struct {
	Name        string
	Description string
	Expression  string
	Sensitive   bool
}

type compiledPattern struct {
	Pattern
	regex *regexp.Regexp
}

// GenericRegexAdapter is the stable compatibility adapter for existing
// intercept_patterns. It receives only normalized text from Processor.
type GenericRegexAdapter struct {
	patterns []compiledPattern
}

// NewGenericRegexAdapter validates and defensively copies all expressions.
func NewGenericRegexAdapter(patterns []Pattern) (*GenericRegexAdapter, error) {
	compiled := make([]compiledPattern, 0, len(patterns))
	for _, pattern := range patterns {
		expression, err := regexp.Compile(pattern.Expression)
		if err != nil {
			return nil, fmt.Errorf("invalid regex %q: %w", pattern.Name, err)
		}
		compiled = append(compiled, compiledPattern{Pattern: pattern, regex: expression})
	}
	return &GenericRegexAdapter{patterns: compiled}, nil
}

func (*GenericRegexAdapter) ID() string { return GenericID }

// Detect preserves pattern order and reports at most one actionable event
// while another occurrence is pending. A match may span chunks, but it must
// reach the active terminal line affected by the newest normalized chunk.
func (a *GenericRegexAdapter) Detect(state *DetectionState, chunk []byte) ([]Event, error) {
	if state == nil {
		return nil, fmt.Errorf("nil detection state")
	}
	if len(chunk) == 0 {
		return nil, nil
	}
	burstStart := len(state.detectionText)
	if state.hasRendered {
		// On a grid the burst is a set of rows, reported by the screen, rather
		// than everything appended since the last call.
		burstStart = state.renderedBurst
	}
	start, end, ok := state.appendDetectionText(chunk)
	if !ok || state.pending != nil {
		return nil, nil
	}
	// The region is everything this write produced, not just the line it
	// happened to end on. burstStart is clamped because the window is trimmed
	// to its last 16 KiB, which can move the whole text left under a large
	// write.
	if burstStart > start {
		burstStart = start
	}
	activeLine := state.detectionText[start:end]
	// Coarse net first: when the whole active line is quoted documentation the
	// burst is documentation too. Kept behind the per-line gates below so that
	// a per-line mistake fails toward reporting rather than toward silence.
	if ignoredContext(activeLine, state.inCodeFence) && burstStart >= start {
		return nil, nil
	}

	for _, pattern := range a.patterns {
		matches := pattern.regex.FindAllStringIndex(state.detectionText, -1)
		for index := len(matches) - 1; index >= 0; index-- {
			matchRange := matches[index]
			if matchRange[1] <= burstStart {
				continue
			}
			matchLineStart := strings.LastIndexByte(state.detectionText[:matchRange[0]], '\n') + 1
			matchLineEnd := matchRange[1]
			if newline := strings.IndexByte(state.detectionText[matchRange[1]:], '\n'); newline >= 0 {
				matchLineEnd += newline
			} else {
				matchLineEnd = len(state.detectionText)
			}
			matchLine := state.detectionText[matchLineStart:matchLineEnd]
			// The fence parity at the MATCH's line, not at the window end. Using
			// the window-end flag here is what turned a fenced example into a
			// real supervision event the last time this region was widened.
			if ignoredContext(matchLine, fenceDepthBefore(state.detectionText, matchLineStart, state.inCodeFence)) {
				continue
			}
			// Anything below the match must be the agent's own furniture. Real
			// content beneath a question means the question was overtaken.
			if matchLineEnd < len(state.detectionText) &&
				!tailIsFurniture(state, state.detectionText[matchLineEnd+1:]) {
				continue
			}
			if quotedMatch(matchLine, matchRange[0]-matchLineStart, matchRange[1]-matchLineStart) {
				continue
			}
			match := state.detectionText[matchRange[0]:matchRange[1]]
			sensitive := pattern.Sensitive || IsSensitiveText(match) ||
				IsSensitiveText(pattern.Name+" "+pattern.Description)
			eventType := EventConfirmation
			risk := RiskUnknown
			if sensitive {
				eventType = EventCredential
				risk = RiskHigh
			}
			summary := strings.TrimSpace(pattern.Description)
			if summary == "" {
				summary = strings.TrimSpace(pattern.Name)
			}
			candidate := Event{
				questionLine: matchLine,

				SessionID: state.SessionID,
				AgentID:   state.AgentID,
				Adapter:   GenericID,
				Type:      eventType,
				Summary:   summary,
				Match:     match,
				Sensitive: sensitive,
				Risk:      risk,
				Metadata:  map[string]string{"pattern": pattern.Name},
			}
			// On a rendered screen the answered question stays painted until
			// the agent redraws without it, so the answer has to be remembered
			// rather than the text forgotten.
			if state.hasRendered && state.answersTheSameQuestion(matchLine) {
				continue
			}
			candidate.Signature = stableSignature(
				state.SessionID,
				GenericID,
				eventType,
				pattern.Name,
				match,
			)
			return []Event{state.replacePending(candidate)}, nil
		}
	}
	return nil, nil
}

func (*GenericRegexAdapter) EncodeDecision(event Event, decision Decision, manualInput string) ([]byte, error) {
	if decision != DecisionManual {
		return nil, fmt.Errorf("%w: %q", ErrDecisionUnsupported, decision)
	}
	if !event.Actionable() {
		return nil, fmt.Errorf("%w for type %q", ErrDecisionUnsupported, event.Type)
	}
	if strings.IndexByte(manualInput, 0) >= 0 {
		return nil, fmt.Errorf("invalid manual input: NUL byte")
	}
	return []byte(manualInput + "\r"), nil
}

func ignoredContext(line string, inCodeFence bool) bool {
	trimmed := strings.TrimSpace(line)
	lower := strings.ToLower(trimmed)
	return inCodeFence ||
		strings.HasPrefix(trimmed, "> ") ||
		markdownTableRow(trimmed) ||
		strings.HasPrefix(trimmed, "```") ||
		strings.HasPrefix(lower, "log:") ||
		strings.HasPrefix(lower, "previous:") ||
		strings.HasPrefix(lower, "historique:")
}

func quotedMatch(line string, start, end int) bool {
	if start < 0 || end > len(line) || start >= end {
		return false
	}
	for _, quote := range []byte{'`', '\'', '"'} {
		before := strings.LastIndexByte(line[:start], quote)
		if before < 0 {
			continue
		}
		after := strings.IndexByte(line[end:], quote)
		if after >= 0 {
			return true
		}
	}
	return false
}

// markdownTableRow reports a table row, which is quoted documentation rather
// than a live question.
//
// Leading "| " alone is not enough: an agent that draws its prompt inside an
// ASCII frame writes "| Overwrite file? [Y/n] |", which has the same prefix and
// used to be suppressed at every chunk size. A table row separates cells, so it
// carries more pipes than the two a frame uses to close its sides.
func markdownTableRow(trimmed string) bool {
	if !strings.HasPrefix(trimmed, "| ") {
		return false
	}
	return strings.Count(trimmed, "|") > 2
}

// tailIsFurniture picks the rule that matches the substrate. A screen's blank
// rows are an artefact of its height; a byte stream's are output the agent
// actually wrote.
func tailIsFurniture(state *DetectionState, tail string) bool {
	if state != nil && state.hasRendered {
		return furnitureTailOnScreen(tail)
	}
	return furnitureTail(tail)
}
