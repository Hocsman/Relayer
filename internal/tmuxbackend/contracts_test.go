//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tmuxbackend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

func TestManagerFIFOInterceptionFailedInputReblockAndRearm(t *testing.T) {
	runner := newFakeRunner()
	manager, events := newTestManager(t, runner, Options{
		RunID:         "fifo-contract",
		PersistOnExit: true,
		PollInterval:  minimumPollInterval,
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	})

	info, err := manager.Start(
		context.Background(),
		testSpec(t, "stream-agent"),
		terminal.Size{Columns: 90, Rows: 20},
	)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	target, err := manager.session(info.ID)
	if err != nil {
		t.Fatalf("owned session lookup: %v", err)
	}
	writer, err := os.OpenFile(target.files.outputPath, os.O_WRONLY, 0)
	if err != nil {
		t.Fatalf("open output FIFO writer: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })

	for _, chunk := range []string{
		"\x1b[3",
		"1mOver",
		"write? [Y",
		"/n]\x1b[0",
		"m",
	} {
		if _, err := writer.Write([]byte(chunk)); err != nil {
			t.Fatalf("write fragmented prompt: %v", err)
		}
	}
	first := contractWaitForPrompt(t, events, info.ID)
	if first.Metadata["pattern"] != "overwrite" || first.Match != "Overwrite? [Y/n]" {
		t.Fatalf("first prompt = %#v", first)
	}
	if output, outputErr := manager.Output(info.ID); outputErr != nil || output != "Overwrite? [Y/n]" {
		t.Fatalf("sanitized FIFO output = %q, error %v", output, outputErr)
	}

	deliveryFailure := errors.New("planned load-buffer failure")
	runner.setFailure("load-buffer", deliveryFailure)
	if err := manager.SendEvent(context.Background(), info.ID, first.ID, []byte("first-secret\r")); !errors.Is(err, deliveryFailure) {
		t.Fatalf("failed SendEvent error = %v", err)
	}
	if !target.processor.IsBlocked() {
		t.Fatal("failed delivery did not retain Processor blocked state")
	}
	pending := target.processor.Pending()
	if pending == nil || pending.ID != first.ID || pending.Metadata["pattern"] != "overwrite" {
		t.Fatalf("event was not retained after failed delivery: %#v", pending)
	}

	runner.setFailure("load-buffer", nil)
	if err := manager.SendEvent(context.Background(), info.ID, first.ID, []byte("Y\r")); err != nil {
		t.Fatalf("successful SendEvent: %v", err)
	}
	loads := runner.callsFor("load-buffer")
	if len(loads) != 2 || string(loads[1].Stdin) != "Y\r" {
		t.Fatalf("load-buffer calls = %#v, want final exact Y\\r on stdin", loads)
	}
	if target.processor.IsBlocked() {
		t.Fatal("successful delivery left Processor blocked")
	}
	if pending := target.processor.Pending(); pending != nil {
		t.Fatalf("successful delivery retained event %#v", pending)
	}

	if _, err := writer.Write([]byte("\r\nOVERWRITE second? [y/n]")); err != nil {
		t.Fatalf("write second prompt: %v", err)
	}
	second := contractWaitForPrompt(t, events, info.ID)
	if second.Metadata["pattern"] != "overwrite" || second.Match != "OVERWRITE second? [y/n]" || second.ID == first.ID {
		t.Fatalf("rearmed prompt = %#v", second)
	}
	snapshot, err := manager.Snapshot(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Pending == nil || snapshot.Pending.ID != second.ID || snapshot.Pending.Metadata["pattern"] != "overwrite" || !strings.Contains(snapshot.Output, "OVERWRITE second? [y/n]") {
		t.Fatalf("snapshot did not reconcile prompt/output: %#v", snapshot)
	}
}

func TestManagerImmediateCloseAfterStartJoinsAndClosesCapturedOutput(t *testing.T) {
	for iteration := 0; iteration < 16; iteration++ {
		t.Run(fmt.Sprintf("iteration-%d", iteration), func(t *testing.T) {
			runner := newFakeRunner()
			manager, _ := newTestManager(t, runner, Options{RunID: fmt.Sprintf("immediate-%d", iteration)})
			info, err := manager.Start(
				context.Background(),
				testSpec(t, "immediate"),
				terminal.Size{Columns: 80, Rows: 24},
			)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			target, err := manager.session(info.ID)
			if err != nil {
				t.Fatal(err)
			}
			capturedOutput := target.files.output
			runtimeDirectory := manager.RuntimeDirectory()

			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			closeErr := manager.Close(ctx)
			cancel()
			if closeErr != nil {
				t.Fatalf("immediate Close: %v", closeErr)
			}
			if target.files.output != nil {
				t.Fatal("Close retained the launchFiles output descriptor")
			}
			if _, err := capturedOutput.Read(make([]byte, 1)); err == nil {
				t.Fatal("captured output descriptor remained readable after Close")
			}
			if _, err := os.Stat(runtimeDirectory); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("runtime directory still exists after Close: %v", err)
			}
		})
	}
}

func TestManagerStopEmitsExitedAndLeavesStoppedSnapshot(t *testing.T) {
	runner := newFakeRunner()
	manager, events := newTestManager(t, runner, Options{RunID: "stop-state", PersistOnExit: true})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	info, err := manager.Start(context.Background(), testSpec(t, "stopped-agent"), terminal.Size{})
	if err != nil {
		t.Fatal(err)
	}
	done, err := manager.Done(info.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.Stop(context.Background(), strings.ToUpper(info.ID)); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	exited := waitForExitEvent(t, events, info.ID)
	if exited.Metadata["failed"] == "true" {
		t.Fatalf("explicit Stop emitted a failed process_exit: %#v", exited)
	}
	select {
	case <-done:
	default:
		t.Fatal("Done remains open after Stop")
	}
	snapshot, err := manager.Snapshot(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Snapshot after Stop: %v", err)
	}
	if snapshot.Status != terminal.StatusExited || snapshot.Running || snapshot.Attached || snapshot.ExitCode != nil {
		t.Fatalf("stopped snapshot = %#v", snapshot)
	}
	if err := manager.Stop(context.Background(), info.ID); err != nil {
		t.Fatalf("idempotent Stop: %v", err)
	}
	if kills := runner.callsFor("kill-session"); len(kills) != 1 {
		t.Fatalf("Stop kill calls = %#v, want exactly one", kills)
	}
}

func TestManagerOwnershipGuardRejectsTamperedSessionWithoutKillingIt(t *testing.T) {
	runner := newFakeRunner()
	manager, _ := newTestManager(t, runner, Options{RunID: "owner-guard", PersistOnExit: true})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	info, err := manager.Start(context.Background(), testSpec(t, "owned-agent"), terminal.Size{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.session(info.ID)
	if err != nil {
		t.Fatal(err)
	}

	runner.mu.Lock()
	identity := runner.identities[target.sessionID]
	if identity == nil {
		runner.mu.Unlock()
		t.Fatalf("fake has no immutable identity %q", target.sessionID)
	}
	originalOwner := identity.owner
	identity.owner = "tampered-owner"
	runner.mu.Unlock()

	for operation, call := range map[string]func() error{
		"stop": func() error { return manager.Stop(context.Background(), info.ID) },
		"resize": func() error {
			return manager.Resize(context.Background(), info.ID, terminal.Size{Columns: 100, Rows: 30})
		},
		"attach": func() error {
			_, attachErr := manager.AttachCommand(context.Background(), info.ID)
			return attachErr
		},
	} {
		if err := call(); err == nil || !strings.Contains(err.Error(), "ownership") {
			t.Fatalf("%s with tampered owner error = %v", operation, err)
		}
	}
	if kills := runner.callsFor("kill-session"); len(kills) != 0 {
		t.Fatalf("tampered session was killed: %#v", kills)
	}

	runner.mu.Lock()
	identity.owner = originalOwner
	runner.mu.Unlock()
	if err := manager.Stop(context.Background(), info.ID); err != nil {
		t.Fatalf("Stop after restoring ownership: %v", err)
	}
}

func TestMergedLaunchEnvironmentIsFreshAndExcludesOnlyImplicitTmuxMetadata(t *testing.T) {
	const refreshName = "RELAYER_TMUX_ENV_REFRESH_TEST"
	const removedName = "RELAYER_TMUX_ENV_REMOVED_TEST"
	t.Setenv(refreshName, "first")
	t.Setenv(removedName, "present")
	first := mergedLaunchEnvironment(nil)
	if first[refreshName] != "first" || first[removedName] != "present" {
		t.Fatalf("first environment snapshot = %#v", first)
	}
	t.Setenv(refreshName, "second")
	if err := os.Unsetenv(removedName); err != nil {
		t.Fatalf("unset environment value: %v", err)
	}
	second := mergedLaunchEnvironment(nil)
	if second[refreshName] != "second" {
		t.Fatalf("second environment snapshot retained stale value %q", second[refreshName])
	}
	if _, exists := second[removedName]; exists {
		t.Fatalf("second environment snapshot retained removed variable %q", removedName)
	}

	t.Setenv("TERM", "parent-term")
	t.Setenv("TMUX", "parent-tmux")
	t.Setenv("TMUX_PANE", "%parent")
	implicit := mergedLaunchEnvironment(nil)
	for _, name := range []string{"TERM", "TMUX", "TMUX_PANE"} {
		if _, exists := implicit[name]; exists {
			t.Fatalf("implicit tmux metadata %q was serialized", name)
		}
	}
	explicit := mergedLaunchEnvironment(map[string]string{
		"TERM":      "agent-term",
		"TMUX":      "agent-tmux",
		"TMUX_PANE": "%agent",
	})
	if explicit["TERM"] != "agent-term" || explicit["TMUX"] != "agent-tmux" || explicit["TMUX_PANE"] != "%agent" {
		t.Fatalf("explicit tmux metadata overrides were lost: %#v", explicit)
	}
}

func contractWaitForPrompt(t *testing.T, events <-chan session.Event, id string) adapters.Event {
	t.Helper()
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case event := <-events:
			if adapterEvent, ok := event.(session.AdapterEvent); ok &&
				adapterEvent.Event.SessionID == id && adapterEvent.Event.Actionable() {
				return adapterEvent.Event
			}
		case <-timer.C:
			t.Fatalf("timed out waiting for prompt event for %q", id)
		}
	}
}
