// Package tui implements Relayer's Bubble Tea user interface.
//
// The package deliberately knows nothing about PTYs or process management. A
// Backend supplies the small set of operations needed by the UI, which keeps
// rendering and interaction tests deterministic.
package tui

import (
	"context"
	"os/exec"

	"github.com/Hocsman/Relayer/internal/adapters"
)

// Backend is the process/session boundary consumed by the TUI.
type Backend interface {
	Context() context.Context
	Output(id string) (string, error)
	SendInput(id string, value string) error
	Resize(id string, columns, rows int) error
	BeginShutdown()
}

// DecisionBackend encodes and delivers a manual decision through the adapter
// that produced the exact event occurrence.
type DecisionBackend interface {
	SendDecision(id string, event adapters.Event, manualInput string) error
}

// ContextResizeBackend is an optional non-blocking resize capability used by
// the production adapter. The TUI executes these calls outside Update and can
// cancel a superseded batch. Lightweight test and legacy PTY backends keep the
// synchronous Resize fallback above.
type ContextResizeBackend interface {
	ResizeContext(ctx context.Context, id string, columns, rows int) error
}

// AttachableBackend is an optional capability implemented by backends, such
// as tmux, that can temporarily hand a live session to the user's terminal.
// Resync must restore the pane size and reconcile captured output, lifecycle
// state and prompts before returning.
type AttachableBackend interface {
	Name() string
	AttachCommand(ctx context.Context, id string) (*exec.Cmd, error)
	Resync(ctx context.Context, id string, columns, rows int) error
}

// PromptSnapshotBackend lets the attach callback make backend state
// authoritative over events that may have been queued while Bubble Tea was
// suspended by tea.ExecProcess. Implementations must return cached state and
// must not run an external command from Bubble Tea's Update call.
type EventSnapshotBackend interface {
	PendingEvent(context.Context, string) (*adapters.Event, error)
}

// Pane describes one agent already started by the caller.
type Pane struct {
	ID      string
	Name    string
	Command string
	Backend string
	Adapter string
	Shell   bool
}
