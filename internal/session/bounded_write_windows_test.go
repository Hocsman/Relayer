//go:build windows

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

// A keystroke write to an agent that reads nothing ends with its context, as
// on Unix. The input pipe's WriteFile is synchronous: once conhost stops
// draining the pipe it blocked for good, holding the gateway's write slot for
// the session. It now returns by the context's deadline, having written the
// paste or part of it.
func TestAKeystrokeWriteToAWindowsAgentThatReadsNothingEndsWithItsContext(t *testing.T) {
	manager := newWindowsTestManager(t)
	info, err := manager.Start(agent.Spec{
		ID:      "deaf",
		Name:    "deaf",
		Command: []string{"cmd.exe", "/c", "ping -n 60 127.0.0.1 >nul"},
	}, 80, 24)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	paste := bytes.Repeat([]byte(strings.Repeat("k", 63)+"\r"), 16*1024)
	for attempt := 0; attempt < 3; attempt++ {
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		written := make(chan error, 1)
		go func() { written <- manager.SendRaw(ctx, info.ID, paste) }()
		select {
		case err := <-written:
			cancel()
			if err != nil && !errors.Is(err, context.DeadlineExceeded) && !errors.Is(err, os.ErrDeadlineExceeded) {
				t.Fatalf("a paste into an agent that reads nothing = %v, want nil or its context's deadline", err)
			}
		case <-time.After(10 * time.Second):
			cancel()
			t.Fatal("a keystroke write to an agent that reads nothing outlived its context by nine seconds")
		}
	}
	// The console still works: it is sized as before.
	if err := manager.Resize(info.ID, 100, 30); err != nil {
		t.Fatalf("Resize after the bounded writes: %v", err)
	}
}
