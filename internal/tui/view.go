package tui

import (
	"fmt"
	"strings"
	"time"

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

	if m.showMetrics {
		return m.renderMetricsFullscreen()
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
	if m.showMetrics {
		title = "SUPERVISOR  •  METRICS & OBSERVABILITY OVERLAY"
	}
	titleColor := colorMuted
	if intercepting {
		titleColor = colorBlocked
	}
	title = lipgloss.NewStyle().Foreground(titleColor).Bold(true).MaxWidth(innerWidth).MaxHeight(1).Render(title)

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
	enterHelp += " • M: metrics"
	help := lipgloss.NewStyle().Foreground(colorMuted).MaxWidth(innerWidth).MaxHeight(1).Render(
		enterHelp + " • Ctrl+←/→: focus • Ctrl+PgUp/PgDn: page • ↑/↓, PgUp/PgDn, wheel: history • Ctrl+C: quit",
	)
	content := title + "\n" +
		m.supervisor.View() + "\n" +
		m.input.View() + "\n" + help
	return style.Width(innerWidth).Height(innerHeight).Render(content)
}

func (m *Model) renderMetricsFullscreen() string {
	style := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(colorAgentA).
		Width(maxInt(1, m.width-2)).
		Height(maxInt(1, m.height-2)).
		Padding(1, 2)
	var totalSessions, activeSessions, finishedSessions, errorSessions int
	for _, pane := range m.panes {
		totalSessions++
		if pane.exited {
			if pane.exitErr != nil {
				errorSessions++
			} else {
				finishedSessions++
			}
		} else {
			activeSessions++
		}
	}

	uptime := time.Since(m.sessionStart).Round(time.Second)
	totalDecisions := m.humanAllows + m.humanDenies + m.autoAllows + m.autoDenies

	var avgLatencyStr = "0.00s"
	var minLatencyStr = "N/A"
	var maxLatencyStr = "N/A"
	if len(m.latencies) > 0 {
		var sum time.Duration
		minLat := m.latencies[0]
		maxLat := m.latencies[0]
		for _, d := range m.latencies {
			sum += d
			if d < minLat {
				minLat = d
			}
			if d > maxLat {
				maxLat = d
			}
		}
		avg := sum / time.Duration(len(m.latencies))
		avgLatencyStr = fmt.Sprintf("%.2fs", avg.Seconds())
		minLatencyStr = fmt.Sprintf("%.2fs", minLat.Seconds())
		maxLatencyStr = fmt.Sprintf("%.2fs", maxLat.Seconds())
	}

	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(colorSuccess)
	labelStyle := lipgloss.NewStyle().Bold(true).Foreground(colorAgentA)
	valStyle := lipgloss.NewStyle().Foreground(colorText)
	mutedStyle := lipgloss.NewStyle().Foreground(colorMuted)

	lines := []string{
		headerStyle.Render("═══ RELAYER OBSERVABILITY & SESSION METRICS ═════════════════════════"),
		"",
		fmt.Sprintf("  %s %s   %s %s",
			labelStyle.Render("UPTIME:"), valStyle.Render(uptime.String()),
			labelStyle.Render("SESSIONS:"), valStyle.Render(fmt.Sprintf("%d active / %d total (%d finished, %d err)", activeSessions, totalSessions, finishedSessions, errorSessions)),
		),
		"",
		fmt.Sprintf("  %s %s",
			labelStyle.Render("DECISIONS:"), valStyle.Render(fmt.Sprintf("%d total", totalDecisions)),
		),
		fmt.Sprintf("    • %s %s",
			mutedStyle.Render("Human Operator:"), valStyle.Render(fmt.Sprintf("%d (Allow: %d, Deny: %d)", m.humanAllows+m.humanDenies, m.humanAllows, m.humanDenies)),
		),
		fmt.Sprintf("    • %s %s",
			mutedStyle.Render("Automated Rules:"), valStyle.Render(fmt.Sprintf("%d (Allow: %d, Deny: %d)", m.autoAllows+m.autoDenies, m.autoAllows, m.autoDenies)),
		),
		"",
		fmt.Sprintf("  %s %s   %s %s",
			labelStyle.Render("GUARDRAILS:"), valStyle.Render(fmt.Sprintf("%d intercept(s) blocked", m.guardrailBlocks)),
			labelStyle.Render("DIRECT INPUTS:"), valStyle.Render(fmt.Sprintf("%d line(s) sent", m.operatorInputs)),
		),
		"",
		fmt.Sprintf("  %s %s (samples: %d, min: %s, max: %s)",
			labelStyle.Render("HUMAN REACTION LATENCY (AVG):"),
			valStyle.Render(avgLatencyStr),
			len(m.latencies),
			minLatencyStr,
			maxLatencyStr,
		),
		"",
		mutedStyle.Render("  [Press 'M' or 'Esc' to return to supervisor activity stream]"),
	}

	return style.Render(strings.Join(lines, "\n"))
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
