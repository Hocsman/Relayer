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
	"time"

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
	// duringSnapshot, when set, runs inside every Snapshot: a lifecycle call
	// that moves while the backend is being asked.
	duringSnapshot func()
	// removeRunning makes the next Remove calls refuse with ErrSessionRunning,
	// the way a backend does between reaping a leader and settling its group.
	removeRunning int
	// exited lists the agents whose process the backend reports as gone.
	// Everything else is reported running, as a live backend would.
	exited map[string]bool
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

func (b *lifecycleFakeBackend) Snapshot(ctx context.Context, id string) (terminal.Snapshot, error) {
	snapshot, err := b.routerFakeBackend.Snapshot(ctx, id)
	b.mu.Lock()
	hook := b.duringSnapshot
	b.mu.Unlock()
	if hook != nil {
		hook()
	}
	b.mu.Lock()
	if b.exited[id] {
		snapshot.Running = false
	}
	b.mu.Unlock()
	return snapshot, err
}

// Start launches a fresh process, which runs until the test says otherwise.
func (b *lifecycleFakeBackend) Start(ctx context.Context, spec agent.Spec, size terminal.Size) (terminal.Info, error) {
	info, err := b.routerFakeBackend.Start(ctx, spec, size)
	if err == nil {
		b.setExited(spec.ID, false)
	}
	return info, err
}

func (b *lifecycleFakeBackend) setExited(id string, exited bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.exited == nil {
		b.exited = make(map[string]bool)
	}
	b.exited[id] = exited
}

func (b *lifecycleFakeBackend) Remove(_ context.Context, id string) error {
	b.mu.Lock()
	b.removes = append(b.removes, id)
	err, gone := b.removeErr, b.removeGone
	if b.removeRunning > 0 {
		b.removeRunning--
		err = terminal.ErrSessionRunning
	}
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

func TestAgentLifecycleStartAndRestartSelfExitedAgent(t *testing.T) {
	backend := newLifecycleFakeBackend()
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatalf("newBackendRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	spec := lifecycleTestSpec(t, "self-exit")
	if _, err := router.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	recorder := lifecycleTestRecorder(t)
	lifecycle := newAgentLifecycle(router, recorder, []agent.Spec{spec}, terminal.Size{Columns: 80, Rows: 24}, nil)

	ctx := context.Background()

	// 1. The process exits on its own and the exit is reported; StartAgent
	// then succeeds, and the replacement runs.
	backend.setExited("self-exit", true)
	if !lifecycle.MarkProcessExited("self-exit") {
		t.Fatal("the exit of the current process was reported stale")
	}
	if _, err := lifecycle.StartAgent(ctx, "self-exit", "operator_start"); err != nil {
		t.Fatalf("StartAgent after MarkProcessExited failed: %v", err)
	}
	backend.setExited("self-exit", false)

	// 2. Start on an agent the backend still reports running is refused, and
	// nothing is stopped or removed to find out. v0.8.5 called Remove here, and
	// the tmux backend's Remove kills a live session and reports success.
	backend.mu.Lock()
	removesBefore, stopsBefore := len(backend.removes), len(backend.stops)
	backend.mu.Unlock()
	if _, err := lifecycle.StartAgent(ctx, "self-exit", "operator_start"); !errors.Is(err, errAgentRunning) {
		t.Fatalf("StartAgent on a live agent error = %v, want %v", err, errAgentRunning)
	}
	backend.mu.Lock()
	removesAfter, stopsAfter := len(backend.removes), len(backend.stops)
	backend.mu.Unlock()
	if removesAfter != removesBefore || stopsAfter != stopsBefore {
		t.Fatalf("StartAgent on a live agent removed %d and stopped %d sessions, want none",
			removesAfter-removesBefore, stopsAfter-stopsBefore)
	}

	// 3. The process exits on its own and the exit never reaches
	// MarkProcessExited: the backend reports it gone, so Start proceeds.
	backend.setExited("self-exit", true)
	if _, err := lifecycle.StartAgent(ctx, "self-exit", "operator_start"); err != nil {
		t.Fatalf("StartAgent for self-exited agent failed: %v", err)
	}
	backend.setExited("self-exit", false)

	// 4. RestartAgent should also succeed cleanly
	if _, err := lifecycle.RestartAgent(ctx, "self-exit"); err != nil {
		t.Fatalf("RestartAgent for self-exited agent failed: %v", err)
	}
}

// TestAStaleExitLeavesTheReplacementRunning: the previous process's exit can
// be emitted after its replacement started, and names only the agent. The
// lifecycle asks the backend and ignores an exit while the current process runs;
// v0.8.5 marked the replacement stopped, which let a second Start run beside it.
func TestAStaleExitLeavesTheReplacementRunning(t *testing.T) {
	backend := newLifecycleFakeBackend()
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatalf("newBackendRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	spec := lifecycleTestSpec(t, "replaced")
	if _, err := router.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	lifecycle := newAgentLifecycle(router, lifecycleTestRecorder(t), []agent.Spec{spec}, terminal.Size{Columns: 80, Rows: 24}, nil)

	// The backend reports the current process running: this exit is stale.
	if lifecycle.MarkProcessExited("replaced") {
		t.Fatal("an exit was accepted while the backend reports the current process running")
	}
	if _, err := lifecycle.StartAgent(context.Background(), "replaced", "operator_start"); !errors.Is(err, errAgentRunning) {
		t.Fatalf("StartAgent after a stale exit = %v, want %v: a second process would run beside the first", err, errAgentRunning)
	}

	// Once the backend reports it gone, the exit is the current process's.
	backend.setExited("replaced", true)
	if !lifecycle.MarkProcessExited("replaced") {
		t.Fatal("the current process's exit was reported stale")
	}
}

// TestRestartOfADeadAgentDoesNotStopItAgain: an agent that exited without the
// exit reaching the lifecycle — the TUI never reported exits — used to be
// "stopped" by RestartAgent first, journaling a second session_finished.
func TestRestartOfADeadAgentDoesNotStopItAgain(t *testing.T) {
	backend := newLifecycleFakeBackend()
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatalf("newBackendRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	spec := lifecycleTestSpec(t, "dead")
	if _, err := router.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	recorder := lifecycleTestRecorder(t)
	lifecycle := newAgentLifecycle(router, recorder, []agent.Spec{spec}, terminal.Size{Columns: 80, Rows: 24}, nil)

	backend.setExited("dead", true)
	if _, err := lifecycle.RestartAgent(context.Background(), "dead"); err != nil {
		t.Fatalf("RestartAgent of a dead agent: %v", err)
	}
	backend.mu.Lock()
	stops := len(backend.stops)
	backend.mu.Unlock()
	if stops != 0 {
		t.Fatalf("RestartAgent stopped a process that had already exited %d time(s)", stops)
	}
	for _, entry := range lifecycleTestEntries(t, recorder) {
		if entry.Kind == audit.KindSessionFinished {
			t.Fatalf("RestartAgent journaled %s/%s for a process that had already exited", entry.Kind, entry.Reason)
		}
	}
}

// TestRestartAtTheReapInstantWaitsForTheSessionToSettle: a backend reports the
// process gone as soon as its leader is reaped, but refuses to release the
// session until the leader's group is cleaned up. A Restart in that window
// skipped the stop, found Remove refused, and latched stop_uncertain; every
// later Start of the agent was then refused.
func TestRestartAtTheReapInstantWaitsForTheSessionToSettle(t *testing.T) {
	backend := newLifecycleFakeBackend()
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatalf("newBackendRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	spec := lifecycleTestSpec(t, "settling")
	if _, err := router.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	lifecycle := newAgentLifecycle(router, lifecycleTestRecorder(t), []agent.Spec{spec}, terminal.Size{Columns: 80, Rows: 24}, nil)

	backend.setExited("settling", true)
	backend.mu.Lock()
	backend.removeRunning = 3
	backend.mu.Unlock()
	if _, err := lifecycle.RestartAgent(context.Background(), "settling"); err != nil {
		t.Fatalf("RestartAgent while the session settles: %v", err)
	}
	backend.mu.Lock()
	removes := len(backend.removes)
	backend.mu.Unlock()
	if removes < 4 {
		t.Fatalf("Remove was tried %d time(s), want it retried until the session settled", removes)
	}
}

// TestAnExitDuringARestartIsThePreviousProcesss: the previous process's exit
// is emitted when its session settles, which is what the Restart waits for, so
// it reached the front end while the replacement was starting. The lifecycle
// called it current and the desktop marked the live replacement exited.
func TestAnExitDuringARestartIsThePreviousProcesss(t *testing.T) {
	backend := newLifecycleFakeBackend()
	router, err := newBackendRouter(context.Background(), backend)
	if err != nil {
		t.Fatalf("newBackendRouter: %v", err)
	}
	t.Cleanup(func() { _ = router.Close(context.Background()) })
	spec := lifecycleTestSpec(t, "restarting")
	if _, err := router.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("initial start: %v", err)
	}
	lifecycle := newAgentLifecycle(router, lifecycleTestRecorder(t), []agent.Spec{spec}, terminal.Size{Columns: 80, Rows: 24}, nil)

	// The process has exited and its session is settling.
	backend.setExited("restarting", true)
	backend.mu.Lock()
	backend.removeRunning = 10
	backend.mu.Unlock()

	restarted := make(chan error, 1)
	go func() {
		_, err := lifecycle.RestartAgent(context.Background(), "restarting")
		restarted <- err
	}()
	deadline := time.Now().Add(2 * time.Second)
	for {
		lifecycle.mu.Lock()
		state := lifecycle.states[lifecycleKey("restarting")]
		lifecycle.mu.Unlock()
		if state == agentStateStarting {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the restart never reached starting (state %q)", state)
		}
		time.Sleep(time.Millisecond)
	}

	// The settled session's exit arrives now.
	if lifecycle.MarkProcessExited("restarting") {
		t.Fatal("the previous process's exit was called current, and the live replacement would be shown exited")
	}
	if err := <-restarted; err != nil {
		t.Fatalf("RestartAgent: %v", err)
	}
}

// TestAnExitIsStaleOnlyIfAStartBeganMeanwhile: the lifecycle can move while
// the backend is asked about an exit. An operator Stop finishing then is the
// same process, and treating the exit as stale left the web card running for
// an agent that had stopped; only a Start beginning then makes it stale.
func TestAnExitIsStaleOnlyIfAStartBeganMeanwhile(t *testing.T) {
	for _, move := range []struct {
		name      string
		from, to  string
		wantFresh bool
	}{
		{"an operator stop finishes", agentStateStopping, agentStateStopped, true},
		{"a start begins", agentStateStopped, agentStateStarting, false},
	} {
		t.Run(move.name, func(t *testing.T) {
			backend := newLifecycleFakeBackend()
			router, err := newBackendRouter(context.Background(), backend)
			if err != nil {
				t.Fatalf("newBackendRouter: %v", err)
			}
			t.Cleanup(func() { _ = router.Close(context.Background()) })
			spec := lifecycleTestSpec(t, "moving")
			if _, err := router.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24}); err != nil {
				t.Fatalf("initial start: %v", err)
			}
			lifecycle := newAgentLifecycle(router, lifecycleTestRecorder(t), []agent.Spec{spec}, terminal.Size{Columns: 80, Rows: 24}, nil)
			key := lifecycleKey("moving")
			lifecycle.mu.Lock()
			lifecycle.states[key] = move.from
			lifecycle.mu.Unlock()
			backend.setExited("moving", true)
			backend.mu.Lock()
			backend.duringSnapshot = func() {
				lifecycle.mu.Lock()
				lifecycle.states[key] = move.to
				lifecycle.mu.Unlock()
			}
			backend.mu.Unlock()

			if got := lifecycle.MarkProcessExited("moving"); got != move.wantFresh {
				t.Fatalf("MarkProcessExited = %v, want %v", got, move.wantFresh)
			}
		})
	}
}
