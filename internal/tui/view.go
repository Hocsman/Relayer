package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initializing Relayer…"
	}
	if !m.layoutRenderable() {
		return lipgloss.NewStyle().
			Foreground(colorBlocked).
			Bold(true).
			Width(m.width).
			Height(m.height).
			MaxWidth(m.width).
			MaxHeight(m.height).
			Align(lipgloss.Center, lipgloss.Center).
			Render(fmt.Sprintf(
				"Terminal too small (%dx%d). Enlarge the window to show %d agent(s).",
				m.width,
				m.height,
				len(m.panes),
			))
	}

	agents := m.renderAgentArea()
	supervisor := m.renderSupervisorPane(m.layout.Supervisor)
	return lipgloss.JoinVertical(lipgloss.Left, agents, supervisor)
}

func (m *Model) layoutRenderable() bool {
	if m.width < minTerminalWidth || m.height < minTerminalHeight || m.layout.Supervisor.Height < 6 {
		return false
	}
	for _, cell := range m.layout.Cells {
		if cell.Outer.Width < 3 || cell.Outer.Height < 3 {
			return false
		}
	}
	return true
}

func (m *Model) renderAgentArea() string {
	cells := m.layout.Cells
	switch len(cells) {
	case 1:
		return m.renderAgentPane(cells[0])
	case 2:
		return lipgloss.JoinHorizontal(
			lipgloss.Top,
			m.renderAgentPane(cells[0]),
			m.renderAgentPane(cells[1]),
		)
	case 3:
		top := lipgloss.JoinHorizontal(
			lipgloss.Top,
			m.renderAgentPane(cells[0]),
			m.renderAgentPane(cells[1]),
		)
		return lipgloss.JoinVertical(lipgloss.Left, top, m.renderAgentPane(cells[2]))
	case 4:
		top := lipgloss.JoinHorizontal(
			lipgloss.Top,
			m.renderAgentPane(cells[0]),
			m.renderAgentPane(cells[1]),
		)
		bottom := lipgloss.JoinHorizontal(
			lipgloss.Top,
			m.renderAgentPane(cells[2]),
			m.renderAgentPane(cells[3]),
		)
		return lipgloss.JoinVertical(lipgloss.Left, top, bottom)
	default:
		return ""
	}
}

func (m *Model) renderAgentPane(cell Cell) string {
	index := cell.AgentIndex
	pane := m.panes[index]
	style := agentPanelStyle(index, pane.blocked || m.auditUnavailable)
	innerWidth := maxInt(1, cell.Outer.Width-style.GetHorizontalFrameSize())
	innerHeight := maxInt(1, cell.Outer.Height-style.GetVerticalFrameSize())

	status := "RUNNING"
	statusColor := agentColor(index)
	if m.auditUnavailable && !pane.exited {
		status = "AUDIT UNAVAILABLE"
		statusColor = colorBlocked
	} else if pane.policyFrozen {
		status = "DELIVERY UNCERTAIN"
		statusColor = colorBlocked
	} else if pane.blocked {
		status = "ACTION REQUIRED"
		statusColor = colorBlocked
	} else if pane.exited && pane.exitErr == nil {
		status = "FINISHED"
		statusColor = colorSuccess
	} else if pane.exited {
		status = "ERROR"
		statusColor = colorBlocked
	}

	focusMarker := "  "
	if m.focus.Kind == FocusAgent && m.focus.AgentID == pane.sessionID {
		focusMarker = "▶ "
	}
	title := lipgloss.NewStyle().Foreground(agentColor(index)).Bold(true).Render(focusMarker+pane.name) + "  " +
		lipgloss.NewStyle().Foreground(statusColor).Render("● "+status)
	title += "  " + lipgloss.NewStyle().Foreground(colorMuted).Render("["+strings.ToUpper(pane.backend)+"]")
	title += "  " + lipgloss.NewStyle().Foreground(colorMuted).Render("ADAPTER "+strings.ToUpper(pane.adapter))
	if pane.shell {
		title += "  " + lipgloss.NewStyle().Foreground(colorMuted).Render("SHELL")
	}
	if pane.policyTag != "" {
		title += "  " + lipgloss.NewStyle().Foreground(statusColor).Render("POLICY "+pane.policyTag)
	}
	title = lipgloss.NewStyle().MaxWidth(innerWidth).MaxHeight(1).Render(title)
	content := title + "\n" + pane.viewport.View()
	return style.Width(innerWidth).Height(innerHeight).Render(content)
}

func (m *Model) renderSupervisorPane(outer Rect) string {
	intercepting := m.hasBlockedPane() || m.auditUnavailable
	style := supervisorPanelStyle(intercepting)
	innerWidth := maxInt(1, outer.Width-style.GetHorizontalFrameSize())
	innerHeight := maxInt(1, outer.Height-style.GetVerticalFrameSize())

	title := "SUPERVISOR  •  POLICY " + strings.ToUpper(string(m.policyConfig.DefaultAction))
	if m.policyConfig.DryRun {
		title = "SUPERVISOR  •  DRY RUN"
	}
	if m.auditUnavailable {
		title = "SUPERVISOR  •  AUDIT UNAVAILABLE  •  STOP REQUIRED"
	} else if m.hasFrozenPane() {
		title = "SUPERVISOR  •  DELIVERY UNCERTAIN  •  STOP REQUIRED"
	} else if intercepting {
		title = "SUPERVISOR  •  HUMAN DECISION REQUIRED"
		if m.policyConfig.DryRun {
			title += "  •  DRY RUN"
		}
	} else if m.lineInputTarget != "" {
		title = "SUPERVISOR  •  DIRECT INSTRUCTION"
	}
	if m.inputTarget != "" {
		if paneIndex := m.paneIndex(m.inputTarget); paneIndex >= 0 {
			title += "  →  " + m.panes[paneIndex].name
		}
	} else if m.lineInputTarget != "" {
		if paneIndex := m.paneIndex(m.lineInputTarget); paneIndex >= 0 {
			title += "  →  " + m.panes[paneIndex].name
		}
	}
	// Only the agent being answered is named, and only four are visible per
	// page. Without a count, an operator answering one prompt has no way to
	// learn that others are queued behind it, possibly on another page. One
	// waiting prompt is already the named one, so the count starts at two.
	if waiting := len(m.pending); waiting > 1 {
		title += fmt.Sprintf("  •  %d PENDING", waiting)
	}
	title += fmt.Sprintf(
		"  •  BACKEND %s  •  PAGE %d/%d",
		m.backendLabel(),
		m.layout.Page+1,
		m.layout.PageCount,
	)
	enterHelp := "Enter: answer"
	if m.hasBackend("tmux") {
		enterHelp = "Enter: open/answer • Ctrl+B then D: back to Relayer"
	}
	if m.lineInputTarget != "" {
		enterHelp = "Enter: send the instruction • Esc: cancel"
	} else {
		enterHelp += " • I: direct instruction"
	}
	// The semantic answers only exist while a prompt is waiting, so the hint
	// appears exactly when the keys do something.
	if m.inputTarget != "" && m.lineInputTarget == "" {
		enterHelp += " • F2: allow • F3: deny"
	}
	help := lipgloss.NewStyle().Foreground(colorMuted).MaxWidth(innerWidth).MaxHeight(1).Render(
		enterHelp + " • Ctrl+←/→: focus • Ctrl+PgUp/PgDn: page • ↑/↓, PgUp/PgDn, wheel: history • Ctrl+C: quit",
	)
	titleColor := colorMuted
	if intercepting {
		titleColor = colorBlocked
	}
	title = lipgloss.NewStyle().Foreground(titleColor).Bold(true).MaxWidth(innerWidth).MaxHeight(1).Render(title)
	content := title + "\n" +
		m.supervisor.View() + "\n" +
		m.input.View() + "\n" + help
	return style.Width(innerWidth).Height(innerHeight).Render(content)
}

func (m *Model) hasBackend(name string) bool {
	for _, pane := range m.panes {
		if strings.EqualFold(pane.backend, name) {
			return true
		}
	}
	return false
}

func (m *Model) hasFrozenPane() bool {
	for index := range m.panes {
		if m.panes[index].policyFrozen {
			return true
		}
	}
	return false
}
