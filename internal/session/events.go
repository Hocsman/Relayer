// Package session owns PTY-backed process lifecycles and exposes neutral,
// typed events. It deliberately has no dependency on Bubble Tea.
package session

import (
	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/terminal"
)

// Event is emitted by Manager for asynchronous session state changes.
// The unexported marker keeps the event set closed while allowing consumers to
// use type switches over the exported concrete types.
type Event interface {
	sessionEvent()
}

// OutputAvailable invalidates a consumer's cached output snapshot. The latest
// bounded snapshot is retrieved with Manager.Output.
type OutputAvailable struct {
	SessionID string
}

func (OutputAvailable) sessionEvent() {}

// AdapterEvent carries the single backend-neutral semantic representation
// produced by an adapter or by a real session lifecycle transition.
type AdapterEvent struct {
	Event adapters.Event
}

func (AdapterEvent) sessionEvent() {}

// Exited is retained for source compatibility. Managers now publish a real
// AdapterEvent with type process_exit instead of emitting this legacy value.
type Exited struct {
	SessionID string
	Err       error
}

func (Exited) sessionEvent() {}

// Error reports an unexpected error while reading the PTY.
type Error struct {
	SessionID string
	Err       error
}

func (Error) sessionEvent() {}

// Info remains an alias for compatibility with the original PTY API while
// every backend now shares terminal.Info.
type Info = terminal.Info
