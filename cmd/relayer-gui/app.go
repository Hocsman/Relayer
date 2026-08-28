package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"

	"github.com/Hocsman/Relayer/internal/adapters"
	appcore "github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/preflight"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	eventSnapshot     = "relayer:snapshot"
	eventSemantic     = "relayer:event"
	eventStatus       = "relayer:status"
	eventError        = "relayer:error"
	maxResolvedEvents = 1024
	outputFrameDelay  = 40 * time.Millisecond
)

var (
	errAuditUnavailable    = errors.New("The audit journal is unavailable. No decision was sent.")
	errDecisionStale       = errors.New("This request is no longer the event currently awaited.")
	errDecisionInFlight    = errors.New("A decision is already in progress for this agent.")
	errEmptyDecision       = errors.New("An empty answer is not a decision.")
	errUnsupportedDecision = errors.New("This answer cannot be encoded for this request.")
	errDeliveryUncertain   = errors.New("The delivery state is indeterminate. Stop the session before any further input.")
	errLineInFlight        = errors.New("A line is already being delivered to this agent.")
	errLinePromptPending   = errors.New("A supervision request must be answered before any free-text input.")
	errLineUnavailable     = errors.New("The session does not accept free-text input in its current state.")
	errLineInvalid         = errors.New("The line entered is invalid.")
	errLineUnsupported     = errors.New("This backend does not support free-text input.")
	errRuntimeStopped      = errors.New("The Relayer engine is stopped.")
	errRunStale            = errors.New("This Relayer run is no longer active.")
)

type eventKey struct {
	sessionID string
	eventID   string
}

type pendingEvent struct {
	event adapters.Event
	view  SupervisionEvent
}

type desktopEngine interface {
	Metadata() appcore.DesktopMetadata
	Sessions() []appcore.DesktopSession
	StartupLogs() []string
	SupportedDecisions(adapters.Event) []adapters.Decision
	Events() <-chan session.Event
	Output(string) (string, error)
	PendingEvent(context.Context, string) (*adapters.Event, error)
	Evaluate(adapters.Event) policy.Evaluation
	ApplyDecision(context.Context, string, adapters.Event, adapters.Decision, string) error
	SendLine(context.Context, string, string) error
	Resize(context.Context, string, terminal.Size) error
	Stop(context.Context, string) error
	RecordAudit(audit.Entry) error
	BeginShutdown(context.Context) error
	BeginRestart(context.Context) error
	Close(context.Context) error
}

type runGeneration struct {
	id         string
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
	engine     desktopEngine
	plan       *appcore.DesktopPlan
}

// App is the narrow Wails bridge. The browser receives display-safe DTOs and
// stable operation identifiers; semantic events and decisions remain in Go.
type App struct {
	ctx    context.Context // Wails window lifecycle, never a per-run context.
	engine desktopEngine   // compatibility alias for the active generation.
	active *runGeneration
	emitFn func(context.Context, string, ...interface{})

	lifecycleMu      sync.Mutex
	nextGeneration   uint64
	finalShutdown    bool
	lifecycleBlocked bool
	prepareEngine    func(appcore.DesktopOptions) (*appcore.DesktopPlan, error)
	startEngine      func(context.Context, *appcore.DesktopPlan, string) (desktopEngine, error)
	runIDGenerator   func() (string, error)
	runPreflight     func(context.Context, appcore.PreflightOptions) (preflight.Report, error)

	mu               sync.RWMutex
	state            AppState
	agentIndex       map[string]int
	pending          map[eventKey]pendingEvent
	ingesting        map[eventKey]struct{}
	resolved         map[eventKey]struct{}
	resolvedOrder    []eventKey
	inFlight         map[string]eventKey
	lineInFlight     map[string]bool
	stoppingSessions map[string]bool
	outputRunning    map[string]bool
	outputDirty      map[string]bool
	frozen           map[string]bool
	auditFailed      bool
	shuttingDown     bool
	startupErr       error
	configPath       string

	deliveryMu        sync.Mutex
	deliveryAvailable bool
	deliveryWG        sync.WaitGroup

	eventWG sync.WaitGroup

	profilesMu            sync.Mutex
	activeConfigRevision  string
	profileRevisionHash   string
	profileRevisionToken  string
	profileDetector       toolcatalog.Detector
	profileTokenGenerator func() (string, error)

	shutdownOnce sync.Once
	shutdownDone chan struct{}
	shutdownErr  error
}

func NewApp() *App {
	return &App{
		state: AppState{
			RunStatus:     "idle",
			Policy:        PolicyState{DefaultAction: "ask"},
			Audit:         AuditState{Status: "disabled", Mode: "off"},
			Agents:        []AgentState{},
			PendingEvents: []SupervisionEvent{},
		},
		agentIndex:            make(map[string]int),
		pending:               make(map[eventKey]pendingEvent),
		ingesting:             make(map[eventKey]struct{}),
		resolved:              make(map[eventKey]struct{}),
		inFlight:              make(map[string]eventKey),
		lineInFlight:          make(map[string]bool),
		stoppingSessions:      make(map[string]bool),
		outputRunning:         make(map[string]bool),
		outputDirty:           make(map[string]bool),
		frozen:                make(map[string]bool),
		deliveryAvailable:     true,
		shutdownDone:          make(chan struct{}),
		profileDetector:       toolcatalog.DefaultDetector(),
		profileTokenGenerator: newOpaqueProfileToken,
		prepareEngine:         appcore.PrepareDesktopRuntime,
		startEngine: func(ctx context.Context, plan *appcore.DesktopPlan, runID string) (desktopEngine, error) {
			return appcore.StartDesktopRuntime(ctx, plan, runID)
		},
		runIDGenerator: newOpaqueProfileToken,
		runPreflight:   appcore.RunPreflight,
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx = ctx
	a.emitFn = wailsruntime.EventsEmit

	configPath, err := desktopConfigPath()
	if err != nil {
		a.failStartup(errors.New(safeDisplayError(err)))
		return
	}
	a.profilesMu.Lock()
	a.configPath = configPath
	a.profilesMu.Unlock()
	a.mu.Lock()
	a.state.RunStatus = "idle"
	a.mu.Unlock()
}

func (a *App) failStartup(err error) {
	a.mu.Lock()
	a.startupErr = err
	a.state.RunStatus = "failed"
	a.mu.Unlock()
}

func desktopConfigPath() (string, error) {
	if explicit := strings.TrimSpace(os.Getenv("RELAYER_CONFIG")); explicit != "" {
		return filepath.Clean(explicit), nil
	}
	root, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(root, "relayer", config.DefaultPath), nil
}

// initializeState remains a compact test helper for an already-created
// engine. Production generations are activated through activateRun.
func (a *App) initializeState(engine desktopEngine) {
	metadata := engine.Metadata()
	ctx, cancel := context.WithCancel(context.Background())
	run := &runGeneration{
		id:         metadata.RunID,
		generation: 1,
		ctx:        ctx,
		cancel:     cancel,
		engine:     engine,
	}
	if strings.TrimSpace(run.id) == "" {
		run.id = "test-run"
	}
	a.activateRun(run)
}

func (a *App) activateRun(run *runGeneration) {
	if run == nil || run.engine == nil {
		return
	}
	engine := run.engine
	metadata := engine.Metadata()
	sessions := engine.Sessions()
	agents := make([]AgentState, 0, len(sessions))
	index := make(map[string]int, len(sessions))
	for _, item := range sessions {
		output, _ := engine.Output(item.ID)
		index[strings.ToLower(item.ID)] = len(agents)
		agents = append(agents, AgentState{
			SessionID:      item.ID,
			AgentID:        item.ID,
			Name:           item.Name,
			DisplayCommand: item.Command,
			Backend:        item.Backend,
			Adapter:        item.Adapter,
			Status:         "running",
			Output:         output,
			Revision:       1,
			Running:        true,
			Simulated:      item.Simulated,
		})
	}
	a.mu.Lock()
	a.active = run
	a.engine = engine
	a.agentIndex = index
	a.pending = make(map[eventKey]pendingEvent)
	a.ingesting = make(map[eventKey]struct{})
	a.resolved = make(map[eventKey]struct{})
	a.resolvedOrder = nil
	a.inFlight = make(map[string]eventKey)
	a.lineInFlight = make(map[string]bool)
	a.stoppingSessions = make(map[string]bool)
	a.outputRunning = make(map[string]bool)
	a.outputDirty = make(map[string]bool)
	a.frozen = make(map[string]bool)
	a.auditFailed = false
	a.shuttingDown = false
	a.agentIndex = index
	a.state = AppState{
		RunID:     run.id,
		RunStatus: "running",
		StartedAt: time.Now().UTC().Format(time.RFC3339Nano),
		Policy: PolicyState{
			DefaultAction: metadata.PolicyAction,
			DryRun:        metadata.PolicyDryRun,
		},
		Audit: AuditState{
			Enabled: metadata.AuditEnabled,
			Mode:    metadata.AuditMode,
			Status:  map[bool]string{true: "ready", false: "disabled"}[metadata.AuditEnabled],
			Path:    metadata.AuditPath,
		},
		Agents:        agents,
		Notices:       safeNotices(engine.StartupLogs()),
		PendingEvents: []SupervisionEvent{},
	}
	a.mu.Unlock()
	a.openDelivery()
}

func (a *App) isActiveRun(run *runGeneration) bool {
	if run == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.active == run && !a.shuttingDown
}

func (a *App) activeRun(expectedRunID string) (*runGeneration, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.active == nil || a.active.engine == nil || a.shuttingDown {
		return nil, errRuntimeStopped
	}
	if strings.TrimSpace(expectedRunID) == "" || expectedRunID != a.active.id {
		return nil, errRunStale
	}
	return a.active, nil
}

func (a *App) consumeEvents(run *runGeneration) {
	defer a.eventWG.Done()
	events := run.engine.Events()
	for {
		select {
		case <-run.ctx.Done():
			return
		case message, open := <-events:
			if !open {
				return
			}
			switch value := message.(type) {
			case session.OutputAvailable:
				a.scheduleOutputRefresh(run, value.SessionID)
			case session.AdapterEvent:
				a.handleAdapterEventForRun(run, value.Event.Clone())
			case session.Error:
				a.markSessionError(run, value.SessionID, "backend_stream_failed")
			case session.Exited:
				a.markLegacyExit(run, value.SessionID)
			}
		}
	}
}

func (a *App) scheduleOutputRefresh(run *runGeneration, sessionID string) {
	key := strings.ToLower(strings.TrimSpace(sessionID))
	if key == "" {
		return
	}
	a.mu.Lock()
	if a.shuttingDown || a.active != run {
		a.mu.Unlock()
		return
	}
	if a.outputRunning[key] {
		a.outputDirty[key] = true
		a.mu.Unlock()
		return
	}
	a.outputRunning[key] = true
	a.outputDirty[key] = false
	a.eventWG.Add(1)
	a.mu.Unlock()

	go func() {
		defer a.eventWG.Done()
		for {
			timer := time.NewTimer(outputFrameDelay)
			select {
			case <-run.ctx.Done():
				if !timer.Stop() {
					select {
					case <-timer.C:
					default:
					}
				}
				a.mu.Lock()
				delete(a.outputRunning, key)
				delete(a.outputDirty, key)
				a.mu.Unlock()
				return
			case <-timer.C:
			}
			a.refreshOutputForRun(run, sessionID)
			a.mu.Lock()
			if a.outputDirty[key] && !a.shuttingDown && a.active == run {
				a.outputDirty[key] = false
				a.mu.Unlock()
				continue
			}
			delete(a.outputRunning, key)
			delete(a.outputDirty, key)
			a.mu.Unlock()
			return
		}
	}()
}

// GetState returns a deep display snapshot. A startup failure rejects the
// promise with a redacted local message rather than silently starting a demo.
func (a *App) GetState() (AppState, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.startupErr != nil {
		return AppState{}, a.startupErr
	}
	return cloneAppState(a.state), nil
}

func (a *App) refreshOutput(sessionID string) {
	a.mu.RLock()
	run := a.active
	a.mu.RUnlock()
	if run != nil {
		a.refreshOutputForRun(run, sessionID)
	}
}

func (a *App) refreshOutputForRun(run *runGeneration, sessionID string) {
	if !a.isActiveRun(run) {
		return
	}
	output, err := run.engine.Output(sessionID)
	if err != nil {
		a.emitSafeError(run, "output_refresh_failed", "The bounded output of the session could not be refreshed.", sessionID)
		return
	}
	a.mu.Lock()
	index, found := a.agentIndex[strings.ToLower(sessionID)]
	if !found {
		a.mu.Unlock()
		return
	}
	agent := &a.state.Agents[index]
	agent.Output = output
	agent.Revision++
	payload := snapshotFromAgent(run.id, *agent)
	a.mu.Unlock()
	a.emit(eventSnapshot, payload)
}

func (a *App) handleAdapterEvent(event adapters.Event) {
	a.mu.RLock()
	run := a.active
	a.mu.RUnlock()
	if run != nil {
		a.handleAdapterEventForRun(run, event)
	}
}

func (a *App) handleAdapterEventForRun(run *runGeneration, event adapters.Event) {
	if !a.isActiveRun(run) {
		return
	}
	key := makeEventKey(event.SessionID, event.ID)
	if key.sessionID == "" || key.eventID == "" {
		a.emitSafeError(run, "invalid_event", "An invalid event was ignored.", event.SessionID)
		return
	}
	if !a.reserveEvent(key) {
		return
	}
	defer a.releaseEventReservation(key)

	backend := a.backendFor(run, event.SessionID)
	if event.Type == adapters.EventProcessExit {
		a.handleProcessExit(run, event, backend)
		return
	}
	if !event.Actionable() {
		return
	}
	if !a.sessionRunning(event.SessionID) {
		a.mu.Lock()
		a.markResolvedLocked(key)
		a.mu.Unlock()
		return
	}
	// OutputAvailable is intentionally coalescable. Refreshing here guarantees
	// that an essential semantic event still brings the latest bounded tail to
	// the WebView even when its preceding output invalidation was dropped.
	a.refreshOutputForRun(run, event.SessionID)
	if !a.recordAudit(run, eventDetectedEntry(event, backend)) {
		a.addFrozenEvent(run, event, policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionAsk, Reason: policy.ReasonNoEngine})
		return
	}
	evaluation := run.engine.Evaluate(event)
	if !a.recordAudit(run, policyAuditEntry(event, backend, evaluation)) {
		a.addFrozenEvent(run, event, evaluation)
		return
	}

	view := supervisionView(run.id, event, evaluation, "pending", run.engine.SupportedDecisions(event))
	a.mu.Lock()
	a.pending[key] = pendingEvent{event: event.Clone(), view: view}
	a.setAgentWaitingLocked(event.SessionID)
	a.rebuildPendingLocked()
	a.mu.Unlock()
	a.emit(eventSemantic, view)
	a.scheduleAutomatic(run, event.SessionID)
}

func (a *App) sessionRunning(sessionID string) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	index, found := a.agentIndex[strings.ToLower(strings.TrimSpace(sessionID))]
	return found && a.state.Agents[index].Running
}

func (a *App) reserveEvent(key eventKey) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	if _, exists := a.resolved[key]; exists {
		return false
	}
	if _, exists := a.pending[key]; exists {
		return false
	}
	if _, exists := a.ingesting[key]; exists {
		return false
	}
	a.ingesting[key] = struct{}{}
	return true
}

func (a *App) releaseEventReservation(key eventKey) {
	a.mu.Lock()
	delete(a.ingesting, key)
	a.mu.Unlock()
}

func eventDetectedEntry(event adapters.Event, backend string) audit.Entry {
	entry := eventAuditEntry(audit.KindEventDetected, event, backend)
	entry.Outcome = audit.OutcomeDetected
	entry.Reason = "event_detected"
	return entry
}

func (a *App) handleProcessExit(run *runGeneration, event adapters.Event, backend string) {
	key := makeEventKey(event.SessionID, event.ID)
	// Lifecycle state still has to converge even when audit has failed, so the
	// result is deliberately ignored rather than short-circuiting the exit.
	_ = a.recordAudit(run, eventDetectedEntry(event, backend))
	finished := eventAuditEntry(audit.KindSessionFinished, event, backend)
	finished.Outcome = audit.OutcomeFinished
	if event.Metadata["failed"] == "true" {
		finished.Outcome = audit.OutcomeFailed
	}
	finished.Reason = "process_exit"
	_ = a.recordAudit(run, finished)

	a.mu.Lock()
	if _, duplicate := a.resolved[key]; duplicate {
		a.mu.Unlock()
		return
	}
	a.markResolvedLocked(key)
	index, found := a.agentIndex[strings.ToLower(event.SessionID)]
	if found {
		agent := &a.state.Agents[index]
		agent.Running = false
		agent.Attached = false
		agent.Status = "exited"
		if event.Metadata["failed"] == "true" {
			agent.Status = "failed"
		}
		if value := strings.TrimSpace(event.Metadata["exit_code"]); value != "" {
			if code, err := strconv.Atoi(value); err == nil {
				agent.ExitCode = &code
			}
		}
	}
	a.clearSessionPendingLocked(event.SessionID)
	a.rebuildPendingLocked()
	status := "exited"
	if found {
		status = a.state.Agents[index].Status
	}
	a.mu.Unlock()
	a.refreshOutputForRun(run, event.SessionID)
	a.emit(eventStatus, StatusEvent{RunID: run.id, Scope: "session", SessionID: event.SessionID, Status: status})
}

// scheduleAutomatic serialises every automatic decision for a session and
// never overtakes an earlier human prompt. The reservation and WaitGroup Add
// happen under a.mu so Shutdown cannot begin waiting between those steps.
func (a *App) scheduleAutomatic(run *runGeneration, sessionID string) {
	sessionKey := strings.ToLower(strings.TrimSpace(sessionID))
	if sessionKey == "" {
		return
	}
	a.mu.Lock()
	if a.shuttingDown || a.active != run || a.auditFailed || a.frozen[sessionKey] || a.stoppingSessions[sessionKey] {
		a.mu.Unlock()
		return
	}
	if _, busy := a.inFlight[sessionKey]; busy || a.lineInFlight[sessionKey] {
		a.mu.Unlock()
		return
	}
	key, item, found := a.firstPendingForSessionLocked(sessionKey)
	if !found || !item.view.Evaluation.Automatic || item.view.DeliveryStatus != "pending" {
		a.mu.Unlock()
		return
	}
	item.view.DeliveryStatus = "delivering"
	a.pending[key] = item
	a.inFlight[sessionKey] = key
	a.rebuildPendingLocked()
	view := item.view
	event := item.event.Clone()
	evaluation := policy.Evaluation{
		Action:         policy.Action(item.view.Evaluation.Action),
		ProposedAction: policy.Action(item.view.Evaluation.ProposedAction),
		RuleName:       item.view.Evaluation.RuleName,
		Reason:         item.view.Evaluation.Reason,
		EventID:        item.event.ID,
		Automatic:      item.view.Evaluation.Automatic,
		DryRun:         item.view.Evaluation.DryRun,
	}
	a.eventWG.Add(1)
	a.mu.Unlock()
	a.emit(eventSemantic, view)
	go func() {
		defer a.eventWG.Done()
		a.applyAutomaticForRun(run, key, event, evaluation)
	}()
}

func (a *App) firstPendingForSessionLocked(sessionKey string) (eventKey, pendingEvent, bool) {
	var selectedKey eventKey
	var selected pendingEvent
	found := false
	for key, item := range a.pending {
		if key.sessionID != sessionKey {
			continue
		}
		if !found || eventBefore(item.event, selected.event) {
			selectedKey, selected, found = key, item, true
		}
	}
	return selectedKey, selected, found
}

func eventBefore(left, right adapters.Event) bool {
	if left.Sequence != right.Sequence {
		return left.Sequence < right.Sequence
	}
	if !left.Timestamp.Equal(right.Timestamp) {
		return left.Timestamp.Before(right.Timestamp)
	}
	return left.ID < right.ID
}

func (a *App) finishDecision(run *runGeneration, key eventKey, advance bool) {
	a.mu.Lock()
	if current, exists := a.inFlight[key.sessionID]; exists && current == key {
		delete(a.inFlight, key.sessionID)
	}
	shuttingDown := a.shuttingDown
	a.mu.Unlock()
	if advance && !shuttingDown && a.isActiveRun(run) {
		a.scheduleAutomatic(run, key.sessionID)
	}
}

func (a *App) applyAutomatic(key eventKey, event adapters.Event, evaluation policy.Evaluation) {
	a.mu.RLock()
	run := a.active
	a.mu.RUnlock()
	if run != nil {
		a.applyAutomaticForRun(run, key, event, evaluation)
	}
}

func (a *App) applyAutomaticForRun(run *runGeneration, key eventKey, event adapters.Event, evaluation policy.Evaluation) {
	advance := false
	defer func() { a.finishDecision(run, key, advance) }()
	decision, supported := adapterDecisionForPolicy(evaluation.Action)
	if !supported {
		a.fallbackToAsk(key, "fallback_unsupported")
		return
	}
	backend := a.backendFor(run, event.SessionID)
	auditDecision := auditDecisionForPolicy(evaluation.Action)
	if !a.recordAudit(run, decisionAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy)) {
		a.markDelivery(key, "failed", "audit_unavailable")
		return
	}
	if !a.beginDelivery() {
		if !a.recordAudit(run, deliveryAuditEntry(
			event,
			backend,
			auditDecision,
			audit.DecisionByPolicy,
			audit.OutcomeCancelled,
			"runtime_stopped",
		)) {
			return
		}
		a.markDelivery(key, "failed", "runtime_stopped")
		return
	}
	defer a.endDelivery()
	ctx, cancel := context.WithTimeout(run.ctx, 8*time.Second)
	err := run.engine.ApplyDecision(ctx, event.SessionID, event, decision, "")
	cancel()
	if err == nil {
		if !a.recordAudit(run, deliveryAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy, audit.OutcomeApplied, "delivery_applied")) {
			return
		}
		if a.pendingExists(key) {
			a.resolveEvent(key)
			advance = true
		}
		return
	}
	if errors.Is(err, adapters.ErrDecisionUnsupported) {
		if !a.recordAudit(run, deliveryAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy, audit.OutcomeFallbackUnsupported, "fallback_unsupported")) {
			return
		}
		if a.pendingExists(key) {
			a.fallbackToAsk(key, "fallback_unsupported")
		}
		return
	}
	if errors.Is(err, adapters.ErrEventMismatch) {
		if !a.recordAudit(run, deliveryAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy, audit.OutcomeFallbackStale, "fallback_stale")) {
			return
		}
		if a.pendingExists(key) {
			a.resolveEvent(key)
			a.reconcilePending(run, event.SessionID)
			advance = true
		}
		return
	}
	if !a.recordAudit(run, deliveryAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy, audit.OutcomeFallbackDeliveryUncertain, "delivery_uncertain")) {
		return
	}
	if a.pendingExists(key) {
		a.freezeSession(run, key, "delivery_uncertain")
	}
}

func (a *App) fallbackToAsk(key eventKey, reason string) {
	a.mu.Lock()
	item, exists := a.pending[key]
	if !exists {
		a.mu.Unlock()
		return
	}
	item.view.DeliveryStatus = "pending"
	item.view.Evaluation.Action = "ask"
	item.view.Evaluation.Automatic = false
	item.view.Evaluation.Reason = reason
	a.pending[key] = item
	a.rebuildPendingLocked()
	view := item.view
	a.mu.Unlock()
	a.emit(eventSemantic, view)
}

func (a *App) addFrozenEvent(run *runGeneration, event adapters.Event, evaluation policy.Evaluation) {
	key := makeEventKey(event.SessionID, event.ID)
	// A frozen event offers no answer at all: the session is already blocked on
	// an audit or delivery failure and nothing more may be sent to it.
	view := supervisionView(run.id, event, evaluation, "failed", nil)
	a.mu.Lock()
	a.pending[key] = pendingEvent{event: event.Clone(), view: view}
	a.frozen[key.sessionID] = true
	if index, found := a.agentIndex[key.sessionID]; found {
		a.state.Agents[index].InputFrozen = true
	}
	a.setAgentWaitingLocked(event.SessionID)
	a.rebuildPendingLocked()
	a.mu.Unlock()
	a.emit(eventSemantic, view)
}

func (a *App) markDelivery(key eventKey, status, reason string) {
	a.mu.Lock()
	item, exists := a.pending[key]
	if !exists {
		a.mu.Unlock()
		return
	}
	item.view.DeliveryStatus = status
	item.view.Evaluation.Reason = reason
	a.pending[key] = item
	if status == "uncertain" || status == "failed" {
		a.frozen[key.sessionID] = true
		if index, found := a.agentIndex[key.sessionID]; found {
			a.state.Agents[index].InputFrozen = true
		}
	}
	a.rebuildPendingLocked()
	view := item.view
	a.mu.Unlock()
	a.emit(eventSemantic, view)
}

func (a *App) freezeSession(run *runGeneration, key eventKey, reason string) {
	a.markDelivery(key, "uncertain", reason)
	a.emitSafeError(run, "delivery_uncertain", "Delivery is indeterminate. The session is frozen to prevent a second answer.", key.sessionID)
}

func (a *App) resolveEvent(key eventKey) {
	a.mu.Lock()
	item, exists := a.pending[key]
	status := "running"
	if exists {
		delete(a.pending, key)
		a.markResolvedLocked(key)
		if a.hasPendingForSessionLocked(key.sessionID) {
			a.setAgentWaitingLocked(key.sessionID)
			status = "waiting"
		} else {
			a.restoreAgentRunningLocked(key.sessionID)
		}
		a.rebuildPendingLocked()
		item.view.DeliveryStatus = "delivered"
	}
	a.mu.Unlock()
	if exists {
		a.emit(eventSemantic, item.view)
		a.mu.RLock()
		runID := a.state.RunID
		a.mu.RUnlock()
		a.emit(eventStatus, StatusEvent{RunID: runID, Scope: "session", SessionID: key.sessionID, Status: status})
	}
}

func (a *App) hasPendingForSessionLocked(sessionKey string) bool {
	for key := range a.pending {
		if key.sessionID == sessionKey {
			return true
		}
	}
	return false
}

func (a *App) reconcilePending(run *runGeneration, sessionID string) {
	ctx, cancel := context.WithTimeout(run.ctx, 2*time.Second)
	pending, err := run.engine.PendingEvent(ctx, sessionID)
	cancel()
	if err != nil || pending == nil {
		return
	}
	a.handleAdapterEventForRun(run, pending.Clone())
}

// SubmitDecision relays a manual value to the exact canonical occurrence. The
// value is never copied into application state, events, errors or audit data.
func (a *App) SubmitDecision(runID, sessionID, eventID, manualInput string) error {
	if strings.TrimSpace(manualInput) == "" {
		return errEmptyDecision
	}
	return a.applyHumanDecision(runID, sessionID, eventID, adapters.DecisionManual, manualInput)
}

// SubmitAutomaticDecision relays an answer the adapter encodes itself, so the
// operator does not have to know the keystroke a given CLI expects.
//
// The set of answers a given occurrence accepts is reported on the event, and
// the adapter is asked again here: a decision that arrived from a stale
// interface must be refused by the core rather than by the screen that offered
// it.
func (a *App) SubmitAutomaticDecision(runID, sessionID, eventID, decision string) error {
	switch adapters.Decision(decision) {
	case adapters.DecisionAllow, adapters.DecisionDeny:
	default:
		return errUnsupportedDecision
	}
	run, runErr := a.activeRun(runID)
	if runErr != nil {
		return runErr
	}
	a.mu.RLock()
	item, exists := a.pending[makeEventKey(sessionID, eventID)]
	a.mu.RUnlock()
	if !exists {
		return errDecisionStale
	}
	offered := false
	for _, supported := range run.engine.SupportedDecisions(item.event) {
		if supported == adapters.Decision(decision) {
			offered = true
		}
	}
	if !offered {
		return errUnsupportedDecision
	}
	return a.applyHumanDecision(runID, sessionID, eventID, adapters.Decision(decision), "")
}

func (a *App) applyHumanDecision(
	runID, sessionID, eventID string,
	decision adapters.Decision,
	manualInput string,
) error {
	run, runErr := a.activeRun(runID)
	if runErr != nil {
		return runErr
	}
	key := makeEventKey(sessionID, eventID)
	if !a.beginDelivery() {
		a.mu.RLock()
		frozen := a.auditFailed || a.frozen[key.sessionID]
		a.mu.RUnlock()
		if frozen {
			return errDeliveryUncertain
		}
		return errRuntimeStopped
	}
	defer a.endDelivery()

	a.mu.Lock()
	item, exists := a.pending[key]
	frozen := a.frozen[key.sessionID] || a.auditFailed
	shuttingDown := a.shuttingDown
	if !exists {
		a.mu.Unlock()
		return errDecisionStale
	}
	if shuttingDown {
		a.mu.Unlock()
		return errRuntimeStopped
	}
	if a.stoppingSessions[key.sessionID] {
		a.mu.Unlock()
		return errRuntimeStopped
	}
	if frozen || item.view.DeliveryStatus == "uncertain" || item.view.DeliveryStatus == "failed" {
		a.mu.Unlock()
		return errDeliveryUncertain
	}
	if _, busy := a.inFlight[key.sessionID]; busy || a.lineInFlight[key.sessionID] || item.view.DeliveryStatus != "pending" {
		a.mu.Unlock()
		return errDecisionInFlight
	}
	a.inFlight[key.sessionID] = key
	item.view.DeliveryStatus = "delivering"
	a.pending[key] = item
	a.rebuildPendingLocked()
	view := item.view
	a.mu.Unlock()
	a.emit(eventSemantic, view)
	advance := false
	defer func() { a.finishDecision(run, key, advance) }()

	backend := a.backendFor(run, sessionID)
	if !a.recordAudit(run, decisionAuditEntry(item.event, backend, humanAuditDecision(decision), audit.DecisionByHuman)) {
		return errAuditUnavailable
	}
	ctx, cancel := context.WithTimeout(run.ctx, 8*time.Second)
	err := run.engine.ApplyDecision(ctx, sessionID, item.event, decision, manualInput)
	cancel()
	if err != nil {
		if errors.Is(err, adapters.ErrEventMismatch) {
			if !a.recordAudit(run, deliveryAuditEntry(item.event, backend, humanAuditDecision(decision), audit.DecisionByHuman, audit.OutcomeFallbackStale, "fallback_stale")) {
				return errAuditUnavailable
			}
			a.resolveEvent(key)
			a.reconcilePending(run, sessionID)
			advance = true
			return errDecisionStale
		}
		if !a.recordAudit(run, deliveryAuditEntry(item.event, backend, humanAuditDecision(decision), audit.DecisionByHuman, audit.OutcomeFallbackDeliveryUncertain, "delivery_uncertain")) {
			return errAuditUnavailable
		}
		a.freezeSession(run, key, "delivery_uncertain")
		return errDeliveryUncertain
	}
	if !a.recordAudit(run, deliveryAuditEntry(item.event, backend, humanAuditDecision(decision), audit.DecisionByHuman, audit.OutcomeApplied, "delivery_applied")) {
		return errAuditUnavailable
	}
	a.resolveEvent(key)
	advance = true
	return nil
}

// SubmitLine sends one ordinary application line to a detached, running
// session. The line crosses this method only as a call argument: it is never
// copied into bridge state, events, errors or audit entries.
func (a *App) SubmitLine(runID, sessionID, line string) error {
	run, err := a.activeRun(runID)
	if err != nil {
		return err
	}
	// Delivery admission must precede the per-session claim so lifecycle
	// shutdown cannot start waiting between those two operations.
	if !a.beginDelivery() {
		a.mu.RLock()
		frozen := a.auditFailed || a.frozen[strings.ToLower(strings.TrimSpace(sessionID))]
		a.mu.RUnlock()
		if frozen {
			return errDeliveryUncertain
		}
		return errRuntimeStopped
	}
	defer a.endDelivery()

	sessionKey := strings.ToLower(strings.TrimSpace(sessionID))
	a.mu.Lock()
	if a.active != run || a.shuttingDown {
		a.mu.Unlock()
		return errRuntimeStopped
	}
	if a.auditFailed {
		a.mu.Unlock()
		return errAuditUnavailable
	}
	if a.frozen[sessionKey] {
		a.mu.Unlock()
		return errDeliveryUncertain
	}
	index, found := a.agentIndex[sessionKey]
	if !found {
		a.mu.Unlock()
		return errLineUnavailable
	}
	agent := a.state.Agents[index]
	if a.stoppingSessions[sessionKey] {
		a.mu.Unlock()
		return errLineUnavailable
	}
	if _, busy := a.inFlight[sessionKey]; busy || a.hasPendingForSessionLocked(sessionKey) {
		a.mu.Unlock()
		a.reconcilePending(run, sessionID)
		a.emitSafeError(run, "line_prompt_pending", "Answer the supervision request before sending a line.", sessionID)
		return errLinePromptPending
	}
	if !agent.Running || agent.Attached || (agent.Status != "running" && agent.Status != "detached") {
		a.mu.Unlock()
		return errLineUnavailable
	}
	if a.lineInFlight[sessionKey] {
		a.mu.Unlock()
		return errLineInFlight
	}
	a.lineInFlight[sessionKey] = true
	a.mu.Unlock()
	defer a.finishLine(run, sessionKey)

	if !a.recordAudit(run, operatorInputAuditEntry(agent, audit.OutcomeInFlight, "operator_input_started")) {
		return errAuditUnavailable
	}
	ctx, cancel := context.WithTimeout(run.ctx, 8*time.Second)
	err = run.engine.SendLine(ctx, sessionID, line)
	line = ""
	cancel()
	if err == nil {
		if !a.recordAudit(run, operatorInputAuditEntry(agent, audit.OutcomeApplied, "operator_input_applied")) {
			return errAuditUnavailable
		}
		return nil
	}

	switch {
	case errors.Is(err, terminal.ErrEventPending):
		if !a.recordAudit(run, operatorInputAuditEntry(agent, audit.OutcomeFallbackStale, "operator_input_prompt_pending")) {
			return errAuditUnavailable
		}
		a.reconcilePending(run, sessionID)
		a.emitSafeError(run, "line_prompt_pending", "A supervision request arrived before the line. No free text was sent.", sessionID)
		return errLinePromptPending
	case errors.Is(err, terminal.ErrInvalidLine):
		if !a.recordAudit(run, operatorInputAuditEntry(agent, audit.OutcomeSkipped, "operator_input_invalid")) {
			return errAuditUnavailable
		}
		a.emitSafeError(run, "line_invalid", "The input must be a single UTF-8 line, with no control characters and no more than 4096 bytes.", sessionID)
		return errLineInvalid
	case errors.Is(err, terminal.ErrLineUnsupported):
		if !a.recordAudit(run, operatorInputAuditEntry(agent, audit.OutcomeSkipped, "operator_input_unsupported")) {
			return errAuditUnavailable
		}
		a.emitSafeError(run, "line_unsupported", "This backend cannot send a line reliably.", sessionID)
		return errLineUnsupported
	case errors.Is(err, terminal.ErrClosed):
		if !a.recordAudit(run, operatorInputAuditEntry(agent, audit.OutcomeSkipped, "operator_input_session_unavailable")) {
			return errAuditUnavailable
		}
		a.markLineSessionUnavailable(run, sessionKey, "exited")
		a.emitSafeError(run, "line_session_unavailable", "The session ended before delivery. No line was sent.", sessionID)
		return errLineUnavailable
	case errors.Is(err, terminal.ErrSessionNotFound):
		if !a.recordAudit(run, operatorInputAuditEntry(agent, audit.OutcomeSkipped, "operator_input_session_unavailable")) {
			return errAuditUnavailable
		}
		a.markLineSessionUnavailable(run, sessionKey, "failed")
		a.emitSafeError(run, "line_session_unavailable", "The session is no longer available. No line was sent.", sessionID)
		return errLineUnavailable
	default:
		if !a.recordAudit(run, operatorInputAuditEntry(agent, audit.OutcomeFallbackDeliveryUncertain, "operator_input_delivery_uncertain")) {
			return errAuditUnavailable
		}
		a.freezeLineSession(run, sessionKey)
		return errDeliveryUncertain
	}
}

func (a *App) finishLine(run *runGeneration, sessionKey string) {
	a.mu.Lock()
	delete(a.lineInFlight, sessionKey)
	advance := !a.shuttingDown
	if index, found := a.agentIndex[sessionKey]; !found || !a.state.Agents[index].Running {
		advance = false
	}
	a.mu.Unlock()
	if advance && a.isActiveRun(run) {
		a.scheduleAutomatic(run, sessionKey)
	}
}

func (a *App) freezeLineSession(run *runGeneration, sessionKey string) {
	a.mu.Lock()
	if a.active != run {
		a.mu.Unlock()
		return
	}
	a.frozen[sessionKey] = true
	displaySessionID := sessionKey
	if index, found := a.agentIndex[sessionKey]; found {
		a.state.Agents[index].InputFrozen = true
		displaySessionID = a.state.Agents[index].SessionID
	}
	a.mu.Unlock()
	a.emitSafeError(run, "delivery_uncertain", "Delivery is indeterminate. The session is frozen to prevent another send.", displaySessionID)
}

func (a *App) markLineSessionUnavailable(run *runGeneration, sessionKey, status string) {
	a.mu.Lock()
	if a.active != run {
		a.mu.Unlock()
		return
	}
	displaySessionID := sessionKey
	if index, found := a.agentIndex[sessionKey]; found {
		agent := &a.state.Agents[index]
		agent.Running = false
		agent.Attached = false
		agent.Status = status
		displaySessionID = agent.SessionID
	}
	a.mu.Unlock()
	a.emit(eventStatus, StatusEvent{RunID: run.id, Scope: "session", SessionID: displaySessionID, Status: status})
}

func (a *App) ResizeSession(runID, sessionID string, columns, rows int) error {
	if columns < 1 || rows < 1 || columns > 65535 || rows > 65535 {
		return errors.New("Invalid terminal dimensions.")
	}
	run, err := a.activeRun(runID)
	if err != nil {
		return err
	}
	if !a.beginDelivery() {
		return errRuntimeStopped
	}
	defer a.endDelivery()
	ctx, cancel := context.WithTimeout(run.ctx, 3*time.Second)
	err = run.engine.Resize(ctx, sessionID, terminal.Size{Columns: columns, Rows: rows})
	cancel()
	if err != nil {
		a.emitSafeError(run, "resize_failed", "Resizing the session failed.", sessionID)
		return errors.New("Resizing the session failed.")
	}
	return nil
}

func (a *App) StopSession(runID, sessionID string) error {
	run, err := a.activeRun(runID)
	if err != nil {
		return err
	}
	if !a.beginDelivery() {
		return errRuntimeStopped
	}
	defer a.endDelivery()
	sessionKey := strings.ToLower(strings.TrimSpace(sessionID))
	a.mu.Lock()
	if a.active != run || a.shuttingDown {
		a.mu.Unlock()
		return errRuntimeStopped
	}
	if a.lineInFlight[sessionKey] {
		a.mu.Unlock()
		return errLineInFlight
	}
	if _, busy := a.inFlight[sessionKey]; busy {
		a.mu.Unlock()
		return errDecisionInFlight
	}
	if a.stoppingSessions[sessionKey] {
		a.mu.Unlock()
		return errLineUnavailable
	}
	index, found := a.agentIndex[sessionKey]
	if !found || !a.state.Agents[index].Running {
		a.mu.Unlock()
		return errLineUnavailable
	}
	a.stoppingSessions[sessionKey] = true
	a.state.Agents[index].Status = "stopping"
	displaySessionID := a.state.Agents[index].SessionID
	a.mu.Unlock()
	a.emit(eventStatus, StatusEvent{RunID: run.id, Scope: "session", SessionID: displaySessionID, Status: "stopping"})
	ctx, cancel := context.WithTimeout(run.ctx, 6*time.Second)
	err = run.engine.Stop(ctx, sessionID)
	cancel()
	if err != nil {
		a.mu.Lock()
		delete(a.stoppingSessions, sessionKey)
		a.mu.Unlock()
		a.freezeLineSession(run, sessionKey)
		a.emitSafeError(run, "stop_failed", "This session could not be stopped cleanly.", sessionID)
		return errors.New("This session could not be stopped cleanly.")
	}
	a.mu.Lock()
	delete(a.stoppingSessions, sessionKey)
	a.mu.Unlock()
	a.markLineSessionUnavailable(run, sessionKey, "exited")
	return nil
}

func (a *App) Shutdown() error {
	a.shutdownOnce.Do(func() {
		defer close(a.shutdownDone)
		a.lifecycleMu.Lock()
		a.finalShutdown = true
		a.mu.RLock()
		run := a.active
		a.mu.RUnlock()
		if run != nil {
			a.shutdownErr = a.stopGenerationLocked(run, false, "stopping")
		}
		status := "stopped"
		if a.shutdownErr != nil {
			status = "failed"
		}
		a.setRunStatus(status, "")
		a.lifecycleMu.Unlock()
	})
	<-a.shutdownDone
	if a.shutdownErr != nil {
		return errors.New("Relayer did not shut down completely. Check the local sessions.")
	}
	return nil
}

func (a *App) onShutdown(_ context.Context) {
	_ = a.Shutdown()
}

func (a *App) recordAudit(run *runGeneration, entry audit.Entry) bool {
	if run == nil || run.engine == nil {
		return false
	}
	if err := run.engine.RecordAudit(entry); err != nil {
		a.freezeAudit(run)
		return false
	}
	return true
}

func (a *App) freezeAudit(run *runGeneration) {
	if !a.isActiveRun(run) {
		return
	}
	a.closeDelivery()
	a.mu.Lock()
	if a.auditFailed {
		a.mu.Unlock()
		return
	}
	a.auditFailed = true
	a.state.Audit.Status = "failed"
	for _, agent := range a.state.Agents {
		if agent.Running {
			a.frozen[strings.ToLower(agent.SessionID)] = true
		}
	}
	for index := range a.state.Agents {
		if a.state.Agents[index].Running {
			a.state.Agents[index].InputFrozen = true
		}
	}
	for key, item := range a.pending {
		item.view.DeliveryStatus = "failed"
		item.view.Evaluation.Reason = "audit_unavailable"
		a.pending[key] = item
	}
	a.rebuildPendingLocked()
	a.mu.Unlock()
	a.emit(eventStatus, StatusEvent{RunID: run.id, Scope: "audit", Status: "failed"})
	a.emitSafeError(run, "audit_unavailable", "The local audit journal is unavailable. No further decision will be sent.", "")
}

func (a *App) beginDelivery() bool {
	a.deliveryMu.Lock()
	defer a.deliveryMu.Unlock()
	if !a.deliveryAvailable {
		return false
	}
	a.deliveryWG.Add(1)
	return true
}

func (a *App) endDelivery() { a.deliveryWG.Done() }

func (a *App) closeDelivery() {
	a.deliveryMu.Lock()
	a.deliveryAvailable = false
	a.deliveryMu.Unlock()
}

func (a *App) openDelivery() {
	a.deliveryMu.Lock()
	a.deliveryAvailable = true
	a.deliveryMu.Unlock()
}

func (a *App) markSessionError(run *runGeneration, sessionID, reason string) {
	backend := a.backendFor(run, sessionID)
	_ = a.recordAudit(run, audit.Entry{
		Kind:       audit.KindBackendError,
		SessionID:  sessionID,
		AgentID:    sessionID,
		Backend:    backend,
		DecisionBy: audit.DecisionBySystem,
		Outcome:    audit.OutcomeFailed,
		Reason:     reason,
	})
	a.mu.Lock()
	if index, found := a.agentIndex[strings.ToLower(sessionID)]; found {
		a.state.Agents[index].Status = "failed"
	}
	a.mu.Unlock()
	a.emit(eventStatus, StatusEvent{RunID: run.id, Scope: "session", SessionID: sessionID, Status: "failed"})
	a.emitSafeError(run, "backend_stream_failed", "The backend stream failed.", sessionID)
}

func (a *App) markLegacyExit(run *runGeneration, sessionID string) {
	a.mu.Lock()
	if index, found := a.agentIndex[strings.ToLower(sessionID)]; found {
		a.state.Agents[index].Status = "failed"
		a.state.Agents[index].Running = false
	}
	a.clearSessionPendingLocked(sessionID)
	a.rebuildPendingLocked()
	a.mu.Unlock()
	a.emit(eventStatus, StatusEvent{RunID: run.id, Scope: "session", SessionID: sessionID, Status: "failed"})
}

func (a *App) backendFor(run *runGeneration, sessionID string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.active != run {
		return ""
	}
	if index, found := a.agentIndex[strings.ToLower(sessionID)]; found {
		return a.state.Agents[index].Backend
	}
	return ""
}

func (a *App) pendingExists(key eventKey) bool {
	a.mu.RLock()
	defer a.mu.RUnlock()
	_, exists := a.pending[key]
	return exists
}

func (a *App) setAgentWaitingLocked(sessionID string) {
	if index, found := a.agentIndex[strings.ToLower(sessionID)]; found && a.state.Agents[index].Running {
		a.state.Agents[index].Status = "waiting"
	}
}

func (a *App) restoreAgentRunningLocked(sessionID string) {
	if index, found := a.agentIndex[strings.ToLower(sessionID)]; found && a.state.Agents[index].Running {
		a.state.Agents[index].Status = "running"
	}
}

func (a *App) clearSessionPendingLocked(sessionID string) {
	normalized := strings.ToLower(strings.TrimSpace(sessionID))
	delete(a.inFlight, normalized)
	for key := range a.pending {
		if key.sessionID == normalized {
			delete(a.pending, key)
			a.markResolvedLocked(key)
		}
	}
}

func (a *App) markResolvedLocked(key eventKey) {
	if _, exists := a.resolved[key]; exists {
		return
	}
	a.resolved[key] = struct{}{}
	a.resolvedOrder = append(a.resolvedOrder, key)
	if len(a.resolvedOrder) <= maxResolvedEvents {
		return
	}
	oldest := a.resolvedOrder[0]
	a.resolvedOrder = append(a.resolvedOrder[:0], a.resolvedOrder[1:]...)
	delete(a.resolved, oldest)
}

func (a *App) rebuildPendingLocked() {
	items := make([]SupervisionEvent, 0, len(a.pending))
	for _, item := range a.pending {
		items = append(items, item.view)
	}
	sort.Slice(items, func(left, right int) bool {
		if items[left].Timestamp == items[right].Timestamp {
			if items[left].SessionID == items[right].SessionID {
				return items[left].ID < items[right].ID
			}
			return items[left].SessionID < items[right].SessionID
		}
		return items[left].Timestamp < items[right].Timestamp
	})
	a.state.PendingEvents = items
}

func (a *App) emitSafeError(run *runGeneration, code, message, sessionID string) {
	runID := ""
	if run != nil {
		runID = run.id
	}
	a.emit(eventError, SafeErrorEvent{
		RunID:     runID,
		Code:      code,
		Message:   message,
		SessionID: sessionID,
		Timestamp: time.Now().UTC().Format(time.RFC3339Nano),
	})
}

func (a *App) emit(name string, payload interface{}) {
	if a.ctx != nil && a.emitFn != nil {
		a.emitFn(a.ctx, name, payload)
	}
}

func makeEventKey(sessionID, eventID string) eventKey {
	return eventKey{
		sessionID: strings.ToLower(strings.TrimSpace(sessionID)),
		eventID:   strings.TrimSpace(eventID),
	}
}

func supervisionView(
	runID string,
	event adapters.Event,
	evaluation policy.Evaluation,
	delivery string,
	decisions []adapters.Decision,
) SupervisionEvent {
	offered := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		switch decision {
		case adapters.DecisionAllow, adapters.DecisionDeny:
			offered = append(offered, string(decision))
		}
	}
	timestamp := event.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	return SupervisionEvent{
		RunID:     runID,
		ID:        event.ID,
		SessionID: event.SessionID,
		AgentID:   event.AgentID,
		Adapter:   event.Adapter,
		Type:      string(event.Type),
		Summary:   safeEventSummary(event),
		Sensitive: requiresSecretHandling(event),
		Risk:      string(event.Risk),
		Timestamp: timestamp.UTC().Format(time.RFC3339Nano),
		Evaluation: PolicyEvaluation{
			Action:         string(evaluation.Action),
			ProposedAction: string(evaluation.ProposedAction),
			RuleName:       safeRuleName(evaluation.RuleName),
			Reason:         safeReason(evaluation.Reason),
			Automatic:      evaluation.Automatic,
			DryRun:         evaluation.DryRun,
		},
		DeliveryStatus: delivery,
		Decisions:      offered,
	}
}

func snapshotFromAgent(runID string, agent AgentState) SnapshotEvent {
	return SnapshotEvent{
		RunID:       runID,
		SessionID:   agent.SessionID,
		Revision:    agent.Revision,
		Output:      agent.Output,
		Status:      agent.Status,
		Running:     agent.Running,
		Attached:    agent.Attached,
		InputFrozen: agent.InputFrozen,
		ExitCode:    cloneInt(agent.ExitCode),
	}
}

func cloneAppState(state AppState) AppState {
	clone := state
	clone.Agents = append([]AgentState(nil), state.Agents...)
	for index := range clone.Agents {
		clone.Agents[index].ExitCode = cloneInt(state.Agents[index].ExitCode)
	}
	clone.PendingEvents = append([]SupervisionEvent(nil), state.PendingEvents...)
	return clone
}

func cloneInt(value *int) *int {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}

// safeNotices bounds what the startup log may put on screen.
//
// Its lines are built by the application from configuration facts rather than
// from terminal output, but this is the first path that displays them, so the
// count and length are capped and control characters are dropped rather than
// trusted to be absent.
func safeNotices(logs []string) []string {
	const (
		maxNotices    = 16
		maxNoticeRune = 240
	)
	notices := make([]string, 0, len(logs))
	for _, line := range logs {
		if len(notices) == maxNotices {
			break
		}
		cleaned := strings.Map(func(r rune) rune {
			if unicode.IsControl(r) {
				return -1
			}
			return r
		}, strings.TrimSpace(line))
		if cleaned == "" {
			continue
		}
		if runes := []rune(cleaned); len(runes) > maxNoticeRune {
			cleaned = string(runes[:maxNoticeRune])
		}
		notices = append(notices, cleaned)
	}
	return notices
}

// humanAuditDecision names the answer a human actually gave. A free-text reply
// stays "ask": the journal records that a human was consulted and answered, not
// that the answer permitted anything — only the adapter knows what the bytes
// mean.
func humanAuditDecision(decision adapters.Decision) audit.Decision {
	switch decision {
	case adapters.DecisionAllow:
		return audit.DecisionAllow
	case adapters.DecisionDeny:
		return audit.DecisionDeny
	default:
		return audit.DecisionAsk
	}
}
