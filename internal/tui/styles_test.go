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
		{name: "agent H distinct", style: agentPanelStyle(7, false), wantBorder: lipgloss.RoundedBorder(), wantColor: lipgloss.Color("#87D7FF")},
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
	normalAgent := application.renderAgentPane(application.layout.Cells[0])
	normalSupervisor := application.renderSupervisorPane(application.layout.Supervisor)
	if !strings.Contains(normalAgent, lipgloss.RoundedBorder().TopLeft) || strings.Contains(normalAgent, lipgloss.DoubleBorder().TopLeft) {
		t.Fatal("normal agent border is not rounded")
	}
	if !strings.Contains(normalSupervisor, lipgloss.RoundedBorder().TopLeft) || strings.Contains(normalSupervisor, lipgloss.DoubleBorder().TopLeft) {
		t.Fatal("normal supervisor border is not rounded")
	}

	application, _ = updateModel(t, application, session.PromptDetected{
		SessionID: "agent-a", Pattern: "confirmation", Description: "human confirmation", Match: "Continue? [Y/n]",
	})
	if !application.input.Focused() || application.focus.Kind != FocusSupervisor || application.inputTarget != "agent-a" {
		t.Fatalf("prompt did not focus supervisor input")
	}
	if !reflect.DeepEqual(application.input.PromptStyle, inputActivePromptStyle) ||
		!reflect.DeepEqual(application.input.TextStyle, inputActiveTextStyle) ||
		!reflect.DeepEqual(application.input.PlaceholderStyle, inputActivePlaceholderStyle) {
		t.Fatal("input does not use interception styles")
	}
	blockedAgent := application.renderAgentPane(application.layout.Cells[0])
	blockedSupervisor := application.renderSupervisorPane(application.layout.Supervisor)
	if !strings.Contains(blockedAgent, lipgloss.DoubleBorder().TopLeft) ||
		!strings.Contains(blockedSupervisor, lipgloss.DoubleBorder().TopLeft) {
		t.Fatal("prompt did not render double interception borders")
	}
}

func TestViewOccupiesWholeTerminalInEveryVisualState(t *testing.T) {
	application, _, _ := newModelHarness(t)
	assertFullTerminalView(t, application)
	application, _ = updateModel(t, application, session.PromptDetected{
		SessionID: "agent-b", Pattern: "password", Description: "credential required", Match: "Password:", Sensitive: true,
	})
	assertFullTerminalView(t, application)
	application, _ = updateModel(t, application, tea.WindowSizeMsg{Width: 29, Height: 9})
	assertFullTerminalView(t, application)
	application, _ = updateModel(t, application, tea.WindowSizeMsg{Width: 1, Height: 1})
	assertFullTerminalView(t, application)
}

func assertFullTerminalView(t *testing.T, application *Model) {
	t.Helper()
	rendered := application.View()
	if got := lipgloss.Width(rendered); got != application.width {
		t.Fatalf("rendered width = %d, want %d", got, application.width)
	}
	if got := lipgloss.Height(rendered); got != application.height {
		t.Fatalf("rendered height = %d, want %d", got, application.height)
	}
}

func TestEveryRenderedCellMatchesItsGeometry(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	for count := 1; count <= 8; count++ {
		application, err := NewModel(backend, make(chan session.Event), testPanes(count), 121, 41, nil)
		if err != nil {
			t.Fatal(err)
		}
		for page := 0; page < pageCount(count); page++ {
			application.setPage(page)
			for _, cell := range application.layout.Cells {
				rendered := application.renderAgentPane(cell)
				if got := lipgloss.Width(rendered); got != cell.Outer.Width {
					t.Fatalf("count %d page %d agent %d width = %d, want %d", count, page, cell.AgentIndex, got, cell.Outer.Width)
				}
				if got := lipgloss.Height(rendered); got != cell.Outer.Height {
					t.Fatalf("count %d page %d agent %d height = %d, want %d", count, page, cell.AgentIndex, got, cell.Outer.Height)
				}
			}
			renderedSupervisor := application.renderSupervisorPane(application.layout.Supervisor)
			if lipgloss.Width(renderedSupervisor) != application.layout.Supervisor.Width || lipgloss.Height(renderedSupervisor) != application.layout.Supervisor.Height {
				t.Fatalf("supervisor rendered %dx%d, want %dx%d", lipgloss.Width(renderedSupervisor), lipgloss.Height(renderedSupervisor), application.layout.Supervisor.Width, application.layout.Supervisor.Height)
			}
			assertFullTerminalView(t, application)
		}
	}
}
