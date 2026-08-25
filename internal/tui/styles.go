package tui

import (
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/lipgloss"
)

var (
	colorAgentA    = lipgloss.Color("#00D7FF")
	colorAgentB    = lipgloss.Color("#FF5AF7")
	colorAgentC    = lipgloss.Color("#FFD75F")
	colorAgentD    = lipgloss.Color("#5F87FF")
	colorAgentE    = lipgloss.Color("#5FFFAF")
	colorAgentF    = lipgloss.Color("#AF87FF")
	colorAgentG    = lipgloss.Color("#FF875F")
	colorAgentH    = lipgloss.Color("#87D7FF")
	colorMuted     = lipgloss.Color("#4B5563")
	colorText      = lipgloss.Color("#E5E7EB")
	colorBlocked   = lipgloss.Color("#FF0000")
	colorSuccess   = lipgloss.Color("#00D75F")
	colorInputHint = lipgloss.Color("#9CA3AF")

	// Normal state: each agent keeps its own visual identity while the
	// supervisor stays deliberately understated.
	agentABorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorAgentA)
	agentBBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorAgentB)
	supervisorBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.RoundedBorder()).
				BorderForeground(colorMuted)

	// Interception state: a double, bright-red border is more prominent than
	// the normal rounded frame without changing its one-cell frame size.
	interceptionBorderStyle = lipgloss.NewStyle().
				Border(lipgloss.DoubleBorder()).
				BorderForeground(colorBlocked)

	inputInactivePromptStyle      = lipgloss.NewStyle().Foreground(colorMuted)
	inputInactiveTextStyle        = lipgloss.NewStyle().Foreground(colorInputHint)
	inputInactivePlaceholderStyle = lipgloss.NewStyle().Foreground(colorMuted).Italic(true)
	inputActivePromptStyle        = lipgloss.NewStyle().Foreground(colorBlocked).Bold(true)
	inputActiveTextStyle          = lipgloss.NewStyle().Foreground(colorText)
	inputActivePlaceholderStyle   = lipgloss.NewStyle().Foreground(colorInputHint)
	inputActiveCursorStyle        = lipgloss.NewStyle().Foreground(colorBlocked).Reverse(true)
)

func agentPanelStyle(index int, blocked bool) lipgloss.Style {
	if blocked {
		return interceptionBorderStyle
	}
	if index < 2 {
		if index == 0 {
			return agentABorderStyle
		}
		return agentBBorderStyle
	}
	return lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(agentColor(index))
}

func supervisorPanelStyle(blocked bool) lipgloss.Style {
	if blocked {
		return interceptionBorderStyle
	}
	return supervisorBorderStyle
}

func agentColor(index int) lipgloss.Color {
	colors := [...]lipgloss.Color{
		colorAgentA,
		colorAgentB,
		colorAgentC,
		colorAgentD,
		colorAgentE,
		colorAgentF,
		colorAgentG,
		colorAgentH,
	}
	if index < 0 {
		index = 0
	}
	return colors[index%len(colors)]
}

func setInputInterceptionStyle(input *textinput.Model, active bool) {
	if active {
		input.PromptStyle = inputActivePromptStyle
		input.TextStyle = inputActiveTextStyle
		input.PlaceholderStyle = inputActivePlaceholderStyle
		input.Cursor.Style = inputActiveCursorStyle
		input.Cursor.TextStyle = inputActiveTextStyle
		return
	}
	input.PromptStyle = inputInactivePromptStyle
	input.TextStyle = inputInactiveTextStyle
	input.PlaceholderStyle = inputInactivePlaceholderStyle
	input.Cursor.Style = inputInactivePromptStyle
	input.Cursor.TextStyle = inputInactiveTextStyle
}
