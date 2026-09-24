//go:build linux

package session

import (
	"bytes"
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
)

// deafPaste is a megabyte of lines, as a paste is. Complete lines fill a
// canonical terminal's input buffer; characters with no line end are let
// through, to be erased, and never block.
func deafPaste() []byte {
	return bytes.Repeat([]byte(strings.Repeat("k", 63)+"\r"), 16*1024)
}

// startDeafSession starts an agent that never reads its terminal, with a
// manager whose events are drained, and returns the manager and the session.
func startDeafSession(t *testing.T, shell string) (*Manager, Info) {
	t.Helper()
	events := make(chan Event, 1024)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go func() {
		for {
			select {
			case <-events:
			case <-ctx.Done():
				return
			}
		}
	}()
	manager, err := NewManager(context.Background(), events, integrationPatterns, 4096)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)
	info, err := manager.Start(agent.Spec{ID: "deaf", Name: "deaf", Shell: shell}, 80, 24)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	return manager, info
}

// A paste, a megabyte of lines, fills the terminal's input buffer of an
// agent that reads nothing, about 20 KiB in, and the rest of the write waits
// for room. The write ends with its context: it returns at the context's
// deadline, having written part of the paste. The master was left in blocking
// mode by creack/pty, and the write blocked in the kernel whatever its
// context said, holding the gateway's write slot for the session and, with
// it, the run's end.
func TestAKeystrokeWriteToAnAgentThatReadsNothingEndsWithItsContext(t *testing.T) {
	manager, info := startDeafSession(t, "exec sleep 60")
	paste := deafPaste()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	written := make(chan error, 1)
	go func() { written <- manager.SendRaw(ctx, info.ID, paste) }()
	select {
	case err := <-written:
		if !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, os.ErrDeadlineExceeded) {
			t.Fatalf("a paste into an agent that reads nothing = %v, want its context's deadline", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a keystroke write to an agent that reads nothing outlived its context by nine seconds")
	}
	// The terminal still works: it is sized and read as before.
	if err := manager.Resize(info.ID, 100, 30); err != nil {
		t.Fatalf("Resize after the bounded write: %v", err)
	}
}

// A keystroke write blocked on an agent that reads nothing ends when the
// session stops, even when a process that left the agent's process group
// still holds the terminal open: the session closes the master once the agent
// has exited, and the close ends the write. A master in blocking mode kept the
// write in the kernel until the last process holding the terminal exited.
func TestAKeystrokeWriteBlockedOnAnAgentThatReadsNothingEndsWhenItStops(t *testing.T) {
	manager, info := startDeafSession(t, "setsid sleep 20 & exec sleep 60")
	paste := deafPaste()
	written := make(chan error, 1)
	go func() { written <- manager.SendRaw(context.Background(), info.ID, paste) }()
	select {
	case err := <-written:
		t.Fatalf("a paste into an agent that reads nothing returned at once (%v): nothing blocked", err)
	case <-time.After(500 * time.Millisecond):
	}
	_ = manager.Stop(info.ID)
	select {
	case err := <-written:
		if err == nil {
			t.Fatal("a paste cut off by the stop reported it was written")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a keystroke write blocked on an agent that reads nothing outlived its stop")
	}
}
