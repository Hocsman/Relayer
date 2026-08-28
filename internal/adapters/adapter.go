package adapters

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"
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

func (s *DetectionState) acknowledge(eventID string) error {
	if s == nil || s.pending == nil {
		return nil
	}
	if eventID != "" && s.pending.ID != eventID {
		return fmt.Errorf("%w: reçu %q, attendu %q", ErrEventMismatch, eventID, s.pending.ID)
	}
	s.pending = nil
	s.detectionText = ""
	s.inCodeFence = false
	return nil
}

func (s *DetectionState) restore(event Event) error {
	if s == nil {
		return fmt.Errorf("état de détection nil")
	}
	if s.pending != nil && s.pending.ID != event.ID {
		return fmt.Errorf("%w: impossible de restaurer %q", ErrEventMismatch, event.ID)
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
