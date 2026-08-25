package tui

import tea "github.com/charmbracelet/bubbletea"

func batchCommands(commands ...tea.Cmd) tea.Cmd {
	filtered := commands[:0]
	for _, command := range commands {
		if command != nil {
			filtered = append(filtered, command)
		}
	}
	if len(filtered) == 0 {
		return nil
	}
	if len(filtered) == 1 {
		return filtered[0]
	}
	return tea.Batch(filtered...)
}

func maxInt(left, right int) int {
	if left > right {
		return left
	}
	return right
}
