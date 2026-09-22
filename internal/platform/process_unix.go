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

// SetGracefulCancel makes cancelling the command's context ask its whole process
// group to stop, instead of os/exec's default of killing the leader outright.
// A leader that has not exited after grace is then killed by os/exec itself.
//
// Without it, every path that cancels a parent context before stopping the
// sessions — a supervisor shutting down, a signal to relayer serve — sent
// SIGKILL to each agent before any SIGTERM, so no agent could shut down
// cleanly. os/exec only calls Cancel while the process has not been waited
// for, so the process-group ID is still reserved when it is signalled.
func SetGracefulCancel(command *exec.Cmd, grace time.Duration) {
	if command == nil {
		return
	}
	command.Cancel = func() error {
		TerminateProcessGroup(command)
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
