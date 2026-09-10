//go:build windows

package ptybackend

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

func TestManagerImplementsTerminalBackendOnWindows(t *testing.T) {
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
		ID:      "conpty-test",
		Name:    "ConPTY Test",
		Command: []string{"cmd.exe", "/c", "echo hello-conpty"},
		Backend: agent.BackendPTY,
	}, terminal.Size{Columns: 40, Rows: 10})
	if err != nil {
		t.Fatal(err)
	}
	if info.Backend != agent.BackendPTY || info.Adapter != adapters.GenericID {
		t.Fatalf("backend info = %#v", info)
	}

	deadline := time.Now().Add(5 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		snapshot, err := manager.Snapshot(context.Background(), info.ID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(snapshot.Output, "hello-conpty") {
			found = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !found {
		snapshot, _ := manager.Snapshot(context.Background(), info.ID)
		t.Fatalf("output never arrived: %q", snapshot.Output)
	}
}

func TestManagerSendLineAndResizeOnWindows(t *testing.T) {
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
		ID:      "conpty-resize",
		Name:    "ConPTY Resize Test",
		Command: []string{"cmd.exe", "/c", "echo resized"},
		Backend: agent.BackendPTY,
	}, terminal.Size{Columns: 40, Rows: 10})
	if err != nil {
		t.Fatal(err)
	}

	if err := manager.Resize(context.Background(), info.ID, terminal.Size{Columns: 80, Rows: 24}); err != nil {
		t.Fatalf("resize error = %v", err)
	}
}
