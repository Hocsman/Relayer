// Package terminal defines the process-neutral contract implemented by every
// Relayer terminal backend. It deliberately contains no Bubble Tea, PTY, tmux
// command or interception code.
package terminal

import (
	"context"
	"errors"
	"fmt"
	"os/exec"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
)

// SessionID is stable for the lifetime of one Relayer run.
type SessionID = string

// Size is the usable character-cell area exposed to the child terminal.
type Size struct {
	Columns int
	Rows    int
}

// Normalize returns a size accepted by PTY and tmux implementations.
func (s Size) Normalize() Size {
	return Size{
		Columns: clamp(s.Columns, 1, 65535),
		Rows:    clamp(s.Rows, 1, 65535),
	}
}

// Status describes both process lifecycle and client attachment state.
type Status string

const (
	StatusRunning  Status = "running"
	StatusDetached Status = "detached"
	StatusAttached Status = "attached"
	StatusExited   Status = "exited"
	StatusFailed   Status = "failed"
)

// Info is immutable, display-safe metadata returned after startup. Backend is
// always concrete (pty or tmux), never the auto selector.
type Info struct {
	ID             SessionID
	Name           string
	DisplayCommand string
	Backend        string
	Adapter        string
	Shell          bool
}

// Snapshot reconciles bounded output, process status and a possible prompt
// after asynchronous activity such as returning from an attached tmux client.
type Snapshot struct {
	ID       SessionID
	Status   Status
	Running  bool
	Attached bool
	ExitCode *int
	Output   string
	Pending  *adapters.Event
	Revision uint64
}

var (
	ErrClosed          = errors.New("terminal backend closed")
	ErrSessionNotFound = errors.New("terminal session not found")
	ErrNotAttachable   = errors.New("terminal session not attachable")
	ErrUnavailable     = errors.New("backend terminal indisponible")
	ErrUnsupported     = errors.New("terminal backend not supported")
	// These aliases preserve one errors.Is identity from the Processor through
	// session, backend, router and presentation boundaries.
	ErrEventPending          = adapters.ErrEventPending
	ErrInvalidLine           = adapters.ErrInvalidLine
	ErrLineUnsupported       = adapters.ErrLineUnsupported
	ErrLineDeliveryUncertain = adapters.ErrLineDeliveryUncertain
)

// MaxLineBytes is the maximum line size before the core appends one carriage
// return. It aliases the Processor limit so transports cannot drift.
const MaxLineBytes = adapters.MaxLineBytes

// OperationError adds safe context to a backend failure without requiring an
// implementation to expose command arguments or environment values.
type OperationError struct {
	Backend   string
	Operation string
	SessionID SessionID
	Err       error
}

func (e *OperationError) Error() string {
	message := e.Backend + " " + e.Operation
	if e.SessionID != "" {
		message += " (session " + e.SessionID + ")"
	}
	return fmt.Sprintf("%s: %v", message, e.Err)
}

func (e *OperationError) Unwrap() error { return e.Err }

// Backend is the authoritative terminal boundary used by the application.
// Implementations own their processes and resources; all potentially blocking
// operations accept a context. Send transmits data exactly as supplied.
type Backend interface {
	Name() string
	Start(context.Context, agent.Spec, Size) (Info, error)
	Send(context.Context, SessionID, []byte) error
	Resize(context.Context, SessionID, Size) error
	Snapshot(context.Context, SessionID) (Snapshot, error)
	AttachCommand(context.Context, SessionID) (*exec.Cmd, error)
	Stop(context.Context, SessionID) error
	Close(context.Context) error
}

// EventSender atomically delivers a decision for the exact pending event.
// Implementations acknowledge eventID only after data is written successfully.
type EventSender interface {
	SendEvent(context.Context, SessionID, string, []byte) error
}

// LineSender is the optional, atomic ordinary-input boundary. Implementations
// must reject input while an actionable event is pending and must append the
// line terminator in the core rather than accepting pre-encoded raw bytes.
type LineSender interface {
	SendLine(context.Context, SessionID, string) error
}

// PendingEventProvider returns only cached semantic state. Implementations must
// not query a process or spawn an external command; Bubble Tea uses this path
// while reducing an already-delivered event.
type PendingEventProvider interface {
	PendingEvent(context.Context, SessionID) (*adapters.Event, error)
}

func clamp(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
