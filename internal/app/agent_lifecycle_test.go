package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/terminal"
)

// lifecycleFakeBackend is a concrete test backend with an operator-controlled
// Remove capability, mirroring what the PTY and tmux managers prove before
// releasing a session identity.
type lifecycleFakeBackend struct {
	*routerFakeBackend

	mu          sync.Mutex
	removes     []string
	removeErr   error
	removeGone  bool
	stopFailure error
}

func (b *lifecycleFakeBackend) Stop(ctx context.Context, id string) error {
	b.mu.Lock()
	err := b.stopFailure
	b.mu.Unlock()
	if err != nil {
		return err
	}
	return b.routerFakeBackend.Stop(ctx, id)
}

func (b *lifecycleFakeBackend) Remove(_ context.Context, id string) error {
	b.mu.Lock()
	b.removes = append(b.removes, id)
	err, gone := b.removeErr, b.removeGone
	b.mu.Unlock()
	if err != nil {
		return err
	}
	if gone {
		return terminal.ErrSessionNotFound
	}
	return nil
}

func newLifecycleFakeBackend() *lifecycleFakeBackend {
	return &lifecycleFakeBackend{
		routerFakeBackend: newRouterFakeBackend(agent.BackendPTY),
	}
}

func lifecycleTestRecorder(t *testing.T) *audit.Recorder {
	t.Helper()
	// Detailed mode keeps bounded lifecycle metadata (restart_count); metadata
	// mode deliberately omits every metadata map from the journal.
	path := filepath.Join(t.TempDir(), "private", "audit.jsonl")
	recorder, err := audit.Open(audit.Config{
		Enabled:       true,
		Mode:          audit.ModeDetailed,
		Path:          path,
		MaxFileSizeMB: 1,
		MaxFiles:      1,
	}, audit.WithRunID("run-lifecycle"))
	if err != nil {
		t.Fatalf("open test audit recorder: %v", err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	return recorder
}

func lifecycleTestSpec(t *testing.T, id string) agent.Spec {
	t.Helper()
	spec, err := agent.ValidateSpec(agent.Spec{
		ID:      id,
		Name:    id,
		Command: []string{"fixture-agent"},
		Cwd:     t.TempDir(),
		Adapter: agent.AdapterGeneric,
		Backend: agent.BackendPTY,
	}, ".", agent.BackendPTY)
	if err != nil {
		t.Fatalf("validate lifecycle spec: %v", err)
	}
	return spec
}

// lifecycleTestEntries reads the recorder's journal file directly; reopening
// it through the audit package would fight the still-open recorder for path
// ownership.
func lifecycleTestEntries(t *testing.T, recorder *audit.Recorder) []audit.Entry {
	t.Helper()
	raw, err := os.ReadFile(recorder.Path())
	if err != nil {
		t.Fatalf("read test audit journal: %v", err)
	}
	var entries []audit.Entry
	for _, line := range bytes.Split(raw, []byte("\n")) {
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var entry audit.Entry
		if err := json.Unmarshal(line, &entry); err != nil {
			t.Fatalf("parse test audit line: %v", err)
		}
		entries = append(entries, entry)
	}
	return entries
}

func TestAgentLifecycleStopThenStartReusesIdentity(t *testing.T) {
	backend := newLifecycleFakeBackend()
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatalf("newBackendRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	spec := lifecycleTestSpec(t, "restartable")
	if _, err := router.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	recorder := lifecycleTestRecorder(t)
	lifecycle := newAgentLifecycle(router, recorder, []agent.Spec{spec}, terminal.Size{Columns: 80, Rows: 24}, nil)

	ctx := context.Background()
	if err := lifecycle.StopAgent(ctx, "restartable", "operator_stop"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	if !lifecycle.finishedRecorded("restartable") {
		t.Fatal("operator-stopped agent has no terminal record before the hot start")
	}
	if _, err := lifecycle.StartAgent(ctx, "restartable", "operator_start"); err != nil {
		t.Fatalf("StartAgent: %v", err)
	}

	backend.mu.Lock()
	starts, removes := len(backend.starts), len(backend.removes)
	backend.mu.Unlock()
	if starts != 2 {
		t.Fatalf("backend starts = %d, want 2 (initial + hot restart)", starts)
	}
	if removes != 1 {
		t.Fatalf("backend removes = %d, want 1", removes)
	}

	entries := lifecycleTestEntries(t, recorder)
	var started, finished int
	for _, entry := range entries {
		if entry.Kind == audit.KindSessionStarted && entry.DecisionBy == audit.DecisionByHuman && entry.Reason == "operator_start" {
			started++
		}
		if entry.Kind == audit.KindSessionFinished && entry.DecisionBy == audit.DecisionByHuman && entry.Reason == "operator_stop" {
			finished++
		}
	}
	if started != 1 || finished != 1 {
		t.Fatalf("hot lifecycle audit = %#v, want one operator session_started and one operator session_finished", lifecycleEntryKinds(entries))
	}
}

func lifecycleEntryKinds(entries []audit.Entry) []string {
	kinds := make([]string, 0, len(entries))
	for _, entry := range entries {
		kinds = append(kinds, string(entry.Kind))
	}
	return kinds
}

func TestAgentLifecycleRemoveFailureBlocksStart(t *testing.T) {
	backend := newLifecycleFakeBackend()
	backend.mu.Lock()
	backend.removeErr = errors.New("fixture release refused")
	backend.mu.Unlock()
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatalf("newBackendRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	spec := lifecycleTestSpec(t, "locked")
	if _, err := router.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	recorder := lifecycleTestRecorder(t)
	lifecycle := newAgentLifecycle(router, recorder, []agent.Spec{spec}, terminal.Size{Columns: 80, Rows: 24}, nil)

	ctx := context.Background()
	if err := lifecycle.StopAgent(ctx, "locked", "operator_stop"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	if _, err := lifecycle.StartAgent(ctx, "locked", "operator_start"); err == nil {
		t.Fatal("StartAgent accepted an identity whose backend could not prove the previous process gone")
	}
	// A StopAgent retry is the recovery path for an identity left uncertain by
	// a refused release; it must succeed once the backend cooperates again.
	backend.mu.Lock()
	backend.removeErr = nil
	backend.mu.Unlock()
	if err := lifecycle.StopAgent(ctx, "locked", "operator_stop_retry"); err != nil {
		t.Fatalf("StopAgent retry: %v", err)
	}
	if _, err := lifecycle.StartAgent(ctx, "locked", "operator_start"); err != nil {
		t.Fatalf("StartAgent after confirmed retry: %v", err)
	}
}

func TestAgentLifecycleStopUncertainLocksIdentity(t *testing.T) {
	backend := newLifecycleFakeBackend()
	backend.mu.Lock()
	backend.stopFailure = errors.New("fixture stop uncertain")
	backend.mu.Unlock()
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatalf("newBackendRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	spec := lifecycleTestSpec(t, "uncertain")
	if _, err := router.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	recorder := lifecycleTestRecorder(t)
	lifecycle := newAgentLifecycle(router, recorder, []agent.Spec{spec}, terminal.Size{Columns: 80, Rows: 24}, nil)

	ctx := context.Background()
	if err := lifecycle.StopAgent(ctx, "uncertain", "operator_stop"); err == nil {
		t.Fatal("StopAgent accepted an unconfirmed stop")
	}
	if _, err := lifecycle.StartAgent(ctx, "uncertain", "operator_start"); !errors.Is(err, errAgentStopUncertain) {
		t.Fatalf("StartAgent after uncertain stop = %v, want errAgentStopUncertain", err)
	}

	backend.mu.Lock()
	backend.stopFailure = nil
	backend.mu.Unlock()
	if err := lifecycle.StopAgent(ctx, "uncertain", "operator_stop_retry"); err != nil {
		t.Fatalf("StopAgent retry: %v", err)
	}
	if _, err := lifecycle.StartAgent(ctx, "uncertain", "operator_start"); err != nil {
		t.Fatalf("StartAgent after confirmed retry: %v", err)
	}
}

func TestAgentLifecycleStartFailureRecordsBackendErrorAndAllowsRetry(t *testing.T) {
	backend := newLifecycleFakeBackend()
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatalf("newBackendRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	spec := lifecycleTestSpec(t, "retryable")
	if _, err := router.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	recorder := lifecycleTestRecorder(t)
	lifecycle := newAgentLifecycle(router, recorder, []agent.Spec{spec}, terminal.Size{Columns: 80, Rows: 24}, nil)

	ctx := context.Background()
	if err := lifecycle.StopAgent(ctx, "retryable", "operator_stop"); err != nil {
		t.Fatalf("StopAgent: %v", err)
	}
	backend.mu.Lock()
	backend.startErr = errors.New("fixture start refused")
	backend.mu.Unlock()
	if _, err := lifecycle.StartAgent(ctx, "retryable", "operator_start"); err == nil {
		t.Fatal("StartAgent accepted a refused start")
	}

	entries := lifecycleTestEntries(t, recorder)
	var backendErrors, startedRecords int
	for _, entry := range entries {
		if entry.Kind == audit.KindBackendError && entry.Reason == "operator_start_failed" && entry.DecisionBy == audit.DecisionByHuman {
			backendErrors++
		}
		if entry.Kind == audit.KindSessionStarted && entry.DecisionBy == audit.DecisionByHuman {
			startedRecords++
		}
	}
	if backendErrors != 1 {
		t.Fatalf("operator_start_failed backend_error entries = %d, want 1", backendErrors)
	}
	if startedRecords != 1 {
		t.Fatalf("session_started entries = %d, want 1 before the successful retry", startedRecords)
	}

	backend.mu.Lock()
	backend.startErr = nil
	backend.mu.Unlock()
	if _, err := lifecycle.StartAgent(ctx, "retryable", "operator_start"); err != nil {
		t.Fatalf("StartAgent retry: %v", err)
	}
	entries = lifecycleTestEntries(t, recorder)
	startedRecords = 0
	for _, entry := range entries {
		if entry.Kind == audit.KindSessionStarted && entry.DecisionBy == audit.DecisionByHuman {
			startedRecords++
		}
	}
	if startedRecords != 2 {
		t.Fatalf("session_started entries after retry = %d, want 2", startedRecords)
	}
}

func TestAgentLifecycleRestartIsTransactionalStopThenStart(t *testing.T) {
	backend := newLifecycleFakeBackend()
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatalf("newBackendRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	spec := lifecycleTestSpec(t, "hot")
	if _, err := router.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	recorder := lifecycleTestRecorder(t)
	lifecycle := newAgentLifecycle(router, recorder, []agent.Spec{spec}, terminal.Size{Columns: 80, Rows: 24}, nil)

	ctx := context.Background()
	if _, err := lifecycle.RestartAgent(ctx, "hot"); err != nil {
		t.Fatalf("RestartAgent: %v", err)
	}

	backend.mu.Lock()
	stops, starts := strings.Join(backend.stops, ","), len(backend.starts)
	backend.mu.Unlock()
	if !strings.Contains(stops, "hot") {
		t.Fatalf("restart stops = %#v, want the running agent stopped first", backend.stops)
	}
	if starts != 2 {
		t.Fatalf("restart starts = %d, want 2", starts)
	}
}
