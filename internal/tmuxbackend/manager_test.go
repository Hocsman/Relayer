//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tmuxbackend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/intercept"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

type fakeRunner struct {
	mu sync.Mutex

	lookPath    string
	lookPathErr error
	calls       []CommandSpec
	commands    []CommandSpec
	fail        map[string]error
	display     string
	capture     string
	killDelay   time.Duration
	nextID      int
	identities  map[string]*fakeIdentity
	newOutput   *string
}

type fakeIdentity struct {
	name      string
	sessionID string
	windowID  string
	paneID    string
	owner     string
}

func newFakeRunner() *fakeRunner {
	return &fakeRunner{
		lookPath:   "/test/bin/tmux",
		fail:       make(map[string]error),
		display:    "0\t\t0\t1\n",
		identities: make(map[string]*fakeIdentity),
	}
}

func (r *fakeRunner) LookPath(string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.lookPath, r.lookPathErr
}

func (r *fakeRunner) Run(ctx context.Context, spec CommandSpec) ([]byte, error) {
	copy := cloneCommandSpec(spec)
	r.mu.Lock()
	r.calls = append(r.calls, copy)
	operation := commandOperation(spec.Args)
	failure := r.fail[operation]
	delay := r.killDelay
	var output []byte
	if failure == nil {
		switch operation {
		case "new-session":
			r.nextID++
			identity := &fakeIdentity{
				name:      optionValue(spec.Args, "-s"),
				sessionID: fmt.Sprintf("$%d", r.nextID),
				windowID:  fmt.Sprintf("@%d", r.nextID),
				paneID:    fmt.Sprintf("%%%d", r.nextID),
			}
			r.identities[identity.name] = identity
			r.identities[identity.sessionID] = identity
			r.identities[identity.windowID] = identity
			r.identities[identity.paneID] = identity
			output = []byte(identity.sessionID + "\t" + identity.windowID + "\t" + identity.paneID + "\n")
			if r.newOutput != nil {
				output = []byte(*r.newOutput)
			}
		case "set-option":
			if index := argumentIndex(spec.Args, "@relayer_owner"); index >= 0 && index+1 < len(spec.Args) {
				if identity := r.identities[optionValue(spec.Args, "-t")]; identity != nil {
					identity.owner = spec.Args[index+1]
				}
			}
		case "pipe-pane":
			fields := strings.Split(strings.TrimSpace(r.display), "\t")
			if len(fields) == 4 {
				// A shell command enables capture; tmux's no-command form
				// disables an existing pane pipe.
				fields[3] = "0"
				if len(spec.Args) > 3 {
					fields[3] = "1"
				}
				r.display = strings.Join(fields, "\t") + "\n"
			}
		case "display-message":
			identity := r.identities[optionValue(spec.Args, "-t")]
			format := spec.Args[len(spec.Args)-1]
			if identity != nil {
				switch {
				case strings.Contains(format, "pane_dead"):
					output = []byte(identity.sessionID + "\t" + identity.paneID + "\t" + identity.owner + "\t" + r.display)
				case strings.Contains(format, "window_id"):
					output = []byte(identity.sessionID + "\t" + identity.windowID + "\t" + identity.paneID + "\t" + identity.owner + "\n")
				default:
					output = []byte(identity.sessionID + "\t" + identity.owner + "\n")
				}
			}
		case "capture-pane":
			output = []byte(r.capture)
		}
	}
	r.mu.Unlock()
	if operation == "kill-session" && delay > 0 {
		timer := time.NewTimer(delay)
		defer timer.Stop()
		select {
		case <-timer.C:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if failure != nil {
		return nil, failure
	}
	return output, nil
}

func (r *fakeRunner) Command(ctx context.Context, spec CommandSpec) *exec.Cmd {
	r.mu.Lock()
	r.commands = append(r.commands, cloneCommandSpec(spec))
	r.mu.Unlock()
	return exec.CommandContext(ctx, "/usr/bin/true")
}

func (r *fakeRunner) setDisplay(value string) {
	r.mu.Lock()
	r.display = value
	r.mu.Unlock()
}

func (r *fakeRunner) setCapture(value string) {
	r.mu.Lock()
	r.capture = value
	r.mu.Unlock()
}

func (r *fakeRunner) setFailure(operation string, err error) {
	r.mu.Lock()
	r.fail[operation] = err
	r.mu.Unlock()
}

func (r *fakeRunner) setNewSessionOutput(value string) {
	r.mu.Lock()
	r.newOutput = &value
	r.mu.Unlock()
}

func (r *fakeRunner) callsFor(operation string) []CommandSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	var result []CommandSpec
	for _, call := range r.calls {
		if commandOperation(call.Args) == operation {
			result = append(result, cloneCommandSpec(call))
		}
	}
	return result
}

func (r *fakeRunner) allCalls() []CommandSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]CommandSpec, len(r.calls))
	for index, call := range r.calls {
		result[index] = cloneCommandSpec(call)
	}
	return result
}

func (r *fakeRunner) allCommands() []CommandSpec {
	r.mu.Lock()
	defer r.mu.Unlock()
	result := make([]CommandSpec, len(r.commands))
	for index, command := range r.commands {
		result[index] = cloneCommandSpec(command)
	}
	return result
}

func (r *fakeRunner) sessionID(name string) string {
	r.mu.Lock()
	defer r.mu.Unlock()
	if identity := r.identities[name]; identity != nil {
		return identity.sessionID
	}
	return ""
}

func argumentIndex(arguments []string, target string) int {
	for index, argument := range arguments {
		if argument == target {
			return index
		}
	}
	return -1
}

func cloneCommandSpec(spec CommandSpec) CommandSpec {
	return CommandSpec{
		Path:  spec.Path,
		Args:  append([]string(nil), spec.Args...),
		Stdin: append([]byte(nil), spec.Stdin...),
	}
}

func TestResolveBinaryReturnsRecognizableMissingErrors(t *testing.T) {
	runner := newFakeRunner()
	runner.lookPathErr = exec.ErrNotFound
	_, err := ResolveBinary(runner, "tmux-custom")
	if !errors.Is(err, ErrTmuxNotFound) || !errors.Is(err, terminal.ErrUnavailable) {
		t.Fatalf("ResolveBinary error = %v, want tmux and terminal sentinels", err)
	}
	if strings.Contains(err.Error(), "exec:") {
		t.Fatalf("ResolveBinary leaked runner internals: %v", err)
	}
}

func TestManagerStartBuildsOwnedArgvSafeTmuxCommands(t *testing.T) {
	runner := newFakeRunner()
	manager, events := newTestManager(t, runner, Options{RunID: "run-safe"})
	spec := testSpec(t, "agent;$(bad)`'\n")
	spec.Command = []string{"program", "space value", "semi;colon", "$TOKEN", "`code`", "", "line\nbreak"}
	spec.Env = map[string]string{"PRIVATE_TOKEN": "do-not-put-in-argv"}
	info, err := manager.Start(context.Background(), spec, terminal.Size{Columns: 91, Rows: 27})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Backend != agent.BackendTmux || info.ID != strings.TrimSpace(spec.ID) {
		t.Fatalf("Start info = %#v", info)
	}

	starts := runner.callsFor("new-session")
	if len(starts) != 1 {
		t.Fatalf("new-session calls = %#v", starts)
	}
	tmuxName := SessionName("run-safe", info.ID)
	if got := optionValue(starts[0].Args, "-s"); got != tmuxName {
		t.Fatalf("new-session target = %q, want %q", got, tmuxName)
	}
	if got := optionValue(starts[0].Args, "-x"); got != "91" {
		t.Fatalf("new-session columns = %q", got)
	}
	if got := optionValue(starts[0].Args, "-y"); got != "27" {
		t.Fatalf("new-session rows = %q", got)
	}
	joined := strings.Join(starts[0].Args, "\x00")
	for _, private := range append(append([]string(nil), spec.Command...), "do-not-put-in-argv") {
		if private != "" && strings.Contains(joined, private) {
			t.Fatalf("user command/env %q leaked into tmux argv: %#v", private, starts[0].Args)
		}
	}
	if !strings.Contains(starts[0].Args[len(starts[0].Args)-1], HelperSubcommand) {
		t.Fatalf("new-session does not invoke private helper: %#v", starts[0].Args)
	}

	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	drainEvents(events)
}

func TestManagerCloseHonorsPersistenceOwnershipAndIdempotence(t *testing.T) {
	for _, persist := range []bool{false, true} {
		t.Run(fmt.Sprintf("persist=%t", persist), func(t *testing.T) {
			runner := newFakeRunner()
			manager, _ := newTestManager(t, runner, Options{RunID: "close-run", PersistOnExit: persist})
			for _, id := range []string{"agent-a", "agent-b"} {
				if _, err := manager.Start(context.Background(), testSpec(t, id), terminal.Size{Columns: 40, Rows: 12}); err != nil {
					t.Fatalf("Start %s: %v", id, err)
				}
			}
			if err := manager.Close(context.Background()); err != nil {
				t.Fatalf("first Close: %v", err)
			}
			if err := manager.Close(context.Background()); err != nil {
				t.Fatalf("second Close: %v", err)
			}
			kills := runner.callsFor("kill-session")
			if persist && len(kills) != 0 {
				t.Fatalf("persistent Close killed sessions: %#v", kills)
			}
			if persist {
				pipes := runner.callsFor("pipe-pane")
				if len(pipes) != 4 {
					t.Fatalf("persistent pipe calls = %#v, want two enables and two disables", pipes)
				}
				for _, call := range pipes[2:] {
					if len(call.Args) != 3 || call.Args[1] != "-t" || !strings.HasPrefix(call.Args[2], "%") {
						t.Fatalf("persistent Close did not use exact pane disable form: %#v", call.Args)
					}
				}
			}
			if !persist {
				want := map[string]bool{
					runner.sessionID(SessionName("close-run", "agent-a")): true,
					runner.sessionID(SessionName("close-run", "agent-b")): true,
				}
				if len(kills) != len(want) {
					t.Fatalf("kill calls = %#v, want two owned targets", kills)
				}
				for _, call := range kills {
					target := optionValue(call.Args, "-t")
					if !want[target] {
						t.Fatalf("Close targeted non-owned session %q", target)
					}
					delete(want, target)
				}
			}
		})
	}
}

func TestManagerRejectsTmuxOperationsAfterShutdownWithoutNewCommands(t *testing.T) {
	for _, test := range []struct {
		name     string
		shutdown func(*Manager) error
	}{
		{
			name: "begin shutdown",
			shutdown: func(manager *Manager) error {
				manager.BeginShutdown()
				return nil
			},
		},
		{
			name: "close persistent backend",
			shutdown: func(manager *Manager) error {
				return manager.Close(context.Background())
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := newFakeRunner()
			manager, _ := newTestManager(t, runner, Options{RunID: "closed-guard", PersistOnExit: true})
			info, err := manager.Start(context.Background(), testSpec(t, "agent"), terminal.Size{})
			if err != nil {
				t.Fatal(err)
			}
			if err := test.shutdown(manager); err != nil {
				t.Fatalf("shutdown: %v", err)
			}
			callsBefore := len(runner.allCalls())

			operations := map[string]func() error{
				"send": func() error {
					return manager.Send(context.Background(), info.ID, []byte("must-not-be-sent"))
				},
				"resize": func() error {
					return manager.Resize(context.Background(), info.ID, terminal.Size{})
				},
				"snapshot": func() error {
					_, snapshotErr := manager.Snapshot(context.Background(), info.ID)
					return snapshotErr
				},
				"attach": func() error {
					_, attachErr := manager.AttachCommand(context.Background(), info.ID)
					return attachErr
				},
				"resync": func() error {
					return manager.Resync(context.Background(), info.ID, 80, 24)
				},
				"stop": func() error {
					return manager.Stop(context.Background(), info.ID)
				},
			}
			for name, operation := range operations {
				if err := operation(); !errors.Is(err, ErrClosed) {
					t.Fatalf("%s error = %v, want ErrClosed", name, err)
				}
			}
			if got := len(runner.allCalls()); got != callsBefore {
				t.Fatalf("post-shutdown operations issued %d tmux commands", got-callsBefore)
			}

			// Bounded local state remains readable for the final TUI refresh.
			if _, err := manager.Output(info.ID); err != nil {
				t.Fatalf("cached Output after shutdown: %v", err)
			}
			if _, err := manager.Done(info.ID); err != nil {
				t.Fatalf("Done after shutdown: %v", err)
			}
			_ = manager.Close(context.Background())
		})
	}
}

func TestManagerStopKillsOwnedSessionEvenWhenPersistent(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "persist-stop", PersistOnExit: true})
	if _, err := manager.Start(context.Background(), testSpec(t, "stop-me"), terminal.Size{}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), "stop-me"); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	kills := runner.callsFor("kill-session")
	if len(kills) != 1 || optionValue(kills[0].Args, "-t") != runner.sessionID(SessionName("persist-stop", "stop-me")) {
		t.Fatalf("Stop kill calls = %#v", kills)
	}
}

func TestManagerCleanupOnSuccessOnlyAndExactlyOnce(t *testing.T) {
	for _, test := range []struct {
		name     string
		status   string
		cleanup  bool
		wantKill bool
	}{
		{name: "success cleanup", status: "1\t0\t0\t0\n", cleanup: true, wantKill: true},
		{name: "success retained", status: "1\t0\t0\t0\n", cleanup: false},
		{name: "failure retained", status: "1\t7\t0\t0\n", cleanup: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			runner := newFakeRunner()
			manager, events := newTestManager(t, runner, Options{
				RunID:            "cleanup-run",
				PersistOnExit:    true,
				CleanupOnSuccess: test.cleanup,
				PollInterval:     minimumPollInterval,
			})
			if _, err := manager.Start(context.Background(), testSpec(t, "agent"), terminal.Size{}); err != nil {
				t.Fatal(err)
			}
			runner.setDisplay(test.status)
			waitForExitEvent(t, events, "agent")
			deadline := time.Now().Add(time.Second)
			for test.wantKill && len(runner.callsFor("kill-session")) == 0 && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}
			if err := manager.Close(context.Background()); err != nil {
				t.Fatalf("Close: %v", err)
			}
			kills := runner.callsFor("kill-session")
			if test.wantKill && len(kills) != 1 {
				t.Fatalf("successful cleanup kills = %#v, want exactly one", kills)
			}
			if !test.wantKill && len(kills) != 0 {
				t.Fatalf("retained result unexpectedly killed: %#v", kills)
			}
		})
	}
}

func TestManagerSendKeepsSensitiveInputOutOfArgsAndErrors(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "input-run", PersistOnExit: true})
	if _, err := manager.Start(context.Background(), testSpec(t, "input"), terminal.Size{}); err != nil {
		t.Fatal(err)
	}
	secret := "p@ss word;'$`\nsecond-line"
	if err := manager.Send(context.Background(), "input", []byte(secret)); err != nil {
		t.Fatalf("Send: %v", err)
	}
	loads := runner.callsFor("load-buffer")
	if len(loads) != 1 || string(loads[0].Stdin) != secret {
		t.Fatalf("load-buffer transport = %#v", loads)
	}
	for _, call := range runner.allCalls() {
		if strings.Contains(strings.Join(call.Args, "\x00"), secret) {
			t.Fatalf("secret leaked into command argv: %#v", call.Args)
		}
	}

	runner.setFailure("load-buffer", errors.New("load failed"))
	err := manager.Send(context.Background(), "input", []byte(secret))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("safe load error = %v", err)
	}
	_ = manager.Close(context.Background())
}

func TestManagerRetriesPrivateBufferDeletionOnClose(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "buffer-retry", PersistOnExit: true})
	if _, err := manager.Start(context.Background(), testSpec(t, "input"), terminal.Size{}); err != nil {
		t.Fatal(err)
	}
	secret := "answer-that-must-not-remain-in-tmux"
	runner.setFailure("paste-buffer", errors.New("paste failed"))
	runner.setFailure("delete-buffer", errors.New("delete failed"))
	err := manager.Send(context.Background(), "input", []byte(secret))
	if err == nil || !strings.Contains(err.Error(), "suppression du buffer") || strings.Contains(err.Error(), secret) {
		t.Fatalf("Send cleanup error = %v", err)
	}
	if got := len(manager.secretBuffers()); got != 1 {
		t.Fatalf("tracked private buffers = %d, want one", got)
	}

	runner.setFailure("paste-buffer", nil)
	runner.setFailure("delete-buffer", nil)
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close retry: %v", err)
	}
	if got := len(manager.secretBuffers()); got != 0 {
		t.Fatalf("Close retained %d private buffers", got)
	}
	deletes := runner.callsFor("delete-buffer")
	if len(deletes) != 2 {
		t.Fatalf("delete-buffer calls = %#v, want immediate cleanup and Close retry", deletes)
	}
	for _, call := range runner.allCalls() {
		if strings.Contains(strings.Join(call.Args, "\x00"), secret) {
			t.Fatalf("secret leaked into argv: %#v", call.Args)
		}
	}
}

func TestManagerCleansPossiblyCreatedBufferAfterLoadError(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "uncertain-load", PersistOnExit: true})
	if _, err := manager.Start(context.Background(), testSpec(t, "input"), terminal.Size{}); err != nil {
		t.Fatal(err)
	}

	secret := "secret-accepted-before-client-error"
	loadErr := errors.New("load acknowledgement lost")
	runner.setFailure("load-buffer", loadErr)
	if err := manager.Send(context.Background(), "input", []byte(secret)); !errors.Is(err, loadErr) {
		t.Fatalf("Send error = %v, want load error", err)
	}
	if got := len(runner.callsFor("delete-buffer")); got != 1 {
		t.Fatalf("delete-buffer calls after uncertain load = %d, want one", got)
	}
	if got := len(manager.secretBuffers()); got != 0 {
		t.Fatalf("successful uncertain-load cleanup retained %d tracked buffer(s)", got)
	}
	if got := len(runner.callsFor("paste-buffer")); got != 0 {
		t.Fatalf("paste-buffer ran after load error: %d call(s)", got)
	}
	for _, call := range runner.allCalls() {
		if strings.Contains(strings.Join(call.Args, "\x00"), secret) {
			t.Fatalf("secret leaked into command argv: %#v", call.Args)
		}
	}
	runner.setFailure("load-buffer", nil)
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestManagerRetriesPossiblyCreatedBufferCleanupAfterLoadAndDeleteErrors(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "uncertain-load-retry", PersistOnExit: true})
	if _, err := manager.Start(context.Background(), testSpec(t, "input"), terminal.Size{}); err != nil {
		t.Fatal(err)
	}

	loadErr := errors.New("load acknowledgement lost")
	deleteErr := errors.New("delete temporarily unavailable")
	runner.setFailure("load-buffer", loadErr)
	runner.setFailure("delete-buffer", deleteErr)
	err := manager.Send(context.Background(), "input", []byte("must-not-remain"))
	if !errors.Is(err, loadErr) || !errors.Is(err, deleteErr) {
		t.Fatalf("Send error = %v, want joined load and cleanup errors", err)
	}
	if got := len(manager.secretBuffers()); got != 1 {
		t.Fatalf("failed uncertain-load cleanup retained %d buffers, want one tracked", got)
	}

	runner.setFailure("load-buffer", nil)
	runner.setFailure("delete-buffer", nil)
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close cleanup retry: %v", err)
	}
	if got := len(manager.secretBuffers()); got != 0 {
		t.Fatalf("Close retained %d tracked buffer(s)", got)
	}
	if got := len(runner.callsFor("delete-buffer")); got != 2 {
		t.Fatalf("delete-buffer calls = %d, want immediate attempt and Close retry", got)
	}
}

func TestManagerMonitorStopsAfterPersistentOwnershipLossWithoutKilling(t *testing.T) {
	runner := newFakeRunner()
	manager, events := newTestManager(t, runner, Options{
		RunID:         "monitor-owner-loss",
		PersistOnExit: true,
		PollInterval:  minimumPollInterval,
	})
	info, err := manager.Start(context.Background(), testSpec(t, "agent"), terminal.Size{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.session(info.ID)
	if err != nil {
		t.Fatal(err)
	}

	runner.mu.Lock()
	identity := runner.identities[target.sessionID]
	// A hostile marker containing a field separator makes the inspection shape
	// malformed as well as mismatched; it must still terminalize supervision.
	identity.owner = "foreign\towner"
	runner.mu.Unlock()

	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	var (
		ownershipErrors int
		exitedEvents    int
		processExits    int
	)
	for exitedEvents == 0 {
		select {
		case emitted := <-events:
			switch event := emitted.(type) {
			case session.Error:
				if event.SessionID == info.ID && errors.Is(event.Err, errOwnershipInvalid) {
					ownershipErrors++
				}
			case session.Exited:
				if event.SessionID == info.ID {
					exitedEvents++
					if !errors.Is(event.Err, errOwnershipInvalid) {
						t.Fatalf("ownership loss Exited error = %v", event.Err)
					}
				}
			case session.AdapterEvent:
				if event.Event.SessionID == info.ID && event.Event.Type == adapters.EventProcessExit {
					processExits++
				}
			}
		case <-deadline.C:
			t.Fatal("monitor did not terminalize persistent ownership loss")
		}
	}
	if ownershipErrors != 1 {
		t.Fatal("monitor did not emit the typed ownership error before Exited")
	}
	select {
	case <-target.done:
	default:
		t.Fatal("ownership loss left session Done open")
	}
	if target.isPresent() {
		t.Fatal("ownership loss left foreign target marked present")
	}
	if kills := runner.callsFor("kill-session"); len(kills) != 0 {
		t.Fatalf("ownership loss killed foreign session: %#v", kills)
	}
	if probes := runner.callsFor("has-session"); len(probes) != 0 {
		t.Fatalf("typed ownership loss used an ambiguous existence probe: %#v", probes)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close after ownership loss: %v", err)
	}
	for {
		select {
		case emitted := <-events:
			switch event := emitted.(type) {
			case session.Error:
				if event.SessionID == info.ID && errors.Is(event.Err, errOwnershipInvalid) {
					ownershipErrors++
				}
			case session.Exited:
				if event.SessionID == info.ID {
					exitedEvents++
				}
			case session.AdapterEvent:
				if event.Event.SessionID == info.ID && event.Event.Type == adapters.EventProcessExit {
					processExits++
				}
			}
		default:
			if ownershipErrors != 1 {
				t.Fatalf("ownership loss errors = %d, want exactly one", ownershipErrors)
			}
			if exitedEvents != 1 {
				t.Fatalf("ownership loss Exited events = %d, want exactly one", exitedEvents)
			}
			if processExits != 0 {
				t.Fatalf("ownership loss emitted %d false process_exit event(s)", processExits)
			}
			return
		}
	}
}

func TestManagerCloseRetriesOwnedCleanupAfterFailure(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "close-retry"})
	if _, err := manager.Start(context.Background(), testSpec(t, "agent"), terminal.Size{}); err != nil {
		t.Fatal(err)
	}
	runner.setFailure("kill-session", errors.New("temporary kill failure"))
	if err := manager.Close(context.Background()); err == nil {
		t.Fatal("first Close unexpectedly hid the kill failure")
	}
	if got := len(runner.callsFor("kill-session")); got != 1 {
		t.Fatalf("first Close kill attempts = %d, want one", got)
	}

	runner.setFailure("kill-session", nil)
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("second Close retry: %v", err)
	}
	if got := len(runner.callsFor("kill-session")); got != 2 {
		t.Fatalf("second Close kill attempts = %d, want one retry", got)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("idempotent Close after cleanup: %v", err)
	}
	if got := len(runner.callsFor("kill-session")); got != 2 {
		t.Fatalf("successful idempotent Close issued another kill: %d", got)
	}
}

func TestManagerCloseCancelsAndWaitsForRegisteredOperations(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "operation-gate", PersistOnExit: true})
	operationCtx, finishOperation, err := manager.beginOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closeDone := make(chan error, 1)
	go func() { closeDone <- manager.Close(context.Background()) }()
	select {
	case <-operationCtx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the registered operation")
	}
	select {
	case err := <-closeDone:
		t.Fatalf("Close returned before the operation deregistered: %v", err)
	default:
	}
	finishOperation()
	select {
	case err := <-closeDone:
		if err != nil {
			t.Fatalf("Close: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Close did not resume after the operation deregistered")
	}
	if _, _, err := manager.beginOperation(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("operation admitted after Close: %v", err)
	}
}

func TestManagerCloseTimeoutCanBeRetried(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "close-timeout", PersistOnExit: true})
	_, finishOperation, err := manager.beginOperation(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 20*time.Millisecond)
	err = manager.Close(closeCtx)
	cancelClose()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timed Close error = %v", err)
	}
	if _, statErr := os.Stat(manager.RuntimeDirectory()); statErr != nil {
		t.Fatalf("timed Close removed runtime before operations stopped: %v", statErr)
	}
	finishOperation()
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close retry: %v", err)
	}
	if _, statErr := os.Stat(manager.RuntimeDirectory()); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("successful Close retained runtime: %v", statErr)
	}
}

func TestManagerStartRollbackRequiresMarkerBeforeKillAndRemovesArtifacts(t *testing.T) {
	for _, test := range []struct {
		operation string
		wantKill  bool
	}{
		{operation: "set-option", wantKill: false},
		{operation: "pipe-pane", wantKill: true},
	} {
		t.Run(test.operation, func(t *testing.T) {
			runner := newFakeRunner()
			runner.setFailure(test.operation, errors.New("configured failure"))
			manager, _ := newTestManager(t, runner, Options{RunID: "rollback"})
			_, err := manager.Start(context.Background(), testSpec(t, "unsafe;id"), terminal.Size{})
			if err == nil {
				t.Fatal("Start unexpectedly succeeded")
			}
			kills := runner.callsFor("kill-session")
			wantTarget := runner.sessionID(SessionName("rollback", "unsafe;id"))
			if test.wantKill && (len(kills) != 1 || optionValue(kills[0].Args, "-t") != wantTarget) {
				t.Fatalf("rollback kills = %#v, want %q", kills, wantTarget)
			}
			if !test.wantKill && len(kills) != 0 {
				t.Fatalf("unmarked rollback issued kill-session: %#v", kills)
			}
			entries, readErr := os.ReadDir(manager.RuntimeDirectory())
			if readErr != nil {
				t.Fatalf("read runtime: %v", readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("rollback retained artifacts: %#v", entries)
			}
			_ = manager.Close(context.Background())
		})
	}
}

func TestManagerTargetsNeverContainHostileAgentID(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "hostile", PersistOnExit: true})
	id := "other-session; kill-server; $(bad)\n'"
	if _, err := manager.Start(context.Background(), testSpec(t, id), terminal.Size{}); err != nil {
		t.Fatal(err)
	}
	if _, err := manager.AttachCommand(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := manager.Resize(context.Background(), id, terminal.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatal(err)
	}
	if err := manager.Stop(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	wantSession := runner.sessionID(SessionName("hostile", id))
	for _, call := range runner.allCalls() {
		for index, argument := range call.Args {
			if argument != "-t" || index+1 >= len(call.Args) {
				continue
			}
			target := call.Args[index+1]
			if len(target) < 2 || !strings.ContainsRune("$@%", rune(target[0])) || !validTmuxID(target, target[0]) || strings.Contains(target, id) {
				t.Fatalf("unsafe/non-owned -t target %q in %#v", target, call.Args)
			}
		}
	}
	for _, command := range runner.allCommands() {
		if got := optionValue(command.Args, "-t"); got != wantSession {
			t.Fatalf("attach target = %q, want %q", got, wantSession)
		}
	}
	_ = manager.Close(context.Background())
}

func TestManagerCloseKillsInParallelWithinGlobalBudget(t *testing.T) {
	runner := newFakeRunner()
	runner.killDelay = 150 * time.Millisecond
	manager, _ := newTestManager(t, runner, Options{RunID: "parallel-close"})
	for index := 0; index < 6; index++ {
		id := fmt.Sprintf("agent-%d", index)
		if _, err := manager.Start(context.Background(), testSpec(t, id), terminal.Size{}); err != nil {
			t.Fatal(err)
		}
	}
	started := time.Now()
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 500*time.Millisecond {
		t.Fatalf("parallel Close took %v; sequential kills likely", elapsed)
	}
	if got := len(runner.callsFor("kill-session")); got != 6 {
		t.Fatalf("kill count = %d, want 6", got)
	}
}

func TestParseSnapshotStates(t *testing.T) {
	for _, test := range []struct {
		text     string
		status   terminal.Status
		running  bool
		attached bool
		exitCode *int
	}{
		{text: "0\t\t0\n", status: terminal.StatusDetached, running: true},
		{text: "0\t\t2\n", status: terminal.StatusAttached, running: true, attached: true},
		{text: "1\t0\t0\n", status: terminal.StatusExited, exitCode: intPointer(0)},
		{text: "1\t42\t0\n", status: terminal.StatusFailed, exitCode: intPointer(42)},
	} {
		snapshot, err := parseSnapshot("id", test.text)
		if err != nil {
			t.Fatalf("parse %q: %v", test.text, err)
		}
		if snapshot.Status != test.status || snapshot.Running != test.running || snapshot.Attached != test.attached || !reflect.DeepEqual(snapshot.ExitCode, test.exitCode) {
			t.Fatalf("parse %q = %#v", test.text, snapshot)
		}
	}
	for _, invalid := range []string{"", "0\t0", "x\t0\t0", "1\tx\t0", "0\t\t-1"} {
		if _, err := parseSnapshot("id", invalid); err == nil {
			t.Fatalf("invalid snapshot %q accepted", invalid)
		}
	}
}

func TestManagerRestoresDeadPipeAndRevalidatesIt(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "pipe-restore", PersistOnExit: true})
	if _, err := manager.Start(context.Background(), testSpec(t, "pipe-agent"), terminal.Size{}); err != nil {
		t.Fatalf("Start: %v", err)
	}
	if got := len(runner.callsFor("pipe-pane")); got != 1 {
		t.Fatalf("initial pipe-pane calls = %d, want 1", got)
	}
	runner.setDisplay("0\t\t0\t0\n")
	snapshot, err := manager.Snapshot(context.Background(), "pipe-agent")
	if err != nil {
		t.Fatalf("Snapshot did not restore pipe: %v", err)
	}
	if !snapshot.Running {
		t.Fatalf("restored snapshot = %#v", snapshot)
	}
	if got := len(runner.callsFor("pipe-pane")); got != 2 {
		t.Fatalf("pipe-pane calls after loss = %d, want one reinstall", got)
	}
	_ = manager.Close(context.Background())
}

func TestParseIdentityRequiresImmutableTmuxIDs(t *testing.T) {
	want := tmuxIdentity{sessionID: "$12", windowID: "@34", paneID: "%56"}
	got, err := parseIdentity("$12\t@34\t%56\n")
	if err != nil || got != want {
		t.Fatalf("parseIdentity = %#v, %v; want %#v", got, err, want)
	}
	for _, invalid := range []string{
		"", "name\t@1\t%1", "$1\twindow\t%1", "$1\t@1\tpane",
		"$-1\t@1\t%1", "$1\t@1", "$1\t@1\t%1\textra",
	} {
		if _, err := parseIdentity(invalid); err == nil {
			t.Fatalf("malformed identity %q accepted", invalid)
		}
	}
}

func TestExecRunnerKeepsDiagnosticsOutOfMachineReadableOutput(t *testing.T) {
	runner := execRunner{}
	output, err := runner.Run(context.Background(), CommandSpec{
		Path: "/bin/sh",
		Args: []string{
			"-c",
			`printf '%s\n' 'non-fatal tmux diagnostic' >&2; printf '$12\t@34\t%%56\n'`,
		},
	})
	if err != nil {
		t.Fatalf("execRunner.Run: %v", err)
	}
	want := "$12\t@34\t%56\n"
	if got := string(output); got != want {
		t.Fatalf("machine-readable output = %q, want %q", got, want)
	}
	if _, err := parseIdentity(string(output)); err != nil {
		t.Fatalf("parse stdout identity: %v", err)
	}
}

func TestMalformedIdentityRollbackNeverKillsAnUnmarkedName(t *testing.T) {
	runner := newFakeRunner()
	runner.setNewSessionOutput("malformed identity\n")
	manager, _ := newTestManager(t, runner, Options{RunID: "malformed-rollback", PersistOnExit: true})
	_, err := manager.Start(context.Background(), testSpec(t, "agent"), terminal.Size{})
	if err == nil || !strings.Contains(err.Error(), "identifiants immuables") {
		t.Fatalf("Start error = %v, want malformed identity", err)
	}
	if _, lookupErr := manager.session("agent"); !errors.Is(lookupErr, ErrSessionNotFound) {
		t.Fatalf("malformed Start published an internal cleanup target: %v", lookupErr)
	}
	if err := manager.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
	kills := runner.callsFor("kill-session")
	if len(kills) != 0 {
		t.Fatalf("malformed unmarked creation issued kill-session: %#v", kills)
	}
}

func TestMissingTargetProbeRequiresDefinitiveExitStatus(t *testing.T) {
	exitErr := exec.Command("/usr/bin/false").Run()
	if !isMissingTargetProbe(exitErr) {
		t.Fatalf("exit status was not recognized as missing probe: %v", exitErr)
	}
	for _, transient := range []error{
		errors.New("temporary I/O failure"),
		context.DeadlineExceeded,
		context.Canceled,
		exec.ErrNotFound,
	} {
		if isMissingTargetProbe(transient) {
			t.Fatalf("transient probe error classified as missing: %v", transient)
		}
	}
}

func newTestManager(t *testing.T, runner *fakeRunner, options Options) (*Manager, chan session.Event) {
	t.Helper()
	events := make(chan session.Event, 1024)
	if options.Runner == nil {
		options.Runner = runner
	}
	if options.TmuxPath == "" {
		options.TmuxPath = "tmux"
	}
	if options.HelperPath == "" {
		options.HelperPath = "/test/bin/relayer"
	}
	if options.RuntimeDir == "" {
		options.RuntimeDir = t.TempDir()
	}
	manager, err := NewManager(context.Background(), events, intercept.DefaultPatterns(), 4096, options)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	return manager, events
}

func testSpec(t *testing.T, id string) agent.Spec {
	t.Helper()
	return agent.Spec{
		ID:      id,
		Name:    "Agent " + id,
		Command: []string{"fake-agent"},
		Cwd:     t.TempDir(),
		Adapter: agent.AdapterGeneric,
		Backend: agent.BackendTmux,
	}
}

func optionValue(arguments []string, option string) string {
	for index, argument := range arguments {
		if argument == option && index+1 < len(arguments) {
			return arguments[index+1]
		}
	}
	return ""
}

func waitForExitEvent(t *testing.T, events <-chan session.Event, id string) adapters.Event {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if adapterEvent, ok := event.(session.AdapterEvent); ok &&
				adapterEvent.Event.SessionID == id && adapterEvent.Event.Type == adapters.EventProcessExit {
				return adapterEvent.Event
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for exit event for %s", id)
		}
	}
}

func drainEvents(events <-chan session.Event) {
	for {
		select {
		case <-events:
		default:
			return
		}
	}
}

func intPointer(value int) *int { return &value }
