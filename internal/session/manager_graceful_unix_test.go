//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package session

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
)

// startTermTrappingAgent launches a leader that, on SIGTERM, writes a marker
// file and exits. Only a graceful stop can leave the marker behind: SIGKILL
// cannot be trapped.
func startTermTrappingAgent(t *testing.T, manager *Manager, events <-chan Event, id string) string {
	t.Helper()

	marker := filepath.Join(t.TempDir(), "stopped-gracefully")
	info, err := manager.Start(agent.Spec{
		ID:    id,
		Name:  id,
		Shell: `trap 'printf done > "$RELAYER_MARKER"; exit 0' TERM; printf 'READY\n'; while :; do sleep 0.05; done`,
		Env:   map[string]string{"RELAYER_MARKER": marker},
	}, 40, 10)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	deadline := time.After(3 * time.Second)
	for {
		select {
		case event := <-events:
			if output, ok := event.(OutputAvailable); ok && output.SessionID == info.ID {
				if content, _ := manager.Output(info.ID); strings.Contains(content, "READY") {
					return marker
				}
			}
		case <-deadline:
			t.Fatal("the agent never became ready")
		}
	}
}

func assertStoppedGracefully(t *testing.T, marker, path string) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for {
		if data, err := os.ReadFile(marker); err == nil && string(data) == "done" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s: the agent never handled SIGTERM; it was killed before its grace period", path)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestEveryStopPathGivesTheAgentItsGracePeriod covers the paths v0.8.5 left
// SIGKILL-first. Manager.Close cancelled the Manager's context before asking
// anything to stop, and a session's command is an exec.CommandContext whose
// default cancel is an immediate SIGKILL of the leader; a caller that cancelled
// a parent context first — a supervisor shutting down, a signal to relayer
// serve — did the same. Only Stop on a single agent sent SIGTERM first.
func TestEveryStopPathGivesTheAgentItsGracePeriod(t *testing.T) {
	t.Run("Stop", func(t *testing.T) {
		events := make(chan Event, 256)
		manager, err := NewManager(context.Background(), events, integrationPatterns, 4096)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		t.Cleanup(manager.Close)
		marker := startTermTrappingAgent(t, manager, events, "graceful-stop")
		if err := manager.Stop("graceful-stop"); err != nil {
			t.Fatalf("Stop: %v", err)
		}
		assertStoppedGracefully(t, marker, "Stop")
	})

	t.Run("Close", func(t *testing.T) {
		events := make(chan Event, 256)
		manager, err := NewManager(context.Background(), events, integrationPatterns, 4096)
		if err != nil {
			t.Fatalf("NewManager: %v", err)
		}
		marker := startTermTrappingAgent(t, manager, events, "graceful-close")
		manager.Close()
		assertStoppedGracefully(t, marker, "Close")
	})

	t.Run("parent context cancelled first", func(t *testing.T) {
		events := make(chan Event, 256)
		parent, cancel := context.WithCancel(context.Background())
		manager, err := NewManager(parent, events, integrationPatterns, 4096)
		if err != nil {
			cancel()
			t.Fatalf("NewManager: %v", err)
		}
		marker := startTermTrappingAgent(t, manager, events, "graceful-cancel")
		cancel()
		manager.Close()
		assertStoppedGracefully(t, marker, "parent context cancelled first")
	})
}

// TestShutdownSendsASingleSIGTERM: a caller that cancels a parent context and
// then closes used to send two — the context's cancellation hook, then Close —
// and an agent whose handler runs only once, or treats a second TERM as
// "force", was killed by the second.
func TestShutdownSendsASingleSIGTERM(t *testing.T) {
	events := make(chan Event, 256)
	parent, cancel := context.WithCancel(context.Background())
	manager, err := NewManager(parent, events, integrationPatterns, 4096)
	if err != nil {
		cancel()
		t.Fatalf("NewManager: %v", err)
	}
	counter := filepath.Join(t.TempDir(), "terms")
	info, err := manager.Start(agent.Spec{
		ID:    "counts-terms",
		Name:  "counts terms",
		Shell: `trap 'printf x >> "$RELAYER_COUNT"' TERM; printf 'READY\n'; while [ ! -s "$RELAYER_COUNT" ]; do sleep 0.05; done; sleep 0.5; exit 0`,
		Env:   map[string]string{"RELAYER_COUNT": counter},
	}, 40, 10)
	if err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for ready := false; !ready; {
		select {
		case event := <-events:
			if output, ok := event.(OutputAvailable); ok && output.SessionID == info.ID {
				content, _ := manager.Output(info.ID)
				ready = strings.Contains(content, "READY")
			}
		case <-deadline:
			cancel()
			t.Fatal("the agent never became ready")
		}
	}

	cancel()
	manager.Close()

	data, err := os.ReadFile(counter)
	if err != nil {
		t.Fatalf("read the TERM count: %v", err)
	}
	if got := len(data); got != 1 {
		t.Fatalf("the agent received %d SIGTERMs during one shutdown, want exactly 1", got)
	}
}
