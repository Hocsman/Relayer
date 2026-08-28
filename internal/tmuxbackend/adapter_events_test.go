//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tmuxbackend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/intercept"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

func TestNewManagerWithRegistryResolvesSpecAdapter(t *testing.T) {
	runner := newFakeRunner()
	events := make(chan session.Event, 64)
	registry, err := adapters.NewRegistry(intercept.DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	options := Options{
		Runner:        runner,
		TmuxPath:      "tmux",
		HelperPath:    "/test/bin/relayer",
		RuntimeDir:    t.TempDir(),
		RunID:         "registry",
		PollInterval:  minimumPollInterval,
		handoffWaiter: skipLaunchHandoff,
	}
	manager, err := NewManagerWithRegistry(context.Background(), events, registry, 4096, options)
	if err != nil {
		t.Fatalf("NewManagerWithRegistry: %v", err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })

	spec := testSpec(t, "auto-adapter")
	spec.Adapter = ""
	info, err := manager.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Adapter != adapters.GenericID {
		t.Fatalf("resolved adapter = %q, want %q", info.Adapter, adapters.GenericID)
	}

	unknown := testSpec(t, "unknown-adapter")
	unknown.Adapter = "not-registered"
	startsBefore := len(runner.callsFor("new-session"))
	if _, err := manager.Start(context.Background(), unknown, terminal.Size{}); !errors.Is(err, adapters.ErrUnknownAdapter) {
		t.Fatalf("unknown adapter error = %v", err)
	}
	if got := len(runner.callsFor("new-session")); got != startsBefore {
		t.Fatalf("unknown adapter launched %d tmux sessions", got-startsBefore)
	}
}

func TestNewManagerWithRegistryRejectsNilRegistry(t *testing.T) {
	_, err := NewManagerWithRegistry(
		context.Background(),
		make(chan session.Event, 1),
		nil,
		1024,
		Options{Runner: newFakeRunner(), TmuxPath: "tmux"},
	)
	if err == nil || !strings.Contains(err.Error(), "registry") {
		t.Fatalf("nil registry error = %v", err)
	}
}

func TestManagerEventIdentitySnapshotAndExactResolution(t *testing.T) {
	runner := newFakeRunner()
	manager, events := newTestManager(t, runner, Options{
		RunID:         "typed-events",
		PersistOnExit: true,
		PollInterval:  minimumPollInterval,
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	info, err := manager.Start(context.Background(), testSpec(t, "typed-agent"), terminal.Size{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.session(info.ID)
	if err != nil {
		t.Fatal(err)
	}

	if err := target.processor.Consume([]byte("Overwrite? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	first := contractWaitForPrompt(t, events, info.ID)
	if first.Adapter != info.Adapter || first.Sequence != 1 || first.ID == "" {
		t.Fatalf("first typed event = %#v", first)
	}
	snapshot, err := manager.Snapshot(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Pending == nil || snapshot.Pending.ID != first.ID || snapshot.Revision != first.Sequence {
		t.Fatalf("snapshot = %#v, event = %#v", snapshot, first)
	}
	repeated, err := manager.Snapshot(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if repeated.Pending == nil || repeated.Pending.ID != first.ID || repeated.Revision != snapshot.Revision {
		t.Fatalf("repeated snapshot changed occurrence: first %#v repeated %#v", snapshot, repeated)
	}
	commandsBeforeCachedPending := len(runner.allCommands())
	cachedPending, err := manager.PendingEvent(context.Background(), info.ID)
	if err != nil || cachedPending == nil || cachedPending.ID != first.ID {
		t.Fatalf("cached PendingEvent = event %#v error %v", cachedPending, err)
	}
	if commandsAfter := len(runner.allCommands()); commandsAfter != commandsBeforeCachedPending {
		t.Fatalf("PendingEvent launched %d tmux command(s)", commandsAfter-commandsBeforeCachedPending)
	}

	loadsBefore := len(runner.callsFor("load-buffer"))
	if err := manager.SendEvent(context.Background(), info.ID, "evt-stale", []byte("N\r")); !errors.Is(err, adapters.ErrEventMismatch) {
		t.Fatalf("stale event error = %v", err)
	}
	if got := len(runner.callsFor("load-buffer")); got != loadsBefore {
		t.Fatalf("stale event wrote %d tmux buffers", got-loadsBefore)
	}
	if err := manager.SendEvent(context.Background(), info.ID, first.ID, []byte("Y\r")); err != nil {
		t.Fatalf("resolve first event: %v", err)
	}
	if pending := target.processor.Pending(); pending != nil {
		t.Fatalf("resolved event remains pending: %#v", pending)
	}

	if err := target.processor.Consume([]byte("Overwrite? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	second := contractWaitForPrompt(t, events, info.ID)
	if second.ID == first.ID || second.Sequence != first.Sequence+1 || second.Signature != first.Signature {
		t.Fatalf("successive occurrence = %#v after %#v", second, first)
	}
}

func TestManagerAttachResyncDeduplicatesEventAndResize(t *testing.T) {
	runner := newFakeRunner()
	manager, events := newTestManager(t, runner, Options{
		RunID:         "attach-resync",
		PersistOnExit: true,
		PollInterval:  minimumPollInterval,
	})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	startSize := terminal.Size{Columns: 80, Rows: 24}
	info, err := manager.Start(context.Background(), testSpec(t, "attached-agent"), startSize)
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.session(info.ID)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := manager.AttachCommand(context.Background(), info.ID); err != nil {
		t.Fatalf("AttachCommand: %v", err)
	}
	commandsBeforeDuplicate := len(runner.allCommands())
	if _, err := manager.AttachCommand(context.Background(), info.ID); err == nil {
		t.Fatal("duplicate AttachCommand succeeded")
	}
	if got := len(runner.allCommands()); got != commandsBeforeDuplicate {
		t.Fatalf("duplicate attach created %d commands", got-commandsBeforeDuplicate)
	}

	const promptText = "Overwrite during attach? [Y/n]"
	if err := target.processor.Consume([]byte(promptText)); err != nil {
		t.Fatal(err)
	}
	suppressed := target.processor.Pending()
	if suppressed == nil {
		t.Fatal("Processor lost event observed during attach")
	}
	assertNoActionableEvent(t, events, info.ID)
	runner.setCapture(promptText)
	if err := manager.Resync(context.Background(), info.ID, startSize.Columns, startSize.Rows); err != nil {
		t.Fatalf("Resync: %v", err)
	}
	reconciled := contractWaitForPrompt(t, events, info.ID)
	if reconciled.ID != suppressed.ID {
		t.Fatalf("Resync replaced occurrence ID: live %#v snapshot %#v", suppressed, reconciled)
	}
	if got := len(runner.callsFor("resize-window")); got != 0 {
		t.Fatalf("same-size Resync issued %d resize commands", got)
	}

	if err := manager.Resync(context.Background(), info.ID, startSize.Columns, startSize.Rows); err != nil {
		t.Fatalf("repeated Resync: %v", err)
	}
	assertNoActionableEvent(t, events, info.ID)
	runner.setCapture("")
	if err := manager.Resync(context.Background(), info.ID, 100, 30); err != nil {
		t.Fatalf("Resync after direct answer: %v", err)
	}
	if pending := target.processor.Pending(); pending != nil {
		t.Fatalf("answered attached prompt remains pending: %#v", pending)
	}
	if got := len(runner.callsFor("resize-window")); got != 1 {
		t.Fatalf("changed-size Resync issued %d resize commands", got)
	}
	if err := manager.Resize(context.Background(), info.ID, terminal.Size{Columns: 100, Rows: 30}); err != nil {
		t.Fatal(err)
	}
	if got := len(runner.callsFor("resize-window")); got != 1 {
		t.Fatalf("repeated resize issued %d resize commands", got)
	}
}

func TestManagerProcessExitIsSingleTypedEvent(t *testing.T) {
	runner := newFakeRunner()
	manager, events := newTestManager(t, runner, Options{RunID: "typed-exit", PersistOnExit: true})
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	info, err := manager.Start(context.Background(), testSpec(t, "exit-agent"), terminal.Size{})
	if err != nil {
		t.Fatal(err)
	}
	target, err := manager.session(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := target.processor.Consume([]byte("Overwrite before exit? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	prompt := contractWaitForPrompt(t, events, info.ID)
	if err := manager.Stop(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	exitEvent := waitForExitEvent(t, events, info.ID)
	if exitEvent.Type != adapters.EventProcessExit || exitEvent.Sequence != prompt.Sequence+1 || exitEvent.Adapter != info.Adapter {
		t.Fatalf("process_exit = %#v", exitEvent)
	}
	snapshot, err := manager.Snapshot(context.Background(), info.ID)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Revision != exitEvent.Sequence || snapshot.Pending != nil {
		t.Fatalf("stopped snapshot = %#v, exit %#v", snapshot, exitEvent)
	}
	if err := manager.Stop(context.Background(), info.ID); err != nil {
		t.Fatal(err)
	}
	assertNoProcessExitEvent(t, events, info.ID)
}

func assertNoActionableEvent(t *testing.T, events <-chan session.Event, id string) {
	t.Helper()
	deadline := time.NewTimer(40 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if adapterEvent, ok := event.(session.AdapterEvent); ok &&
				adapterEvent.Event.SessionID == id && adapterEvent.Event.Actionable() {
				t.Fatalf("unexpected actionable event: %#v", adapterEvent.Event)
			}
		case <-deadline.C:
			return
		}
	}
}

func assertNoProcessExitEvent(t *testing.T, events <-chan session.Event, id string) {
	t.Helper()
	deadline := time.NewTimer(40 * time.Millisecond)
	defer deadline.Stop()
	for {
		select {
		case event := <-events:
			if legacy, ok := event.(session.Exited); ok && legacy.SessionID == id {
				t.Fatalf("legacy Exited duplicated process_exit: %#v", legacy)
			}
			if adapterEvent, ok := event.(session.AdapterEvent); ok &&
				adapterEvent.Event.SessionID == id && adapterEvent.Event.Type == adapters.EventProcessExit {
				t.Fatalf("duplicate process_exit event: %#v", adapterEvent.Event)
			}
		case <-deadline.C:
			return
		}
	}
}

var _ terminal.EventSender = (*Manager)(nil)
var _ terminal.Backend = (*Manager)(nil)
