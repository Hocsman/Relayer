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

// ProcessGroupHasLiveMember reports whether the command's process group still
// has a member that can run. Unlike ProcessGroupExists it does not count a
// member left as a zombie: it runs no code and has closed every descriptor,
// the PTY slave included. It only keeps the group's number reserved until
// whoever adopted it reaps it, which an init that reaps on a timer takes a
// second or two to do, and Relayer itself running as PID 1 never does.
//
// It is only meaningful once the group was sent SIGKILL: the kernel then lets
// no member start a new one, so a group seen with nothing but zombies stays
// that way. Only Linux can tell a zombie apart here; elsewhere, and wherever
// /proc may not show every member, it is ProcessGroupExists.
func ProcessGroupHasLiveMember(command *exec.Cmd) bool {
	if command == nil || command.Process == nil || command.Process.Pid <= 0 {
		return false
	}
	return ProcessGroupIDHasLiveMember(command.Process.Pid)
}

// ProcessGroupIDHasLiveMember is ProcessGroupHasLiveMember for a group known
// only by its number.
func ProcessGroupIDHasLiveMember(pgid int) bool {
	if pgid <= 0 {
		return false
	}
	if err := signalProcessGroup(pgid, syscall.Signal(0)); err != nil && !errors.Is(err, syscall.EPERM) {
		return false
	}
	return !groupHoldsOnlyZombies(pgid)
}

// ReapProcessGroupZombies waits for every member of the command's process
// group that has exited and is Relayer's child, and returns how many it
// reaped. Only the leader starts as Relayer's child, and it is reaped by
// os/exec; the other members become Relayer's children only when they are
// orphaned and Relayer adopts them, which it does as PID 1 in a container
// started without an init. Nothing else would ever reap those, so each Stop
// would leave its agent's orphans in the process table for good.
//
// It never blocks and never waits for a process outside the group, so it
// cannot take the exit status of a child os/exec is waiting for elsewhere.
// Call it only once the leader has been reaped, as its own Wait would
// otherwise lose the leader's status.
func ReapProcessGroupZombies(command *exec.Cmd) int {
	if command == nil || command.Process == nil || command.Process.Pid <= 1 {
		return 0
	}
	reaped := 0
	for {
		var status syscall.WaitStatus
		pid, err := syscall.Wait4(-command.Process.Pid, &status, syscall.WNOHANG, nil)
		if errors.Is(err, syscall.EINTR) {
			continue
		}
		if err != nil || pid <= 0 {
			return reaped
		}
		reaped++
	}
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
