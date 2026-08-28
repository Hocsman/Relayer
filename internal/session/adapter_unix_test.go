//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package session

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
)

func TestManagerAdapterEventResolutionSnapshotAndProcessExit(t *testing.T) {
	registry, err := adapters.NewRegistry(integrationPatterns)
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	events := make(chan Event, 128)
	manager, err := NewManagerWithRegistry(context.Background(), events, registry, 4096)
	if err != nil {
		t.Fatalf("NewManagerWithRegistry: %v", err)
	}
	t.Cleanup(manager.Close)

	info, err := manager.Start(agent.Spec{
		ID:      "adapter-runtime",
		Name:    "adapter runtime",
		Adapter: adapters.GenericID,
		Shell: `printf 'Overwrite? [Y/n]'; IFS= read -r first
printf '\nfirst:%s\n' "$first"; IFS= read -r second; printf 'second:%s\n' "$second"`,
	}, 80, 24)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Adapter != adapters.GenericID {
		t.Fatalf("Info.Adapter = %q, want %q", info.Adapter, adapters.GenericID)
	}

	prompt := waitForActionableAdapterEvent(t, events, info.ID)
	if prompt.ID == "" || prompt.Signature == "" || prompt.Sequence == 0 ||
		prompt.AgentID != info.ID || prompt.Adapter != adapters.GenericID ||
		prompt.Type != adapters.EventConfirmation || prompt.Metadata["pattern"] != "overwrite" {
		t.Fatalf("actionable event = %#v", prompt)
	}
	pending, err := manager.PendingEvent(info.ID)
	if err != nil {
		t.Fatalf("PendingEvent: %v", err)
	}
	if pending == nil || pending.ID != prompt.ID {
		t.Fatalf("pending = %#v, want event %q", pending, prompt.ID)
	}
	pending.Metadata["pattern"] = "mutated"
	again, _ := manager.PendingEvent(info.ID)
	if again == nil || again.Metadata["pattern"] != "overwrite" {
		t.Fatalf("PendingEvent leaked mutable metadata: %#v", again)
	}
	if revision, revisionErr := manager.Revision(info.ID); revisionErr != nil || revision == 0 {
		t.Fatalf("Revision = (%d, %v), want a positive revision", revision, revisionErr)
	}

	if err := manager.SendDataForEvent(info.ID, "evt-stale", []byte("wrong\r")); !errors.Is(err, adapters.ErrEventMismatch) {
		t.Fatalf("stale pending event returned %v, want %v", err, adapters.ErrEventMismatch)
	}
	if stillPending, _ := manager.PendingEvent(info.ID); stillPending == nil || stillPending.ID != prompt.ID {
		t.Fatalf("mismatched decision changed pending event to %#v", stillPending)
	}
	if err := manager.SendDataForEvent(info.ID, prompt.ID, []byte("accepted\r")); err != nil {
		t.Fatalf("SendDataForEvent: %v", err)
	}
	if pending, _ := manager.PendingEvent(info.ID); pending != nil {
		t.Fatalf("resolved event remains pending: %#v", pending)
	}

	waitForSessionOutput(t, manager, info.ID, "first:accepted")
	// The occurrence ID is a CAS token: once resolved it must never authorize a
	// later write merely because the session currently has no pending event.
	if err := manager.SendDataForEvent(info.ID, prompt.ID, []byte("stale\r")); !errors.Is(err, adapters.ErrEventMismatch) {
		t.Fatalf("resolved event ID returned %v, want %v", err, adapters.ErrEventMismatch)
	}
	if err := manager.SendData(info.ID, []byte("finish\r")); err != nil {
		t.Fatalf("legacy SendData: %v", err)
	}

	var exitEvent *adapters.Event
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for exitEvent == nil {
		select {
		case message := <-events:
			semantic, ok := message.(AdapterEvent)
			if !ok || semantic.Event.SessionID != info.ID || semantic.Event.Type != adapters.EventProcessExit {
				continue
			}
			copy := semantic.Event.Clone()
			exitEvent = &copy
		case <-deadline.C:
			t.Fatal("timed out waiting for process_exit")
		}
	}
	if exitEvent.ID == "" || exitEvent.Signature == "" || exitEvent.Sequence <= prompt.Sequence ||
		exitEvent.Adapter != adapters.GenericID || exitEvent.Sensitive || exitEvent.Match != "" ||
		exitEvent.Metadata["exit_code"] != "0" ||
		exitEvent.Timestamp.IsZero() || exitEvent.Timestamp.Location() != time.UTC {
		t.Fatalf("process_exit event = %#v", exitEvent)
	}
	waitForSessionOutput(t, manager, info.ID, "second:finish")
	output, _ := manager.Output(info.ID)
	if !strings.Contains(output, "second:finish") || strings.Contains(output, "second:stale") {
		t.Fatalf("stale occurrence wrote to PTY: %q", output)
	}
}

func TestManagerFailedEventDeliveryKeepsPendingOccurrence(t *testing.T) {
	events := make(chan Event, 64)
	manager, err := NewManager(context.Background(), events, integrationPatterns, 4096)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)
	info, err := manager.Start(agent.Spec{
		ID:    "failed-delivery",
		Name:  "failed delivery",
		Shell: `printf 'Overwrite? [Y/n]'; IFS= read -r answer`,
	}, 80, 24)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	prompt := waitForActionableAdapterEvent(t, events, info.ID)
	session, err := manager.session(info.ID)
	if err != nil {
		t.Fatal(err)
	}
	session.closePTY()
	if err := manager.SendDataForEvent(info.ID, prompt.ID, []byte("yes\r")); err == nil {
		t.Fatal("SendDataForEvent unexpectedly succeeded on a closed PTY")
	}
	pending, err := manager.PendingEvent(info.ID)
	if err != nil || pending == nil || pending.ID != prompt.ID {
		t.Fatalf("failed delivery lost pending occurrence: pending=%#v err=%v", pending, err)
	}
}

func TestManagerPublishesFailedProcessExitWithoutLegacyOrErrorText(t *testing.T) {
	events := make(chan Event, 32)
	manager, err := NewManager(context.Background(), events, integrationPatterns, 1024)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)
	info, err := manager.Start(agent.Spec{
		ID:      "failed-exit",
		Name:    "failed exit",
		Command: []string{"/bin/sh", "-c", "exit 7"},
	}, 40, 10)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case message := <-events:
			if _, legacy := message.(Exited); legacy {
				t.Fatalf("manager emitted legacy Exited: %#v", message)
			}
			semantic, ok := message.(AdapterEvent)
			if !ok || semantic.Event.SessionID != info.ID || semantic.Event.Type != adapters.EventProcessExit {
				continue
			}
			if semantic.Event.Metadata["exit_code"] != "7" || semantic.Event.Sensitive ||
				semantic.Event.Match != "" || !strings.Contains(semantic.Event.Summary, "error") {
				t.Fatalf("failed process_exit = %#v", semantic.Event)
			}
			_, waitErr, exitCode, resultErr := manager.Result(info.ID)
			if resultErr != nil || waitErr == nil || exitCode == nil || *exitCode != 7 {
				t.Fatalf("Result = (wait=%v, code=%v, err=%v)", waitErr, exitCode, resultErr)
			}
			return
		case <-deadline.C:
			t.Fatal("timed out waiting for failed process_exit")
		}
	}
}

func waitForActionableAdapterEvent(t *testing.T, events <-chan Event, sessionID string) adapters.Event {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	for {
		select {
		case message := <-events:
			if semantic, ok := message.(AdapterEvent); ok &&
				semantic.Event.SessionID == sessionID && semantic.Event.Actionable() {
				return semantic.Event.Clone()
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for actionable AdapterEvent for %q", sessionID)
		}
	}
}

func waitForSessionOutput(t *testing.T, manager *Manager, sessionID, wanted string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		output, err := manager.Output(sessionID)
		if err != nil {
			t.Fatalf("Output: %v", err)
		}
		if strings.Contains(output, wanted) {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	output, _ := manager.Output(sessionID)
	t.Fatalf("output %q never contained %q", output, wanted)
}
