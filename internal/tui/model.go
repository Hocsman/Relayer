package tui

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
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
	backend   string
	adapter   string
	shell     bool
	viewport  viewport.Model
	blocked   bool
	prompt    adapters.Event
	exited    bool
	exitErr   error
	policyTag string
	// policyFrozen prevents a second write after a transport error that may
	// have occurred after partial or complete delivery.
	policyFrozen bool
}

type eventKey struct {
	sessionID string
	eventID   string
}

type automaticAttempt struct {
	event      adapters.Event
	evaluation policy.Evaluation
}

// Model is Relayer's Bubble Tea model. It uses a pointer receiver deliberately:
// viewport and pane slices must retain one identity as the number of agents and
// the visible page change.
type Model struct {
	backend      Backend
	events       <-chan session.Event
	policy       PolicyEvaluator
	policyConfig policy.Config
	auditor      *audit.Recorder
	auditGate    *deliveryGate
	// auditUnavailable is terminal for this Model. Once a synchronous audit
	// write fails, no further decision or attachment may reach a backend.
	auditUnavailable bool

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
	// lineInputTarget owns the shared text field while an operator composes a
	// direct line for one focused agent. lineWritePending keeps at most one
	// asynchronous line delivery in flight across the TUI.
	lineInputTarget  string
	lineWritePending string
	// lineDeferredEvents holds the latest canonical prompt observed while a
	// direct line still awaits its terminal audit result. Policy must not run
	// before that result is reduced by Update.
	lineDeferredEvents map[string]adapters.Event
	attachPending      string
	// attachFinishedAudited prevents a failed terminal client followed by a
	// Resync callback from producing two terminal records for one attachment.
	attachFinishedAudited bool
	attachReturned        bool
	execProcess           execProcessFunc

	// lastTitleCount edge-triggers the terminal window title so eight
	// simultaneous prompts produce one title change rather than eight.
	lastTitleCount   int
	titleInitialized bool

	resizeGeneration uint64
	resizeInFlight   bool
	resizeRequests   []resizeRequest

	resolvedEventIDs   map[eventKey]struct{}
	resolvedEventOrder []eventKey
	automaticInFlight  map[eventKey]automaticAttempt
	automaticBySession map[string]eventKey
	deferredEvents     map[string]adapters.Event
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
	defaultPolicy, err := policy.New(policy.DefaultConfig())
	if err != nil {
		return nil, err
	}
	return NewModelWithPolicy(
		backend,
		events,
		panes,
		initialWidth,
		initialHeight,
		startupLogs,
		defaultPolicy,
	)
}

// NewModelWithPolicy adds pure policy evaluation while keeping NewModel's
// historical, safe ask-by-default behavior for existing callers.
func NewModelWithPolicy(
	backend Backend,
	events <-chan session.Event,
	panes []Pane,
	initialWidth int,
	initialHeight int,
	startupLogs []string,
	evaluator PolicyEvaluator,
) (*Model, error) {
	auditor, err := newDisabledAuditRecorder()
	if err != nil {
		return nil, err
	}
	return NewModelWithPolicyAndAudit(
		backend,
		events,
		panes,
		initialWidth,
		initialHeight,
		startupLogs,
		evaluator,
		auditor,
	)
}

// NewModelWithPolicyAndAudit enables the synchronous local audit trail. The
// caller retains ownership of auditor and must close it after Bubble Tea and
// the backends have completed their lifecycle.
func NewModelWithPolicyAndAudit(
	backend Backend,
	events <-chan session.Event,
	panes []Pane,
	initialWidth int,
	initialHeight int,
	startupLogs []string,
	evaluator PolicyEvaluator,
	auditor *audit.Recorder,
) (*Model, error) {
	if backend == nil {
		return nil, errors.New("backend TUI nil")
	}
	if evaluator == nil {
		return nil, errors.New("moteur de politique TUI nil")
	}
	if auditor == nil {
		return nil, errors.New("enregistreur d'audit TUI nil")
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
		backend:            backend,
		events:             events,
		policy:             evaluator,
		policyConfig:       evaluator.Config(),
		auditor:            auditor,
		auditGate:          newDeliveryGate(),
		panes:              make([]agentPane, len(panes)),
		supervisor:         viewport.New(1, 1),
		input:              input,
		focus:              FocusTarget{Kind: FocusAgent, AgentID: panes[0].ID},
		inputTarget:        "",
		execProcess:        tea.ExecProcess,
		resolvedEventIDs:   make(map[eventKey]struct{}),
		automaticInFlight:  make(map[eventKey]automaticAttempt),
		automaticBySession: make(map[string]eventKey),
		deferredEvents:     make(map[string]adapters.Event),
		lineDeferredEvents: make(map[string]adapters.Event),
	}
	for index, pane := range panes {
		name := pane.Name
		if strings.TrimSpace(name) == "" {
			name = pane.ID
		}
		backendName := strings.ToLower(strings.TrimSpace(pane.Backend))
		if backendName == "" {
			if named, ok := backend.(interface{ Name() string }); ok {
				backendName = strings.ToLower(strings.TrimSpace(named.Name()))
			}
		}
		if backendName == "" {
			backendName = "pty"
		}
		adapterName := strings.ToLower(strings.TrimSpace(pane.Adapter))
		if adapterName == "" {
			adapterName = adapters.GenericID
		}
		result.panes[index] = agentPane{
			sessionID: pane.ID,
			name:      name,
			command:   pane.Command,
			backend:   backendName,
			adapter:   adapterName,
			shell:     pane.Shell,
			viewport:  viewport.New(1, 1),
		}
	}
	result.appendLog(fmt.Sprintf(
		"Relayer démarré avec %d session(s) • backend %s",
		len(panes),
		result.backendLabel(),
	))
	for _, message := range startupLogs {
		result.appendLog(message)
	}
	// The application already starts every terminal at this exact geometry.
	// A production context-aware backend waits for Bubble Tea's authoritative
	// WindowSizeMsg before scheduling an asynchronous resize batch; legacy/test
	// backends retain their synchronous initialization behavior.
	_ = result.resize(initialWidth, initialHeight, false)
	return result, nil
}

func semanticEventKey(sessionID, eventID string) eventKey {
	return eventKey{
		sessionID: strings.ToLower(strings.TrimSpace(sessionID)),
		eventID:   strings.TrimSpace(eventID),
	}
}

func (m *Model) eventResolved(sessionID, eventID string) bool {
	key := semanticEventKey(sessionID, eventID)
	_, resolved := m.resolvedEventIDs[key]
	return key.sessionID != "" && key.eventID != "" && resolved
}

func (m *Model) rememberResolved(sessionID, eventID string) {
	key := semanticEventKey(sessionID, eventID)
	if key.sessionID == "" || key.eventID == "" || m.eventResolved(sessionID, eventID) {
		return
	}
	const retainedResolvedEvents = 256
	m.resolvedEventIDs[key] = struct{}{}
	m.resolvedEventOrder = append(m.resolvedEventOrder, key)
	if len(m.resolvedEventOrder) <= retainedResolvedEvents {
		return
	}
	oldest := m.resolvedEventOrder[0]
	m.resolvedEventOrder = m.resolvedEventOrder[1:]
	delete(m.resolvedEventIDs, oldest)
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
	if m.auditUnavailable {
		m.pending = nil
		m.inputTarget = ""
		m.lineInputTarget = ""
		m.input.Reset()
		m.input.Blur()
		setInputInterceptionStyle(&m.input, false)
		return nil
	}
	for len(m.pending) > 0 && m.paneIndex(m.pending[0]) < 0 {
		m.pending = m.pending[1:]
	}
	if len(m.pending) == 0 {
		m.inputTarget = ""
		if m.lineInputTarget != "" {
			return m.input.Focus()
		}
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
	if m.lineInputTarget != "" {
		m.cancelLineInputForPrompt()
	}
	targetIndex := m.paneIndex(m.inputTarget)
	m.setPage(targetIndex / maxAgentsPerPage)
	m.focus = FocusTarget{Kind: FocusSupervisor}
	target := &m.panes[targetIndex]
	if requiresSecretHandling(target.prompt) {
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
	if (m.focus.Kind == FocusSupervisor && m.inputTarget != "") || m.lineInputTarget != "" {
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

func (m *Model) backendLabel() string {
	names := make([]string, 0, len(m.panes))
	for _, pane := range m.panes {
		name := strings.ToUpper(pane.backend)
		found := false
		for _, existing := range names {
			if existing == name {
				found = true
				break
			}
		}
		if !found {
			names = append(names, name)
		}
	}
	return strings.Join(names, "/")
}

func (m *Model) setPage(page int) {
	m.page = clampInt(page, 0, pageCount(len(m.panes))-1)
	m.layout = CalculateLayout(m.width, m.height, len(m.panes), m.page)
}
