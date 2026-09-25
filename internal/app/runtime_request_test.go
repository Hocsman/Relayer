package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/Hocsman/Relayer/internal/tmuxbackend"
)

// request is what the terminal command will prepare its runtime from, over the
// harness's configuration, fake backends and journal.
func (h *tuiRunHarness) request(cli options) runtimeRequest {
	return runtimeRequest{
		configPath:   h.configPath,
		diagnostics:  io.Discard,
		cli:          cli,
		dependencies: h.dependencies(),
		initialSize:  terminal.Size{Columns: h.columns, Rows: h.rows},
	}
}

// startRuntime prepares a runtime from request and starts it. The runtime is
// closed when the test ends, before its directory is removed: on Windows an
// open journal keeps the directory from being removed.
func (h *tuiRunHarness) startRuntime(request runtimeRequest) *DesktopRuntime {
	h.t.Helper()
	plan, err := prepareRuntime(request)
	if err != nil {
		h.t.Fatalf("prepare the runtime: %v", err)
	}
	runID, err := newDesktopRunID()
	if err != nil {
		h.t.Fatalf("reserve a run ID: %v", err)
	}
	runtime, err := StartDesktopRuntime(context.Background(), plan, runID)
	if err != nil {
		h.t.Fatalf("start the runtime: %v", err)
	}
	h.t.Cleanup(func() { _ = closeRuntimeWithin(runtime, nil) })
	return runtime
}

// closeRuntimeWithin closes the runtime with a cause, within a bound a test can
// wait for.
func closeRuntimeWithin(runtime *DesktopRuntime, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	return runtime.closeWithCause(ctx, cause)
}

// enableRecording turns session recording on in the harness's configuration.
// The directory it names is under the test's own and does not exist yet: the
// store creates it, private, when it opens.
func (h *tuiRunHarness) enableRecording() string {
	h.t.Helper()
	directory := filepath.Join(filepath.Dir(h.configPath), "recordings")
	file, err := os.OpenFile(h.configPath, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		h.t.Fatalf("open the run's configuration: %v", err)
	}
	_, writeErr := fmt.Fprintf(file, "recording:\n  enabled: true\n  path: %s\n", strconv.Quote(directory))
	if err := errors.Join(writeErr, file.Close()); err != nil {
		h.t.Fatalf("enable recording in the run's configuration: %v", err)
	}
	return directory
}

// runtimeRecordingBackend streams transcripts as the real backends do: it
// takes the recorder the run hands it and opens a transcript for each session
// it starts. A run that records therefore leaves recording entries in its
// journal, and one that does not leaves none.
type runtimeRecordingBackend struct {
	*routerFakeBackend
	recorder  terminal.Recorder
	recorders int
}

func (b *runtimeRecordingBackend) SetRecorder(recorder terminal.Recorder) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.recorders++
	b.recorder = recorder
}

func (b *runtimeRecordingBackend) Start(ctx context.Context, spec agent.Spec, size terminal.Size) (terminal.Info, error) {
	info, err := b.routerFakeBackend.Start(ctx, spec, size)
	if err != nil {
		return info, err
	}
	b.mu.Lock()
	recorder := b.recorder
	b.mu.Unlock()
	if recorder != nil {
		recorder.StartSession(info, size, time.Now())
	}
	return info, nil
}

func (b *runtimeRecordingBackend) recordersGiven() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.recorders
}

// runtimeAttachBackend keeps the context each attach client is built on and
// each resync runs on, which is what decides how long either may live.
type runtimeAttachBackend struct {
	*routerFakeBackend
	attachContexts []context.Context
	resyncs        []runtimeResyncCall
}

type runtimeResyncCall struct {
	ctx           context.Context
	id            string
	columns, rows int
}

func (b *runtimeAttachBackend) AttachCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	b.mu.Lock()
	b.attachContexts = append(b.attachContexts, ctx)
	b.mu.Unlock()
	return b.routerFakeBackend.AttachCommand(ctx, id)
}

func (b *runtimeAttachBackend) Resync(ctx context.Context, id string, columns, rows int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resyncs = append(b.resyncs, runtimeResyncCall{ctx: ctx, id: id, columns: columns, rows: rows})
	return nil
}

func (b *runtimeAttachBackend) calls() ([]context.Context, []runtimeResyncCall) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]context.Context(nil), b.attachContexts...), append([]runtimeResyncCall(nil), b.resyncs...)
}

// startAttachRuntime starts one tmux agent, agent-1, on a runtimeAttachBackend.
func startAttachRuntime(t *testing.T) (*DesktopRuntime, *runtimeAttachBackend) {
	t.Helper()
	h := newTUIRunHarness(t, false, agent.BackendTmux)
	backend := &runtimeAttachBackend{routerFakeBackend: h.tmux}
	request := h.request(options{})
	request.dependencies.newTmux = func(context.Context, chan<- session.Event, *adapters.Registry, int, tmuxbackend.Options) (terminal.Backend, error) {
		return backend, nil
	}
	return h.startRuntime(request), backend
}

// The terminal command prepares its runtime from its own command line and from
// the dependencies it is given: --pane1 replaces the first agent's argv, the
// injected lookup and probe choose tmux, and the injected backends and journal
// are the ones the run starts on. The desktop prepares from the configuration
// alone, so it has no override to inherit.
func TestPreparedRuntimeHonoursPaneOverridesAndInjectedDependencies(t *testing.T) {
	h := newTUIRunHarness(t, false, agent.BackendPTY, agent.BackendTmux)
	cli, err := parseOptions([]string{"--config", h.configPath, "--pane1", `replacement --flag "two words"`}, io.Discard)
	if err != nil {
		t.Fatalf("parse the command line: %v", err)
	}
	var probed []string
	request := h.request(cli)
	request.dependencies.probeTmux = func(_ context.Context, path string) error {
		probed = append(probed, path)
		return nil
	}
	runtime := h.startRuntime(request)

	ptyStarts, tmuxStarts := h.starts(h.pty), h.starts(h.tmux)
	if len(ptyStarts) != 1 || len(tmuxStarts) != 1 {
		t.Fatalf("the fake backends started %d PTY and %d tmux agent(s), want one each", len(ptyStarts), len(tmuxStarts))
	}
	if got, want := ptyStarts[0].spec.Command, []string{"replacement", "--flag", "two words"}; ptyStarts[0].spec.ID != "agent-1" || !reflect.DeepEqual(got, want) {
		t.Fatalf("the PTY backend started %s with %q, want agent-1 with %q", ptyStarts[0].spec.ID, got, want)
	}
	if ptyStarts[0].spec.Shell != "" {
		t.Fatalf("agent 1 runs through a shell: %q", ptyStarts[0].spec.Shell)
	}
	if got, want := tmuxStarts[0].spec.Command, []string{"runner", "argument-2"}; tmuxStarts[0].spec.ID != "agent-2" || !reflect.DeepEqual(got, want) {
		t.Fatalf("the tmux backend started %s with %q, want agent-2 with its configured %q", tmuxStarts[0].spec.ID, got, want)
	}

	// On a host with tmux installed the real lookup would find it elsewhere, and
	// on one without it the preparation would fail.
	if !reflect.DeepEqual(probed, []string{"/fake/bin/tmux"}) {
		t.Fatalf("the injected probe saw %q, want the injected lookup's tmux", probed)
	}
	built := h.builtTmuxOptions()
	if len(built) != 1 {
		t.Fatalf("the tmux backend was built %d time(s), want once", len(built))
	}
	if built[0].TmuxPath != "/fake/bin/tmux" || built[0].RunID != runtime.runID {
		t.Fatalf("tmux built with path %q and run ID %q, want %q and the run's %q",
			built[0].TmuxPath, built[0].RunID, "/fake/bin/tmux", runtime.runID)
	}
	assertTUIJournal(t, "once the runtime started", h.entries(), cleanTUIRunStart())
	if got := h.entries()[0].RunID; got != runtime.runID {
		t.Fatalf("the journal's run_id is %q, want the run's %q", got, runtime.runID)
	}

	// The desktop and the gateway print the resolution warnings and the
	// recorder's failures on the writer they give, and the desktop gives the
	// size of its first viewport, so both must reach the plan.
	desktop := newTUIRunHarness(t, false, agent.BackendPTY, agent.BackendPTY)
	var diagnostics bytes.Buffer
	viewport := terminal.Size{Columns: 150, Rows: 50}
	plan, err := PrepareDesktopRuntime(DesktopOptions{
		ConfigPath:  desktop.configPath,
		InitialSize: viewport,
		Diagnostics: &diagnostics,
	})
	if err != nil {
		t.Fatalf("PrepareDesktopRuntime: %v", err)
	}
	if plan.diagnostics != &diagnostics {
		t.Fatalf("the desktop's plan writes its diagnostics to %T, want the writer it was given", plan.diagnostics)
	}
	if plan.initialSize != viewport {
		t.Fatalf("the desktop's plan starts its agents at %+v, want the given %+v", plan.initialSize, viewport)
	}
	if len(plan.resolution.Specs) != 2 {
		t.Fatalf("the desktop's plan has %d agent(s), want the two configured", len(plan.resolution.Specs))
	}
	for index, spec := range plan.resolution.Specs {
		if want := []string{"runner", fmt.Sprintf("argument-%d", index+1)}; !reflect.DeepEqual(spec.Command, want) || plan.resolution.Simulated[index] {
			t.Fatalf("the desktop's agent %d runs %q (simulated %t), want its configured %q",
				index+1, spec.Command, plan.resolution.Simulated[index], want)
		}
	}
	for _, warning := range plan.resolution.Warnings {
		if strings.Contains(warning, "--pane") {
			t.Fatalf("the desktop's plan warns about a pane override: %q", warning)
		}
	}
	if !plan.recording || plan.sizeFor != nil {
		t.Fatalf("the desktop's plan has recording %t and a size per agent %t, want recording and one size for all",
			plan.recording, plan.sizeFor != nil)
	}
	if plan.dependencies.newAuditForRun == nil || plan.dependencies.newPTY == nil || plan.dependencies.newTmux == nil {
		t.Fatal("the desktop's plan is not built on the production dependencies")
	}
}

// With a size per agent, each agent starts at its own; the lifecycle keeps the
// whole terminal for an agent it starts again later, as the terminal command
// does today. Without one, as on the desktop, every agent starts at the
// initial size.
func TestEachAgentStartsAtItsOwnSize(t *testing.T) {
	initial := terminal.Size{Columns: 160, Rows: 48}
	sizeFor := func(index, count int) terminal.Size {
		return terminal.Size{Columns: 40 + index, Rows: 10 * count}
	}

	h := newTUIRunHarness(t, false, agent.BackendPTY, agent.BackendPTY, agent.BackendTmux)
	request := h.request(options{})
	request.initialSize = initial
	request.sizeFor = sizeFor
	runtime := h.startRuntime(request)

	starts := append(h.starts(h.pty), h.starts(h.tmux)...)
	if len(starts) != 3 {
		t.Fatalf("starts = %#v, want three", starts)
	}
	for index, start := range starts {
		want := sizeFor(index, 3)
		if id := fmt.Sprintf("agent-%d", index+1); start.spec.ID != id || start.size != want {
			t.Fatalf("start %d = %s at %+v, want %s at %+v", index+1, start.spec.ID, start.size, id, want)
		}
	}
	if runtime.lifecycle.size != initial {
		t.Fatalf("the lifecycle starts agents again at %+v, want the whole terminal %+v", runtime.lifecycle.size, initial)
	}

	desktop := newTUIRunHarness(t, false, agent.BackendPTY, agent.BackendPTY)
	desktopRequest := desktop.request(options{})
	desktopRequest.initialSize = initial
	desktop.startRuntime(desktopRequest)
	desktopStarts := desktop.starts(desktop.pty)
	if len(desktopStarts) != 2 {
		t.Fatalf("without a size per agent, starts = %#v, want two", desktopStarts)
	}
	for index, start := range desktopStarts {
		if start.size != initial {
			t.Fatalf("without a size per agent, agent %d starts at %+v, want the initial %+v", index+1, start.size, initial)
		}
	}
}

// Recording is enabled in the configuration, but the terminal interface
// prepares its runtime without it: the store is never opened, so its directory
// is never created, no backend is handed a recorder, and the journal has no
// recording entry. The same configuration prepared with recording, as the
// desktop's is, records, which is what shows the fake would see a recorder.
func TestARuntimePreparedWithoutRecordingOpensNoStore(t *testing.T) {
	run := func(t *testing.T, recording bool) (directory string, recorders int, entries []audit.Entry) {
		t.Helper()
		h := newTUIRunHarness(t, false, agent.BackendPTY)
		directory = h.enableRecording()
		backend := &runtimeRecordingBackend{routerFakeBackend: h.pty}
		request := h.request(options{})
		request.recording = recording
		request.dependencies.newPTY = func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
			return backend, nil
		}
		runtime := h.startRuntime(request)
		// The transcripts close, and their entries are written, when the run
		// closes.
		if err := closeRuntimeWithin(runtime, nil); err != nil {
			t.Fatalf("close the runtime: %v", err)
		}
		return directory, backend.recordersGiven(), h.entries()
	}
	recordingEntries := func(entries []audit.Entry) []audit.Kind {
		var kinds []audit.Kind
		for _, entry := range entries {
			if strings.HasPrefix(string(entry.Kind), "recording_") {
				kinds = append(kinds, entry.Kind)
			}
		}
		return kinds
	}

	t.Run("prepared with recording", func(t *testing.T) {
		directory, recorders, entries := run(t, true)
		if _, err := os.Stat(directory); err != nil {
			t.Fatalf("the recording directory: %v", err)
		}
		if recorders != 1 {
			t.Fatalf("the backend was handed %d recorder(s), want one", recorders)
		}
		if got, want := recordingEntries(entries), []audit.Kind{audit.KindRecordingStarted, audit.KindRecordingFinished}; !reflect.DeepEqual(got, want) {
			t.Fatalf("recording entries = %q, want %q", got, want)
		}
	})

	t.Run("prepared without recording", func(t *testing.T) {
		directory, recorders, entries := run(t, false)
		if _, err := os.Stat(directory); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("the recording directory was created (stat: %v)", err)
		}
		if recorders != 0 {
			t.Fatalf("the backend was handed %d recorder(s), want none", recorders)
		}
		if got := recordingEntries(entries); len(got) != 0 {
			t.Fatalf("the journal has recording entries %q", got)
		}
	})
}

// A run whose interface failed ends failed in the journal even when its close
// goes well. The cause is not part of the close's own result, which its caller
// joins with the cause itself, and a second close journals nothing more. The
// run without a cause closes through Close, which is how the desktop and the
// gateway end every run, so it pins that Close passes no cause.
func TestCloseWithCauseJournalsRunFinishedFailed(t *testing.T) {
	clean := newTUIRunHarness(t, false, agent.BackendPTY, agent.BackendTmux)
	cleanRuntime := clean.startRuntime(clean.request(options{}))
	cleanCtx, cancelClean := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelClean()
	if err := cleanRuntime.Close(cleanCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	assertTUIJournal(t, "after a close without a cause", clean.entries(),
		append(cleanTUIRunStart(), cleanTUIRunEnd(audit.OutcomeSucceeded)...))

	h := newTUIRunHarness(t, false, agent.BackendPTY, agent.BackendTmux)
	runtime := h.startRuntime(h.request(options{}))
	if err := closeRuntimeWithin(runtime, errors.New("planned interface failure")); err != nil {
		t.Fatalf("closeWithCause = %v, want nil: the close itself went well", err)
	}
	want := append(cleanTUIRunStart(), cleanTUIRunEnd(audit.OutcomeFailed)...)
	assertTUIJournal(t, "after a close with a cause", h.entries(), want)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runtime.Close(ctx); err != nil {
		t.Fatalf("a second close = %v, want the first one's result", err)
	}
	assertTUIJournal(t, "after a second close", h.entries(), want)
}

// Once the run shuts down, neither an attach client nor a resync reaches the
// backend: a person must not attach to a session the run is closing. The
// shutdown ends the router's context with the runtime's, so the router refuses
// as well as the runtime's own guard. The guard alone refuses a runtime that
// was never started: it has no router, and the call would dereference nil.
func TestAttachCommandAndResyncAreRefusedOnceTheRuntimeShutsDown(t *testing.T) {
	runtime, backend := startAttachRuntime(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	unstarted := &DesktopRuntime{}
	if command, err := unstarted.attachCommand("agent-1"); !errors.Is(err, terminal.ErrUnavailable) || command != nil {
		t.Fatalf("attachCommand on a runtime without a router = %v, %v; want %v", command, err, terminal.ErrUnavailable)
	}
	if err := unstarted.resync(ctx, "agent-1", 80, 24); !errors.Is(err, terminal.ErrUnavailable) {
		t.Fatalf("resync on a runtime without a router = %v, want %v", err, terminal.ErrUnavailable)
	}

	if command, err := runtime.attachCommand("agent-1"); err != nil || command == nil {
		t.Fatalf("attachCommand while the run is up = %v, %v; want a client", command, err)
	}
	if err := runtime.resync(ctx, "agent-1", 80, 24); err != nil {
		t.Fatalf("resync while the run is up: %v", err)
	}

	if err := runtime.BeginShutdown(ctx); err != nil {
		t.Fatalf("BeginShutdown: %v", err)
	}
	if command, err := runtime.attachCommand("agent-1"); !errors.Is(err, terminal.ErrClosed) || command != nil {
		t.Fatalf("attachCommand after BeginShutdown = %v, %v; want %v", command, err, terminal.ErrClosed)
	}
	if err := runtime.resync(ctx, "agent-1", 80, 24); !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("resync after BeginShutdown = %v, want %v", err, terminal.ErrClosed)
	}

	if err := runtime.Close(ctx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if command, err := runtime.attachCommand("agent-1"); !errors.Is(err, terminal.ErrClosed) || command != nil {
		t.Fatalf("attachCommand after Close = %v, %v; want %v", command, err, terminal.ErrClosed)
	}
	if err := runtime.resync(ctx, "agent-1", 80, 24); !errors.Is(err, terminal.ErrClosed) {
		t.Fatalf("resync after Close = %v, want %v", err, terminal.ErrClosed)
	}

	attaches, resyncs := backend.calls()
	if len(attaches) != 1 || len(resyncs) != 1 {
		t.Fatalf("the backend built %d client(s) and ran %d resync(s), want only the one of each made while the run was up",
			len(attaches), len(resyncs))
	}
	if got, want := resyncs[0], (runtimeResyncCall{ctx: resyncs[0].ctx, id: "agent-1", columns: 80, rows: 24}); got != want {
		t.Fatalf("resync = %+v, want %+v", got, want)
	}
}

// The attach client runs for as long as the person stays attached, so it is
// built on the run's context, which a call cannot bound and only the run's
// shutdown ends. A resync is bounded by its caller.
func TestTheAttachClientLivesOnTheRunNotOnItsCaller(t *testing.T) {
	runtime, backend := startAttachRuntime(t)

	if _, err := runtime.attachCommand("agent-1"); err != nil {
		t.Fatalf("attachCommand: %v", err)
	}
	resyncCtx, cancelResync := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancelResync()
	if err := runtime.resync(resyncCtx, "agent-1", 100, 30); err != nil {
		t.Fatalf("resync: %v", err)
	}

	attaches, resyncs := backend.calls()
	if len(attaches) != 1 || len(resyncs) != 1 {
		t.Fatalf("the backend built %d client(s) and ran %d resync(s), want one of each", len(attaches), len(resyncs))
	}
	client := attaches[0]
	if err := client.Err(); err != nil {
		t.Fatalf("the client's context ended with the call that built it: %v", err)
	}
	if deadline, bounded := client.Deadline(); bounded {
		t.Fatalf("the client's context ends at %v, want it to end only with the run", deadline)
	}
	want, _ := resyncCtx.Deadline()
	if got, bounded := resyncs[0].ctx.Deadline(); !bounded || !got.Equal(want) {
		t.Fatalf("the resync runs until %v (bounded %t), want its caller's %v", got, bounded, want)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := runtime.BeginShutdown(ctx); err != nil {
		t.Fatalf("BeginShutdown: %v", err)
	}
	select {
	case <-client.Done():
	default:
		t.Fatal("the client's context outlived the run's shutdown")
	}
}
