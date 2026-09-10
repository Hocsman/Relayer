//go:build windows

package main

func desktopAgentExecutionSupported() bool { return true }
func desktopUnsupportedReason() string     { return "" }
