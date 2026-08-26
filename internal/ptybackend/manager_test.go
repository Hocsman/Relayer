//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package ptybackend

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

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
	if info.Backend != agent.BackendPTY {
		t.Fatalf("backend = %q", info.Backend)
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
