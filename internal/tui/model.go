package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/session"
	"github.com/charmbracelet/bubbles/textinput"
	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
)

const maxSystemLogLines = 200

// FocusKind makes the keyboard target explicit instead of overloading a pane
// index with a supervisor sentinel.
type FocusKind uint8

const (
	FocusAgent FocusKind = iota + 1
	FocusSupervisor
)

// FocusTarget identifies either an agent by its stable ID or the supervisor.
// AgentID is empty when Kind is FocusSupervisor.
type FocusTarget struct {
	Kind    FocusKind
	AgentID string
}

type agentPane struct {
	sessionID string
	name      string
	command   string
	shell     bool
	viewport  viewport.Model
	blocked   bool
	prompt    session.PromptDetected
	exited    bool
	exitErr   error
}

// Model is Relayer's Bubble Tea model. It uses a pointer receiver deliberately:
// viewport and pane slices must retain one identity as the number of agents and
// the visible page change.
type Model struct {
	backend Backend
	events  <-chan session.Event

	panes      []agentPane
	supervisor viewport.Model
	input      textinput.Model
	logs       []string

	width        int
	height       int
	layout       Geometry
	page         int
	focus        FocusTarget
	pending      []string
	inputTarget  string
	writePending bool
}

// NewModel builds a ready-to-run Bubble Tea model around one to eight existing
// sessions. Pane identity is copied and validated before any resize occurs.
func NewModel(
	backend Backend,
	events <-chan session.Event,
	panes []Pane,
	initialWidth int,
	initialHeight int,
	startupLogs []string,
) (*Model, error) {
	if backend == nil {
		return nil, errors.New("backend TUI nil")
	}
	if len(panes) < 1 || len(panes) > maxAgentCount {
		return nil, fmt.Errorf("la TUI exige entre 1 et %d agents (reçu: %d)", maxAgentCount, len(panes))
	}
	seen := make([]string, 0, len(panes))
	for index, pane := range panes {
		if strings.TrimSpace(pane.ID) == "" {
			return nil, fmt.Errorf("panneau %d: ID vide", index+1)
		}
		for _, existingID := range seen {
			if strings.EqualFold(existingID, pane.ID) {
				return nil, fmt.Errorf("ID de panneau dupliqué: %q", pane.ID)
			}
		}
		seen = append(seen, pane.ID)
	}

	input := textinput.New()
	input.Prompt = "› "
	input.Placeholder = "En attente d'une validation interactive…"
	input.CharLimit = 4096
	input.Blur()
	setInputInterceptionStyle(&input, false)

	result := &Model{
		backend:     backend,
		events:      events,
		panes:       make([]agentPane, len(panes)),
		supervisor:  viewport.New(1, 1),
		input:       input,
		focus:       FocusTarget{Kind: FocusAgent, AgentID: panes[0].ID},
		inputTarget: "",
	}
	for index, pane := range panes {
		name := pane.Name
		if strings.TrimSpace(name) == "" {
			name = pane.ID
		}
		result.panes[index] = agentPane{
			sessionID: pane.ID,
			name:      name,
			command:   pane.Command,
			shell:     pane.Shell,
			viewport:  viewport.New(1, 1),
		}
	}
	result.appendLog(fmt.Sprintf("Relayer démarré avec %d session(s) PTY", len(panes)))
	for _, message := range startupLogs {
		result.appendLog(message)
	}
	result.resize(initialWidth, initialHeight)
	return result, nil
}

func (m *Model) Init() tea.Cmd {
	return waitForBackendEvent(m.backend.Context(), m.events)
}

func (m *Model) paneIndex(sessionID string) int {
	for index := range m.panes {
		if m.panes[index].sessionID == sessionID {
			return index
		}
	}
	return -1
}

func (m *Model) focusedPaneIndex() int {
	if m.focus.Kind != FocusAgent {
		return -1
	}
	return m.paneIndex(m.focus.AgentID)
}

func (m *Model) activateNextPrompt() tea.Cmd {
	for len(m.pending) > 0 && m.paneIndex(m.pending[0]) < 0 {
		m.pending = m.pending[1:]
	}
	if len(m.pending) == 0 {
		m.inputTarget = ""
		m.input.Blur()
		m.input.EchoMode = textinput.EchoNormal
		m.input.Placeholder = "En attente d'une validation interactive…"
		setInputInterceptionStyle(&m.input, false)
		if m.focus.Kind == FocusSupervisor && len(m.panes) > 0 {
			m.focus = FocusTarget{Kind: FocusAgent, AgentID: m.panes[0].sessionID}
			m.setPage(0)
		}
		return nil
	}

	m.inputTarget = m.pending[0]
	targetIndex := m.paneIndex(m.inputTarget)
	m.setPage(targetIndex / maxAgentsPerPage)
	m.focus = FocusTarget{Kind: FocusSupervisor}
	target := &m.panes[targetIndex]
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
	if m.focus.Kind == FocusSupervisor && m.inputTarget != "" {
		return m.input.Focus()
	}
	m.input.Blur()
	return nil
}

func (m *Model) removePending(sessionID string) {
	filtered := m.pending[:0]
	for _, pendingID := range m.pending {
		if pendingID != sessionID {
			filtered = append(filtered, pendingID)
		}
	}
	m.pending = filtered
}

func (m *Model) prependPending(sessionID string) {
	for _, pendingID := range m.pending {
		if pendingID == sessionID {
			return
		}
	}
	m.pending = append([]string{sessionID}, m.pending...)
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

func (m *Model) hasBlockedPane() bool {
	for _, pane := range m.panes {
		if pane.blocked {
			return true
		}
	}
	return false
}

func (m *Model) setPage(page int) {
	m.page = clampInt(page, 0, pageCount(len(m.panes))-1)
	m.layout = CalculateLayout(m.width, m.height, len(m.panes), m.page)
}
