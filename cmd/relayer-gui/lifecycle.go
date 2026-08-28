package main

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	appcore "github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/terminal"
)

var (
	errLifecycleFailed      = errors.New("Le cycle des agents n'a pas pu être terminé de façon sûre.")
	errExecutionUnsupported = errors.New("L'exécution des agents n'est pas disponible sur cette plateforme.")
)

func (a *App) desktopOptions(path string) appcore.DesktopOptions {
	return appcore.DesktopOptions{
		ConfigPath: path,
		InitialSize: terminal.Size{
			Columns: 120,
			Rows:    32,
		},
		Diagnostics: os.Stderr,
	}
}

// SaveAgentProfilesAndRestart is the sole mutating lifecycle transaction. It
// first publishes and preflights the complete candidate configuration, then
// drains and strictly stops the previous run. If candidate startup fails, the
// exact previous YAML is restored with CAS and its immutable plan is launched
// as a fresh generation. An uncertain cleanup never starts another process.
func (a *App) SaveAgentProfilesAndRestart(request RestartAgentProfilesRequest) (AgentLifecycleResult, error) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()

	if a.finalShutdown {
		return AgentLifecycleResult{}, errRuntimeStopped
	}
	if a.lifecycleBlocked {
		return AgentLifecycleResult{}, errLifecycleFailed
	}
	if !desktopAgentExecutionSupported() {
		return AgentLifecycleResult{}, errExecutionUnsupported
	}

	a.mu.RLock()
	oldRun := a.active
	a.mu.RUnlock()
	if oldRun == nil {
		if strings.TrimSpace(request.ExpectedRunID) != "" {
			return AgentLifecycleResult{}, errRunStale
		}
	} else if request.ExpectedRunID != oldRun.id {
		return AgentLifecycleResult{}, errRunStale
	}

	a.profilesMu.Lock()
	defer a.profilesMu.Unlock()
	path, err := a.profileConfigPathLocked()
	if err != nil {
		return AgentLifecycleResult{}, errProfilesSave
	}
	snapshot, err := config.CaptureFileSnapshot(path)
	if err != nil {
		return AgentLifecycleResult{}, errProfilesSave
	}
	defer snapshot.Discard()
	previous, err := config.Load(path)
	if err != nil {
		return AgentLifecycleResult{}, errProfilesSave
	}
	if previous.Revision != snapshot.Revision() {
		return AgentLifecycleResult{}, errProfilesStale
	}
	rollbackToken, err := a.profileTokenGenerator()
	if err != nil {
		return AgentLifecycleResult{}, errProfilesSave
	}

	updated, candidateToken, err := a.saveAgentProfilesLocked(SaveAgentProfilesRequest{
		ExpectedRevision: request.ExpectedRevision,
		Profiles:         request.Profiles,
	})
	if err != nil {
		return AgentLifecycleResult{}, err
	}
	changed := updated.Revision != snapshot.Revision()
	restore := func() (config.Result, error) {
		if !changed {
			a.profileRevisionHash = previous.Revision
			a.profileRevisionToken = rollbackToken
			return previous, nil
		}
		restored, revision, restoreErr := snapshot.Restore(updated.Revision)
		if restoreErr != nil {
			return config.Result{}, restoreErr
		}
		a.profileRevisionHash = revision
		a.profileRevisionToken = rollbackToken
		return restored, nil
	}

	prepare := a.prepareEngine
	if prepare == nil {
		prepare = appcore.PrepareDesktopRuntime
	}
	candidatePlan, err := prepare(a.desktopOptions(path))
	if err != nil {
		_, _ = restore()
		return AgentLifecycleResult{}, errLifecycleFailed
	}

	outcome := "started"
	if oldRun != nil {
		outcome = "restarted"
		stopErr := a.stopGenerationLocked(oldRun, true, "restarting")
		a.activeConfigRevision = ""
		if stopErr != nil {
			a.lifecycleBlocked = true
			_, _ = restore()
			a.setRunStatus("failed", "")
			return AgentLifecycleResult{}, errLifecycleFailed
		}
	}

	candidate, err := a.startGenerationLocked(candidatePlan)
	if err == nil {
		a.activeConfigRevision = updated.Revision
		view := a.agentProfilesViewLocked(updated, candidateToken)
		state, _ := a.GetState()
		return AgentLifecycleResult{Outcome: outcome, State: state, Profiles: view}, nil
	}
	if errors.Is(err, appcore.ErrCleanupUncertain) {
		a.lifecycleBlocked = true
		a.activeConfigRevision = ""
		a.setRunStatus("failed", "")
		return AgentLifecycleResult{}, errLifecycleFailed
	}

	restored, restoreErr := restore()
	if restoreErr != nil {
		a.lifecycleBlocked = true
		a.setRunStatus("failed", "")
		return AgentLifecycleResult{}, errLifecycleFailed
	}
	if oldRun == nil {
		a.setRunStatus("idle", "")
		return AgentLifecycleResult{}, errLifecycleFailed
	}
	rollbackPlan := oldRun.plan
	if rollbackPlan == nil {
		rollbackPlan, restoreErr = prepare(a.desktopOptions(path))
		if restoreErr != nil {
			a.setRunStatus("failed", "")
			return AgentLifecycleResult{}, errLifecycleFailed
		}
	}
	a.setRunStatus("rollback", oldRun.id)
	rollback, rollbackErr := a.startGenerationLocked(rollbackPlan)
	if rollbackErr != nil {
		a.lifecycleBlocked = true
		a.setRunStatus("failed", "")
		return AgentLifecycleResult{}, errLifecycleFailed
	}
	_ = candidate
	_ = rollback
	a.activeConfigRevision = rollback.engine.Metadata().ConfigRevision
	view := a.agentProfilesViewLocked(restored, rollbackToken)
	state, _ := a.GetState()
	return AgentLifecycleResult{Outcome: "rolled_back", State: state, Profiles: view}, nil
}

// StopRun strictly removes all sessions belonging to the expected run and
// returns the bridge to an idle, configurable state.
func (a *App) StopRun(expectedRunID string) (AppState, error) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.finalShutdown {
		return AppState{}, errRuntimeStopped
	}
	if a.lifecycleBlocked {
		return AppState{}, errLifecycleFailed
	}
	a.mu.RLock()
	run := a.active
	a.mu.RUnlock()
	if run == nil {
		return cloneAppState(a.state), nil
	}
	if strings.TrimSpace(expectedRunID) == "" || expectedRunID != run.id {
		return AppState{}, errRunStale
	}
	if err := a.stopGenerationLocked(run, true, "stopping"); err != nil {
		a.lifecycleBlocked = true
		a.setRunStatus("failed", "")
		return AppState{}, errLifecycleFailed
	}
	a.profilesMu.Lock()
	a.activeConfigRevision = ""
	a.profilesMu.Unlock()
	a.setRunStatus("idle", "")
	return a.GetState()
}

func (a *App) startGenerationLocked(plan *appcore.DesktopPlan) (*runGeneration, error) {
	if plan == nil {
		return nil, errLifecycleFailed
	}
	generator := a.runIDGenerator
	if generator == nil {
		generator = newOpaqueProfileToken
	}
	runID, err := generator()
	if err != nil || strings.TrimSpace(runID) == "" {
		return nil, errLifecycleFailed
	}
	a.nextGeneration++
	parent := a.ctx
	if parent == nil {
		parent = context.Background()
	}
	runCtx, cancel := context.WithCancel(parent)
	a.setRunStatus("starting", runID)
	starter := a.startEngine
	if starter == nil {
		starter = func(ctx context.Context, candidate *appcore.DesktopPlan, id string) (desktopEngine, error) {
			return appcore.StartDesktopRuntime(ctx, candidate, id)
		}
	}
	engine, err := starter(runCtx, plan, runID)
	if err != nil {
		cancel()
		return nil, err
	}
	run := &runGeneration{
		id:         runID,
		generation: a.nextGeneration,
		ctx:        runCtx,
		cancel:     cancel,
		engine:     engine,
		plan:       plan,
	}
	a.activateRun(run)
	a.eventWG.Add(1)
	go a.consumeEvents(run)
	a.emit(eventStatus, StatusEvent{RunID: run.id, Scope: "run", Status: "running"})
	return run, nil
}

func (a *App) stopGenerationLocked(run *runGeneration, strict bool, status string) error {
	if run == nil {
		return nil
	}
	a.closeDelivery()
	a.mu.Lock()
	if a.active != run {
		a.mu.Unlock()
		return errRunStale
	}
	a.shuttingDown = true
	a.state.RunStatus = status
	a.mu.Unlock()
	a.emit(eventStatus, StatusEvent{RunID: run.id, Scope: "run", Status: status})

	// Stop backend I/O before waiting: a PTY write can otherwise remain blocked
	// below Go's context boundary. The audit recorder stays open while admitted
	// operations converge, and no replacement run is started until both the
	// strict stop and every terminal delivery outcome have completed.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	var result error
	if strict {
		result = run.engine.BeginRestart(ctx)
	} else {
		result = run.engine.BeginShutdown(ctx)
	}
	cancel()
	run.cancel()
	a.deliveryWG.Wait()
	a.eventWG.Wait()
	ctx, cancel = context.WithTimeout(context.Background(), 12*time.Second)
	result = errors.Join(result, run.engine.Close(ctx))
	cancel()

	a.mu.Lock()
	if a.active == run {
		a.active = nil
		a.engine = nil
		a.agentIndex = make(map[string]int)
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
		a.state.Agents = []AgentState{}
		a.state.PendingEvents = []SupervisionEvent{}
	}
	a.mu.Unlock()
	return result
}

func (a *App) setRunStatus(status, runID string) {
	a.mu.Lock()
	a.state.RunStatus = status
	if runID != "" || status == "idle" || status == "failed" || status == "stopped" {
		a.state.RunID = runID
	}
	if status == "idle" || status == "failed" || status == "stopped" {
		a.state.StartedAt = ""
	}
	if status == "idle" {
		a.state.Policy = PolicyState{DefaultAction: "ask"}
		a.state.Audit = AuditState{Status: "disabled", Mode: "off"}
	}
	a.mu.Unlock()
	a.emit(eventStatus, StatusEvent{RunID: runID, Scope: "run", Status: status})
}
