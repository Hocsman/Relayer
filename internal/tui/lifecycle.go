package tui

import (
	"fmt"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

// lifecycleConfirmation arms one destructive per-agent action for a short
// window. The second, identical keypress executes it; any other key disarms
// it, so a stopped or restarted agent is always a deliberate choice.
type lifecycleConfirmation struct {
	action    string
	sessionID string
	armedAt   time.Time
}

const lifecycleConfirmTimeout = 5 * time.Second

// lifecycleKeyAction maps a keypress to a per-agent lifecycle action. 'x'
// stops a running agent, 'r' restarts it (or starts it again once exited).
func lifecycleKeyAction(message tea.KeyMsg) (string, bool) {
	if message.Type != tea.KeyRunes || len(message.Runes) != 1 {
		return "", false
	}
	switch message.Runes[0] {
	case 'x', 'X':
		return "stop", true
	case 'r', 'R':
		return "restart", true
	default:
		return "", false
	}
}

// lifecycleConfirmKey reports whether a keypress repeats the armed action.
func lifecycleConfirmKey(message tea.KeyMsg, action string) bool {
	pressed, ok := lifecycleKeyAction(message)
	return ok && pressed == action
}

// disarmLifecycleConfirm clears an armed confirmation when any unrelated key
// arrives. Confirmation keys themselves keep the armed state so the second
// press can confirm.
func (m *Model) disarmLifecycleConfirm(message tea.KeyMsg) {
	if m.lifecycleConfirm.sessionID == "" {
		return
	}
	if lifecycleConfirmKey(message, m.lifecycleConfirm.action) {
		return
	}
	m.lifecycleConfirm = lifecycleConfirmation{}
}

// requestAgentLifecycle gates, arms, and — on the confirmed second press —
// dispatches one per-agent lifecycle command outside Update.
func (m *Model) requestAgentLifecycle(action string, paneIndex int) tea.Cmd {
	backend, ok := m.backend.(SessionLifecycleBackend)
	if !ok {
		m.appendLog("Per-agent lifecycle is not supported by this backend")
		return nil
	}
	pane := m.panes[paneIndex]
	if _, busy := m.lifecycleBusy[pane.sessionID]; busy {
		return nil
	}
	if action == "stop" && pane.exited {
		m.appendLog(fmt.Sprintf("%s already finished; press r to restart it", pane.name))
		return nil
	}
	if m.writePending && m.inputTarget == pane.sessionID {
		m.appendLog(fmt.Sprintf("%s is receiving an answer; retry once it settles", pane.name))
		return nil
	}
	if m.lineWritePending == pane.sessionID {
		m.appendLog(fmt.Sprintf("%s is receiving a line; retry once it settles", pane.name))
		return nil
	}
	if _, busy := m.automaticBySession[pane.sessionID]; busy {
		m.appendLog(fmt.Sprintf("%s is receiving an automatic decision; retry once it settles", pane.name))
		return nil
	}
	if m.attachPending == pane.sessionID {
		m.appendLog(fmt.Sprintf("%s is attaching to tmux; retry after it returns", pane.name))
		return nil
	}

	keyLabel := "x"
	if action == "restart" {
		keyLabel = "r"
	}
	confirm := m.lifecycleConfirm
	if confirm.action == action &&
		confirm.sessionID == pane.sessionID &&
		time.Since(confirm.armedAt) <= lifecycleConfirmTimeout {
		m.lifecycleConfirm = lifecycleConfirmation{}
		m.lifecycleBusy[pane.sessionID] = action
		m.appendLog(fmt.Sprintf("%s %s…", lifecycleVerb(action), pane.name))
		return runAgentLifecycle(backend, action, pane.sessionID, pane.name)
	}
	m.lifecycleConfirm = lifecycleConfirmation{
		action:    action,
		sessionID: pane.sessionID,
		armedAt:   time.Now(),
	}
	m.appendLog(fmt.Sprintf(
		"Press %s again within %ds to confirm %s of %s",
		keyLabel,
		int(lifecycleConfirmTimeout/time.Second),
		action,
		pane.name,
	))
	return nil
}

// applyLifecycleResult settles one finished lifecycle command. Stops converge
// through the backend's own process_exit event; restarts reset the pane so
// the fresh process presents a clean, unfrozen surface.
func (m *Model) applyLifecycleResult(message agentLifecycleMsg) tea.Cmd {
	delete(m.lifecycleBusy, message.SessionID)
	if message.Err != nil {
		m.appendLog(fmt.Sprintf("%s of %s failed: %v", message.Action, message.Name, message.Err))
		return nil
	}
	if message.Action == "stop" {
		m.appendLog(fmt.Sprintf("%s stopped by operator", message.Name))
		return nil
	}
	if paneIndex := m.paneIndex(message.SessionID); paneIndex >= 0 {
		pane := &m.panes[paneIndex]
		pane.exited = false
		pane.exitErr = nil
		pane.blocked = false
		pane.policyFrozen = false
		pane.policyTag = ""
		setViewportContent(&pane.viewport, "")
		m.removePending(message.SessionID)
		m.clearAutomaticState(message.SessionID)
		delete(m.deferredEvents, message.SessionID)
		delete(m.lineDeferredEvents, message.SessionID)
		if m.inputTarget == message.SessionID {
			m.inputTarget = ""
			m.input.Blur()
		}
		m.refreshPaneOutput(paneIndex)
	}
	if m.policyTracker != nil {
		// The restart is itself the human interaction: the fresh process
		// begins a new consecutive automatic-decision streak.
		m.policyTracker.Reset(message.SessionID)
	}
	m.appendLog(fmt.Sprintf("%s restarted by operator", message.Name))
	// The fresh PTY starts with the launch geometry; re-apply the current
	// layout so the pane's real cell size reaches it immediately.
	return m.resize(m.width, m.height, true)
}

func lifecycleVerb(action string) string {
	if action == "stop" {
		return "Stopping"
	}
	return "Restarting"
}

// runAgentLifecycle performs the blocking process work outside Update and
// reports the settled result back to the model.
func runAgentLifecycle(backend SessionLifecycleBackend, action, sessionID, name string) tea.Cmd {
	return func() tea.Msg {
		message := agentLifecycleMsg{Action: action, SessionID: sessionID, Name: name}
		switch action {
		case "stop":
			message.Err = backend.StopSession(sessionID)
		case "restart":
			message.Err = backend.RestartSession(sessionID)
		default:
			message.Err = fmt.Errorf("unknown lifecycle action %q", action)
		}
		return message
	}
}
