// Package intercept sanitizes terminal output, retains a bounded history, and
// detects interactive prompts that require human input.
package intercept

import (
	"context"
	"errors"
	"fmt"
	"io"
	"regexp"
	"sync"

	"github.com/Hocsman/Relayer/internal/buffer"
	"github.com/acarl005/stripansi"
)

const detectionWindowSize = 16 * 1024

type compiledPattern struct {
	Pattern
	regex *regexp.Regexp
}

// Detection describes a prompt match emitted by an Interceptor.
type Detection struct {
	Pattern     string
	Description string
	Match       string
	Sensitive   bool
}

// Hooks receives lightweight output notifications and prompt detections.
// Hooks run synchronously after the Interceptor releases its lock, so they may
// safely call Output, Acknowledge, or Reblock.
type Hooks struct {
	OnOutput func()
	OnPrompt func(Detection)
}

// Interceptor owns the bounded output history and rolling ANSI-free detection
// window for one terminal stream. Consume is safe to call concurrently with
// Acknowledge, IsBlocked, Output, and Reblock.
type Interceptor struct {
	patterns []compiledPattern
	output   *buffer.Buffer
	hooks    Hooks

	mu         sync.Mutex
	detectTail string
	ansiCarry  string
	blocked    bool
}

// New validates patterns and creates an Interceptor with bounded output.
func New(patterns []Pattern, capacity int, hooks Hooks) (*Interceptor, error) {
	compiled := make([]compiledPattern, 0, len(patterns))
	for _, pattern := range patterns {
		re, err := regexp.Compile(pattern.Expression)
		if err != nil {
			return nil, fmt.Errorf("regex %q invalide: %w", pattern.Name, err)
		}
		compiled = append(compiled, compiledPattern{
			Pattern: pattern,
			regex:   re,
		})
	}

	if hooks.OnOutput == nil {
		hooks.OnOutput = func() {}
	}
	if hooks.OnPrompt == nil {
		hooks.OnPrompt = func(Detection) {}
	}

	return &Interceptor{
		patterns: compiled,
		output:   buffer.New(capacity),
		hooks:    hooks,
	}, nil
}

// Run reads terminal output until EOF, cancellation, or an unexpected read
// error. The blocking read is intended to run in its own goroutine.
func (i *Interceptor) Run(ctx context.Context, reader io.Reader) error {
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
			// A backend may write a private wake byte to unblock a FIFO/PTY
			// during cancellation. Never retain or inspect that byte.
			return nil
		default:
		}
		if count > 0 {
			i.Consume(readBuffer[:count])
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

// Consume sanitizes and retains a terminal-output chunk, then searches the
// rolling clean-text window for the first configured prompt. A partial ANSI
// sequence at the end of a chunk is carried into the next call.
func (i *Interceptor) Consume(chunk []byte) {
	if len(chunk) == 0 {
		return
	}

	i.mu.Lock()
	complete, carry := splitIncompleteANSI(i.ansiCarry + string(chunk))
	if len(carry) > maxANSICarrySize {
		// A malformed OSC/CSI must not hide all later output or grow forever.
		complete += carry
		carry = ""
	}
	i.ansiCarry = carry
	clean := sanitizeTerminalText(stripansi.Strip(complete))

	if clean != "" {
		_, _ = i.output.Write([]byte(clean))
		i.detectTail += clean
		if len(i.detectTail) > detectionWindowSize {
			i.detectTail = i.detectTail[len(i.detectTail)-detectionWindowSize:]
		}
	}

	var detected *Detection
	if !i.blocked && clean != "" {
		for _, pattern := range i.patterns {
			match := pattern.regex.FindString(i.detectTail)
			if match == "" {
				continue
			}
			i.blocked = true
			detected = &Detection{
				Pattern:     pattern.Name,
				Description: pattern.Description,
				Match:       match,
				Sensitive:   pattern.Sensitive || IsSensitiveText(match),
			}
			break
		}
	}
	i.mu.Unlock()

	if clean != "" {
		i.hooks.OnOutput()
	}
	if detected != nil {
		i.hooks.OnPrompt(*detected)
	}
}

// Acknowledge clears the blocked state and old detection tail so a later
// prompt can be detected without retriggering the prompt just answered.
func (i *Interceptor) Acknowledge() {
	i.mu.Lock()
	i.blocked = false
	i.detectTail = ""
	i.mu.Unlock()
}

// IsBlocked reports whether a prompt is currently awaiting acknowledgement.
func (i *Interceptor) IsBlocked() bool {
	i.mu.Lock()
	defer i.mu.Unlock()
	return i.blocked
}

// Output returns an ordered copy of the bounded, sanitized terminal history.
func (i *Interceptor) Output() string {
	return i.output.String()
}

// Reblock restores the blocked state when delivering an answer fails.
func (i *Interceptor) Reblock() {
	i.mu.Lock()
	i.blocked = true
	i.mu.Unlock()
}
