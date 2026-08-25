package tui

import (
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/session"
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
		{name: "agent A cyan", style: agentPanelStyle(0, false), wantBorder: lipgloss.RoundedBorder(), wantColor: lipgloss.Color("#00D7FF")},
		{name: "agent B magenta", style: agentPanelStyle(1, false), wantBorder: lipgloss.RoundedBorder(), wantColor: lipgloss.Color("#FF5AF7")},
		{name: "supervisor gray", style: supervisorPanelStyle(false), wantBorder: lipgloss.RoundedBorder(), wantColor: lipgloss.Color("#4B5563")},
		{name: "blocked agent red double", style: agentPanelStyle(0, true), wantBorder: lipgloss.DoubleBorder(), wantColor: lipgloss.Color("#FF0000")},
		{name: "blocked supervisor red double", style: supervisorPanelStyle(true), wantBorder: lipgloss.DoubleBorder(), wantColor: lipgloss.Color("#FF0000")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := test.style.GetBorderStyle(); got != test.wantBorder {
				t.Fatalf("border = %#v, want %#v", got, test.wantBorder)
			}
			gotColor, ok := test.style.GetBorderTopForeground().(lipgloss.Color)
			if !ok || gotColor != test.wantColor {
				t.Fatalf("border color = %#v, want %q", test.style.GetBorderTopForeground(), test.wantColor)
			}
			_, top, right, bottom, left := test.style.GetBorder()
			if !top || !right || !bottom || !left {
				t.Fatalf("not all border sides enabled")
			}
		})
	}
}

func TestPromptSwitchesRenderedBordersAndFocusesInput(t *testing.T) {
	application, _, _ := newModelHarness(t)
	if application.input.Focused() || application.input.EchoMode != textinput.EchoNormal {
		t.Fatal("input is active before a prompt")
	}
	if !reflect.DeepEqual(application.input.PromptStyle, inputInactivePromptStyle) ||
		!reflect.DeepEqual(application.input.TextStyle, inputInactiveTextStyle) ||
		!reflect.DeepEqual(application.input.PlaceholderStyle, inputInactivePlaceholderStyle) {
		t.Fatal("input does not use inactive styles")
	}
	normalAgent := application.renderAgentPane(0, application.leftWidth, application.topHeight)
	normalSupervisor := application.renderSupervisorPane(application.width, application.supervisorHeight)
	if !strings.Contains(normalAgent, lipgloss.RoundedBorder().TopLeft) || strings.Contains(normalAgent, lipgloss.DoubleBorder().TopLeft) {
		t.Fatal("normal agent border is not rounded")
	}
	if !strings.Contains(normalSupervisor, lipgloss.RoundedBorder().TopLeft) || strings.Contains(normalSupervisor, lipgloss.DoubleBorder().TopLeft) {
		t.Fatal("normal supervisor border is not rounded")
	}

	application, _ = updateModel(t, application, session.PromptDetected{
		SessionID: 10, Pattern: "confirmation", Description: "human confirmation", Match: "Continue? [Y/n]",
	})
	if !application.input.Focused() || application.activePanel != 2 || application.inputTarget != 0 {
		t.Fatalf("prompt did not focus supervisor input")
	}
	if !reflect.DeepEqual(application.input.PromptStyle, inputActivePromptStyle) ||
		!reflect.DeepEqual(application.input.TextStyle, inputActiveTextStyle) ||
		!reflect.DeepEqual(application.input.PlaceholderStyle, inputActivePlaceholderStyle) {
		t.Fatal("input does not use interception styles")
	}
	blockedAgent := application.renderAgentPane(0, application.leftWidth, application.topHeight)
	blockedSupervisor := application.renderSupervisorPane(application.width, application.supervisorHeight)
	if !strings.Contains(blockedAgent, lipgloss.DoubleBorder().TopLeft) ||
		!strings.Contains(blockedSupervisor, lipgloss.DoubleBorder().TopLeft) {
		t.Fatal("prompt did not render double interception borders")
	}
}

func TestViewOccupiesWholeTerminalInEveryVisualState(t *testing.T) {
	application, _, _ := newModelHarness(t)
	assertFullTerminalView(t, application)
	application, _ = updateModel(t, application, session.PromptDetected{
		SessionID: 20, Pattern: "password", Description: "credential required", Match: "Password:", Sensitive: true,
	})
	assertFullTerminalView(t, application)
	application, _ = updateModel(t, application, tea.WindowSizeMsg{Width: 29, Height: 9})
	assertFullTerminalView(t, application)
}

func assertFullTerminalView(t *testing.T, application Model) {
	t.Helper()
	rendered := application.View()
	if got := lipgloss.Width(rendered); got != application.width {
		t.Fatalf("rendered width = %d, want %d", got, application.width)
	}
	if got := lipgloss.Height(rendered); got != application.height {
		t.Fatalf("rendered height = %d, want %d", got, application.height)
	}
}
