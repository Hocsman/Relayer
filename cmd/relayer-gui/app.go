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

	"github.com/Hocsman/Relayer/internal/adapters"
	appcore "github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/policy"
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
	errAuditUnavailable  = errors.New("Le journal d'audit est indisponible; aucune décision n'a été envoyée.")
	errDecisionStale     = errors.New("Cette demande n'est plus l'événement actuellement attendu.")
	errDecisionInFlight  = errors.New("Une décision est déjà en cours pour cet agent.")
	errDeliveryUncertain = errors.New("L'état de livraison est indéterminé; arrêtez la session avant toute nouvelle saisie.")
	errRuntimeStopped    = errors.New("Le moteur Relayer est arrêté.")
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
	Events() <-chan session.Event
	Output(string) (string, error)
	PendingEvent(context.Context, string) (*adapters.Event, error)
	Evaluate(adapters.Event) policy.Evaluation
	ApplyDecision(context.Context, string, adapters.Event, adapters.Decision, string) error
	Resize(context.Context, string, terminal.Size) error
	Stop(context.Context, string) error
	RecordAudit(audit.Entry) error
	BeginShutdown(context.Context) error
	Close(context.Context) error
}

// App is the narrow Wails bridge. The browser receives display-safe DTOs and
// stable operation identifiers; semantic events and decisions remain in Go.
type App struct {
	ctx    context.Context
	cancel context.CancelFunc
	engine desktopEngine
	emitFn func(context.Context, string, ...interface{})

	mu            sync.RWMutex
	state         AppState
	agentIndex    map[string]int
	pending       map[eventKey]pendingEvent
	ingesting     map[eventKey]struct{}
	resolved      map[eventKey]struct{}
	resolvedOrder []eventKey
	inFlight      map[string]eventKey
	outputRunning map[string]bool
	outputDirty   map[string]bool
	frozen        map[string]bool
	auditFailed   bool
	shuttingDown  bool
	startupErr    error
	configPath    string

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
			RunStatus:     "starting",
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
		outputRunning:         make(map[string]bool),
		outputDirty:           make(map[string]bool),
		frozen:                make(map[string]bool),
		deliveryAvailable:     true,
		shutdownDone:          make(chan struct{}),
		profileDetector:       toolcatalog.DefaultDetector(),
		profileTokenGenerator: newOpaqueProfileToken,
	}
}

func (a *App) startup(ctx context.Context) {
	a.ctx, a.cancel = context.WithCancel(ctx)
	a.emitFn = wailsruntime.EventsEmit
	if !desktopAgentExecutionSupported() {
		a.failStartup(errors.New(desktopUnsupportedReason()))
		return
	}

	configPath, err := desktopConfigPath()
	if err != nil {
		a.failStartup(errors.New(safeDisplayError(err)))
		return
	}
	a.profilesMu.Lock()
	a.configPath = configPath
	a.profilesMu.Unlock()
	engine, err := appcore.NewDesktopRuntime(a.ctx, appcore.DesktopOptions{
		ConfigPath: configPath,
		InitialSize: terminal.Size{
			Columns: 120,
			Rows:    32,
		},
		Diagnostics: os.Stderr,
	})
	if err != nil {
		a.failStartup(errors.New(safeDisplayError(err)))
		return
	}
	a.engine = engine
	a.profilesMu.Lock()
	a.activeConfigRevision = engine.Metadata().ConfigRevision
	a.profilesMu.Unlock()
	a.initializeState(engine)
	a.eventWG.Add(1)
	go a.consumeEvents()
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

func (a *App) initializeState(engine desktopEngine) {
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
		})
	}
	a.mu.Lock()
	a.agentIndex = index
	a.state = AppState{
		RunID:     metadata.RunID,
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
		PendingEvents: []SupervisionEvent{},
	}
	a.mu.Unlock()
}

func (a *App) consumeEvents() {
	defer a.eventWG.Done()
	events := a.engine.Events()
	for {
		select {
		case <-a.ctx.Done():
			return
		case message, open := <-events:
			if !open {
				return
			}
			switch value := message.(type) {
			case session.OutputAvailable:
				a.scheduleOutputRefresh(value.SessionID)
			case session.AdapterEvent:
				a.handleAdapterEvent(value.Event.Clone())
			case session.Error:
				a.markSessionError(value.SessionID, "backend_stream_failed")
			case session.Exited:
				a.markLegacyExit(value.SessionID)
			}
		}
	}
}

func (a *App) scheduleOutputRefresh(sessionID string) {
	key := strings.ToLower(strings.TrimSpace(sessionID))
	if key == "" {
		return
	}
	a.mu.Lock()
	if a.shuttingDown {
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
			case <-a.ctx.Done():
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
			a.refreshOutput(sessionID)
			a.mu.Lock()
			if a.outputDirty[key] && !a.shuttingDown {
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
	output, err := a.engine.Output(sessionID)
	if err != nil {
		a.emitSafeError("output_refresh_failed", "Impossible d'actualiser la sortie bornée de la session.", sessionID)
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
	payload := snapshotFromAgent(*agent)
	a.mu.Unlock()
	a.emit(eventSnapshot, payload)
}

func (a *App) handleAdapterEvent(event adapters.Event) {
	key := makeEventKey(event.SessionID, event.ID)
	if key.sessionID == "" || key.eventID == "" {
		a.emitSafeError("invalid_event", "Un événement invalide a été ignoré.", event.SessionID)
		return
	}
	if !a.reserveEvent(key) {
		return
	}
	defer a.releaseEventReservation(key)

	backend := a.backendFor(event.SessionID)
	if event.Type == adapters.EventProcessExit {
		a.handleProcessExit(event, backend)
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
	a.refreshOutput(event.SessionID)
	if !a.recordAudit(eventDetectedEntry(event, backend)) {
		a.addFrozenEvent(event, policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionAsk, Reason: policy.ReasonNoEngine})
		return
	}
	evaluation := a.engine.Evaluate(event)
	if !a.recordAudit(policyAuditEntry(event, backend, evaluation)) {
		a.addFrozenEvent(event, evaluation)
		return
	}

	view := supervisionView(event, evaluation, "pending")
	a.mu.Lock()
	a.pending[key] = pendingEvent{event: event.Clone(), view: view}
	a.setAgentWaitingLocked(event.SessionID)
	a.rebuildPendingLocked()
	a.mu.Unlock()
	a.emit(eventSemantic, view)
	a.scheduleAutomatic(event.SessionID)
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

func (a *App) handleProcessExit(event adapters.Event, backend string) {
	key := makeEventKey(event.SessionID, event.ID)
	if !a.recordAudit(eventDetectedEntry(event, backend)) {
		// Lifecycle state still has to converge even when audit has failed.
	}
	finished := eventAuditEntry(audit.KindSessionFinished, event, backend)
	finished.Outcome = audit.OutcomeFinished
	if event.Metadata["failed"] == "true" {
		finished.Outcome = audit.OutcomeFailed
	}
	finished.Reason = "process_exit"
	_ = a.recordAudit(finished)

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
	a.refreshOutput(event.SessionID)
	a.emit(eventStatus, StatusEvent{Scope: "session", SessionID: event.SessionID, Status: status})
}

// scheduleAutomatic serialises every automatic decision for a session and
// never overtakes an earlier human prompt. The reservation and WaitGroup Add
// happen under a.mu so Shutdown cannot begin waiting between those steps.
func (a *App) scheduleAutomatic(sessionID string) {
	sessionKey := strings.ToLower(strings.TrimSpace(sessionID))
	if sessionKey == "" {
		return
	}
	a.mu.Lock()
	if a.shuttingDown || a.auditFailed || a.frozen[sessionKey] {
		a.mu.Unlock()
		return
	}
	if _, busy := a.inFlight[sessionKey]; busy {
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
		a.applyAutomatic(key, event, evaluation)
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

func (a *App) finishDecision(key eventKey, advance bool) {
	a.mu.Lock()
	if current, exists := a.inFlight[key.sessionID]; exists && current == key {
		delete(a.inFlight, key.sessionID)
	}
	shuttingDown := a.shuttingDown
	a.mu.Unlock()
	if advance && !shuttingDown {
		a.scheduleAutomatic(key.sessionID)
	}
}

func (a *App) applyAutomatic(key eventKey, event adapters.Event, evaluation policy.Evaluation) {
	advance := false
	defer func() { a.finishDecision(key, advance) }()
	decision, supported := adapterDecisionForPolicy(evaluation.Action)
	if !supported {
		a.fallbackToAsk(key, "fallback_unsupported")
		return
	}
	backend := a.backendFor(event.SessionID)
	auditDecision := auditDecisionForPolicy(evaluation.Action)
	if !a.recordAudit(decisionAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy)) {
		a.markDelivery(key, "failed", "audit_unavailable")
		return
	}
	if !a.beginDelivery() {
		if !a.recordAudit(deliveryAuditEntry(
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
	ctx, cancel := context.WithTimeout(a.ctx, 8*time.Second)
	err := a.engine.ApplyDecision(ctx, event.SessionID, event, decision, "")
	cancel()
	if err == nil {
		if !a.recordAudit(deliveryAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy, audit.OutcomeApplied, "delivery_applied")) {
			return
		}
		if a.pendingExists(key) {
			a.resolveEvent(key)
			advance = true
		}
		return
	}
	if errors.Is(err, adapters.ErrDecisionUnsupported) {
		if !a.recordAudit(deliveryAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy, audit.OutcomeFallbackUnsupported, "fallback_unsupported")) {
			return
		}
		if a.pendingExists(key) {
			a.fallbackToAsk(key, "fallback_unsupported")
		}
		return
	}
	if errors.Is(err, adapters.ErrEventMismatch) {
		if !a.recordAudit(deliveryAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy, audit.OutcomeFallbackStale, "fallback_stale")) {
			return
		}
		if a.pendingExists(key) {
			a.resolveEvent(key)
			a.reconcilePending(event.SessionID)
			advance = true
		}
		return
	}
	if !a.recordAudit(deliveryAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy, audit.OutcomeFallbackDeliveryUncertain, "delivery_uncertain")) {
		return
	}
	if a.pendingExists(key) {
		a.freezeSession(key, "delivery_uncertain")
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

func (a *App) addFrozenEvent(event adapters.Event, evaluation policy.Evaluation) {
	key := makeEventKey(event.SessionID, event.ID)
	view := supervisionView(event, evaluation, "failed")
	a.mu.Lock()
	a.pending[key] = pendingEvent{event: event.Clone(), view: view}
	a.frozen[key.sessionID] = true
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
	}
	a.rebuildPendingLocked()
	view := item.view
	a.mu.Unlock()
	a.emit(eventSemantic, view)
}

func (a *App) freezeSession(key eventKey, reason string) {
	a.markDelivery(key, "uncertain", reason)
	a.emitSafeError("delivery_uncertain", "Livraison indéterminée: la session est gelée pour éviter une seconde réponse.", key.sessionID)
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
		a.emit(eventStatus, StatusEvent{Scope: "session", SessionID: key.sessionID, Status: status})
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

func (a *App) reconcilePending(sessionID string) {
	ctx, cancel := context.WithTimeout(a.ctx, 2*time.Second)
	pending, err := a.engine.PendingEvent(ctx, sessionID)
	cancel()
	if err != nil || pending == nil {
		return
	}
	a.handleAdapterEvent(pending.Clone())
}

// SubmitDecision relays a manual value to the exact canonical occurrence. The
// value is never copied into application state, events, errors or audit data.
func (a *App) SubmitDecision(sessionID, eventID, manualInput string) error {
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
	if frozen || item.view.DeliveryStatus == "uncertain" || item.view.DeliveryStatus == "failed" {
		a.mu.Unlock()
		return errDeliveryUncertain
	}
	if _, busy := a.inFlight[key.sessionID]; busy || item.view.DeliveryStatus != "pending" {
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
	defer func() { a.finishDecision(key, advance) }()

	backend := a.backendFor(sessionID)
	if !a.recordAudit(decisionAuditEntry(item.event, backend, audit.DecisionAsk, audit.DecisionByHuman)) {
		return errAuditUnavailable
	}
	ctx, cancel := context.WithTimeout(a.ctx, 8*time.Second)
	err := a.engine.ApplyDecision(ctx, sessionID, item.event, adapters.DecisionManual, manualInput)
	cancel()
	if err != nil {
		if errors.Is(err, adapters.ErrEventMismatch) {
			if !a.recordAudit(deliveryAuditEntry(item.event, backend, audit.DecisionAsk, audit.DecisionByHuman, audit.OutcomeFallbackStale, "fallback_stale")) {
				return errAuditUnavailable
			}
			a.resolveEvent(key)
			a.reconcilePending(sessionID)
			advance = true
			return errDecisionStale
		}
		if !a.recordAudit(deliveryAuditEntry(item.event, backend, audit.DecisionAsk, audit.DecisionByHuman, audit.OutcomeFallbackDeliveryUncertain, "delivery_uncertain")) {
			return errAuditUnavailable
		}
		a.freezeSession(key, "delivery_uncertain")
		return errDeliveryUncertain
	}
	if !a.recordAudit(deliveryAuditEntry(item.event, backend, audit.DecisionAsk, audit.DecisionByHuman, audit.OutcomeApplied, "delivery_applied")) {
		return errAuditUnavailable
	}
	a.resolveEvent(key)
	advance = true
	return nil
}

func (a *App) ResizeSession(sessionID string, columns, rows int) error {
	if columns < 1 || rows < 1 || columns > 65535 || rows > 65535 {
		return errors.New("Dimensions de terminal invalides.")
	}
	if a.engine == nil {
		return errRuntimeStopped
	}
	ctx, cancel := context.WithTimeout(a.ctx, 3*time.Second)
	err := a.engine.Resize(ctx, sessionID, terminal.Size{Columns: columns, Rows: rows})
	cancel()
	if err != nil {
		a.emitSafeError("resize_failed", "Le redimensionnement de la session a échoué.", sessionID)
		return errors.New("Le redimensionnement de la session a échoué.")
	}
	return nil
}

func (a *App) StopSession(sessionID string) error {
	if a.engine == nil {
		return errRuntimeStopped
	}
	ctx, cancel := context.WithTimeout(a.ctx, 6*time.Second)
	err := a.engine.Stop(ctx, sessionID)
	cancel()
	if err != nil {
		a.emitSafeError("stop_failed", "Impossible d'arrêter cette session proprement.", sessionID)
		return errors.New("Impossible d'arrêter cette session proprement.")
	}
	return nil
}

func (a *App) Shutdown() error {
	a.shutdownOnce.Do(func() {
		defer close(a.shutdownDone)
		a.closeDelivery()
		a.mu.Lock()
		a.shuttingDown = true
		a.state.RunStatus = "stopping"
		a.mu.Unlock()
		a.emit(eventStatus, StatusEvent{Scope: "run", Status: "stopping"})
		if a.cancel != nil {
			a.cancel()
		}
		if a.engine != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			a.shutdownErr = a.engine.BeginShutdown(ctx)
			cancel()
		}
		a.eventWG.Wait()
		a.deliveryWG.Wait()
		if a.engine != nil {
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			closeErr := a.engine.Close(ctx)
			cancel()
			if closeErr != nil {
				a.shutdownErr = errors.Join(a.shutdownErr, closeErr)
			}
		}
		status := "stopped"
		if a.shutdownErr != nil {
			status = "failed"
		}
		a.mu.Lock()
		a.state.RunStatus = status
		a.mu.Unlock()
		a.emit(eventStatus, StatusEvent{Scope: "run", Status: status})
	})
	<-a.shutdownDone
	if a.shutdownErr != nil {
		return errors.New("La fermeture de Relayer est incomplète; vérifiez les sessions locales.")
	}
	return nil
}

func (a *App) onShutdown(_ context.Context) {
	_ = a.Shutdown()
}

func (a *App) recordAudit(entry audit.Entry) bool {
	if a.engine == nil {
		return false
	}
	if err := a.engine.RecordAudit(entry); err != nil {
		a.freezeAudit()
		return false
	}
	return true
}

func (a *App) freezeAudit() {
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
	for key, item := range a.pending {
		item.view.DeliveryStatus = "failed"
		item.view.Evaluation.Reason = "audit_unavailable"
		a.pending[key] = item
	}
	a.rebuildPendingLocked()
	a.mu.Unlock()
	a.emit(eventStatus, StatusEvent{Scope: "audit", Status: "failed"})
	a.emitSafeError("audit_unavailable", "Audit local indisponible: aucune nouvelle décision ne sera envoyée.", "")
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

func (a *App) markSessionError(sessionID, reason string) {
	backend := a.backendFor(sessionID)
	_ = a.recordAudit(audit.Entry{
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
	a.emit(eventStatus, StatusEvent{Scope: "session", SessionID: sessionID, Status: "failed"})
	a.emitSafeError("backend_stream_failed", "Le flux du backend a rencontré une erreur.", sessionID)
}

func (a *App) markLegacyExit(sessionID string) {
	a.mu.Lock()
	if index, found := a.agentIndex[strings.ToLower(sessionID)]; found {
		a.state.Agents[index].Status = "failed"
		a.state.Agents[index].Running = false
	}
	a.clearSessionPendingLocked(sessionID)
	a.rebuildPendingLocked()
	a.mu.Unlock()
	a.emit(eventStatus, StatusEvent{Scope: "session", SessionID: sessionID, Status: "failed"})
}

func (a *App) backendFor(sessionID string) string {
	a.mu.RLock()
	defer a.mu.RUnlock()
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

func (a *App) emitSafeError(code, message, sessionID string) {
	a.emit(eventError, SafeErrorEvent{
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

func supervisionView(event adapters.Event, evaluation policy.Evaluation, delivery string) SupervisionEvent {
	timestamp := event.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	return SupervisionEvent{
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
	}
}

func snapshotFromAgent(agent AgentState) SnapshotEvent {
	return SnapshotEvent{
		SessionID: agent.SessionID,
		Revision:  agent.Revision,
		Output:    agent.Output,
		Status:    agent.Status,
		Running:   agent.Running,
		Attached:  agent.Attached,
		ExitCode:  cloneInt(agent.ExitCode),
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
