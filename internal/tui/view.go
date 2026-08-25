package tui

import (
	"fmt"

	"github.com/charmbracelet/lipgloss"
)

func (m Model) View() string {
	if m.width == 0 || m.height == 0 {
		return "Initialisation de Relayer…"
	}
	if m.width < minTerminalWidth || m.height < minTerminalHeight {
		return lipgloss.NewStyle().
			Foreground(colorBlocked).
			Bold(true).
			Width(m.width).
			Height(m.height).
			MaxWidth(m.width).
			MaxHeight(m.height).
			Align(lipgloss.Center, lipgloss.Center).
			Render(fmt.Sprintf(
				"Terminal trop petit (%dx%d). Minimum conseillé: %dx%d.",
				m.width,
				m.height,
				minTerminalWidth,
				minTerminalHeight,
			))
	}

	left := m.renderAgentPane(0, m.leftWidth, m.topHeight)
	right := m.renderAgentPane(1, m.rightWidth, m.topHeight)
	top := lipgloss.JoinHorizontal(lipgloss.Top, left, right)
	supervisor := m.renderSupervisorPane(m.width, m.supervisorHeight)
	return lipgloss.JoinVertical(lipgloss.Left, top, supervisor)
}

func (m Model) renderAgentPane(index, outerWidth, outerHeight int) string {
	pane := m.panes[index]
	style := agentPanelStyle(index, pane.blocked)
	innerWidth := maxInt(1, outerWidth-style.GetHorizontalFrameSize())
	innerHeight := maxInt(1, outerHeight-style.GetVerticalFrameSize())

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
	if m.activePanel == index {
		focusMarker = "▶ "
	}
	title := lipgloss.NewStyle().Foreground(agentColor(index)).Bold(true).Render(focusMarker+pane.name) + "  " +
		lipgloss.NewStyle().Foreground(statusColor).Render("● "+status)
	title = lipgloss.NewStyle().MaxWidth(innerWidth).Render(title)
	content := title + "\n" + pane.viewport.View()
	return style.Width(innerWidth).Height(innerHeight).Render(content)
}

func (m Model) renderSupervisorPane(outerWidth, outerHeight int) string {
	intercepting := m.hasBlockedPane()
	style := supervisorPanelStyle(intercepting)
	innerWidth := maxInt(1, outerWidth-style.GetHorizontalFrameSize())
	innerHeight := maxInt(1, outerHeight-style.GetVerticalFrameSize())

	title := "SUPERVISEUR  •  AUTOMATIQUE"
	if intercepting {
		title = "SUPERVISEUR  •  ACTION HUMAINE REQUISE"
	}
	if m.inputTarget >= 0 {
		title += "  →  " + m.panes[m.inputTarget].name
	}
	help := lipgloss.NewStyle().Foreground(colorMuted).MaxWidth(innerWidth).Render(
		"Ctrl+←/→: panneau • ↑/↓, PgUp/PgDn, molette: historique • Entrée: envoyer • Ctrl+C: quitter",
	)
	titleColor := colorMuted
	if intercepting {
		titleColor = colorBlocked
	}
	title = lipgloss.NewStyle().Foreground(titleColor).Bold(true).MaxWidth(innerWidth).Render(title)
	content := title + "\n" +
		m.supervisor.View() + "\n" +
		m.input.View() + "\n" + help
	return style.Width(innerWidth).Height(innerHeight).Render(content)
}
