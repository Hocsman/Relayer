//go:build !darwin && !linux

package main

func desktopAgentExecutionSupported() bool { return false }

func desktopUnsupportedReason() string {
	return "This build does not run agents on Windows yet: a ConPTY backend has to be implemented and tested first."
}
