//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

// Package platform contains the small amount of process behaviour that is
// inherently operating-system specific. Keeping it outside the session
// package makes the PTY lifecycle compile cleanly on unsupported platforms.
package platform

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"syscall"
	"time"
)

// NewShellCommand constructs the explicitly requested Unix shell invocation.
// The script is passed as one argument; it is never interpolated by Relayer.
func NewShellCommand(ctx context.Context, script string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "/bin/sh", "-c", script), nil
}

// TerminateProcessGroup asks the whole process group led by command to stop.
// creack/pty starts commands in a new session on Unix, so the command PID is
// also the process-group ID.
func TerminateProcessGroup(command *exec.Cmd) {
	if command == nil || command.Process == nil || command.Process.Pid <= 0 {
		return
	}
	_ = signalProcessGroup(command.Process.Pid, syscall.SIGTERM)
}

// KillProcessGroup forcefully stops the process group and then kills the
// leader as a fallback in case group signalling failed.
func KillProcessGroup(command *exec.Cmd) {
	if command == nil || command.Process == nil || command.Process.Pid <= 0 {
		return
	}
	_ = signalProcessGroup(command.Process.Pid, syscall.SIGKILL)
	_ = command.Process.Kill()
}

// ProcessGroupExists reports whether the command's process group still exists.
func ProcessGroupExists(command *exec.Cmd) bool {
	if command == nil || command.Process == nil || command.Process.Pid <= 0 {
		return false
	}
	err := signalProcessGroup(command.Process.Pid, syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}

// SetGracefulCancel makes cancelling the command's context call stop — the
// caller's graceful stop request — instead of os/exec's default of killing the
// leader outright. A leader still running after grace is then killed by
// os/exec itself.
//
// Without it, every path that cancels a parent context before stopping the
// sessions — a supervisor shutting down, a signal to relayer serve — sent
// SIGKILL to each agent before any SIGTERM, so no agent could shut down
// cleanly. The caller passes its own stop request rather than a signal, so the
// cancellation and a later explicit stop are one request and one SIGTERM: an
// agent whose handler runs only once was killed by a second.
//
// os/exec may call Cancel after the leader was reaped but before Wait returned,
// and the session only learns of the reap when Wait returns. In that moment the
// stop request signals the group by number, as the session's own cleanup does
// right after the reap. That is safe on Unix: the kernel keeps a group's number
// reserved while any member lives, and a group with no member left answers
// ESRCH unless the whole PID space wrapped around in between. Windows has no
// such rule, which is why it holds the process handle instead.
func SetGracefulCancel(command *exec.Cmd, grace time.Duration, stop func()) {
	if command == nil || stop == nil {
		return
	}
	command.Cancel = func() error {
		stop()
		return nil
	}
	command.WaitDelay = grace
}

// IsPTYCloseError recognizes the EIO returned by many Unix PTY masters after
// the slave side has closed.
func IsPTYCloseError(err error) bool {
	return errors.Is(err, syscall.EIO)
}

func signalProcessGroup(pid int, signal os.Signal) error {
	// On Unix a negative PID addresses the complete process group.
	group, err := os.FindProcess(-pid)
	if err != nil {
		return err
	}
	return group.Signal(signal)
}
