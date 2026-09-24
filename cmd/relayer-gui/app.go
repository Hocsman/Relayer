package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
	"unicode"

	appcore "github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/preflight"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
	"github.com/Hocsman/Relayer/internal/telemetry"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
	wailsruntime "github.com/wailsapp/wails/v2/pkg/runtime"
)

const (
	eventSnapshot    = "relayer:snapshot"
	eventSemantic    = "relayer:event"
	eventStatus      = "relayer:status"
	eventError       = "relayer:error"
	outputFrameDelay = 40 * time.Millisecond
)

// The bridge's refusals are the supervision core's: the same values, so every
// errors.Is against them holds on either side of the package boundary.
var (
	errAuditUnavailable    = supervise.ErrAuditUnavailable
	errDecisionStale       = supervise.ErrDecisionStale
	errDecisionInFlight    = supervise.ErrDecisionInFlight
	errEmptyDecision       = supervise.ErrEmptyDecision
	errUnsupportedDecision = supervise.ErrUnsupportedDecision
	errDeliveryUncertain   = supervise.ErrDeliveryUncertain
	errLineInFlight        = supervise.ErrLineInFlight
	errLinePromptPending   = supervise.ErrLinePromptPending
	errLineUnavailable     = supervise.ErrLineUnavailable
	errRuntimeStopped      = supervise.ErrRuntimeStopped
	errRunStale            = supervise.ErrRunStale
	errAgentUnknown        = supervise.ErrAgentUnknown
	errAgentStillRunning   = supervise.ErrAgentStillRunning
)

// desktopEngine is the runtime of one run: what the supervision core decides
// with, and what only the bridge uses — the run's description, its output, the
// terminal size and the run's own lifecycle.
type desktopEngine interface {
	supervise.Engine
	Metadata() appcore.DesktopMetadata
	Sessions() []appcore.DesktopSession
	StartupLogs() []string
	Events() <-chan session.Event
	Output(string) (string, error)
	AnsiOutput(string) (string, error)
	Resize(context.Context, string, terminal.Size) error
	Stop(context.Context, string) error
	BeginShutdown(context.Context) error
	BeginRestart(context.Context) error
	Close(context.Context) error
	TelemetrySnapshot() telemetry.Snapshot
}

var _ supervise.Engine = (*appcore.DesktopRuntime)(nil)

// runGeneration is one run: its runtime and the supervision core that decides
// what reaches its agents. Both are replaced together by the next run.
type runGeneration struct {
	id         string
	generation uint64
	ctx        context.Context
	cancel     context.CancelFunc
	engine     desktopEngine
	sup        *supervise.Supervisor
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

	// mu guards the bridge's own display state. A lock of the supervision core
	// may be taken while it is held, never the reverse: the core calls the
	// bridge's sink only after releasing its own.
	mu sync.RWMutex
	// state holds what only the bridge knows. The agents' supervision fields
	// (status, running, attached, frozen input, exit code), the pending
	// prompts and the journal's failure are the active run's core's, laid over
	// it by stateLocked.
	state         AppState
	agentIndex    map[string]int
	outputRunning map[string]bool
	outputDirty   map[string]bool
	startupErr    error
	configPath    string

	// eventWG tracks the bridge's own goroutines: the event loop and the
	// output refreshes. The core tracks its automatic decisions itself.
	eventWG sync.WaitGroup

	profilesMu            sync.Mutex
	activeConfigRevision  string
	profileRevisionHash   string
	profileRevisionToken  string
	profileDetector       toolcatalog.Detector
	profileTokenGenerator func() (string, error)

	// notifier is replaced when settings are saved and read by the event
	// consumer, so it has its own lock; the consumer read it with none.
	notifierMu sync.RWMutex
	notifier   notify.Notifier

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
		outputRunning:         make(map[string]bool),
		outputDirty:           make(map[string]bool),
		shutdownDone:          make(chan struct{}),
		profileDetector:       toolcatalog.DefaultDetector(),
		profileTokenGenerator: newOpaqueProfileToken,
		notifier:              newNotifier(notify.DefaultConfig()),
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
		a.failStartup(errors.New(supervise.SafeDisplayError(err)))
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
	if err := a.activateRun(run); err != nil {
		cancel()
	}
}

// activateRun makes run the active run, with a supervision core of its own.
// It changes nothing when it fails, and the caller still owns the run's
// runtime: a run that is not active is never drained or closed by
// stopGenerationLocked or Shutdown.
func (a *App) activateRun(run *runGeneration) error {
	if run == nil || run.engine == nil {
		return errLifecycleFailed
	}
	engine := run.engine
	metadata := engine.Metadata()
	sessions := engine.Sessions()
	agents := make([]AgentState, 0, len(sessions))
	specs := make([]supervise.AgentSpec, 0, len(sessions))
	index := make(map[string]int, len(sessions))
	for _, item := range sessions {
		output, _ := engine.Output(item.ID)
		index[strings.ToLower(item.ID)] = len(agents)
		// Status, Running and the other supervision fields are the core's,
		// which starts every agent running.
		agents = append(agents, AgentState{
			SessionID:      item.ID,
			AgentID:        item.ID,
			Name:           item.Name,
			DisplayCommand: item.Command,
			Backend:        item.Backend,
			Adapter:        item.Adapter,
			Output:         output,
			Revision:       1,
			Simulated:      item.Simulated,
		})
		specs = append(specs, supervise.AgentSpec{
			SessionID: item.ID,
			AgentID:   item.ID,
			Name:      item.Name,
			Backend:   item.Backend,
			Adapter:   item.Adapter,
		})
	}
	// Each run gets a core of its own, so nothing of the previous run's
	// prompts, answers, freezes or journal failure carries over.
	sup, err := supervise.New(run.ctx, engine, supervise.Options{
		RunID:  run.id,
		Agents: specs,
		Sink:   desktopSink{app: a, run: run},
	})
	if err != nil {
		return errLifecycleFailed
	}
	run.sup = sup
	a.setNotifier(newNotifier(metadata.Notifications))
	a.mu.Lock()
	a.active = run
	a.engine = engine
	a.agentIndex = index
	a.outputRunning = make(map[string]bool)
	a.outputDirty = make(map[string]bool)
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
	return nil
}

// isActiveRun reports whether run is the active run and is not draining.
func (a *App) isActiveRun(run *runGeneration) bool {
	if run == nil {
		return false
	}
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.active == run && !runDraining(run)
}

// runDraining reports whether a run takes nothing more in. A run without a
// core has nothing to take in.
func runDraining(run *runGeneration) bool {
	return run.sup == nil || run.sup.Draining()
}

func (a *App) activeRun(expectedRunID string) (*runGeneration, error) {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.active == nil || a.active.engine == nil || runDraining(a.active) {
		return nil, errRuntimeStopped
	}
	if strings.TrimSpace(expectedRunID) == "" || expectedRunID != a.active.id {
		return nil, errRunStale
	}
	return a.active, nil
}

// supervisor returns the supervision core of the active run, or nil between
// runs. The core refuses a nil receiver's operations with errRuntimeStopped,
// after checking their arguments, which is the order the bridge always had.
func (a *App) supervisor() *supervise.Supervisor {
	a.mu.RLock()
	defer a.mu.RUnlock()
	if a.active == nil {
		return nil
	}
	return a.active.sup
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
			default:
				run.sup.Handle(message)
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
	if a.active != run || runDraining(run) {
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
			if a.outputDirty[key] && a.active == run && !runDraining(run) {
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
	return a.stateLocked(), nil
}

// stateLocked is a deep copy of the display state: the bridge's own fields,
// with the active run's supervision state laid over them from one reading of
// its core. The caller holds a.mu.
func (a *App) stateLocked() AppState {
	state := cloneAppState(a.state)
	if a.active == nil || a.active.sup == nil {
		return state
	}
	core := a.active.sup.State()
	agents := make(map[string]supervise.Agent, len(core.Agents))
	for _, agent := range core.Agents {
		agents[strings.ToLower(agent.SessionID)] = agent
	}
	for index := range state.Agents {
		if agent, found := agents[strings.ToLower(state.Agents[index].SessionID)]; found {
			state.Agents[index] = withSupervision(state.Agents[index], agent)
		}
	}
	state.PendingEvents = nil
	for _, view := range core.Pending {
		state.PendingEvents = append(state.PendingEvents, supervisionEventFromView(view))
	}
	if core.AuditFailed {
		state.Audit.Status = "failed"
	}
	return state
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
	output, err := run.engine.AnsiOutput(sessionID)
	if err != nil {
		output, err = run.engine.Output(sessionID)
	}
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
	displayed := *agent
	if supervised, found := run.sup.Agent(agent.SessionID); found {
		displayed = withSupervision(displayed, supervised)
	}
	payload := snapshotFromAgent(run.id, displayed)
	a.mu.Unlock()
	a.emit(eventSnapshot, payload)
}

// SubmitDecision relays a manual value to the exact canonical occurrence. The
// value is never copied into application state, events, errors or audit data.
//
// The desktop has one operator, at the machine, and no roles or connections:
// it passes the core the zero Actor here, in SubmitAutomaticDecision and in
// SubmitLine, so that its journal names nobody, exactly as it always has.
func (a *App) SubmitDecision(runID, sessionID, eventID, manualInput string) error {
	return a.supervisor().SubmitDecision(runID, sessionID, eventID, manualInput, supervise.Actor{})
}

// SubmitAutomaticDecision relays an answer the adapter encodes itself, so the
// operator does not have to know the keystroke a given CLI expects.
func (a *App) SubmitAutomaticDecision(runID, sessionID, eventID, decision string) error {
	return a.supervisor().SubmitAutomaticDecision(runID, sessionID, eventID, decision, supervise.Actor{})
}

// SubmitLine sends one ordinary application line to a detached, running
// session. The line crosses this method only as a call argument: it is never
// copied into bridge state, events, errors or audit entries.
func (a *App) SubmitLine(runID, sessionID, line string) error {
	return a.supervisor().SubmitLine(runID, sessionID, line, supervise.Actor{})
}

func (a *App) ResizeSession(runID, sessionID string, columns, rows int) error {
	if columns < 1 || rows < 1 || columns > 65535 || rows > 65535 {
		return errors.New("invalid terminal dimensions")
	}
	run, err := a.activeRun(runID)
	if err != nil {
		return err
	}
	release, admitted := run.sup.Admit()
	if !admitted {
		return errRuntimeStopped
	}
	defer release()
	ctx, cancel := context.WithTimeout(run.ctx, 3*time.Second)
	err = run.engine.Resize(ctx, sessionID, terminal.Size{Columns: columns, Rows: rows})
	cancel()
	if err != nil {
		a.emitSafeError(run, "resize_failed", "resizing the session failed", sessionID)
		return errors.New("resizing the session failed")
	}
	return nil
}

// StopSession strictly stops one agent while the other agents of the run keep
// running.
func (a *App) StopSession(runID, sessionID string) error {
	return a.supervisor().StopSession(runID, sessionID)
}

// StartSession launches a fresh process for one stopped or exited agent under
// its unchanged identity and plan specification, without interrupting the
// other agents of the run.
func (a *App) StartSession(runID, sessionID string) error {
	return a.supervisor().StartSession(runID, sessionID)
}

// RestartSession transactionally stops then starts one agent in place. An
// unconfirmed stop never produces a replacement process; a failed start
// leaves the agent down with its identity locked for an explicit retry.
func (a *App) RestartSession(runID, sessionID string) error {
	return a.supervisor().RestartSession(runID, sessionID)
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
		return errors.New("shutdown did not complete, check the local sessions")
	}
	return nil
}

func (a *App) onShutdown(_ context.Context) {
	_ = a.Shutdown()
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

func (a *App) currentNotifier() notify.Notifier {
	a.notifierMu.RLock()
	defer a.notifierMu.RUnlock()
	return a.notifier
}

// newNotifier builds the desktop notifier. Webhook delivery failures go to
// standard error, like the lifecycle's diagnostics: they were dropped, so a
// webhook that had stopped working looked exactly like one with nothing to say.
// The lines never carry the URL or the headers.
func newNotifier(config notify.Config) notify.Notifier {
	return notify.NewWithDiagnostics(config, nil, os.Stderr)
}

func (a *App) setNotifier(notifier notify.Notifier) {
	a.notifierMu.Lock()
	a.notifier = notifier
	a.notifierMu.Unlock()
}
