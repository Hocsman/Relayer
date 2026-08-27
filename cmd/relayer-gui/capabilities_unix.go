//go:build darwin || linux

package main

func desktopAgentExecutionSupported() bool { return true }
func desktopUnsupportedReason() string     { return "" }
