package adapters

import (
	"bytes"
	"crypto/rand"
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

	// instance tells this process apart from any earlier one under the same
	// session, and is part of every occurrence ID it produces. The sequence in
	// an ID starts again with each process, and the signature of a prompt can
	// be as little as its "[y/n]": without the instance, a restarted agent's
	// first prompt had the same ID as the previous process's, so a decision on
	// the old prompt passed the ID check and was delivered to the new one, a
	// late prompt of the old process shadowed the new one, and its exit was
	// dropped as a duplicate. Probes built to rescan the screen carry the same
	// instance, so an ID never depends on which state computed it. The token is
	// never written anywhere: IDs of sensitive prompts are withheld from the
	// journal because they derive from the match, and must stay unguessable.
	instance string

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
// question, the entry goes. A row parked with the primary screen while a
// full-screen program shows the alternate one has not stopped carrying it.
// On the rendered screen the row also decides what the entry SUPPRESSES, see
// answeredAt and answeredOnItsRow: the answered question is the one on its own
// row, and the same words on another row are another question. The generic
// adapter makes one exception, for an answered row that has been blanked. A
// snapshot, whose text maps onto no row, and the Codex adapter still compare
// the line alone, see answersTheSameQuestion.
//
// anchor is the row the MATCH was found on at detection time, carried here from
// the detector rather than searched for afterwards. A zero anchor means the
// occurrence reached the state without ever crossing a screen — raised on the
// byte window before the agent first repainted, or restored from a snapshot —
// and is the only case that still has to be found by its text.
//
// rowBlank records that the row was blank at the last write: a frame caught
// between its erase and its repaint. The Processor sets it, because only the
// screen knows; see answeredAt for the one thing it changes.
type answeredQuestion struct {
	signature string
	match     string
	line      string
	anchor    screen.RowID
	rowBlank  bool
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
func (s *DetectionState) rememberAnswered(signature, match string, anchor screen.RowID, line string) {
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
				// The occurrence was just seen on that row, so it is not blank.
				s.answered[index].rowBlank = false
			}
			if line != "" {
				s.answered[index].line = line
			}
			return
		}
	}
	s.answered = append(s.answered, answeredQuestion{signature: signature, match: match, anchor: anchor, line: line})
	if len(s.answered) > maxAnsweredMemory {
		s.answered = s.answered[len(s.answered)-maxAnsweredMemory:]
	}
}

// answersTheSameQuestion reports whether a candidate is the question the
// operator already dealt with, still painted on the screen.
//
// The question is the LINE, not the fragment a pattern captured. Identifying it
// by the fragment made two unrelated questions the same question: the shipped
// `confirmation` pattern captures the literal "[y/n]", so every yes/no question
// in a session produced the same signature and the second one was swallowed —
// "Run 'npm test'? [y/n]" answered, then "Run 'rm -rf /' as root? [y/n]" never
// reported.
//
// Comparing whole lines still does what the fragment comparison was there for:
// one line can match several patterns — the default set matches "Overwrite
// file? [Y/n]" as an overwrite AND as a yes/no confirmation — and suppressing
// only the answered signature let it come straight back under the other
// pattern's name. Same line, same question, whichever pattern found it.
//
// It knows no row, which is right only where there is none to know: a
// snapshot, whose text maps onto no grid, and the Codex adapter, whose footer
// must end the line, so that nothing typed after the question can still match.
// Detection on the rendered screen asks answeredAt, which knows the row.
func (s *DetectionState) answersTheSameQuestion(line string) bool {
	if s == nil {
		return false
	}
	asked := strings.TrimSpace(line)
	if asked == "" {
		return false
	}
	for _, entry := range s.answered {
		if entry.line != "" && strings.TrimSpace(entry.line) == asked {
			return true
		}
	}
	return false
}

// answeredAt reports whether a candidate found on the rendered screen, on the
// row anchor, is a question the operator already dealt with.
//
// An answer is typed at the question, so whatever shows it again shows it on
// the question's own row. A terminal in cooked mode echoes the keystrokes after
// the cursor, and ConPTY, which renders every Windows session, flushes that echo
// as a write of its own as soon as the agent is slow to print. Even with no
// echo at all, the next write re-reads a screen that still shows the question.
// The whole-line comparison could not see the echo, since "Overwrite file? [y/n]
// y" is not "Overwrite file? [y/n]", and the Aider, Goose and Open Interpreter
// prompts were never compared with anything. The answered question therefore
// came back under a new ID on the very next write: a second card for a human,
// and under an automatic policy a second "y" typed into an agent that had
// already consumed the first.
//
// So the row decides, in two ways:
//
//  1. On the answered row, a line that still BEGINS with the answered question
//     is that question, whatever follows it. What follows is the echo.
//  2. On another row, the same line is the answered question only when that
//     question cannot be accounted for on its own row: one side carries no
//     row, or the answered row is blank. A blank row is a frame caught between
//     its erase and its repaint, which may be drawing the same question a row
//     higher or lower, and asking there would type a second answer. This is
//     all the row-less comparison ever protected: an entry whose row shows
//     something else has expired before detection runs.
//
// While the answered row still shows the question, the same words on another
// row are the agent asking again, and the operator is asked. The row-less
// comparison swallowed that question; after an echo, the repeat of the answered
// one stood in for it, so nothing seemed lost.
//
// This is the generic adapter's rule: Claude's through it, and that of the
// configured intercept_patterns every vendor adapter tries first. The Aider,
// Goose and Open Interpreter prompts themselves use answeredOnItsRow, which is
// rule 1 alone; see there for why the blank row is not an excuse for them.
//
// What is left, knowingly:
//
//   - An agent that rejects the answer and asks again on the SAME row, with the
//     rejected input still showing after the question, is taken for the echo
//     and not asked. So is the identical question drawn again on the very row
//     the answered one occupied, whether the input was erased first or a clear
//     homed the cursor there. A re-ask on a new row is asked.
//   - A blank row stays blank for as long as nothing is written on it. After a
//     clear, the identical question drawn above the answered row, which the
//     clear left blank, is taken for the answered one moved, and is not asked.
//     The row-less comparison lost that question too, anywhere on the screen.
//   - A frame that moves the answered question to another row while painting
//     its old row with something else, in one write, asks it again, as it did
//     before. So does one that moves it off its blank row with the echo after
//     it, since the second rule wants the identical line.
func (s *DetectionState) answeredAt(line string, anchor screen.RowID) bool {
	return s.answeredOnTheScreen(line, anchor, true)
}

// answeredOnItsRow is answeredAt without its second rule's blank row: the
// answered question is the one on its own row, and nowhere else.
//
// It is the rule for the Aider, Goose and Open Interpreter prompts, and the
// difference is what each adapter did before it had one. The generic adapter
// compared whole lines anywhere, so it already took the identical question
// after a clear for the answered one; the blank row keeps the one case that
// comparison protected and loses nothing it did not lose. These three never
// consulted the memory at all, so the blank row would be a loss of its own,
// and not a rare one. Clears are common: Ctrl+L at Aider's prompt, `clear`, a
// test runner. A clear leaves the answered row blank for as long as nothing is
// written on it, which on a full screen is twenty lines of output or more, and
// Aider asks for every shell command with one and the same line. The next
// command's question was taken for the answered one and nobody was asked: the
// agent waited for an answer that no card was offering.
//
// What they give up is the frame caught between an erase and a repaint that
// draws the answered question on another row. That asks it again, as it did
// before for every one of their questions. Of the two failures, a question
// never put to anyone is the worse: it blocks the agent without a sign.
//
// When either side carries no row, the line alone decides, as it does for the
// generic adapter: nothing else is left to tell two questions apart.
func (s *DetectionState) answeredOnItsRow(line string, anchor screen.RowID) bool {
	return s.answeredOnTheScreen(line, anchor, false)
}

// answeredOnTheScreen is the body of answeredAt and answeredOnItsRow.
// overABlankRow says whether a blank answered row lets the same line on another
// row stand for the answered question.
func (s *DetectionState) answeredOnTheScreen(line string, anchor screen.RowID, overABlankRow bool) bool {
	if s == nil {
		return false
	}
	asked := strings.TrimSpace(line)
	if asked == "" {
		return false
	}
	for _, entry := range s.answered {
		answered := strings.TrimSpace(entry.line)
		if answered == "" {
			continue
		}
		if anchor != 0 && entry.anchor == anchor {
			if strings.HasPrefix(asked, answered) {
				return true
			}
			continue
		}
		if asked != answered {
			continue
		}
		if anchor == 0 || entry.anchor == 0 || (overABlankRow && entry.rowBlank) {
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

// visibleStart reports the byte offset in detectionText where the visible grid begins.
func (s *DetectionState) visibleStart() int {
	if s == nil || !s.hasRendered {
		return 0
	}
	return s.renderedAnchors.VisibleStart()
}

// NewDetectionState creates independent state for one agent session.
func NewDetectionState(sessionID, agentID, adapterID string) *DetectionState {
	return &DetectionState{
		SessionID: strings.TrimSpace(sessionID),
		AgentID:   strings.TrimSpace(agentID),
		AdapterID: strings.ToLower(strings.TrimSpace(adapterID)),
		instance:  newInstanceToken(),
	}
}

// newInstanceToken returns a random token for one process instance. It is
// never empty, which would make a replacement's IDs repeat its predecessor's:
// since Go 1.24 crypto/rand.Read does not return an error, it stops the
// program if the system cannot supply randomness.
func newInstanceToken() string {
	var raw [8]byte
	_, _ = rand.Read(raw[:])
	return hex.EncodeToString(raw[:])
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
	// that the question has left the screen. The line goes with both: it is how
	// the state tells this question from the next one.
	s.rememberAnswered(signature, s.pending.Match, s.pending.anchor, s.pending.questionLine)
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

// withdrawPending drops the occurrence the agent has taken back off its screen
// and returns it, so the caller can tell the operator it is gone.
//
// Deliberately not acknowledge(): nobody answered this question. acknowledge
// remembers the occurrence as answered so that a screen still showing it does
// not ask again — exactly the wrong thing here. A withdrawn question that the
// agent paints again IS a new question, and the operator has to be asked.
func (s *DetectionState) withdrawPending() *Event {
	if s == nil || s.pending == nil {
		return nil
	}
	withdrawn := s.pending
	s.pending = nil
	return withdrawn
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
	candidate.ID = s.occurrenceIDFor(candidate.Signature, s.sequence)
	candidate.Timestamp = time.Now().UTC()
	clone := candidate.Clone()
	s.pending = &clone
	return clone.Clone()
}

func (s *DetectionState) appendDetectionText(chunk []byte) (candidateStart, candidateEnd int, ok bool) {
	if s == nil {
		return 0, 0, false
	}
	// A repainting agent's text is the rendered screen, not an accumulation of
	// what it wrote. The screen already applied the carriage returns, the
	// erases and the addressing that this loop approximates, so it replaces the
	// window rather than being appended to it.
	if s.hasRendered {
		if s.rendered == "" {
			return 0, 0, false
		}
		s.detectionText = s.rendered
		return activeLineRange(s.detectionText)
	}
	if len(chunk) == 0 {
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

// occurrenceIDFor is the ID of this process's occurrence number sequence of
// an event with the given signature. Every ID an adapter emits or stores must
// come from here, so that it carries the process instance; see instance.
func (s *DetectionState) occurrenceIDFor(signature string, sequence uint64) string {
	if s == nil || s.instance == "" {
		return occurrenceID(signature, sequence)
	}
	return occurrenceID(signature+"\x00"+s.instance, sequence)
}

// occurrenceID is the unsalted derivation. Only occurrenceIDFor and the
// provisional exit ID of NewProcessExitEvent call it.
func occurrenceID(signature string, sequence uint64) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", signature, sequence)))
	return "evt-" + hex.EncodeToString(digest[:12])
}
