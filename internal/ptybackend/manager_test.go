//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package ptybackend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

func TestManagerImplementsTerminalBackendAndSendsExactData(t *testing.T) {
	events := make(chan session.Event, 32)
	manager, err := New(context.Background(), events, nil, 4096)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	})

	info, err := manager.Start(context.Background(), agent.Spec{
		ID:      "pty-adapter",
		Name:    "PTY adapter",
		Command: []string{"/bin/sh", "-c", "IFS= read -r value; printf 'got:%s' \"$value\""},
		Backend: agent.BackendPTY,
	}, terminal.Size{Columns: 40, Rows: 10})
	if err != nil {
		t.Fatal(err)
	}
	if info.Backend != agent.BackendPTY || info.Adapter != adapters.GenericID {
		t.Fatalf("backend info = %#v", info)
	}
	if err := manager.Send(context.Background(), info.ID, []byte("literal value\r")); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		snapshot, snapshotErr := manager.Snapshot(context.Background(), info.ID)
		if snapshotErr != nil {
			t.Fatal(snapshotErr)
		}
		if strings.Contains(snapshot.Output, "got:literal value") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("output never arrived: %q", snapshot.Output)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestManagerWithRegistrySendsExactPendingEventAndSnapshotsRevision(t *testing.T) {
	registry, err := adapters.NewRegistry([]adapters.Pattern{{
		Name:        "overwrite",
		Description: "overwrite confirmation",
		Expression:  `(?i)overwrite.*\[y/n\]`,
	}})
	if err != nil {
		t.Fatalf("NewRegistry: %v", err)
	}
	events := make(chan session.Event, 64)
	manager, err := NewWithRegistry(context.Background(), events, registry, 4096)
	if err != nil {
		t.Fatalf("NewWithRegistry: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	})

	info, err := manager.Start(context.Background(), agent.Spec{
		ID:    "pty-event",
		Name:  "PTY event",
		Shell: `printf 'Overwrite? [Y/n]'; IFS= read -r value; printf '\ngot:%s\n' "$value"; sleep 1`,
	}, terminal.Size{Columns: 40, Rows: 10})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if info.Adapter != adapters.GenericID {
		t.Fatalf("Info.Adapter = %q, want %q", info.Adapter, adapters.GenericID)
	}

	var pending adapters.Event
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	for pending.ID == "" {
		select {
		case message := <-events:
			if semantic, ok := message.(session.AdapterEvent); ok && semantic.Event.Actionable() {
				pending = semantic.Event.Clone()
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for AdapterEvent")
		}
	}
	snapshot, err := manager.Snapshot(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Snapshot: %v", err)
	}
	if snapshot.Pending == nil || snapshot.Pending.ID != pending.ID || snapshot.Revision == 0 {
		t.Fatalf("Snapshot = %#v, want pending %q and positive revision", snapshot, pending.ID)
	}
	snapshot.Pending.Metadata["pattern"] = "mutated"
	again, _ := manager.Snapshot(context.Background(), info.ID)
	if again.Pending == nil || again.Pending.Metadata["pattern"] != "overwrite" {
		t.Fatalf("Snapshot leaked pending metadata mutation: %#v", again.Pending)
	}

	if err := manager.SendEvent(context.Background(), info.ID, "evt-stale", []byte("wrong\r")); !errors.Is(err, adapters.ErrEventMismatch) {
		t.Fatalf("SendEvent stale ID error = %v, want %v", err, adapters.ErrEventMismatch)
	}
	if err := manager.SendEvent(context.Background(), info.ID, pending.ID, []byte("literal value\r")); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}
	after, err := manager.Snapshot(context.Background(), info.ID)
	if err != nil {
		t.Fatalf("Snapshot after SendEvent: %v", err)
	}
	if after.Pending != nil {
		t.Fatalf("resolved event remains pending: %#v", after.Pending)
	}

	outputDeadline := time.Now().Add(2 * time.Second)
	for {
		current, snapshotErr := manager.Snapshot(context.Background(), info.ID)
		if snapshotErr != nil {
			t.Fatalf("Snapshot output: %v", snapshotErr)
		}
		if strings.Contains(current.Output, "got:literal value") {
			break
		}
		if time.Now().After(outputDeadline) {
			t.Fatalf("exact SendEvent bytes never arrived: %q", current.Output)
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestAttachIsRecognizablyUnsupported(t *testing.T) {
	manager, err := New(context.Background(), make(chan session.Event, 1), nil, 1024)
	if err != nil {
		t.Fatal(err)
	}
	defer manager.inner.Close()
	if _, err := manager.AttachCommand(context.Background(), "anything"); !errors.Is(err, terminal.ErrNotAttachable) {
		t.Fatalf("AttachCommand error = %v", err)
	}
}
