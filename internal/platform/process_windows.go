//go:build windows

package platform

import (
	"context"
	"errors"
	"os/exec"
	"strconv"

	"golang.org/x/sys/windows"
)

// NewShellCommand constructs the explicitly requested Windows shell invocation.
func NewShellCommand(ctx context.Context, script string) (*exec.Cmd, error) {
	return exec.CommandContext(ctx, "cmd.exe", "/c", script), nil
}

// TerminateProcessGroup attempts to stop the process tree rooted at command.
func TerminateProcessGroup(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/T", "/PID", strconv.Itoa(command.Process.Pid)).Run()
}

// KillProcessGroup forcefully stops the process tree rooted at command.
func KillProcessGroup(command *exec.Cmd) {
	if command == nil || command.Process == nil {
		return
	}
	_ = exec.Command("taskkill", "/F", "/T", "/PID", strconv.Itoa(command.Process.Pid)).Run()
	_ = command.Process.Kill()
}

// ProcessGroupExists reports whether the command process is still active.
func ProcessGroupExists(command *exec.Cmd) bool {
	if command == nil || command.Process == nil {
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
