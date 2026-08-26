package tui

import (
	"fmt"

	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func (m *Model) Update(message tea.Msg) (tea.Model, tea.Cmd) {
	commands := make([]tea.Cmd, 0, 3)
	if event, ok := message.(backendEventMsg); ok {
		message = event.Event
		commands = append(commands, waitForBackendEvent(m.backend.Context(), m.events))
	}

	switch msg := message.(type) {
	case tea.WindowSizeMsg:
		commands = append(commands, m.resize(msg.Width, msg.Height, true))
	case tea.KeyMsg:
		switch msg.Type {
		case tea.KeyCtrlC:
			m.backend.BeginShutdown()
			return m, tea.Quit
		case tea.KeyCtrlLeft:
			m.moveFocus(-1)
			commands = append(commands, m.syncFocus())
			break
		case tea.KeyCtrlRight:
			m.moveFocus(1)
			commands = append(commands, m.syncFocus())
			break
		case tea.KeyCtrlPgUp:
			m.movePage(-1)
			commands = append(commands, m.syncFocus())
			break
		case tea.KeyCtrlPgDown:
			m.movePage(1)
			commands = append(commands, m.syncFocus())
			break
		default:
			// A pending human response always wins over pane actions. In
			// particular, Enter must keep submitting PTY responses even if the
			// user temporarily moved focus away from the supervisor.
			if msg.Type == tea.KeyEnter && m.inputTarget != "" && !m.writePending {
				commands = append(commands, m.submitInput())
				break
			}
			if m.focus.Kind == FocusSupervisor && isViewportNavigationKey(msg) {
				var command tea.Cmd
				m.supervisor, command = m.supervisor.Update(msg)
				commands = append(commands, command)
				break
			}

			if m.focus.Kind == FocusSupervisor && m.inputTarget != "" {
				var command tea.Cmd
				m.input, command = m.input.Update(msg)
				commands = append(commands, command)
				break
			}

			if paneIndex := m.focusedPaneIndex(); paneIndex >= 0 {
				if msg.Type == tea.KeyEnter {
					commands = append(commands, m.beginAttach(paneIndex))
					break
				}
				var command tea.Cmd
				m.panes[paneIndex].viewport, command = m.panes[paneIndex].viewport.Update(msg)
				commands = append(commands, command)
			}
		}
	case tea.MouseMsg:
		commands = append(commands, m.handleMouse(msg))
	case session.OutputAvailable:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			m.refreshPaneOutput(paneIndex)
		}
	case session.PromptDetected:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			if m.panes[paneIndex].backend == "tmux" {
				if snapshots, ok := m.backend.(PromptSnapshotBackend); ok {
					current, snapshotErr := snapshots.PendingPrompt(m.backend.Context(), msg.SessionID)
					if snapshotErr == nil && current == nil {
						// This event was queued before a completed attach resync and
						// no longer represents a prompt awaiting input.
						break
					}
					if snapshotErr == nil {
						msg = *current
					}
				}
			}
			m.refreshPaneOutput(paneIndex)
			if m.panes[paneIndex].exited {
				break
			}
			if !m.panes[paneIndex].blocked {
				m.panes[paneIndex].blocked = true
				m.panes[paneIndex].prompt = msg
				m.pending = append(m.pending, msg.SessionID)
				m.appendLog(fmt.Sprintf(
					"%s attend une intervention humaine (%s)",
					m.panes[paneIndex].name,
					msg.Description,
				))
			}
			if m.inputTarget == "" && !m.writePending {
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
			wasInputTarget := m.inputTarget == msg.SessionID
			m.removePending(msg.SessionID)
			m.panes[paneIndex].blocked = false
			m.panes[paneIndex].prompt = session.PromptDetected{}
			if wasInputTarget {
				m.inputTarget = ""
				// In particular, never carry a password into the next agent.
				m.input.Reset()
				commands = append(commands, m.activateNextPrompt())
			}
		}
	case session.Error:
		if paneIndex := m.paneIndex(msg.SessionID); paneIndex >= 0 {
			m.appendLog(fmt.Sprintf("Erreur terminal de %s: %v", m.panes[paneIndex].name, msg.Err))
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
				m.prependPending(msg.SessionID)
			}
		}
		commands = append(commands, m.activateNextPrompt())
	case attachFinishedMsg:
		if m.attachPending == msg.SessionID {
			m.attachPending = ""
		}
		paneIndex := m.paneIndex(msg.SessionID)
		if paneIndex < 0 {
			break
		}
		if msg.Err != nil {
			m.appendLog(fmt.Sprintf("Session tmux %s interrompue: %v", m.panes[paneIndex].name, msg.Err))
		} else {
			m.appendLog(fmt.Sprintf("Retour de la session tmux %s", m.panes[paneIndex].name))
		}
		attachable, ok := m.backend.(AttachableBackend)
		if !ok {
			m.appendLog("Resynchronisation tmux impossible: backend incompatible")
			break
		}
		commands = append(commands, resyncAttachedSession(
			m.backend.Context(),
			attachable,
			msg.SessionID,
			m.panes[paneIndex].viewport.Width,
			m.panes[paneIndex].viewport.Height,
		))
	case resyncFinishedMsg:
		paneIndex := m.paneIndex(msg.SessionID)
		if paneIndex < 0 {
			break
		}
		m.refreshPaneOutput(paneIndex)
		if msg.Err != nil {
			m.appendLog(fmt.Sprintf("Resynchronisation de %s impossible: %v", m.panes[paneIndex].name, msg.Err))
		} else {
			commands = append(commands, m.reconcilePrompt(msg.SessionID, msg.Prompt))
			m.appendLog(fmt.Sprintf("%s resynchronisé (sortie, état, prompts et taille)", m.panes[paneIndex].name))
		}
	case resizeFinishedMsg:
		m.resizeInFlight = false
		if m.backend.Context().Err() == nil {
			for _, failure := range msg.Failures {
				m.appendLog("Redimensionnement de " + failure.Name + " impossible: " + failure.Err.Error())
			}
		}
		if msg.Generation < m.resizeGeneration {
			if contextual, ok := m.backend.(ContextResizeBackend); ok {
				commands = append(commands, m.startResize(contextual))
			}
		}
	case backendStoppedMsg:
		return m, tea.Quit
	}

	return m, batchCommands(commands...)
}

func (m *Model) reconcilePrompt(sessionID string, prompt *session.PromptDetected) tea.Cmd {
	paneIndex := m.paneIndex(sessionID)
	if paneIndex < 0 {
		return nil
	}
	wasInputTarget := m.inputTarget == sessionID
	m.removePending(sessionID)
	m.panes[paneIndex].blocked = false
	m.panes[paneIndex].prompt = session.PromptDetected{}
	if wasInputTarget {
		m.inputTarget = ""
		m.input.Reset()
		m.input.Blur()
		setInputInterceptionStyle(&m.input, false)
	}
	if prompt != nil && !m.panes[paneIndex].exited {
		current := *prompt
		current.SessionID = sessionID
		m.panes[paneIndex].blocked = true
		m.panes[paneIndex].prompt = current
		m.pending = append(m.pending, sessionID)
	}
	if m.inputTarget == "" && !m.writePending {
		return m.activateNextPrompt()
	}
	return nil
}

func (m *Model) submitInput() tea.Cmd {
	paneIndex := m.paneIndex(m.inputTarget)
	if paneIndex < 0 {
		return nil
	}
	targetID := m.inputTarget
	prompt := m.panes[paneIndex].prompt
	value := m.input.Value()

	// Clear the old waiting state before the asynchronous write. This lets an
	// immediate second prompt from this session enter the queue.
	m.panes[paneIndex].blocked = false
	m.panes[paneIndex].prompt = session.PromptDetected{}
	m.removePending(targetID)
	m.inputTarget = ""
	m.input.Reset()
	m.input.Blur()
	setInputInterceptionStyle(&m.input, false)
	m.writePending = true
	m.appendLog(fmt.Sprintf("Réponse transmise à %s", m.panes[paneIndex].name))
	return deliverInput(m.backend, targetID, value, prompt)
}

func (m *Model) beginAttach(paneIndex int) tea.Cmd {
	pane := &m.panes[paneIndex]
	if pane.backend != "tmux" {
		return nil
	}
	if m.attachPending != "" {
		return nil
	}
	attachable, ok := m.backend.(AttachableBackend)
	if !ok {
		m.appendLog(fmt.Sprintf("Attachement de %s impossible: backend tmux incompatible", pane.name))
		return nil
	}
	command, err := attachable.AttachCommand(m.backend.Context(), pane.sessionID)
	if err != nil {
		m.appendLog(fmt.Sprintf("Attachement de %s impossible: %v", pane.name, err))
		return nil
	}
	if command == nil {
		m.appendLog(fmt.Sprintf("Attachement de %s impossible: commande vide", pane.name))
		return nil
	}
	sessionID := pane.sessionID
	m.attachPending = sessionID
	m.appendLog(fmt.Sprintf("Ouverture interactive de %s via tmux", pane.name))
	executor := m.execProcess
	if executor == nil {
		executor = tea.ExecProcess
	}
	return executor(command, func(err error) tea.Msg {
		return attachFinishedMsg{SessionID: sessionID, Err: err}
	})
}

func isViewportNavigationKey(message tea.KeyMsg) bool {
	switch message.Type {
	case tea.KeyUp, tea.KeyDown, tea.KeyPgUp, tea.KeyPgDown:
		return true
	default:
		return false
	}
}

func (m *Model) moveFocus(delta int) {
	position := len(m.panes)
	if paneIndex := m.focusedPaneIndex(); paneIndex >= 0 {
		position = paneIndex
	}
	position = (position + delta) % (len(m.panes) + 1)
	if position < 0 {
		position += len(m.panes) + 1
	}
	if position == len(m.panes) {
		m.focus = FocusTarget{Kind: FocusSupervisor}
		return
	}
	m.focus = FocusTarget{Kind: FocusAgent, AgentID: m.panes[position].sessionID}
	m.setPage(position / maxAgentsPerPage)
}

func (m *Model) movePage(delta int) {
	newPage := clampInt(m.page+delta, 0, pageCount(len(m.panes))-1)
	if newPage == m.page {
		return
	}
	m.setPage(newPage)
	if m.focus.Kind == FocusAgent && len(m.layout.Cells) > 0 {
		index := m.layout.Cells[0].AgentIndex
		m.focus.AgentID = m.panes[index].sessionID
	}
}

// handleMouse routes wheel events using the same cells that View renders and
// lets a left click select an agent or the supervisor explicitly.
func (m *Model) handleMouse(message tea.MouseMsg) tea.Cmd {
	event := tea.MouseEvent(message)
	if event.X < 0 || event.X >= m.width || event.Y < 0 || event.Y >= m.height {
		return nil
	}

	if event.Action == tea.MouseActionPress && event.Button == tea.MouseButtonLeft {
		for _, cell := range m.layout.Cells {
			if cell.Outer.Contains(event.X, event.Y) {
				m.focus = FocusTarget{Kind: FocusAgent, AgentID: m.panes[cell.AgentIndex].sessionID}
				m.input.Blur()
				return nil
			}
		}
		if m.layout.Supervisor.Contains(event.X, event.Y) {
			m.focus = FocusTarget{Kind: FocusSupervisor}
			return m.syncFocus()
		}
		return nil
	}

	if event.Action != tea.MouseActionPress ||
		(event.Button != tea.MouseButtonWheelUp && event.Button != tea.MouseButtonWheelDown) {
		return nil
	}
	for _, cell := range m.layout.Cells {
		if cell.Outer.Contains(event.X, event.Y) {
			var command tea.Cmd
			index := cell.AgentIndex
			m.panes[index].viewport, command = m.panes[index].viewport.Update(message)
			return command
		}
	}
	if m.layout.Supervisor.Contains(event.X, event.Y) {
		var command tea.Cmd
		m.supervisor, command = m.supervisor.Update(message)
		return command
	}
	return nil
}
