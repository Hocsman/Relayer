//go:build !aix && !darwin && !dragonfly && !freebsd && !illumos && !linux && !netbsd && !openbsd && !solaris

package platform

import "os/exec"

// Unsupported platforms cannot address Unix process groups. PTY startup will
// normally fail first with pty.ErrUnsupported; these stubs keep the package
// buildable and still kill a leader if one was supplied by another launcher.
func TerminateProcessGroup(*exec.Cmd) {}

func KillProcessGroup(command *exec.Cmd) {
	if command != nil && command.Process != nil {
		_ = command.Process.Kill()
	}
}

func ProcessGroupExists(*exec.Cmd) bool { return false }

func IsPTYCloseError(error) bool { return false }
