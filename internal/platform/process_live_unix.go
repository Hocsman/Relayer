//go:build aix || darwin || dragonfly || freebsd || illumos || netbsd || openbsd || solaris

package platform

// groupHoldsOnlyZombies cannot read a group's members here, so every group
// that exists counts as live.
func groupHoldsOnlyZombies(int) bool { return false }
