package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/lipgloss"
)

func (m *Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initialisation de Relayer…"
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
				"Terminal trop petit (%dx%d). Agrandissez la fenêtre pour afficher %d agent(s).",
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
	style := agentPanelStyle(index, pane.blocked)
	innerWidth := maxInt(1, cell.Outer.Width-style.GetHorizontalFrameSize())
	innerHeight := maxInt(1, cell.Outer.Height-style.GetVerticalFrameSize())

	status := "EN COURS"
	statusColor := agentColor(index)
	if pane.blocked {
		status = "INTERVENTION REQUISE"
		statusColor = colorBlocked
	} else if pane.exited && pane.exitErr == nil {
		status = "TERMINÉ"
		statusColor = colorSuccess
	} else if pane.exited {
		status = "ERREUR"
		statusColor = colorBlocked
	}

	focusMarker := "  "
	if m.focus.Kind == FocusAgent && m.focus.AgentID == pane.sessionID {
		focusMarker = "▶ "
	}
	title := lipgloss.NewStyle().Foreground(agentColor(index)).Bold(true).Render(focusMarker+pane.name) + "  " +
		lipgloss.NewStyle().Foreground(statusColor).Render("● "+status)
	title += "  " + lipgloss.NewStyle().Foreground(colorMuted).Render("["+strings.ToUpper(pane.backend)+"]")
	if pane.shell {
		title += "  " + lipgloss.NewStyle().Foreground(colorMuted).Render("SHELL")
	}
	title = lipgloss.NewStyle().MaxWidth(innerWidth).MaxHeight(1).Render(title)
	content := title + "\n" + pane.viewport.View()
	return style.Width(innerWidth).Height(innerHeight).Render(content)
}

func (m *Model) renderSupervisorPane(outer Rect) string {
	intercepting := m.hasBlockedPane()
	style := supervisorPanelStyle(intercepting)
	innerWidth := maxInt(1, outer.Width-style.GetHorizontalFrameSize())
	innerHeight := maxInt(1, outer.Height-style.GetVerticalFrameSize())

	title := "SUPERVISEUR  •  AUTOMATIQUE"
	if intercepting {
		title = "SUPERVISEUR  •  ACTION HUMAINE REQUISE"
	}
	if m.inputTarget != "" {
		if paneIndex := m.paneIndex(m.inputTarget); paneIndex >= 0 {
			title += "  →  " + m.panes[paneIndex].name
		}
	}
	title += fmt.Sprintf(
		"  •  BACKEND %s  •  PAGE %d/%d",
		m.backendLabel(),
		m.layout.Page+1,
		m.layout.PageCount,
	)
	enterHelp := "Entrée: répondre"
	if m.hasBackend("tmux") {
		enterHelp = "Entrée: ouvrir/répondre • Ctrl+B puis D: revenir à Relayer"
	}
	help := lipgloss.NewStyle().Foreground(colorMuted).MaxWidth(innerWidth).MaxHeight(1).Render(
		enterHelp + " • Ctrl+←/→: focus • Ctrl+PgUp/PgDn: page • ↑/↓, PgUp/PgDn, molette: historique • Ctrl+C: quitter",
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
