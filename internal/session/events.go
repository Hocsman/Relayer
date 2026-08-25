// Package session owns PTY-backed process lifecycles and exposes neutral,
// typed events. It deliberately has no dependency on Bubble Tea.
package session

// Event is emitted by Manager for asynchronous session state changes.
// The unexported marker keeps the event set closed while allowing consumers to
// use type switches over the exported concrete types.
type Event interface {
	sessionEvent()
}

// OutputAvailable invalidates a consumer's cached output snapshot. The latest
// bounded snapshot is retrieved with Manager.Output.
type OutputAvailable struct {
	SessionID int
}

func (OutputAvailable) sessionEvent() {}

// PromptDetected reports an interactive prompt that requires human input.
type PromptDetected struct {
	SessionID   int
	Pattern     string
	Description string
	Match       string
	Sensitive   bool
}

func (PromptDetected) sessionEvent() {}

// Exited reports the result of the single Wait call for a session process.
type Exited struct {
	SessionID int
	Err       error
}

func (Exited) sessionEvent() {}

// Error reports an unexpected error while reading the PTY.
type Error struct {
	SessionID int
	Err       error
}

func (Error) sessionEvent() {}

// Info is the immutable session metadata needed by higher layers.
type Info struct {
	ID      int
	Name    string
	Command string
}
