// Package intercept preserves the historical regex-interceptor API as a thin
// compatibility facade over the backend-neutral adapters package.
package intercept

import (
	"context"
	"io"
	"sync"

	"github.com/Hocsman/Relayer/internal/adapters"
)

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
	processor *adapters.Processor

	mu            sync.Mutex
	lastDetection *adapters.Event
	legacyReblock bool
	// ansiCarry is retained only for source-compatible package tests. ANSI
	// carry ownership moved to adapters.Processor and remains independently
	// bounded there.
	ansiCarry string
}

// New validates patterns and creates an Interceptor with bounded output.
func New(patterns []Pattern, capacity int, hooks Hooks) (*Interceptor, error) {
	if hooks.OnOutput == nil {
		hooks.OnOutput = func() {}
	}
	if hooks.OnPrompt == nil {
		hooks.OnPrompt = func(Detection) {}
	}

	adapter, err := adapters.NewGenericRegexAdapter(patterns)
	if err != nil {
		return nil, err
	}
	result := &Interceptor{}
	processor, err := adapters.NewProcessor(
		adapter,
		adapters.NewDetectionState("legacy", "legacy", adapters.GenericID),
		capacity,
		adapters.Hooks{
			OnOutput: hooks.OnOutput,
			OnEvent: func(event adapters.Event) {
				result.mu.Lock()
				copy := event.Clone()
				result.lastDetection = &copy
				result.mu.Unlock()
				hooks.OnPrompt(Detection{
					Pattern:     event.Metadata["pattern"],
					Description: event.Summary,
					Match:       event.Match,
					Sensitive:   event.Sensitive,
				})
			},
		},
	)
	if err != nil {
		return nil, err
	}
	result.processor = processor
	return result, nil
}

// Run reads terminal output until EOF, cancellation, or an unexpected read
// error. The blocking read is intended to run in its own goroutine.
func (i *Interceptor) Run(ctx context.Context, reader io.Reader) error {
	return i.processor.Run(ctx, reader)
}

// Consume sanitizes and retains a terminal-output chunk, then searches the
// rolling clean-text window for the first configured prompt. A partial ANSI
// sequence at the end of a chunk is carried into the next call.
func (i *Interceptor) Consume(chunk []byte) {
	_ = i.processor.Consume(chunk)
}

// Acknowledge clears the blocked state and old detection tail so a later
// prompt can be detected without retriggering the prompt just answered.
func (i *Interceptor) Acknowledge() {
	i.mu.Lock()
	i.legacyReblock = false
	i.mu.Unlock()
	_ = i.processor.Acknowledge("")
}

// IsBlocked reports whether a prompt is currently awaiting acknowledgement.
func (i *Interceptor) IsBlocked() bool {
	i.mu.Lock()
	legacy := i.legacyReblock
	i.mu.Unlock()
	return legacy || i.processor.IsBlocked()
}

// Output returns an ordered copy of the bounded, sanitized terminal history.
func (i *Interceptor) Output() string {
	return i.processor.Output()
}

// Reblock restores the blocked state when delivering an answer fails.
func (i *Interceptor) Reblock() {
	i.mu.Lock()
	last := i.lastDetection
	if last == nil {
		i.legacyReblock = true
	}
	i.mu.Unlock()
	if last != nil {
		_ = i.processor.Restore(last.Clone())
	}
}
