package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
)

func TestPanelStylesUseAgentIdentityAndInterceptionBorders(t *testing.T) {
	tests := []struct {
		name       string
		style      lipgloss.Style
		wantBorder lipgloss.Border
		wantColor  lipgloss.Color
	}{
		{
			name:       "agent A cyan",
			style:      agentPanelStyle(0, false),
			wantBorder: lipgloss.RoundedBorder(),
			wantColor:  lipgloss.Color("#00D7FF"),
		},
		{
			name:       "agent B magenta",
			style:      agentPanelStyle(1, false),
			wantBorder: lipgloss.RoundedBorder(),
			wantColor:  lipgloss.Color("#FF5AF7"),
		},
		{
			name:       "supervisor gray",
			style:      supervisorPanelStyle(false),
			wantBorder: lipgloss.RoundedBorder(),
			wantColor:  lipgloss.Color("#4B5563"),
		},
		{
			name:       "blocked agent red double",
			style:      agentPanelStyle(0, true),
			wantBorder: lipgloss.DoubleBorder(),
			wantColor:  lipgloss.Color("#FF0000"),
		},
		{
			name:       "blocked supervisor red double",
			style:      supervisorPanelStyle(true),
			wantBorder: lipgloss.DoubleBorder(),
			wantColor:  lipgloss.Color("#FF0000"),
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.style.GetBorderStyle(); got != test.wantBorder {
				t.Fatalf("border = %#v, want %#v", got, test.wantBorder)
			}
			gotColor, ok := test.style.GetBorderTopForeground().(lipgloss.Color)
			if !ok {
				t.Fatalf("border color has type %T, want lipgloss.Color", test.style.GetBorderTopForeground())
			}
			if gotColor != test.wantColor {
				t.Fatalf("border color = %q, want %q", gotColor, test.wantColor)
			}
			_, top, right, bottom, left := test.style.GetBorder()
			if !top || !right || !bottom || !left {
				t.Fatalf("border sides enabled = top:%t right:%t bottom:%t left:%t", top, right, bottom, left)
			}
		})
	}
}

func TestPromptSwitchesRenderedBordersAndFocusesInput(t *testing.T) {
	application := newModelHarness(t)
	application = updateModel(t, application, tea.WindowSizeMsg{Width: 100, Height: 32})

	if application.input.Focused() {
		t.Fatal("text input is focused before an interactive prompt")
	}
	if application.input.EchoMode != textinput.EchoNormal {
		t.Fatalf("inactive text input echo mode = %v, want EchoNormal", application.input.EchoMode)
	}
	if !reflect.DeepEqual(application.input.PromptStyle, inputInactivePromptStyle) ||
		!reflect.DeepEqual(application.input.TextStyle, inputInactiveTextStyle) ||
		!reflect.DeepEqual(application.input.PlaceholderStyle, inputInactivePlaceholderStyle) {
		t.Fatal("inactive text input does not use the normal blurred styles")
	}

	normalAgent := application.renderAgentPane(0, application.leftWidth, application.topHeight)
	normalSupervisor := application.renderSupervisorPane(application.width, application.supervisorHeight)
	if !strings.Contains(normalAgent, lipgloss.RoundedBorder().TopLeft) ||
		strings.Contains(normalAgent, lipgloss.DoubleBorder().TopLeft) {
		t.Fatalf("normal agent does not render a rounded border:\n%s", normalAgent)
	}
	if !strings.Contains(normalSupervisor, lipgloss.RoundedBorder().TopLeft) ||
		strings.Contains(normalSupervisor, lipgloss.DoubleBorder().TopLeft) {
		t.Fatalf("normal supervisor does not render a rounded border:\n%s", normalSupervisor)
	}

	application = updateModel(t, application, PromptDetectedMsg{
		SessionID:   application.panes[0].sessionID,
		Pattern:     "confirmation",
		Description: "human confirmation",
		Match:       "Continue? [Y/n]",
	})
	if !application.input.Focused() {
		t.Fatal("text input was not focused when a prompt was intercepted")
	}
	if application.activePanel != 2 || application.inputTarget != 0 {
		t.Fatalf(
			"prompt focus state = active panel %d, input target %d; want 2/0",
			application.activePanel,
			application.inputTarget,
		)
	}
	if application.input.EchoMode != textinput.EchoNormal {
		t.Fatalf("non-sensitive prompt echo mode = %v, want EchoNormal", application.input.EchoMode)
	}
	if !reflect.DeepEqual(application.input.PromptStyle, inputActivePromptStyle) ||
		!reflect.DeepEqual(application.input.TextStyle, inputActiveTextStyle) ||
		!reflect.DeepEqual(application.input.PlaceholderStyle, inputActivePlaceholderStyle) {
		t.Fatal("focused text input does not use the interception styles")
	}

	blockedAgent := application.renderAgentPane(0, application.leftWidth, application.topHeight)
	blockedSupervisor := application.renderSupervisorPane(application.width, application.supervisorHeight)
	if !strings.Contains(blockedAgent, lipgloss.DoubleBorder().TopLeft) {
		t.Fatalf("blocked agent does not render a double border:\n%s", blockedAgent)
	}
	if !strings.Contains(blockedSupervisor, lipgloss.DoubleBorder().TopLeft) {
		t.Fatalf("intercepting supervisor does not render a double border:\n%s", blockedSupervisor)
	}
}

func TestViewOccupiesTheWholeTerminalInEveryVisualState(t *testing.T) {
	application := newModelHarness(t)
	application = updateModel(t, application, tea.WindowSizeMsg{Width: 121, Height: 40})
	assertFullTerminalView(t, application)

	application = updateModel(t, application, PromptDetectedMsg{
		SessionID:   application.panes[1].sessionID,
		Pattern:     "password",
		Description: "credential required",
		Match:       "Password:",
		Sensitive:   true,
	})
	assertFullTerminalView(t, application)

	application = updateModel(t, application, tea.WindowSizeMsg{Width: 29, Height: 9})
	assertFullTerminalView(t, application)
}

func assertFullTerminalView(t *testing.T, application model) {
	t.Helper()
	rendered := application.View()
	if got := lipgloss.Width(rendered); got != application.width {
		t.Fatalf("rendered width = %d, want terminal width %d", got, application.width)
	}
	if got := lipgloss.Height(rendered); got != application.height {
		t.Fatalf("rendered height = %d, want terminal height %d", got, application.height)
	}
}
