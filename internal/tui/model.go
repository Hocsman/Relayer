package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/session"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

const maxSystemLogLines = 200

type agentPane struct {
	sessionID int
	name      string
	command   string
	viewport  viewport.Model
	blocked   bool
	prompt    session.PromptDetected
	exited    bool
	exitErr   error
}

// Model is Relayer's Bubble Tea model. Its state is deliberately private so
// all mutations continue to flow through Update.
type Model struct {
	backend Backend
	events  <-chan session.Event

	panes      [2]agentPane
	supervisor viewport.Model
	input      textinput.Model
	logs       []string

	width            int
	height           int
	leftWidth        int
	rightWidth       int
	topHeight        int
	supervisorHeight int
	activePanel      int
	pending          []int
	inputTarget      int
	writePending     bool
}

// NewModel builds a ready-to-run Bubble Tea model around two existing panes.
// It immediately applies the initial dimensions so the backend PTYs and the
// visible viewports start with identical geometry.
func NewModel(
	backend Backend,
	events <-chan session.Event,
	panes [2]Pane,
	initialWidth int,
	initialHeight int,
	startupLogs []string,
) Model {
	input := textinput.New()
	input.Prompt = "› "
	input.Placeholder = "En attente d'une validation interactive…"
	input.CharLimit = 4096
	input.Blur()
	setInputInterceptionStyle(&input, false)

	result := Model{
		backend:     backend,
		events:      events,
		supervisor:  viewport.New(1, 1),
		input:       input,
		activePanel: 0,
		inputTarget: -1,
	}
	for index, pane := range panes {
		result.panes[index] = agentPane{
			sessionID: pane.ID,
			name:      pane.Name,
			command:   pane.Command,
			viewport:  viewport.New(1, 1),
		}
	}
	result.appendLog("Relayer démarré avec deux sessions PTY")
	for _, message := range startupLogs {
		result.appendLog(message)
	}
	result.resize(initialWidth, initialHeight)
	return result
}

func (m Model) Init() tea.Cmd {
	return waitForBackendEvent(m.backend.Context(), m.events)
}

func (m *Model) paneIndex(sessionID int) int {
	for index := range m.panes {
		if m.panes[index].sessionID == sessionID {
			return index
		}
	}
	return -1
}

func (m *Model) activateNextPrompt() tea.Cmd {
	if len(m.pending) == 0 {
		m.inputTarget = -1
		m.input.Blur()
		m.input.EchoMode = textinput.EchoNormal
		m.input.Placeholder = "En attente d'une validation interactive…"
		setInputInterceptionStyle(&m.input, false)
		if m.activePanel == 2 {
			m.activePanel = 0
		}
		return nil
	}

	m.inputTarget = m.pending[0]
	m.activePanel = 2
	target := &m.panes[m.inputTarget]
	if target.prompt.Sensitive {
		m.input.EchoMode = textinput.EchoPassword
		m.input.EchoCharacter = '•'
	} else {
		m.input.EchoMode = textinput.EchoNormal
	}
	m.input.Placeholder = fmt.Sprintf("Réponse pour %s (Entrée pour envoyer)", target.name)
	setInputInterceptionStyle(&m.input, true)
	return m.input.Focus()
}

func (m *Model) syncFocus() tea.Cmd {
	if m.activePanel == 2 && m.inputTarget >= 0 {
		return m.input.Focus()
	}
	m.input.Blur()
	return nil
}

func (m *Model) removePending(paneIndex int) {
	filtered := m.pending[:0]
	for _, pendingIndex := range m.pending {
		if pendingIndex != paneIndex {
			filtered = append(filtered, pendingIndex)
		}
	}
	m.pending = filtered
}

func (m *Model) prependPending(paneIndex int) {
	for _, pendingIndex := range m.pending {
		if pendingIndex == paneIndex {
			return
		}
	}
	m.pending = append([]int{paneIndex}, m.pending...)
}

func (m *Model) refreshPaneOutput(paneIndex int) {
	content, err := m.backend.Output(m.panes[paneIndex].sessionID)
	if err != nil {
		return
	}
	setViewportContent(&m.panes[paneIndex].viewport, content)
}

// setViewportContent follows new output only while the user is already at the
// bottom. Once they scroll up, Bubble Tea keeps the same Y offset as new output
// arrives. If bounded history evicts that position, SetContent safely clamps it.
func setViewportContent(target *viewport.Model, content string) {
	wasAtBottom := target.AtBottom()
	previousOffset := target.YOffset
	target.SetContent(content)
	if wasAtBottom {
		target.GotoBottom()
		return
	}
	target.SetYOffset(previousOffset)
}

func (m *Model) appendLog(message string) {
	line := fmt.Sprintf("%s  %s", time.Now().Format("15:04:05"), message)
	m.logs = append(m.logs, line)
	if len(m.logs) > maxSystemLogLines {
		m.logs = append([]string(nil), m.logs[len(m.logs)-maxSystemLogLines:]...)
	}
	setViewportContent(&m.supervisor, strings.Join(m.logs, "\n"))
}

func (m Model) hasBlockedPane() bool {
	for _, pane := range m.panes {
		if pane.blocked {
			return true
		}
	}
	return false
}
