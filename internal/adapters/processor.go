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
)

const maxANSICarrySize = 4 * 1024

// Hooks are invoked synchronously after Processor releases its state lock.
// OnEvent may inspect Processor state; process termination remains the
// responsibility of the owning backend rather than an event callback.
type Hooks struct {
	OnOutput func()
	OnEvent  func(Event)
}

// Processor separates raw transport bytes, normalized detection text and the
// bounded sanitized text rendered by Bubble Tea. Raw bytes are never retained.
type Processor struct {
	adapter Adapter
	state   *DetectionState
	output  *buffer.Buffer
	hooks   Hooks

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
		return nil, errors.New("adaptateur nil")
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
	return &Processor{adapter: adapter, state: state, output: buffer.New(capacity), hooks: hooks}, nil
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
	complete, carry := splitIncompleteANSI(p.ansiCarry + string(chunk))
	if len(carry) > maxANSICarrySize {
		complete += carry
		carry = ""
	}
	p.ansiCarry = carry
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

func (p *Processor) Output() string { return p.output.String() }

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
