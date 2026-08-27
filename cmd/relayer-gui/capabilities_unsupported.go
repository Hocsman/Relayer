//go:build !darwin && !linux

package main

func desktopAgentExecutionSupported() bool { return false }

func desktopUnsupportedReason() string {
	return "Cette version ne lance pas encore d'agents sous Windows: un backend ConPTY doit d'abord être implémenté et testé."
}
