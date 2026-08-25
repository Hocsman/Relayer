package tui

import (
	"fmt"

	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func (m Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	commands := make([]tea.Cmd, 0, 3)
	if event, ok := message.(backendEventMsg); ok {
		message = event.Event
		commands = append(commands, waitForBackendEvent(m.backend.Context(), m.events))
	}

	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		m.resize(msg.Width, msg.Height)
	case tea.KeyMsg:
		if msg.String() == "ctrl+c" {
			m.backend.BeginShutdown()
			return m, tea.Quit
		}
		if msg.String() == "ctrl+left" {
			m.activePanel = (m.activePanel + 2) % 3
			commands = append(commands, m.syncFocus())
			break
		}
		if msg.String() == "ctrl+right" {
			m.activePanel = (m.activePanel + 1) % 3
			commands = append(commands, m.syncFocus())
			break
		}
		if m.activePanel == 2 && isViewportNavigationKey(msg) {
			// Vertical navigation is unambiguous for the supervisor: textinput
			// only needs horizontal cursor movement, while these keys browse logs.
			var command tea.Cmd
			m.supervisor, command = m.supervisor.Update(msg)
			commands = append(commands, command)
			break
		}

		if m.activePanel == 2 && m.inputTarget >= 0 {
			if msg.Type == tea.KeyEnter && !m.writePending {
				paneIndex := m.inputTarget
				prompt := m.panes[paneIndex].prompt
				value := m.input.Value()

				// Clear the old waiting state before the asynchronous write. This
				// allows an immediate second prompt from this session to be queued.
				m.panes[paneIndex].blocked = false
				m.panes[paneIndex].prompt = session.PromptDetected{}
				m.removePending(paneIndex)
				m.inputTarget = -1
				m.input.Reset()
				m.input.Blur()
				setInputInterceptionStyle(&m.input, false)
				m.writePending = true
				m.appendLog(fmt.Sprintf("Réponse transmise à %s", m.panes[paneIndex].name))
				commands = append(commands, deliverInput(
					m.backend,
					m.panes[paneIndex].sessionID,
					value,
					prompt,
				))
				break
			}
			var command tea.Cmd
			m.input, command = m.input.Update(msg)
			commands = append(commands, command)
		} else if m.activePanel >= 0 && m.activePanel < len(m.panes) {
			var command tea.Cmd
			m.panes[m.activePanel].viewport, command = m.panes[m.activePanel].viewport.Update(msg)
			commands = append(commands, command)
		}
	case tea.MouseMsg:
		commands = append(commands, m.handleViewportMouse(msg))
	case session.OutputAvailable:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			m.refreshPaneOutput(paneIndex)
		}
	case session.PromptDetected:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			m.refreshPaneOutput(paneIndex)
			if m.panes[paneIndex].exited {
				break
			}
			if !m.panes[paneIndex].blocked {
				m.panes[paneIndex].blocked = true
				m.panes[paneIndex].prompt = msg
				m.pending = append(m.pending, paneIndex)
				m.appendLog(fmt.Sprintf(
					"%s attend une intervention humaine (%s)",
					m.panes[paneIndex].name,
					msg.Description,
				))
			}
			if m.inputTarget < 0 && !m.writePending {
				commands = append(commands, m.activateNextPrompt())
			}
		}
	case session.Exited:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			m.refreshPaneOutput(paneIndex)
			m.panes[paneIndex].exited = true
			m.panes[paneIndex].exitErr = msg.Err
			if msg.Err == nil {
				m.appendLog(fmt.Sprintf("%s terminé", m.panes[paneIndex].name))
			} else {
				m.appendLog(fmt.Sprintf("%s terminé avec erreur: %v", m.panes[paneIndex].name, msg.Err))
			}
			wasInputTarget := m.inputTarget == paneIndex
			m.removePending(paneIndex)
			m.panes[paneIndex].blocked = false
			m.panes[paneIndex].prompt = session.PromptDetected{}
			if wasInputTarget {
				m.inputTarget = -1
				// In particular, never carry a password into the next agent.
				m.input.Reset()
				commands = append(commands, m.activateNextPrompt())
			}
		}
	case session.Error:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			m.appendLog(fmt.Sprintf("Erreur PTY de %s: %v", m.panes[paneIndex].name, msg.Err))
		}
	case inputDeliveredMsg:
		m.writePending = false
		paneIndex := m.paneIndex(msg.SessionID)
		if paneIndex < 0 {
			break
		}
		if msg.Err != nil {
			m.appendLog(fmt.Sprintf("Échec de l'envoi à %s: %v", m.panes[paneIndex].name, msg.Err))
			if !m.panes[paneIndex].exited && !m.panes[paneIndex].blocked {
				m.panes[paneIndex].blocked = true
				m.panes[paneIndex].prompt = msg.Prompt
				m.prependPending(paneIndex)
			}
		}
		commands = append(commands, m.activateNextPrompt())
	case backendStoppedMsg:
		return m, tea.Quit
	}

	return m, batchCommands(commands...)
}

func isViewportNavigationKey(message tea.KeyMsg) bool {
	switch message.Type {
	case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown:
		return true
	default:
		return false
	}
}

// handleViewportMouse sends vertical wheel events to the viewport below the
// cursor, independently of keyboard focus.
func (m *Model) handleViewportMouse(message tea.MouseMsg) tea.Cmd {
	event := tea.MouseEvent(message)
	if event.Action != tea.MouseActionPress ||
		(event.Button != tea.MouseButtonWheelUp && event.Button != tea.MouseButtonWheelDown) {
		return nil
	}
	if m.width < minTerminalWidth || m.height < minTerminalHeight {
		return nil
	}
	if event.X < 0 || event.X >= m.width || event.Y < 0 || event.Y >= m.height {
		return nil
	}

	if event.Y < m.topHeight {
		paneIndex := 0
		if event.X >= m.leftWidth {
			paneIndex = 1
		}
		var command tea.Cmd
		m.panes[paneIndex].viewport, command = m.panes[paneIndex].viewport.Update(message)
		return command
	}

	var command tea.Cmd
	m.supervisor, command = m.supervisor.Update(message)
	return command
}
