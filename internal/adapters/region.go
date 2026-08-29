package adapters

import (
	"strings"
	"unicode"
)

// The actionable region and the two rules that keep widening it safe.
//
// Detection used to keep a match only when it touched the last non-empty line
// of the accumulated window. That tied supervision to where the operating
// system happened to end a read: an agent that wrote its question and the
// furniture beneath it — a frame, an option list, a key hint — in one write was
// not supervised at all, while the identical bytes split across two reads were.
// It failed silently and in the unsafe direction.
//
// The region is now everything the current write produced. Two rules keep that
// from becoming a flood, and each answers a specific way the previous attempt
// at widening failed:
//
//   - fenceDepthBefore walks the fence parity backwards from the window end, so
//     a match is judged by the fence state at ITS OWN line. The reverted attempt
//     used state.inCodeFence, which describes the end of the window, and turned
//     a documented example inside a fenced block into a real supervision event.
//   - furnitureTail requires everything below the match to be decoration, a key
//     hint, or a bounded run of blank lines. A prompt with real content beneath
//     it has been overtaken and is history, which is what stopped the earlier
//     attempt from resurrecting an answered question.

// maxBlankTailLines bounds the blank run the tail may cross. A prompt separated
// from its footer by a couple of blank lines is still the live question; one
// separated by a screenful is not. The bound is deliberately a named constant
// rather than a judgement call spread through the predicate.
const maxBlankTailLines = 4

// maxContinuationLines bounds the wrapped remainder of a question that the tail
// may cross. Three lines is what a long question costs at a normal terminal
// width; a longer run is a paragraph, not a wrap.
const maxContinuationLines = 4

// fenceDepthBefore reports whether the byte at offset sits inside a markdown
// code fence.
//
// The state carries only inCodeFence, the parity at the END of the window, so
// the parity at an earlier offset is recovered by unwinding the toggles between
// the two. This needs no extra state and stays correct when the fence that
// opened the block has already been trimmed out of the 16 KiB window.
func fenceDepthBefore(text string, offset int, windowEndFence bool) bool {
	if offset < 0 || offset > len(text) {
		return windowEndFence
	}
	inFence := windowEndFence
	// Every complete line after the offset toggles the parity it contributed.
	// The final partial line has not toggled anything yet, so it is skipped.
	rest := text[offset:]
	for {
		newline := strings.IndexByte(rest, '\n')
		if newline < 0 {
			break
		}
		if isFenceMarker(rest[:newline]) {
			inFence = !inFence
		}
		rest = rest[newline+1:]
	}
	return inFence
}

func isFenceMarker(line string) bool {
	return strings.HasPrefix(strings.TrimSpace(line), string(codeFenceMarker))
}

// furnitureTail reports whether everything after the match is the agent's own
// decoration rather than new work.
//
// A question is live while only its frame, its option list and its key hint sit
// beneath it. Once the agent has printed real content below, the question has
// been overtaken: reporting it then would resurrect history, which is exactly
// what killed the previous widening attempt.
//
// The empty tail is furniture by definition, so a match that still reaches the
// active line can never be rejected by this gate. That makes the change
// monotone: nothing detected before this rule existed stops being detected.
func furnitureTail(tail string) bool {
	lines := strings.Split(tail, "\n")

	// A question that does not fit the terminal width continues on the next
	// lines. That wrapped remainder is the question, not new work, so the run of
	// non-blank lines immediately below the match is allowed — but only when
	// what follows it proves the block really is a prompt. Without that anchor
	// this would tolerate any paragraph and resurrect finished questions.
	continuation := 0
	for continuation < len(lines) {
		trimmed := strings.TrimSpace(lines[continuation])
		// Furniture is not continuation. Absorbing an option list here would
		// consume the very anchor the rule then looks for below it.
		if trimmed == "" || isDecorative(trimmed) || isKeyHint(trimmed) || isOptionLine(trimmed) {
			break
		}
		continuation++
	}
	if continuation > 0 {
		if continuation > maxContinuationLines || !hasChoiceAnchor(lines[continuation:]) {
			return false
		}
		lines = lines[continuation:]
	}

	blankRun := 0
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" {
			blankRun++
			if blankRun > maxBlankTailLines {
				return false
			}
			continue
		}
		blankRun = 0
		if isDecorative(trimmed) || isKeyHint(trimmed) || isOptionLine(trimmed) {
			continue
		}
		return false
	}
	return true
}

// hasChoiceAnchor reports whether the lines below a wrapped question contain the
// thing that makes it a question: a choice to pick or a key to press. It is what
// separates "the agent asked something and is waiting" from "the agent printed a
// paragraph and moved on".
func hasChoiceAnchor(lines []string) bool {
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if isOptionLine(trimmed) || isKeyHint(trimmed) {
			return true
		}
	}
	return false
}

// isDecorative matches a line carrying no word at all: box drawing, rules,
// separators. Deliberately expressed as the absence of letters and digits
// rather than as a list of characters, so an unfamiliar box-drawing set or a
// decorative rune Relayer has never seen is covered without being enumerated.
func isDecorative(trimmed string) bool {
	for _, character := range trimmed {
		if unicode.IsLetter(character) || unicode.IsDigit(character) {
			return false
		}
	}
	return true
}

var (
	keyHintKeys = []string{
		"enter", "esc", "escape", "tab", "space", "ctrl", "arrow", "y/n", "[y", "[n",
	}
	keyHintActions = []string{
		"press", "confirm", "cancel", "continue", "select", "choose", "quit",
		"accept", "reject", "toggle", "submit", "abort", "skip",
	}
)

// isKeyHint matches a footer that tells the operator which key to press. It
// requires a token from BOTH closed vocabularies, so a sentence that merely
// mentions one of these words — "the agent will enter the directory" — is not
// mistaken for a hint and cannot let the tail step over real content.
func isKeyHint(trimmed string) bool {
	lower := strings.ToLower(trimmed)
	return containsAny(lower, keyHintKeys) && containsAny(lower, keyHintActions)
}

// isOptionLine matches one entry of a numbered or bulleted choice list, the
// shape every modern agent CLI prints under its question. It is anchored to the
// start of the line and bounded in length so a paragraph that happens to begin
// with a digit is not swallowed.
func isOptionLine(trimmed string) bool {
	const maxOptionLength = 96
	if len(trimmed) > maxOptionLength {
		return false
	}
	rest := strings.TrimLeft(trimmed, "›>*-•· \t")
	if rest == "" {
		return false
	}
	// A bracketed key: "[y] yes", "(n) no". The bracket has to close within a
	// few characters, so a sentence opening with a parenthesis is not swallowed.
	if opener := strings.IndexAny(rest[:1], "[("); opener == 0 {
		closer := strings.IndexAny(rest, "])")
		return closer > 1 && closer <= 4
	}
	if !startsWithDigit(rest) {
		return false
	}
	rest = strings.TrimLeft(rest, "0123456789")
	return strings.HasPrefix(rest, ".") || strings.HasPrefix(rest, ")") ||
		strings.HasPrefix(rest, " ") || rest == ""
}

func startsWithDigit(value string) bool {
	return value != "" && value[0] >= '0' && value[0] <= '9'
}

func containsAny(haystack string, needles []string) bool {
	for _, needle := range needles {
		if strings.Contains(haystack, needle) {
			return true
		}
	}
	return false
}
