// Package tmuxbackend owns Relayer-created tmux sessions.
//
// It deliberately exposes process-neutral operations to callers: tmux command
// construction, ownership tracking and output transport stay inside this
// package. User commands are launched by Relayer's helper from a private JSON
// specification; they are never concatenated into a tmux shell command.
package tmuxbackend

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"time"

	"github.com/Hocsman/Relayer/internal/terminal"
)

var (
	// ErrTmuxNotFound identifies an unavailable requested tmux executable.
	ErrTmuxNotFound = errors.New("tmux introuvable")
	// Aliases keep errors recognizable through the neutral terminal contract.
	ErrSessionNotFound = terminal.ErrSessionNotFound
	ErrClosed          = terminal.ErrClosed
	ErrUnsupported     = terminal.ErrUnsupported
	// ErrStopUncertain means tmux accepted a termination attempt but Relayer
	// could not confirm that the immutable owned session ID disappeared.
	ErrStopUncertain = errors.New("arrêt de la session tmux non confirmé")
)

type Status = terminal.Status
type Size = terminal.Size
type Snapshot = terminal.Snapshot

const (
	StatusRunning  = terminal.StatusRunning
	StatusDetached = terminal.StatusDetached
	StatusAttached = terminal.StatusAttached
	StatusExited   = terminal.StatusExited
	StatusFailed   = terminal.StatusFailed
)

// ExitError preserves a non-zero pane status in session.Exited events.
type ExitError struct {
	Code int
}

func (e *ExitError) Error() string {
	return "le processus tmux s'est terminé avec le code " + strconv.Itoa(e.Code)
}

// Options controls tmux command execution, retention and private runtime data.
// Zero values select conservative defaults.
type Options struct {
	Runner           CommandRunner
	TmuxPath         string
	HelperPath       string
	RuntimeDir       string
	RunID            string
	PersistOnExit    bool
	CleanupOnSuccess bool
	PollInterval     time.Duration
	CaptureLimit     int
}

// InteractiveBackend is the optional capability consumed by the Bubble Tea
// layer when Enter temporarily hands the real terminal to tmux.
type InteractiveBackend interface {
	Name() string
	AttachCommand(context.Context, string) (*exec.Cmd, error)
	Resync(context.Context, string, int, int) error
}
