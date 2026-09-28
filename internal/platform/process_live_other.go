//go:build !linux

package platform

import "os/exec"

// ProcessGroupHasLiveMember is ProcessGroupExists where Relayer cannot tell a
// zombie member from a running one.
func ProcessGroupHasLiveMember(command *exec.Cmd) bool {
	return ProcessGroupExists(command)
}
