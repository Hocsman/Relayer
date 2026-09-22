//go:build windows

package platform

import (
	"context"
	"errors"
	"os/exec"
	"strconv"
	"time"

	"golang.org/x/sys/windows"
)

// NewShellCommand constructs the explicitly requested Windows shell invocation.
func NewShellCommand(ctx context.Context, script string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "cmd.exe", "/c", script), nil
}

// SetGracefulCancel is a no-op on Windows. Agent processes are spawned through
// ConPTY rather than exec.Cmd.Start, so os/exec never watches their context,
// and closing the pseudo console is the graceful stop there.
func SetGracefulCancel(*exec.Cmd, time.Duration) {}

// The functions below address a process by its numeric PID, and Windows hands a
// freed PID to the next process that asks for one. They are only safe while the
// caller holds a handle to the process: Windows does not reuse a PID while any
// handle to the old process is open. The session package holds one for exactly
// that reason. The ProcessState checks cover a caller that has waited for the
// command itself, and are not a substitute for the handle.

// TerminateProcessGroup attempts to stop the process tree rooted at command.
func TerminateProcessGroup(command *exec.Cmd) {
	if command == nil || command.Process == nil || command.ProcessState != nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/PID", strconv.Itoa(command.Process.Pid)).Run()
}

// KillProcessGroup forcefully stops the process tree rooted at command.
func KillProcessGroup(command *exec.Cmd) {
	if command == nil || command.Process == nil || command.ProcessState != nil {
		return
	}
	_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(command.Process.Pid)).Run()
	_ = command.Process.Kill()
}

// ProcessGroupExists reports whether the command process is still active.
func ProcessGroupExists(command *exec.Cmd) bool {
	if command == nil || command.Process == nil || command.ProcessState != nil {
		return false
	}
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(command.Process.Pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(handle)

	var exitCode uint32
	if err := windows.GetExitCodeProcess(handle, &exitCode); err != nil {
		return false
	}
	const statusStillActive = 259
	return exitCode == statusStillActive
}

// IsPTYCloseError recognizes Windows pipe closure errors when the child exits.
func IsPTYCloseError(err error) bool {
	if errors.Is(err, windows.ERROR_BROKEN_PIPE) || errors.Is(err, windows.ERROR_PIPE_NOT_CONNECTED) {
		return true
	}
	return false
}
