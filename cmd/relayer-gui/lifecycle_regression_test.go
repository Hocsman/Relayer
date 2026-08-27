package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	appcore "github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/config"
)

type lifecycleStartResult struct {
	engine *fakeDesktopEngine
	err    error
}

func newLifecycleApp(t *testing.T, oldEngine *fakeDesktopEngine) (*App, string, AgentProfilesView, *appcore.DesktopPlan) {
	t.Helper()
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	loaded, err := config.Load(path)
	if err != nil {
		t.Fatalf("Load default config: %v", err)
	}
	application := NewApp()
	application.ctx = context.Background()
	application.configPath = path
	application.profileDetector = profileDetectorFixture{installed: map[string]string{}}
	application.profileTokenGenerator = profileTokenSequence()
	oldPlan := &appcore.DesktopPlan{}
	if oldEngine != nil {
		oldEngine.metadata.RunID = "run-old"
		oldEngine.metadata.ConfigRevision = loaded.Revision
		application.initializeState(oldEngine)
		application.mu.Lock()
		application.active.plan = oldPlan
		application.mu.Unlock()
		application.activeConfigRevision = loaded.Revision
	}
	view, err := application.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })
	return application, path, view, oldPlan
}

func lifecycleRequest(runID string, view AgentProfilesView, path, agentID string) RestartAgentProfilesRequest {
	return RestartAgentProfilesRequest{
		ExpectedRunID:    runID,
		ExpectedRevision: view.Revision,
		Profiles: []AgentProfileInput{{
			ID: agentID, Name: agentID, PresetID: "custom", Cwd: filepath.Dir(path), Backend: "pty", Argv: []string{"runner"},
		}},
	}
}

func lifecycleRunIDs(ids ...string) func() (string, error) {
	index := 0
	return func() (string, error) {
		if index >= len(ids) {
			return "", errors.New("fixture run ID sequence exhausted")
		}
		value := ids[index]
		index++
		return value, nil
	}
}

func installLifecycleStarter(
	t *testing.T,
	application *App,
	path string,
	results ...lifecycleStartResult,
) func() ([]string, int) {
	t.Helper()
	var mu sync.Mutex
	startIDs := make([]string, 0, len(results))
	index := 0
	prepareCalls := 0
	application.prepareEngine = func(options appcore.DesktopOptions) (*appcore.DesktopPlan, error) {
		mu.Lock()
		defer mu.Unlock()
		prepareCalls++
		if options.ConfigPath != path {
			t.Fatalf("prepare config path = %q, want %q", options.ConfigPath, path)
		}
		return &appcore.DesktopPlan{}, nil
	}
	application.startEngine = func(_ context.Context, _ *appcore.DesktopPlan, runID string) (desktopEngine, error) {
		mu.Lock()
		defer mu.Unlock()
		startIDs = append(startIDs, runID)
		if index >= len(results) {
			return nil, errors.New("fixture start sequence exhausted")
		}
		result := results[index]
		index++
		if result.engine != nil {
			loaded, err := config.Load(path)
			if err != nil {
				return nil, err
			}
			result.engine.metadata.RunID = runID
			result.engine.metadata.ConfigRevision = loaded.Revision
		}
		return result.engine, result.err
	}
	return func() ([]string, int) {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), startIDs...), prepareCalls
	}
}

func readLifecycleFile(t *testing.T, path string) ([]byte, os.FileMode) {
	t.Helper()
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat config: %v", err)
	}
	return payload, info.Mode().Perm()
}

func assertLifecycleFileExact(t *testing.T, path string, want []byte, wantMode os.FileMode) {
	t.Helper()
	got, mode := readLifecycleFile(t, path)
	if !bytes.Equal(got, want) || mode != wantMode {
		t.Fatalf("configuration rollback was not exact: bytesEqual=%t mode=%o wantMode=%o", bytes.Equal(got, want), mode, wantMode)
	}
}

func TestLifecycleStartsFromIdleWithoutEagerFactory(t *testing.T) {
	application, path, view, _ := newLifecycleApp(t, nil)
	candidate := newFakeDesktopEngine("agent-new")
	application.runIDGenerator = lifecycleRunIDs("run-started")
	counts := installLifecycleStarter(t, application, path, lifecycleStartResult{engine: candidate})

	state, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState idle: %v", err)
	}
	if state.RunStatus != "idle" || state.RunID != "" || len(state.Agents) != 0 {
		t.Fatalf("initial lifecycle state = %#v", state)
	}
	if starts, prepares := counts(); len(starts) != 0 || prepares != 0 {
		t.Fatalf("idle startup touched factory: starts=%v prepares=%d", starts, prepares)
	}

	result, err := application.SaveAgentProfilesAndRestart(lifecycleRequest("", view, path, "agent-new"))
	if err != nil {
		t.Fatalf("SaveAgentProfilesAndRestart idle: %v", err)
	}
	if result.Outcome != "started" || result.State.RunStatus != "running" || result.State.RunID != "run-started" {
		t.Fatalf("idle start result = %#v", result)
	}
	if result.Profiles.RestartRequired {
		t.Fatalf("started profiles still require restart: %#v", result.Profiles)
	}
	if starts, prepares := counts(); !reflect.DeepEqual(starts, []string{"run-started"}) || prepares != 1 {
		t.Fatalf("factory calls: starts=%v prepares=%d", starts, prepares)
	}
}

func TestLifecycleRestartSuccessClosesOldBeforePublishingCandidate(t *testing.T) {
	oldEngine := newFakeDesktopEngine("agent-old")
	application, path, view, _ := newLifecycleApp(t, oldEngine)
	candidate := newFakeDesktopEngine("agent-new")
	application.runIDGenerator = lifecycleRunIDs("run-new")
	startedAfterOldClose := false
	application.prepareEngine = func(appcore.DesktopOptions) (*appcore.DesktopPlan, error) {
		return &appcore.DesktopPlan{}, nil
	}
	application.startEngine = func(_ context.Context, _ *appcore.DesktopPlan, runID string) (desktopEngine, error) {
		oldEngine.mu.Lock()
		startedAfterOldClose = oldEngine.closed
		oldEngine.mu.Unlock()
		loaded, err := config.Load(path)
		if err != nil {
			return nil, err
		}
		candidate.metadata.RunID = runID
		candidate.metadata.ConfigRevision = loaded.Revision
		return candidate, nil
	}

	result, err := application.SaveAgentProfilesAndRestart(lifecycleRequest("run-old", view, path, "agent-new"))
	if err != nil {
		t.Fatalf("SaveAgentProfilesAndRestart: %v", err)
	}
	if result.Outcome != "restarted" || result.State.RunID != "run-new" || result.State.RunStatus != "running" {
		t.Fatalf("restart result = %#v", result)
	}
	oldEngine.mu.Lock()
	restartCalls, closeCalls := oldEngine.restartCalls, oldEngine.closeCalls
	oldEngine.mu.Unlock()
	if restartCalls != 1 || closeCalls != 1 || !startedAfterOldClose {
		t.Fatalf("old lifecycle: restart=%d close=%d candidateAfterClose=%t", restartCalls, closeCalls, startedAfterOldClose)
	}
}

func TestLifecycleSaveAndPrepareFailuresLeaveActiveRunAndFileUntouched(t *testing.T) {
	for _, test := range []struct {
		name       string
		mutate     func(*RestartAgentProfilesRequest)
		prepareErr error
		wantErr    error
	}{
		{name: "save", mutate: func(request *RestartAgentProfilesRequest) { request.ExpectedRevision = "stale" }, wantErr: errProfilesStale},
		{name: "prepare", prepareErr: errors.New("fixture prepare failed"), wantErr: errLifecycleFailed},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldEngine := newFakeDesktopEngine("agent-old")
			application, path, view, _ := newLifecycleApp(t, oldEngine)
			before, mode := readLifecycleFile(t, path)
			prepareCalls := 0
			startCalls := 0
			application.prepareEngine = func(appcore.DesktopOptions) (*appcore.DesktopPlan, error) {
				prepareCalls++
				if test.prepareErr != nil {
					return nil, test.prepareErr
				}
				return &appcore.DesktopPlan{}, nil
			}
			application.startEngine = func(context.Context, *appcore.DesktopPlan, string) (desktopEngine, error) {
				startCalls++
				return newFakeDesktopEngine("unexpected"), nil
			}
			request := lifecycleRequest("run-old", view, path, "agent-new")
			if test.mutate != nil {
				test.mutate(&request)
			}
			_, lifecycleErr := application.SaveAgentProfilesAndRestart(request)
			if !errors.Is(lifecycleErr, test.wantErr) {
				t.Fatalf("lifecycle error = %v, want %v", lifecycleErr, test.wantErr)
			}
			assertLifecycleFileExact(t, path, before, mode)
			state, err := application.GetState()
			if err != nil {
				t.Fatalf("GetState: %v", err)
			}
			if state.RunID != "run-old" || state.RunStatus != "running" {
				t.Fatalf("active run changed after %s failure: %#v", test.name, state)
			}
			oldEngine.mu.Lock()
			restartCalls, closeCalls := oldEngine.restartCalls, oldEngine.closeCalls
			oldEngine.mu.Unlock()
			if restartCalls != 0 || closeCalls != 0 || startCalls != 0 {
				t.Fatalf("failure touched runtimes: prepare=%d start=%d restart=%d close=%d", prepareCalls, startCalls, restartCalls, closeCalls)
			}
			if test.name == "save" && prepareCalls != 0 {
				t.Fatalf("save failure reached preflight %d time(s)", prepareCalls)
			}
		})
	}
}

func TestLifecycleExternalConfigurationMismatchNeverStopsActiveGeneration(t *testing.T) {
	oldEngine := newFakeDesktopEngine("agent-old")
	application, path, view, _ := newLifecycleApp(t, oldEngine)
	original, mode := readLifecycleFile(t, path)
	external := append(append([]byte(nil), original...), []byte("\n# external editor owns this revision\n")...)
	if err := os.WriteFile(path, external, mode); err != nil {
		t.Fatalf("publish external edit: %v", err)
	}
	prepareCalls := 0
	startCalls := 0
	application.prepareEngine = func(appcore.DesktopOptions) (*appcore.DesktopPlan, error) {
		prepareCalls++
		return &appcore.DesktopPlan{}, nil
	}
	application.startEngine = func(context.Context, *appcore.DesktopPlan, string) (desktopEngine, error) {
		startCalls++
		return newFakeDesktopEngine("unexpected"), nil
	}

	_, err := application.SaveAgentProfilesAndRestart(lifecycleRequest("run-old", view, path, "agent-new"))
	if !errors.Is(err, errProfilesStale) {
		t.Fatalf("SaveAgentProfilesAndRestart error = %v, want errProfilesStale", err)
	}
	current, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read external edit: %v", readErr)
	}
	if !bytes.Equal(current, external) {
		t.Fatalf("lifecycle overwrote external edit: current=%q external=%q", current, external)
	}
	oldEngine.mu.Lock()
	restartCalls, closeCalls := oldEngine.restartCalls, oldEngine.closeCalls
	oldEngine.mu.Unlock()
	if prepareCalls != 0 || startCalls != 0 || restartCalls != 0 || closeCalls != 0 {
		t.Fatalf("revision mismatch touched runtime: prepare=%d start=%d restart=%d close=%d", prepareCalls, startCalls, restartCalls, closeCalls)
	}
	state, stateErr := application.GetState()
	if stateErr != nil {
		t.Fatalf("GetState: %v", stateErr)
	}
	if state.RunID != "run-old" || state.RunStatus != "running" {
		t.Fatalf("external mismatch changed active run: %#v", state)
	}
}

func TestLifecycleCandidateFailureRestoresExactFileAndStartsRollbackGeneration(t *testing.T) {
	oldEngine := newFakeDesktopEngine("agent-old")
	application, path, view, _ := newLifecycleApp(t, oldEngine)
	before, mode := readLifecycleFile(t, path)
	rollbackEngine := newFakeDesktopEngine("agent-old")
	application.runIDGenerator = lifecycleRunIDs("run-candidate", "run-rollback")
	counts := installLifecycleStarter(t, application, path,
		lifecycleStartResult{err: errors.New("fixture candidate failed")},
		lifecycleStartResult{engine: rollbackEngine},
	)

	result, err := application.SaveAgentProfilesAndRestart(lifecycleRequest("run-old", view, path, "agent-new"))
	if err != nil {
		t.Fatalf("SaveAgentProfilesAndRestart rollback: %v", err)
	}
	if result.Outcome != "rolled_back" || result.State.RunID != "run-rollback" || result.State.RunStatus != "running" {
		t.Fatalf("rollback result = %#v", result)
	}
	assertLifecycleFileExact(t, path, before, mode)
	starts, prepares := counts()
	if !reflect.DeepEqual(starts, []string{"run-candidate", "run-rollback"}) || prepares != 1 {
		t.Fatalf("rollback factory calls: starts=%v prepares=%d", starts, prepares)
	}
	oldEngine.mu.Lock()
	restartCalls, closeCalls := oldEngine.restartCalls, oldEngine.closeCalls
	oldEngine.mu.Unlock()
	if restartCalls != 1 || closeCalls != 1 || application.activeConfigRevision == "" {
		t.Fatalf("rollback cleanup/revision: restart=%d close=%d activeRevision=%q", restartCalls, closeCalls, application.activeConfigRevision)
	}
}

func TestLifecycleRollbackStartupFailureRestoresExactFileAndFailsClosed(t *testing.T) {
	oldEngine := newFakeDesktopEngine("agent-old")
	application, path, view, _ := newLifecycleApp(t, oldEngine)
	before, mode := readLifecycleFile(t, path)
	application.runIDGenerator = lifecycleRunIDs("run-candidate", "run-rollback")
	counts := installLifecycleStarter(t, application, path,
		lifecycleStartResult{err: errors.New("fixture candidate failed")},
		lifecycleStartResult{err: errors.New("fixture rollback failed")},
	)

	_, err := application.SaveAgentProfilesAndRestart(lifecycleRequest("run-old", view, path, "agent-new"))
	if !errors.Is(err, errLifecycleFailed) {
		t.Fatalf("SaveAgentProfilesAndRestart error = %v, want errLifecycleFailed", err)
	}
	assertLifecycleFileExact(t, path, before, mode)
	starts, prepares := counts()
	if !reflect.DeepEqual(starts, []string{"run-candidate", "run-rollback"}) || prepares != 1 {
		t.Fatalf("failed rollback factory calls: starts=%v prepares=%d", starts, prepares)
	}
	state, stateErr := application.GetState()
	if stateErr != nil {
		t.Fatalf("GetState: %v", stateErr)
	}
	if state.RunStatus != "failed" || state.RunID != "" || state.StartedAt != "" || len(state.Agents) != 0 {
		t.Fatalf("failed rollback state = %#v", state)
	}
	oldEngine.mu.Lock()
	restartCalls, closeCalls := oldEngine.restartCalls, oldEngine.closeCalls
	oldEngine.mu.Unlock()
	if restartCalls != 1 || closeCalls != 1 {
		t.Fatalf("old generation cleanup: restart=%d close=%d", restartCalls, closeCalls)
	}
}

func TestLifecycleRollbackCASConflictPreservesExternalEditAndFailsClosed(t *testing.T) {
	oldEngine := newFakeDesktopEngine("agent-old")
	application, path, view, _ := newLifecycleApp(t, oldEngine)
	application.runIDGenerator = lifecycleRunIDs("run-candidate")
	application.prepareEngine = func(appcore.DesktopOptions) (*appcore.DesktopPlan, error) {
		return &appcore.DesktopPlan{}, nil
	}
	var externalEdit []byte
	startCalls := 0
	application.startEngine = func(_ context.Context, _ *appcore.DesktopPlan, _ string) (desktopEngine, error) {
		startCalls++
		candidate, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		externalEdit = append(append([]byte(nil), candidate...), []byte("\n# concurrent external edit\n")...)
		if err := os.WriteFile(path, externalEdit, 0o600); err != nil {
			return nil, err
		}
		return nil, errors.New("fixture candidate failed after external edit")
	}

	_, err := application.SaveAgentProfilesAndRestart(lifecycleRequest("run-old", view, path, "agent-new"))
	if !errors.Is(err, errLifecycleFailed) {
		t.Fatalf("SaveAgentProfilesAndRestart error = %v, want errLifecycleFailed", err)
	}
	current, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read externally edited config: %v", readErr)
	}
	if !bytes.Equal(current, externalEdit) {
		t.Fatalf("rollback overwrote concurrent edit: current=%q external=%q", current, externalEdit)
	}
	if startCalls != 1 {
		t.Fatalf("start calls = %d, want candidate only", startCalls)
	}
	state, stateErr := application.GetState()
	if stateErr != nil {
		t.Fatalf("GetState: %v", stateErr)
	}
	if state.RunStatus != "failed" || state.RunID != "" || len(state.Agents) != 0 {
		t.Fatalf("CAS conflict state = %#v", state)
	}
	oldEngine.mu.Lock()
	restartCalls, closeCalls := oldEngine.restartCalls, oldEngine.closeCalls
	oldEngine.mu.Unlock()
	if restartCalls != 1 || closeCalls != 1 {
		t.Fatalf("old generation cleanup: restart=%d close=%d", restartCalls, closeCalls)
	}
}

func TestLifecycleCloseFailureRestoresExactFileWithoutStartingCandidate(t *testing.T) {
	oldEngine := newFakeDesktopEngine("agent-old")
	oldEngine.closeErr = errors.New("fixture close failed")
	application, path, view, _ := newLifecycleApp(t, oldEngine)
	before, mode := readLifecycleFile(t, path)
	application.runIDGenerator = lifecycleRunIDs("must-not-start")
	counts := installLifecycleStarter(t, application, path, lifecycleStartResult{engine: newFakeDesktopEngine("unexpected")})

	_, err := application.SaveAgentProfilesAndRestart(lifecycleRequest("run-old", view, path, "agent-new"))
	if !errors.Is(err, errLifecycleFailed) {
		t.Fatalf("lifecycle error = %v, want close failure", err)
	}
	assertLifecycleFileExact(t, path, before, mode)
	starts, prepares := counts()
	if len(starts) != 0 || prepares != 1 {
		t.Fatalf("close failure factory calls: starts=%v prepares=%d", starts, prepares)
	}
	state, stateErr := application.GetState()
	if stateErr != nil {
		t.Fatalf("GetState: %v", stateErr)
	}
	if state.RunStatus != "failed" || state.RunID != "" {
		t.Fatalf("close failure state = %#v", state)
	}
}

func TestLifecycleUncertainOldStopPermanentlyBlocksSubsequentRestart(t *testing.T) {
	for _, test := range []struct {
		name       string
		restartErr error
		closeErr   error
	}{
		{name: "begin restart", restartErr: errors.New("fixture BeginRestart uncertain")},
		{name: "close", closeErr: errors.New("fixture Close uncertain")},
	} {
		t.Run(test.name, func(t *testing.T) {
			oldEngine := newFakeDesktopEngine("agent-old")
			oldEngine.restartErr = test.restartErr
			oldEngine.closeErr = test.closeErr
			application, path, view, _ := newLifecycleApp(t, oldEngine)
			before, mode := readLifecycleFile(t, path)
			startCalls := 0
			prepareCalls := 0
			application.prepareEngine = func(appcore.DesktopOptions) (*appcore.DesktopPlan, error) {
				prepareCalls++
				return &appcore.DesktopPlan{}, nil
			}
			application.startEngine = func(context.Context, *appcore.DesktopPlan, string) (desktopEngine, error) {
				startCalls++
				return newFakeDesktopEngine("unexpected"), nil
			}

			_, firstErr := application.SaveAgentProfilesAndRestart(lifecycleRequest("run-old", view, path, "agent-new"))
			if !errors.Is(firstErr, errLifecycleFailed) {
				t.Fatalf("first SaveAgentProfilesAndRestart error = %v, want errLifecycleFailed", firstErr)
			}
			assertLifecycleFileExact(t, path, before, mode)
			if !application.lifecycleBlocked {
				t.Fatal("uncertain old-generation cleanup did not block the lifecycle")
			}
			state, stateErr := application.GetState()
			if stateErr != nil {
				t.Fatalf("GetState after uncertain stop: %v", stateErr)
			}
			if state.RunStatus != "failed" || state.RunID != "" || len(state.Agents) != 0 {
				t.Fatalf("uncertain stop state = %#v", state)
			}
			refreshed, profilesErr := application.GetAgentProfiles()
			if profilesErr != nil {
				t.Fatalf("GetAgentProfiles after rollback: %v", profilesErr)
			}
			_, secondErr := application.SaveAgentProfilesAndRestart(lifecycleRequest("", refreshed, path, "agent-second"))
			if !errors.Is(secondErr, errLifecycleFailed) {
				t.Fatalf("second SaveAgentProfilesAndRestart error = %v, want lifecycle block", secondErr)
			}
			if prepareCalls != 1 || startCalls != 0 {
				t.Fatalf("blocked lifecycle reached factories: prepare=%d start=%d", prepareCalls, startCalls)
			}
			oldEngine.mu.Lock()
			restartCalls, closeCalls := oldEngine.restartCalls, oldEngine.closeCalls
			oldEngine.mu.Unlock()
			if restartCalls != 1 || closeCalls != 1 {
				t.Fatalf("uncertain cleanup calls: restart=%d close=%d", restartCalls, closeCalls)
			}
		})
	}
}

func TestLifecycleCleanupUncertainCandidateDoesNotRestoreOrStartRollback(t *testing.T) {
	oldEngine := newFakeDesktopEngine("agent-old")
	application, path, view, _ := newLifecycleApp(t, oldEngine)
	before, _ := readLifecycleFile(t, path)
	application.runIDGenerator = lifecycleRunIDs("run-uncertain", "must-not-run")
	application.prepareEngine = func(appcore.DesktopOptions) (*appcore.DesktopPlan, error) {
		return &appcore.DesktopPlan{}, nil
	}
	startCalls := 0
	application.startEngine = func(context.Context, *appcore.DesktopPlan, string) (desktopEngine, error) {
		startCalls++
		return nil, errors.Join(appcore.ErrCleanupUncertain, errors.New("fixture candidate cleanup uncertain"))
	}

	_, err := application.SaveAgentProfilesAndRestart(lifecycleRequest("run-old", view, path, "agent-new"))
	if !errors.Is(err, errLifecycleFailed) {
		t.Fatalf("SaveAgentProfilesAndRestart error = %v, want errLifecycleFailed", err)
	}
	after, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatalf("read candidate config: %v", readErr)
	}
	if bytes.Equal(after, before) {
		t.Fatal("cleanup-uncertain candidate unexpectedly restored the previous configuration")
	}
	loaded, loadErr := config.Load(path)
	if loadErr != nil {
		t.Fatalf("Load retained candidate config: %v", loadErr)
	}
	if len(loaded.Agents) != 1 || loaded.Agents[0].ID != "agent-new" {
		t.Fatalf("retained configuration = %#v, want candidate", loaded.Agents)
	}
	if startCalls != 1 {
		t.Fatalf("start calls = %d, want candidate only", startCalls)
	}
	if !application.lifecycleBlocked {
		t.Fatal("cleanup-uncertain candidate did not permanently block lifecycle mutations")
	}
	state, stateErr := application.GetState()
	if stateErr != nil {
		t.Fatalf("GetState: %v", stateErr)
	}
	if state.RunStatus != "failed" || state.RunID != "" || len(state.Agents) != 0 {
		t.Fatalf("cleanup-uncertain state = %#v", state)
	}
	refreshed, profilesErr := application.GetAgentProfiles()
	if profilesErr != nil {
		t.Fatalf("GetAgentProfiles retained candidate: %v", profilesErr)
	}
	_, retryErr := application.SaveAgentProfilesAndRestart(lifecycleRequest("", refreshed, path, "agent-other"))
	if !errors.Is(retryErr, errLifecycleFailed) || startCalls != 1 {
		t.Fatalf("blocked retry error=%v startCalls=%d", retryErr, startCalls)
	}
	oldEngine.mu.Lock()
	restartCalls, closeCalls := oldEngine.restartCalls, oldEngine.closeCalls
	oldEngine.mu.Unlock()
	if restartCalls != 1 || closeCalls != 1 {
		t.Fatalf("old generation cleanup: restart=%d close=%d", restartCalls, closeCalls)
	}
}

func TestLifecycleRejectsStaleRunRPCsAndIgnoresStaleGenerationEvents(t *testing.T) {
	oldEngine := newFakeDesktopEngine("agent-a")
	application, path, view, _ := newLifecycleApp(t, oldEngine)
	candidate := newFakeDesktopEngine("agent-a")
	application.runIDGenerator = lifecycleRunIDs("run-new")
	installLifecycleStarter(t, application, path, lifecycleStartResult{engine: candidate})

	application.mu.RLock()
	oldRun := application.active
	application.mu.RUnlock()
	result, err := application.SaveAgentProfilesAndRestart(lifecycleRequest("run-old", view, path, "agent-a"))
	if err != nil {
		t.Fatalf("restart: %v", err)
	}
	for operation, operationErr := range map[string]error{
		"decision": application.SubmitDecision("run-old", "agent-a", "event-old", "Y"),
		"resize":   application.ResizeSession("run-old", "agent-a", 80, 24),
		"stop":     application.StopSession("run-old", "agent-a"),
	} {
		if !errors.Is(operationErr, errRunStale) {
			t.Fatalf("stale %s error = %v, want errRunStale", operation, operationErr)
		}
	}
	if _, saveErr := application.SaveAgentProfiles("run-old", SaveAgentProfilesRequest{
		ExpectedRevision: result.Profiles.Revision,
		Profiles:         lifecycleRequest("", result.Profiles, path, "agent-a").Profiles,
	}); !errors.Is(saveErr, errRunStale) {
		t.Fatalf("stale profile save error = %v, want errRunStale", saveErr)
	}

	before, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState before stale events: %v", err)
	}
	oldEngine.outputs["agent-a"] = []string{"stale output"}
	application.refreshOutputForRun(oldRun, "agent-a")
	application.handleAdapterEventForRun(oldRun, bridgeEvent("agent-a", "stale-event"))
	after, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState after stale events: %v", err)
	}
	if !reflect.DeepEqual(after, before) {
		t.Fatalf("stale generation changed active state:\nbefore=%#v\nafter=%#v", before, after)
	}
	candidate.mu.Lock()
	resizeCalls := append([]string(nil), candidate.resizeCalls...)
	stopCalls := append([]string(nil), candidate.stopCalls...)
	candidate.mu.Unlock()
	if len(candidate.applySnapshot()) != 0 || len(resizeCalls) != 0 || len(stopCalls) != 0 {
		t.Fatalf("stale RPC reached candidate: apply=%#v resize=%#v stop=%#v", candidate.applySnapshot(), resizeCalls, stopCalls)
	}

	application.handleAdapterEvent(bridgeEvent("agent-a", "current-event"))
	current, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState current event: %v", err)
	}
	if len(current.PendingEvents) != 1 || current.PendingEvents[0].RunID != "run-new" {
		t.Fatalf("current event is not run-scoped: %#v", current.PendingEvents)
	}
}

func TestLifecycleEventPayloadsCarryActiveRunID(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application := newBridgeForTest(engine)
	application.ctx = context.Background()

	type emittedEvent struct {
		name    string
		payload interface{}
	}
	var (
		emittedMu sync.Mutex
		emitted   []emittedEvent
	)
	application.emitFn = func(_ context.Context, name string, payloads ...interface{}) {
		if len(payloads) != 1 {
			t.Errorf("event %q payload count = %d, want one", name, len(payloads))
			return
		}
		emittedMu.Lock()
		emitted = append(emitted, emittedEvent{name: name, payload: payloads[0]})
		emittedMu.Unlock()
	}

	application.mu.RLock()
	run := application.active
	application.mu.RUnlock()
	application.refreshOutputForRun(run, "agent-a")
	application.handleAdapterEventForRun(run, bridgeEvent("agent-a", "run-scoped-prompt"))
	application.emitSafeError(run, "fixture_error", "Fixture display error.", "agent-a")
	exitCode := 0
	application.handleAdapterEventForRun(run, adapters.NewProcessExitEvent("agent-a", "agent-a", "generic", 2, &exitCode, false))

	emittedMu.Lock()
	snapshot := append([]emittedEvent(nil), emitted...)
	emittedMu.Unlock()
	seen := map[string]bool{}
	for _, item := range snapshot {
		seen[item.name] = true
		var runID string
		switch payload := item.payload.(type) {
		case SnapshotEvent:
			runID = payload.RunID
		case SupervisionEvent:
			runID = payload.RunID
		case StatusEvent:
			runID = payload.RunID
		case SafeErrorEvent:
			runID = payload.RunID
		default:
			t.Fatalf("event %q used unexpected payload %T", item.name, item.payload)
		}
		if runID != "run-test" {
			t.Fatalf("event %q runID = %q, want run-test (payload %#v)", item.name, runID, item.payload)
		}
	}
	for _, name := range []string{eventSnapshot, eventSemantic, eventStatus, eventError} {
		if !seen[name] {
			t.Fatalf("event stream did not exercise %q: %#v", name, snapshot)
		}
	}
}

func TestLifecycleStopRunRejectsStaleThenStopsCurrentGeneration(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	application, _, _, _ := newLifecycleApp(t, engine)

	if _, err := application.StopRun("run-stale"); !errors.Is(err, errRunStale) {
		t.Fatalf("StopRun stale error = %v, want errRunStale", err)
	}
	engine.mu.Lock()
	staleRestartCalls, staleCloseCalls := engine.restartCalls, engine.closeCalls
	engine.mu.Unlock()
	if staleRestartCalls != 0 || staleCloseCalls != 0 {
		t.Fatalf("stale StopRun touched engine: restart=%d close=%d", staleRestartCalls, staleCloseCalls)
	}

	state, err := application.StopRun("run-old")
	if err != nil {
		t.Fatalf("StopRun current: %v", err)
	}
	if state.RunStatus != "idle" || state.RunID != "" || state.StartedAt != "" || len(state.Agents) != 0 || len(state.PendingEvents) != 0 {
		t.Fatalf("StopRun state = %#v", state)
	}
	engine.mu.Lock()
	restartCalls, closeCalls := engine.restartCalls, engine.closeCalls
	engine.mu.Unlock()
	if restartCalls != 1 || closeCalls != 1 {
		t.Fatalf("current StopRun cleanup: restart=%d close=%d", restartCalls, closeCalls)
	}
}

func TestLifecycleRestartDrainsAdmittedDeliveryBeforeClosingOldRun(t *testing.T) {
	oldEngine := newFakeDesktopEngine("agent-a")
	oldEngine.auditFailsAfterClose = true
	oldEngine.applyStarted = make(chan struct{}, 1)
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseDelivery := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseDelivery)
	oldEngine.applyRelease = release
	oldEngine.closeStarted = make(chan struct{}, 1)
	application, path, view, _ := newLifecycleApp(t, oldEngine)
	candidate := newFakeDesktopEngine("agent-new")
	application.runIDGenerator = lifecycleRunIDs("run-new")
	startedBeforeOldClose := false
	application.prepareEngine = func(appcore.DesktopOptions) (*appcore.DesktopPlan, error) { return &appcore.DesktopPlan{}, nil }
	application.startEngine = func(_ context.Context, _ *appcore.DesktopPlan, runID string) (desktopEngine, error) {
		oldEngine.mu.Lock()
		startedBeforeOldClose = !oldEngine.closed
		oldEngine.mu.Unlock()
		candidate.metadata.RunID = runID
		return candidate, nil
	}
	event := bridgeEvent("agent-a", "delivery-before-restart")
	application.handleAdapterEvent(event)
	decisionDone := make(chan error, 1)
	go func() { decisionDone <- application.SubmitDecision("run-old", event.SessionID, event.ID, "Y") }()
	select {
	case <-oldEngine.applyStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("delivery did not reach ApplyDecision")
	}

	restartDone := make(chan struct {
		result AgentLifecycleResult
		err    error
	}, 1)
	go func() {
		result, err := application.SaveAgentProfilesAndRestart(lifecycleRequest("run-old", view, path, "agent-new"))
		restartDone <- struct {
			result AgentLifecycleResult
			err    error
		}{result: result, err: err}
	}()
	waitForCondition(t, 2*time.Second, func() bool {
		application.mu.RLock()
		defer application.mu.RUnlock()
		return application.state.RunStatus == "restarting"
	})
	waitForCondition(t, 2*time.Second, func() bool {
		return operationIndex(oldEngine.operationSnapshot(), "restart:begin") >= 0
	})
	oldEngine.mu.Lock()
	restartCalls, closeCalls := oldEngine.restartCalls, oldEngine.closeCalls
	oldEngine.mu.Unlock()
	if restartCalls != 1 || closeCalls != 0 {
		t.Fatalf("restart must stop I/O but keep audit open while delivery drains: restart=%d close=%d", restartCalls, closeCalls)
	}
	releaseDelivery()
	if err := <-decisionDone; err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}
	restarted := <-restartDone
	if restarted.err != nil || restarted.result.Outcome != "restarted" {
		t.Fatalf("restart result = %#v error=%v", restarted.result, restarted.err)
	}
	if startedBeforeOldClose {
		t.Fatal("candidate started before the old engine closed")
	}
	operations := oldEngine.operationSnapshot()
	applyReturn := operationIndex(operations, "apply:return")
	deliveryAudit := operationIndex(operations, "audit:delivery:applied")
	restartBegin := operationIndex(operations, "restart:begin")
	closeStart := operationIndex(operations, "close:start")
	if restartBegin < 0 || applyReturn < 0 || deliveryAudit < 0 || closeStart < 0 ||
		!(restartBegin < applyReturn && applyReturn < deliveryAudit && deliveryAudit < closeStart) {
		t.Fatalf("unsafe restart ordering: %#v", operations)
	}
}

func TestLifecycleConcurrentShutdownWaitsForRestartThenClosesCandidateOnce(t *testing.T) {
	oldEngine := newFakeDesktopEngine("agent-old")
	oldEngine.restartStart = make(chan struct{}, 1)
	releaseRestart := make(chan struct{})
	var releaseOnce sync.Once
	releaseOldRestart := func() { releaseOnce.Do(func() { close(releaseRestart) }) }
	t.Cleanup(releaseOldRestart)
	oldEngine.restartWait = releaseRestart
	application, path, view, _ := newLifecycleApp(t, oldEngine)
	candidate := newFakeDesktopEngine("agent-new")
	application.runIDGenerator = lifecycleRunIDs("run-new")
	installLifecycleStarter(t, application, path, lifecycleStartResult{engine: candidate})

	restartDone := make(chan error, 1)
	go func() {
		_, err := application.SaveAgentProfilesAndRestart(lifecycleRequest("run-old", view, path, "agent-new"))
		restartDone <- err
	}()
	select {
	case <-oldEngine.restartStart:
	case <-time.After(2 * time.Second):
		t.Fatal("restart did not reach strict old-run stop")
	}

	const callers = 8
	shutdownResults := make(chan error, callers)
	for index := 0; index < callers; index++ {
		go func() { shutdownResults <- application.Shutdown() }()
	}
	releaseOldRestart()
	if err := <-restartDone; err != nil {
		t.Fatalf("restart: %v", err)
	}
	for index := 0; index < callers; index++ {
		if err := <-shutdownResults; err != nil {
			t.Fatalf("Shutdown caller %d: %v", index, err)
		}
	}
	oldEngine.mu.Lock()
	oldClose := oldEngine.closeCalls
	oldEngine.mu.Unlock()
	candidate.mu.Lock()
	candidateClose, candidateShutdown := candidate.closeCalls, candidate.shutdownCalls
	candidate.mu.Unlock()
	if oldClose != 1 || candidateClose != 1 || candidateShutdown != 1 {
		t.Fatalf("concurrent lifecycle close counts: old=%d candidate=%d candidateShutdown=%d", oldClose, candidateClose, candidateShutdown)
	}
	state, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState: %v", err)
	}
	if state.RunStatus != "stopped" || state.RunID != "" {
		t.Fatalf("final shutdown state = %#v", state)
	}
}
