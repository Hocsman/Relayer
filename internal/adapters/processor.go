package adapters

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"sync"

	"github.com/Hocsman/Relayer/internal/buffer"
	"github.com/acarl005/stripansi"

	"github.com/Hocsman/Relayer/internal/screen"
)

const maxANSICarrySize = 4 * 1024

// screenAnchor ties an occurrence to the row its question was last seen painted
// on, and to the state of the grid at that moment. An anchor whose eventID no
// longer matches the pending occurrence is stale by construction, so nothing
// has to invalidate it.
type screenAnchor struct {
	eventID string
	evicted uint64
	row     int
	line    string
}

// Hooks are invoked synchronously after Processor releases its state lock.
// OnEvent may inspect Processor state; process termination remains the
// responsibility of the owning backend rather than an event callback.
type Hooks struct {
	OnOutput func()
	OnEvent  func(Event)
	// OnEventWithdrawn reports an occurrence the agent has taken back off its
	// screen before anyone decided on it. It is not a decision and it is not a
	// failure: the question simply stopped being asked, and whatever is showing
	// it to the operator has to stop showing it. A caller that ignores this
	// leaves a card an operator can still click, and the click is refused with
	// ErrEventMismatch rather than delivered.
	OnEventWithdrawn func(Event)
}

// Processor separates raw transport bytes, normalized detection text and the
// bounded sanitized text rendered by Bubble Tea. Raw bytes are never retained.
type Processor struct {
	adapter Adapter
	state   *DetectionState
	output  *buffer.Buffer
	hooks   Hooks

	// screen renders what the agent actually painted. The output buffer above
	// appends, which is exact for an agent that only prints and wrong for one
	// that repaints: every redraw is appended again, so the pane an operator
	// watches fills with copies of the same frame. Output() reads the screen
	// instead, but only once the agent has done something an appended stream
	// cannot express — so an append-only agent keeps today's bytes exactly.
	screen *screen.Screen

	// pendingAnchor is the screen the pending occurrence was last SEEN on.
	//
	// It lives on the Processor rather than on DetectionState because it
	// describes the rendering substrate, not the detection: the Codex adapter
	// probes by copying the state (`vendorProbe := *state`), and a speculative
	// probe has no business carrying what the real screen showed.
	pendingAnchor screenAnchor

	mu                         sync.Mutex
	semanticHooks              sync.WaitGroup
	ansiCarry                  string
	lastSnapshotFingerprint    string
	pendingSnapshotFingerprint string
	terminated                 bool
	terminalEvent              *Event
}

// snapshotFingerprintSource is implemented by adapters whose verified prompt
// structure spans more than the active line. Generic regex detection keeps the
// historical active-line fingerprint to avoid replay from unrelated history.
type snapshotFingerprintSource interface {
	snapshotFingerprintSource(normalized, active string, inCodeFence bool) string
}

// snapshotOccurrenceClassifier limits fingerprint-based occurrence rollover
// to fixture-backed vendor events. A configured regex fallback keeps the exact
// GenericRegexAdapter compatibility contract even when wrapped by a vendor
// adapter.
type snapshotOccurrenceClassifier interface {
	snapshotOccurrenceAware(Event) bool
}

func NewProcessor(adapter Adapter, state *DetectionState, capacity int, hooks Hooks) (*Processor, error) {
	if adapter == nil {
		return nil, errors.New("nil adapter")
	}
	if state == nil {
		return nil, errors.New("nil detection state")
	}
	if state.AdapterID == "" {
		state.AdapterID = adapter.ID()
	}
	if !strings.EqualFold(state.AdapterID, adapter.ID()) {
		return nil, fmt.Errorf("state %q incompatible with adapter %q", state.AdapterID, adapter.ID())
	}
	if hooks.OnOutput == nil {
		hooks.OnOutput = func() {}
	}
	if hooks.OnEvent == nil {
		hooks.OnEvent = func(Event) {}
	}
	if hooks.OnEventWithdrawn == nil {
		hooks.OnEventWithdrawn = func(Event) {}
	}
	return &Processor{
		adapter: adapter,
		state:   state,
		output:  buffer.New(capacity),
		hooks:   hooks,
		// The grid starts at a default until a caller reports the terminal it
		// is actually attached to. It has to exist from the first byte: an
		// agent can repaint before anything has measured its window.
		screen: screen.New(0, 0),
	}, nil
}

func (p *Processor) Run(ctx context.Context, reader io.Reader) error {
	readBuffer := make([]byte, 4096)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		count, err := reader.Read(readBuffer)
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if count > 0 {
			if consumeErr := p.Consume(readBuffer[:count]); consumeErr != nil {
				return consumeErr
			}
		}
		if err != nil {
			if errors.Is(err, io.EOF) {
				return nil
			}
			select {
			case <-ctx.Done():
				return nil
			default:
				return err
			}
		}
	}
}

// Consume accepts raw terminal bytes, strips fragmented ANSI sequences and
// sends only normalized text to the adapter.
func (p *Processor) Consume(chunk []byte) error {
	if len(chunk) == 0 {
		return nil
	}
	p.mu.Lock()
	var (
		withdrawn *Event
		visible   string
		onGrid    bool
	)
	complete, carry := splitIncompleteANSI(p.ansiCarry + string(chunk))
	if len(carry) > maxANSICarrySize {
		complete += carry
		carry = ""
	}
	p.ansiCarry = carry
	// The screen wants the escape sequences the rest of this path throws away.
	if p.screen != nil {
		_, _ = p.screen.Write([]byte(complete))
		if p.screen.Repainted() {
			// Only once the agent has done something an appended stream cannot
			// express. Until then the byte window is exact, and swapping the
			// substrate would change behaviour for every agent to fix one.
			rendered, burst := p.screen.TextAndBurst()
			// Once the question has left the SCREEN, forget that it was
			// answered: the same question asked again is a new question, and
			// remembering forever would turn this guard into silence.
			//
			// Deliberately the visible grid rather than `rendered`, which
			// carries up to 512 rows of scrollback: a question that scrolled
			// out of sight is still findable there, so testing against it left
			// the memory in place for hundreds of lines and swallowed every
			// re-ask in between.
			// Bound to the row, not the text. A row that scrolled away or that
			// the agent rewrote no longer carries the question that was
			// answered, whatever else the screen happens to show.
			visible = p.screen.VisibleText()
			onGrid = true
			live := make([]answeredQuestion, 0, len(p.state.answered))
			for _, entry := range p.state.pendingAnswers() {
				if entry.rowKnown {
					if p.screen.RowStillShows(entry.row, entry.match) {
						live = append(live, entry)
					}
					continue
				}
				if entry.match != "" && strings.Contains(visible, entry.match) {
					live = append(live, entry)
				}
			}
			p.state.keepAnswered(live)
			p.state.UseRenderedScreen(rendered, burst, fenceParity(rendered))
			withdrawn = p.reconcilePendingWithScreen()
		}
		p.screen.ClearDirty()
	}
	ansiFree := stripansi.Strip(expandCursorForward(complete))
	detection := normalizeDetectionText(ansiFree)
	rendered := normalizeRenderedText(ansiFree)
	if rendered != "" {
		_, _ = p.output.Write([]byte(rendered))
	}
	var (
		events []Event
		err    error
	)
	if !p.terminated {
		events, err = p.adapter.Detect(p.state, []byte(detection))
	}
	if onGrid {
		// The occurrence this write raised did not exist yet when the
		// reconciliation ran above, so it is anchored here or it would only
		// become withdrawable after a redraw that happened to keep it.
		p.sightPendingOnScreen()
	}
	if err == nil && len(events) > 0 {
		fingerprint := p.snapshotFingerprint(p.state.detectionText)
		p.lastSnapshotFingerprint = fingerprint
		p.pendingSnapshotFingerprint = fingerprint
		// Add is serialized with termination by p.mu. Once terminated is set,
		// no future Consume can add work, so NewProcessExitEvent may safely
		// wait for every earlier semantic hook before returning.
		p.semanticHooks.Add(len(events))
	}
	p.mu.Unlock()
	if err != nil {
		return err
	}
	if rendered != "" {
		p.hooks.OnOutput()
	}
	if withdrawn != nil {
		p.hooks.OnEventWithdrawn(withdrawn.Clone())
	}
	for _, event := range events {
		func() {
			defer p.semanticHooks.Done()
			p.hooks.OnEvent(event.Clone())
		}()
	}
	return nil
}

// ReconcileSnapshot uses the last active logical line for generic detection.
// A vendor adapter may additionally fingerprint only its verified prompt
// block so a resize remains idempotent while a successive prompt discovered
// after an attach receives a fresh occurrence ID. An unchanged snapshot after
// a successful acknowledgement cannot resurrect the old event.
func (p *Processor) ReconcileSnapshot(raw []byte) (*Event, bool, error) {
	if len(raw) > detectionWindowSize {
		raw = raw[len(raw)-detectionWindowSize:]
	}
	ansiFree := stripansi.Strip(expandCursorForward(string(raw)))
	normalized := normalizeDetectionText(ansiFree)
	active, _ := snapshotActiveLine(normalized)
	fingerprint := p.snapshotFingerprint(normalized)

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminated {
		return nil, false, nil
	}
	if fingerprint == p.lastSnapshotFingerprint {
		return p.state.Pending(), false, nil
	}
	p.lastSnapshotFingerprint = fingerprint

	if active == "" {
		changed := p.state.pending != nil
		_, _ = p.state.acknowledge("")
		// The screen is empty, so the retained text no longer describes it.
		p.state.resetWindow()
		p.pendingSnapshotFingerprint = ""
		return nil, changed, nil
	}
	probeState := NewDetectionState(p.state.SessionID, p.state.AgentID, p.adapter.ID())
	candidates, err := p.adapter.Detect(probeState, []byte(normalized))
	if err != nil {
		return nil, false, err
	}
	if len(candidates) == 0 {
		changed := p.state.pending != nil
		_, _ = p.state.acknowledge("")
		// The snapshot is authoritative about the screen: nothing detectable is
		// on it, so the retained text is stale.
		p.state.resetWindow()
		p.pendingSnapshotFingerprint = ""
		return nil, changed, nil
	}
	candidate := candidates[0]
	if p.state.pending != nil && p.state.pending.Signature == candidate.Signature {
		occurrenceAware := false
		if classifier, ok := p.adapter.(snapshotOccurrenceClassifier); ok {
			occurrenceAware = classifier.snapshotOccurrenceAware(candidate)
		}
		if !occurrenceAware || p.pendingSnapshotFingerprint == "" || p.pendingSnapshotFingerprint == fingerprint {
			p.pendingSnapshotFingerprint = fingerprint
			return p.state.Pending(), false, nil
		}
	}
	resolved := p.state.replacePending(candidate)
	p.pendingSnapshotFingerprint = fingerprint
	return &resolved, true, nil
}

func (p *Processor) snapshotFingerprint(normalized string) string {
	active, inCodeFence := snapshotActiveLine(normalized)
	fingerprintSource := active
	if adapter, ok := p.adapter.(snapshotFingerprintSource); ok {
		fingerprintSource = adapter.snapshotFingerprintSource(normalized, active, inCodeFence)
	}
	return textFingerprint(fmt.Sprintf("%t\x00%s", inCodeFence, fingerprintSource))
}

func (p *Processor) Acknowledge(eventID string) error {
	p.mu.Lock()
	signature, err := p.state.acknowledge(eventID)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	p.pendingSnapshotFingerprint = ""
	if p.screen != nil {
		if remembered := p.state.pendingAnswers(); len(remembered) > 0 {
			newest := remembered[len(remembered)-1]
			if row, found := p.screen.LocateRow(newest.match); found {
				p.state.bindAnsweredRow(row)
			}
		}
	}
	recovered := p.rescanRetainedWindow(signature)
	p.mu.Unlock()
	p.emitSemantic(recovered)
	return nil
}

// rescanRetainedWindow reports a prompt that arrived while another occurrence
// was pending and was therefore never examined.
//
// Detect returns early while an occurrence is pending, so output consumed
// during that window reached the retained text and nothing else. Answering the
// first prompt is the moment that text becomes examinable again.
//
// The caller holds p.mu; the returned events must be emitted after releasing
// it, exactly as Consume does.
func (p *Processor) rescanRetainedWindow(resolved string) []Event {
	if p.terminated || p.state.pending != nil || p.state.detectionText == "" {
		return nil
	}

	// The window is only worth keeping while it holds an unexamined prompt.
	// Otherwise drop it, because answered text that stays behind merges with
	// whatever arrives next: output without a trailing newline would extend the
	// same active line, and a pattern could then match across the boundary and
	// report the old prompt instead of the new one.
	keep := false
	defer func() {
		if !keep {
			p.state.resetWindow()
		}
	}()

	probe := NewDetectionState(p.state.SessionID, p.state.AgentID, p.adapter.ID())
	// The probe must know what has already been answered, or it re-reports every
	// still-painted question the operator has dealt with — which is exactly what
	// resolving a second question did to the first.
	probe.answered = p.state.pendingAnswers()
	probe.hasRendered = p.state.hasRendered
	probe.rendered = p.state.rendered
	candidates, err := p.adapter.Detect(probe, []byte(p.state.detectionText))
	if err != nil || len(candidates) == 0 {
		return nil
	}
	candidate := candidates[0]
	// The occurrence just answered is still in the window. Re-reporting it
	// would reblock the session on a decision the operator already made.
	if resolved != "" && candidate.Signature == resolved {
		return nil
	}
	if !candidate.Actionable() {
		return nil
	}

	keep = true
	event := p.state.replacePending(candidate)
	fingerprint := p.snapshotFingerprint(p.state.detectionText)
	p.lastSnapshotFingerprint = fingerprint
	p.pendingSnapshotFingerprint = fingerprint
	p.semanticHooks.Add(1)
	return []Event{event}
}

// emitSemantic publishes events outside p.mu and balances semanticHooks.
func (p *Processor) emitSemantic(events []Event) {
	for _, event := range events {
		func() {
			defer p.semanticHooks.Done()
			p.hooks.OnEvent(event.Clone())
		}()
	}
}

func (p *Processor) Restore(event Event) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.terminated {
		return ErrProcessorTerminated
	}
	if err := p.state.restore(event); err != nil {
		return err
	}
	p.lastSnapshotFingerprint = ""
	p.pendingSnapshotFingerprint = ""
	return nil
}

// Resolve serializes a decision with detection. The pending occurrence is
// cleared only after delivery succeeds; terminal output arriving concurrently
// is processed afterwards, so an immediate second prompt cannot be overwritten
// by rollback of the first one.
func (p *Processor) Resolve(eventID string, deliver func() error) error {
	if deliver == nil {
		return errors.New("nil decision delivery")
	}
	p.mu.Lock()
	if p.terminated {
		p.mu.Unlock()
		return ErrProcessorTerminated
	}
	if p.state.pending == nil && eventID != "" {
		p.mu.Unlock()
		return fmt.Errorf("%w: no pending event for %q", ErrEventMismatch, eventID)
	}
	if p.state.pending != nil && eventID != "" && p.state.pending.ID != eventID {
		p.mu.Unlock()
		return fmt.Errorf("%w: got %q, want %q", ErrEventMismatch, eventID, p.state.pending.ID)
	}
	if err := deliver(); err != nil {
		p.mu.Unlock()
		return err
	}
	signature, err := p.state.acknowledge(eventID)
	if err != nil {
		p.mu.Unlock()
		return err
	}
	p.pendingSnapshotFingerprint = ""
	if p.screen != nil {
		if remembered := p.state.pendingAnswers(); len(remembered) > 0 {
			newest := remembered[len(remembered)-1]
			if row, found := p.screen.LocateRow(newest.match); found {
				p.state.bindAnsweredRow(row)
			}
		}
	}
	recovered := p.rescanRetainedWindow(signature)
	p.mu.Unlock()
	p.emitSemantic(recovered)
	return nil
}

func (p *Processor) Pending() *Event {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.Pending()
}

func (p *Processor) IsBlocked() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.IsBlocked()
}

// Output returns what the operator should see.
//
// For an agent that repaints, that is the rendered screen: the appended buffer
// holds every redraw stacked on top of the last, which is not what is on the
// agent's terminal. For every other agent the appended buffer is exact and is
// returned unchanged, so nothing that worked before changes.
// Resize tells the rendered screen the size of the terminal the agent is
// attached to. Until it is called the screen uses a default, which is wrong for
// wrapping but never wrong about what was erased.
//
// The size crosses as plain integers rather than a terminal.Size: internal
// terminal imports this package, so the dependency cannot run the other way.
func (p *Processor) Resize(columns, rows int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.screen != nil {
		p.screen.Resize(columns, rows)
	}
}

func (p *Processor) Output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.screen != nil && p.screen.Repainted() {
		return p.screen.Text()
	}
	return p.output.String()
}

func (p *Processor) DetectionWindowLen() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.state.detectionText)
}

// Revision returns the latest semantic occurrence sequence for snapshots.
func (p *Processor) Revision() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.state.sequence
}

// MarkProcessExitEvent atomically terminates any pending interaction and
// reserves the next occurrence sequence under the same lock used by Detect
// and SendLine. It deliberately does not wait for earlier semantic hooks: a
// process owner can mark the transport closed immediately after Wait, perform
// bounded descendant cleanup, then call WaitSemanticEvents before publishing
// the returned process_exit event.
func (p *Processor) MarkProcessExitEvent(exitCode *int, failed bool) Event {
	p.mu.Lock()
	if p.terminalEvent != nil {
		event := p.terminalEvent.Clone()
		p.mu.Unlock()
		return event
	}
	_, _ = p.state.acknowledge("")
	p.state.resetWindow()
	p.pendingSnapshotFingerprint = ""
	p.state.sequence++
	sequence := p.state.sequence
	sessionID := p.state.SessionID
	agentID := p.state.AgentID
	adapterID := p.adapter.ID()
	p.terminated = true
	event := NewProcessExitEvent(sessionID, agentID, adapterID, sequence, exitCode, failed)
	stored := event.Clone()
	p.terminalEvent = &stored
	p.mu.Unlock()
	return event
}

// WaitSemanticEvents waits until every actionable event reserved before
// process termination has reached its hook. It must stay outside p.mu because
// hooks are allowed to inspect Processor state.
func (p *Processor) WaitSemanticEvents() {
	if p == nil {
		return
	}
	p.semanticHooks.Wait()
}

// NewProcessExitEvent marks the processor terminated, then preserves the
// historical ordering guarantee that every earlier semantic hook completes
// before the process_exit event is returned to its caller.
func (p *Processor) NewProcessExitEvent(exitCode *int, failed bool) Event {
	event := p.MarkProcessExitEvent(exitCode, failed)
	// Every actionable detected before termination is delivered first. Hooks
	// remain outside p.mu, so they may inspect Processor state safely.
	p.WaitSemanticEvents()
	return event
}

func normalizeDetectionText(input string) string {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	return strings.Map(func(character rune) rune {
		if character == '\n' || character == '\r' || character == '\t' ||
			(character >= 0x20 && (character < 0x7f || character >= 0xa0)) {
			return character
		}
		return -1
	}, input)
}

func normalizeRenderedText(input string) string {
	input = strings.ReplaceAll(input, "\r\n", "\n")
	input = strings.ReplaceAll(input, "\r", "\n")
	return strings.Map(func(character rune) rune {
		if character == '\n' || character == '\t' ||
			(character >= 0x20 && (character < 0x7f || character >= 0xa0)) {
			return character
		}
		return -1
	}, input)
}

func snapshotActiveLine(text string) (string, bool) {
	text = strings.TrimRight(text, "\r\n")
	if text == "" {
		return "", false
	}
	start := strings.LastIndexAny(text, "\r\n") + 1
	completedEnd := strings.LastIndexByte(text[:start], '\n') + 1
	inCodeFence := false
	for _, line := range strings.Split(text[:completedEnd], "\n") {
		if strings.HasPrefix(strings.TrimSpace(strings.TrimSuffix(line, "\r")), "```") {
			inCodeFence = !inCodeFence
		}
	}
	return strings.TrimSpace(text[start:]), inCodeFence
}

func textFingerprint(value string) string {
	digest := sha256.Sum256([]byte(strings.ToLower(strings.Join(strings.Fields(value), " "))))
	return hex.EncodeToString(digest[:16])
}

func splitIncompleteANSI(input string) (complete string, carry string) {
	for offset := 0; offset < len(input); {
		if input[offset] != 0x1b {
			offset++
			continue
		}
		end, ok := ansiSequenceEnd(input, offset)
		if !ok {
			return input[:offset], input[offset:]
		}
		offset = end
	}
	return input, ""
}

func ansiSequenceEnd(input string, start int) (int, bool) {
	if start+1 >= len(input) {
		return 0, false
	}
	switch input[start+1] {
	case '[':
		for index := start + 2; index < len(input); index++ {
			if input[index] >= 0x40 && input[index] <= 0x7e {
				return index + 1, true
			}
		}
		return 0, false
	case ']':
		for index := start + 2; index < len(input); index++ {
			if input[index] == 0x07 {
				return index + 1, true
			}
			if input[index] == 0x1b {
				if index+1 >= len(input) {
					return 0, false
				}
				if input[index+1] == '\\' {
					return index + 2, true
				}
			}
		}
		return 0, false
	default:
		return start + 2, true
	}
}

// cursorForwardPattern matches CUF (cursor forward), the escape a program uses
// to move right instead of writing spaces.
var cursorForwardPattern = regexp.MustCompile(`\x1b\[([0-9]*)C`)

// maxCursorForwardExpansion bounds one substitution. Terminal output is
// untrusted, and the parameter is caller-controlled: without a bound, a single
// short sequence could inflate the detection window arbitrarily.
const maxCursorForwardExpansion = 256

// expandCursorForward replaces a cursor-forward escape with the spaces it
// visually produces, before the ANSI stripper deletes it outright.
//
// Some agents lay out a prompt by moving the cursor rather than emitting
// spaces. Claude Code 2.1.59 does: its recorded prompts contain no literal
// space at all, only ESC[1C between words. Stripping those without
// substitution leaves the detector matching against
// "DoyouwanttousethisAPIkey?", so any configured pattern containing a space
// can never fire - including the shipped defaults. Words survive, spacing does
// not, and nothing reports the difference.
//
// Only horizontal movement is modelled. Absolute positioning and vertical
// movement need a screen model, which this package deliberately does not have.
func expandCursorForward(value string) string {
	if !strings.Contains(value, "\x1b[") {
		return value
	}
	return cursorForwardPattern.ReplaceAllStringFunc(value, func(match string) string {
		digits := match[2 : len(match)-1]
		count := 1
		if digits != "" {
			parsed, err := strconv.Atoi(digits)
			if err != nil {
				return match
			}
			count = parsed
		}
		if count <= 0 {
			return ""
		}
		if count > maxCursorForwardExpansion {
			count = maxCursorForwardExpansion
		}
		return strings.Repeat(" ", count)
	})
}

// reconcilePendingWithScreen withdraws the occurrence the agent has taken back.
//
// Consume keeps the grid current on every write, but until now the only thing
// it reconciled against that grid was the answered memory. A pending occurrence
// was never compared to the screen at all, so a question the agent withdrew by
// itself — a timeout, a cancel, an ESC typed in the attached view — stayed on
// offer indefinitely. Resolve checks the event ID and the process, not the
// screen, so the decision was then delivered into a terminal that had gone back
// to its prompt: a `y` typed into a shell.
//
// The whole difficulty is that the question's TEXT being absent proves nothing.
// It is absent while the agent is halfway through repainting the frame that
// will show it again — a full-screen frame is larger than one 4 KiB PTY read,
// so the grid is routinely observed between the erase and the redraw. It is
// absent when the question merely scrolled out of view while the agent is still
// waiting. It is even absent when the same characters are still on screen but
// the grid stopped joining two wrapped rows into one logical line. Withdrawing
// on any of those stops the operator being asked about a live question and
// leaves the agent waiting forever — the worse of the two failures, and the one
// the screen substrate exists to prevent.
//
// So absence is never the evidence. REPLACEMENT is. The occurrence is withdrawn
// only when all of the following hold:
//
//  1. the question was SEEN on the visible grid under this occurrence's ID, so
//     there is a row to talk about. An occurrence raised on the byte window
//     carries text this grid never rendered; never seen means never withdrawn.
//  2. nothing has LEFT the grid since that sighting, which Screen.Evicted
//     reports. Equal counts mean no line scrolled, moved or was dropped, so the
//     remembered row still designates the same place.
//  3. that row now carries content that is not blank — a blank row is a frame
//     in progress, not an answer that stopped being wanted.
//  4. and that content is neither the question nor a re-serialisation of the
//     line that carried it. The same characters split differently across rows
//     are the same question, still being asked.
//
// What that leaves uncovered is written down in docs/adapters.md rather than
// guessed at: an agent that erases its question and then paints nothing at all
// keeps its occurrence, and so does one that takes the question back with a
// gesture that moves lines.
//
// The caller holds p.mu; the returned occurrence must be published after
// releasing it, exactly as Consume does with events.
func (p *Processor) reconcilePendingWithScreen() *Event {
	if p.state == nil || p.state.pending == nil || p.screen == nil {
		return nil
	}
	if p.state.pending.Match == "" {
		return nil
	}
	p.sightPendingOnScreen()
	if p.pendingAnchor.eventID != p.state.pending.ID {
		return nil
	}
	if p.pendingAnchor.evicted != p.screen.Evicted() {
		return nil
	}
	line := p.screen.VisibleRowLine(p.pendingAnchor.row)
	if strings.TrimSpace(line) == "" {
		return nil
	}
	if strings.Contains(line, p.state.pending.Match) {
		return nil
	}
	if strings.HasPrefix(p.pendingAnchor.line, line) || strings.HasPrefix(line, p.pendingAnchor.line) {
		// The row holds the same content, serialised differently. A wrapped
		// line the agent repainted row by row stops being joined without a
		// single character changing on screen.
		return nil
	}
	// The snapshot fingerprint described the screen that carried the question.
	p.pendingSnapshotFingerprint = ""
	p.pendingAnchor = screenAnchor{}
	return p.state.withdrawPending()
}

// sightPendingOnScreen anchors the occurrence now awaiting a decision to the
// row it is painted on, if it is painted at this moment.
//
// The anchor is refreshed on every sighting rather than kept from the first: a
// question the agent keeps repainting is still being asked, and each repaint is
// a fresh proof of that — including after a scroll, which is how an occurrence
// becomes withdrawable again once it is back in view.
//
// The caller holds p.mu.
func (p *Processor) sightPendingOnScreen() {
	if p.state == nil || p.state.pending == nil || p.screen == nil {
		return
	}
	match := p.state.pending.Match
	if match == "" {
		return
	}
	row, line, found := p.screen.VisibleRowOf(match)
	if !found {
		return
	}
	p.pendingAnchor = screenAnchor{
		eventID: p.state.pending.ID,
		evicted: p.screen.Evicted(),
		row:     row,
		line:    line,
	}
}

// fenceParity reports whether the end of the text sits inside a markdown code
// fence. The byte-window path maintains this incrementally as it appends; a
// rendered screen has no history to accumulate, so it is recomputed from what
// is currently on screen.
func fenceParity(text string) bool {
	inFence := false
	for _, line := range strings.Split(text, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), string(codeFenceMarker)) {
			inFence = !inFence
		}
	}
	return inFence
}
