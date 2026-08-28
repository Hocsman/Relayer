package adapters

import (
	"context"
	"errors"
	"unicode"
	"unicode/utf8"
)

// MaxLineBytes is the maximum encoded line accepted by the safe line-input
// boundary, before its terminating carriage return is appended.
const MaxLineBytes = 4096

var (
	// ErrEventPending reports that an actionable event must be resolved before
	// direct instructions can be delivered.
	ErrEventPending = errors.New("actionable event pending")
	// ErrInvalidLine reports line input which cannot be encoded unambiguously.
	ErrInvalidLine = errors.New("invalid terminal line")
	// ErrLineUnsupported reports that a transport has no atomic line boundary.
	ErrLineUnsupported = errors.New("line send not supported")
	// ErrLineDeliveryUncertain reports that a transport was called but could
	// not prove whether zero, some, or all line bytes reached the target. The
	// original transport error is deliberately not retained: it may echo the
	// submitted text and must never escape through errors.Unwrap.
	ErrLineDeliveryUncertain = errors.New("line delivery uncertain")
)

// SendLine serializes direct instructions with event detection and process
// termination. It never resolves or acknowledges an event. Delivery happens
// under the same lock as Detect, so either the line is sent first or a prompt
// becomes pending first; the two outcomes cannot race past one another.
func (p *Processor) SendLine(ctx context.Context, line string, deliver func([]byte) error) error {
	if deliver == nil || p == nil {
		return ErrLineUnsupported
	}
	if err := validateLine(line); err != nil {
		return err
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	// Check again after waiting for concurrent detection. In particular, no
	// writer is invoked when cancellation, termination, or a prompt won the
	// race for the processor lock.
	if err := ctx.Err(); err != nil {
		return err
	}
	if p.terminated {
		return ErrProcessorTerminated
	}
	if p.state.pending != nil && p.state.pending.Actionable() {
		return ErrEventPending
	}

	payload := make([]byte, len(line)+1)
	copy(payload, line)
	payload[len(line)] = '\r'
	defer clear(payload)
	if err := deliver(payload); err != nil {
		// A custom writer can echo the submitted line through either Error or an
		// unwrap chain. Collapse every post-call failure to a safe sentinel;
		// presentations treat it as delivery uncertainty and never retry.
		return ErrLineDeliveryUncertain
	}
	return nil
}

func validateLine(line string) error {
	if len(line) > MaxLineBytes || !utf8.ValidString(line) {
		return ErrInvalidLine
	}
	for _, character := range line {
		// A line is application text, never a VT byte channel. Reject every
		// Unicode control character, including CR/LF, NUL, TAB, DEL, C1, ESC
		// and BEL, before constructing transport bytes.
		if unicode.IsControl(character) {
			return ErrInvalidLine
		}
	}
	return nil
}
