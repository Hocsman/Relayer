// Package tui implements Relayer's Bubble Tea user interface.
//
// The package deliberately knows nothing about PTYs or process management. A
// Backend supplies the small set of operations needed by the UI, which keeps
// rendering and interaction tests deterministic.
package tui

import "context"

// Backend is the process/session boundary consumed by the TUI.
type Backend interface {
	Context() context.Context
	Output(id int) (string, error)
	SendInput(id int, value string) error
	Resize(id, columns, rows int) error
	BeginShutdown()
}

// Pane describes one agent already started by the caller.
type Pane struct {
	ID      int
	Name    string
	Command string
}
