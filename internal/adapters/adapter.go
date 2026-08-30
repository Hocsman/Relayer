package adapters

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Hocsman/Relayer/internal/screen"
)

const detectionWindowSize = 16 * 1024

// codeFenceMarker is compared against the trimmed current line to toggle the
// fenced-block suppression state.
var codeFenceMarker = []byte("```")

// Adapter interprets normalized, ANSI-free terminal text. Implementations do
// not own processes, PTYs, tmux sessions, rendered history or audit sinks.
type Adapter interface {
	ID() string
	Detect(state *DetectionState, chunk []byte) ([]Event, error)
	EncodeDecision(event Event, decision Decision, manualInput string) ([]byte, error)
}

// DetectionState is the bounded, per-session state supplied to an Adapter.
// A state must never be shared by two terminal sessions.
type DetectionState struct {
	SessionID string
	AgentID   string
	AdapterID string

	detectionText string
	inCodeFence   bool
	sequence      uint64
	pending       *Event

	// rendered carries the screen the Processor rendered for this chunk, when
	// the agent has repainted. It replaces the accumulated byte window rather
	// than adding to it: on a grid the text IS the state, not a history of
	// writes. renderedBurst is where the rows this write touched begin.
	//
	// These are values, not a pointer to the screen, and deliberately so. The
	// Codex adapter probes by copying DetectionState (`vendorProbe := *state`),
	// so a pointer here would be shared with every speculative probe and those
	// probes would write into the real screen.
	rendered      string
	renderedBurst int
	hasRendered   bool

	// renderedAnchors turns an offset in `rendered` back into the row that
	// painted it. It is the screen's own reading of the text it just produced,
	// and it is a value for the same reason the two fields above are: the probe
	// copies the whole state.
	renderedAnchors screen.Anchors

	// answered is the signature of the occurrence the operator last dealt with,
	// kept only while that question is still painted.
	//
	// A byte window solved this by forgetting the text: answered output could
	// not come back because text only ever accumulated. A screen has no history
	// to forget — the question IS still on the screen, legitimately, until the
	// agent redraws without it. So a repainting agent re-raised every question
	// on the very next frame, which is worse than the defect the screen fixes.
	// answered holds every question the operator has dealt with that is still
	// painted. A single slot is not enough: two questions can sit on screen at
	// once, and answering the second overwrote the memory of the first, which
	// the rescan then re-reported under its original signature.
	answered []answeredQuestion
}

// answeredQuestion is one question the operator dealt with, tied to the row it
// was detected on.
//
// The row decides when the entry EXPIRES: once that row stops carrying the
// question, the entry goes. It does not decide what the entry SUPPRESSES —
// answersTheSameQuestion still compares signature and text, so while an entry
// is alive it silences any candidate of the same signature, wherever that
// candidate sits. Two questions that share a captured fragment are therefore
// still one question to this memory, which is #30 and not the anchor: nothing
// here can tell them apart while "[y/n]" is what a question is called.
//
// anchor is the row the MATCH was found on at detection time, carried here from
// the detector rather than searched for afterwards. A zero anchor means the
// occurrence reached the state without ever crossing a screen — restored from a
// snapshot — and is the only case that still has to fall back to text.
type answeredQuestion struct {
	signature string
	match     string
	anchor    screen.RowID
}

// maxAnsweredMemory bounds the set. A screen cannot show an unbounded number of
// live questions, and a stale entry only costs a suppression that the row check
// would have released anyway.
const maxAnsweredMemory = 16

// pendingAnswers reports the entries whose row still has to be checked, so the
// Processor can ask the screen about each one.
func (s *DetectionState) pendingAnswers() []answeredQuestion {
	if s == nil {
		return nil
	}
	return append([]answeredQuestion(nil), s.answered...)
}

// keepAnswered replaces the memory with the entries that are still live.
func (s *DetectionState) keepAnswered(live []answeredQuestion) {
	if s == nil {
		return
	}
	s.answered = live
}

// rememberAnswered records what the operator just dealt with, so a screen that
// still shows it does not ask again.
func (s *DetectionState) rememberAnswered(signature, match string, anchor screen.RowID) {
	if s == nil || signature == "" {
		return
	}
	for index, entry := range s.answered {
		if entry.signature == signature && entry.match == match {
			// The same question answered again on another row is that row's
			// question now: keep the newer anchor, or the memory would go on
			// watching a line the operator has finished with.
			if anchor != 0 {
				s.answered[index].anchor = anchor
			}
			return
		}
	}
	s.answered = append(s.answered, answeredQuestion{signature: signature, match: match, anchor: anchor})
	if len(s.answered) > maxAnsweredMemory {
		s.answered = s.answered[len(s.answered)-maxAnsweredMemory:]
	}
}

// answersTheSameQuestion reports whether a candidate is the question the
// operator already dealt with, still painted on the screen.
//
// Signature alone is not enough. One line can match several patterns — the
// default set matches "Overwrite file? [Y/n]" as an overwrite AND as a yes/no
// confirmation — so suppressing the answered signature let the same line come
// straight back under the other pattern's signature. Text that is part of the
// answered text is part of the answered question.
func (s *DetectionState) answersTheSameQuestion(signature, match string) bool {
	if s == nil {
		return false
	}
	for _, entry := range s.answered {
		if signature == entry.signature {
			return true
		}
		if match == "" || entry.match == "" {
			continue
		}
		if strings.Contains(entry.match, match) || strings.Contains(match, entry.match) {
			return true
		}
	}
	return false
}

// UseRenderedScreen hands the state the screen text for this chunk, replacing
// the accumulated byte window. Only the Processor calls this, and only for an
// agent that has repainted.
func (s *DetectionState) UseRenderedScreen(text string, burstStart int, inCodeFence bool, anchors screen.Anchors) {
	if s == nil {
		return
	}
	s.rendered = text
	s.renderedBurst = burstStart
	s.hasRendered = true
	s.inCodeFence = inCodeFence
	s.renderedAnchors = anchors
}

// anchorAt names the row that painted the byte at offset in the detection text.
//
// It answers only on the grid path, where the detection text IS the text the
// screen rendered and the anchors came out of that same render. On the byte
// window there is no screen to point at, and inventing a row there would be a
// coordinate about nothing.
func (s *DetectionState) anchorAt(offset int) screen.RowID {
	if s == nil || !s.hasRendered {
		return 0
	}
	id, ok := s.renderedAnchors.RowAt(offset)
	if !ok {
		return 0
	}
	return id
}

// NewDetectionState creates independent state for one agent session.
func NewDetectionState(sessionID, agentID, adapterID string) *DetectionState {
	return &DetectionState{
		SessionID: strings.TrimSpace(sessionID),
		AgentID:   strings.TrimSpace(agentID),
		AdapterID: strings.ToLower(strings.TrimSpace(adapterID)),
	}
}

// Pending returns a defensive copy of the current actionable event.
func (s *DetectionState) Pending() *Event {
	if s == nil || s.pending == nil {
		return nil
	}
	clone := s.pending.Clone()
	return &clone
}

// IsBlocked reports whether an actionable event still awaits a decision.
func (s *DetectionState) IsBlocked() bool {
	return s != nil && s.pending != nil
}

// acknowledge clears the pending occurrence and returns its signature so the
// caller can tell it apart from anything else still in the window.
//
// The detection window is deliberately retained. Detect stops examining output
// while an occurrence is pending, so a prompt that arrived during that window
// is sitting in this text and nowhere else; wiping it here lost that prompt for
// good, on both backends. Retaining it cannot resurrect the acknowledged
// occurrence either: Detect only reports a match that reaches the active line
// produced by new output, which historical text never does.
//
// Callers that know the window no longer reflects the screen - an emptied
// snapshot, a process exit - reset it explicitly.
func (s *DetectionState) acknowledge(eventID string) (string, error) {
	if s == nil || s.pending == nil {
		return "", nil
	}
	if eventID != "" && s.pending.ID != eventID {
		return "", fmt.Errorf("%w: got %q, want %q", ErrEventMismatch, eventID, s.pending.ID)
	}
	signature := s.pending.Signature
	// The match text goes with the signature: it is how the state later notices
	// that the question has left the screen.
	s.rememberAnswered(signature, s.pending.Match, s.pending.anchor)
	s.pending = nil
	return signature, nil
}

// discard clears the pending occurrence WITHOUT remembering it as answered.
//
// It is for the paths that abandon a question rather than answer it: an empty
// or undetectable snapshot, a process that exited. Nothing was delivered to the
// agent there, so there is nothing to suppress later — and remembering it would
// leave an entry describing a screen the caller has just declared stale, which
// is how the memory latched on with no way to be released.
func (s *DetectionState) discard() {
	if s == nil {
		return
	}
	s.pending = nil
}

// resetWindow drops the retained text. It is for the paths where the window is
// known to be stale rather than merely answered.
func (s *DetectionState) resetWindow() {
	if s == nil {
		return
	}
	s.detectionText = ""
	s.inCodeFence = false
}

func (s *DetectionState) restore(event Event) error {
	if s == nil {
		return fmt.Errorf("nil detection state")
	}
	if s.pending != nil && s.pending.ID != event.ID {
		return fmt.Errorf("%w: cannot restore %q", ErrEventMismatch, event.ID)
	}
	clone := event.Clone()
	s.pending = &clone
	if event.Sequence > s.sequence {
		s.sequence = event.Sequence
	}
	return nil
}

func (s *DetectionState) replacePending(candidate Event) Event {
	s.sequence++
	candidate.Sequence = s.sequence
	candidate.ID = occurrenceID(candidate.Signature, s.sequence)
	candidate.Timestamp = time.Now().UTC()
	clone := candidate.Clone()
	s.pending = &clone
	return clone.Clone()
}

func (s *DetectionState) appendDetectionText(chunk []byte) (candidateStart, candidateEnd int, ok bool) {
	if s == nil || len(chunk) == 0 {
		return 0, 0, false
	}
	// A repainting agent's text is the rendered screen, not an accumulation of
	// what it wrote. The screen already applied the carriage returns, the
	// erases and the addressing that this loop approximates, so it replaces the
	// window rather than being appended to it.
	if s.hasRendered {
		s.detectionText = s.rendered
		return activeLineRange(s.detectionText)
	}
	// Accumulate into a byte buffer rather than reassigning the string per
	// rune: `s.detectionText += ...` copied the whole 16 KiB window on every
	// character, which made this quadratic in chunk size and dominated the
	// cost of consuming output while holding the processor lock.
	buffer := make([]byte, 0, len(s.detectionText)+len(chunk))
	buffer = append(buffer, s.detectionText...)
	for _, character := range string(chunk) {
		switch character {
		case '\r':
			if index := bytes.LastIndexByte(buffer, '\n'); index >= 0 {
				buffer = buffer[:index+1]
			} else {
				buffer = buffer[:0]
			}
		case '\n':
			lineStart := bytes.LastIndexByte(buffer, '\n') + 1
			if bytes.HasPrefix(bytes.TrimSpace(buffer[lineStart:]), codeFenceMarker) {
				s.inCodeFence = !s.inCodeFence
			}
			buffer = append(buffer, '\n')
		case '\t':
			buffer = append(buffer, '\t')
		default:
			if character >= 0x20 && (character < 0x7f || character >= 0xa0) {
				buffer = utf8.AppendRune(buffer, character)
			}
		}
	}
	if len(buffer) > detectionWindowSize {
		buffer = buffer[len(buffer)-detectionWindowSize:]
	}
	s.detectionText = string(buffer)
	return activeLineRange(s.detectionText)
}

func activeLineRange(text string) (start, end int, ok bool) {
	end = len(text)
	for end > 0 && (text[end-1] == '\n' || text[end-1] == '\r') {
		end--
	}
	if end == 0 {
		return 0, 0, false
	}
	start = strings.LastIndexByte(text[:end], '\n') + 1
	if strings.TrimSpace(text[start:end]) == "" {
		return 0, 0, false
	}
	return start, end, true
}

func stableSignature(sessionID, adapterID string, eventType EventType, pattern, match string) string {
	canonicalMatch := strings.ToLower(strings.Join(strings.Fields(match), " "))
	payload := strings.Join([]string{sessionID, adapterID, string(eventType), pattern, canonicalMatch}, "\x00")
	digest := sha256.Sum256([]byte(payload))
	return hex.EncodeToString(digest[:16])
}

func occurrenceID(signature string, sequence uint64) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", signature, sequence)))
	return "evt-" + hex.EncodeToString(digest[:12])
}
