//go:build windows

package session

import (
	"context"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/intercept"
	"golang.org/x/sys/windows"
)

// newWindowsTestManager starts a Manager whose events are drained, so no
// essential event sender can hold a test up.
func newWindowsTestManager(t *testing.T) *Manager {
	t.Helper()

	events := make(chan Event, 256)
	ctx, cancel := context.WithCancel(context.Background())
	manager, err := NewManager(ctx, events, []intercept.Pattern{{
		Name:        "never",
		Description: "never matches",
		Expression:  `\A\z never`,
	}}, 1024)
	if err != nil {
		cancel()
		t.Fatalf("NewManager: %v", err)
	}
	go func() {
		for {
			select {
			case <-events:
			case <-ctx.Done():
				return
			}
		}
	}()
	t.Cleanup(func() {
		manager.Close()
		cancel()
	})
	return manager
}

func waitDone(t *testing.T, manager *Manager, id string) {
	t.Helper()
	done, err := manager.Done(id)
	if err != nil {
		t.Fatalf("Done: %v", err)
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the session did not finish")
	}
}

// TestExitedSessionKeepsItsPIDReserved is the Windows half of the PID-reuse
// fix. Windows gives a freed PID to the next process that asks for one, and a
// Stop, Close or Restart can address a session long after its leader exited.
// Relayer now holds a handle to the leader until the session is released, and
// Windows never reuses a PID while a handle to the old process is open: the
// number keeps meaning this session's process, so it cannot name another one.
//
// The proof is that the PID still opens and still reports this process's exit
// code after the leader is gone. v0.8.5 closed the handle as soon as the wait
// returned, and the same OpenProcess then failed or found a stranger.
func TestExitedSessionKeepsItsPIDReserved(t *testing.T) {
	manager := newWindowsTestManager(t)
	info, err := manager.Start(agent.Spec{
		ID:      "exits-with-three",
		Name:    "exits with three",
		Command: []string{"cmd.exe", "/c", "exit 3"},
	}, 80, 24)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	session, err := manager.session(info.ID)
	if err != nil {
		t.Fatalf("session: %v", err)
	}
	pid := uint32(session.cmd.Process.Pid)
	waitDone(t, manager, info.ID)

	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, pid)
	if err != nil {
		t.Fatalf("the exited leader's PID %d no longer opens: %v", pid, err)
	}
	var exitCode uint32
	err = windows.GetExitCodeProcess(handle, &exitCode)
	_ = windows.CloseHandle(handle)
	if err != nil {
		t.Fatalf("GetExitCodeProcess: %v", err)
	}
	if exitCode != 3 {
		t.Fatalf("PID %d reports exit code %d, want 3: it no longer names this session's process", pid, exitCode)
	}

	// A Stop of the exited session is the path that killed strangers. It must
	// succeed without addressing anything.
	if err := manager.Stop(info.ID); err != nil {
		t.Fatalf("Stop of an exited session: %v", err)
	}

	if err := manager.Remove(info.ID); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if held := session.proc.waitable(); held != 0 {
		t.Fatal("Remove kept the process handle: the PID would stay reserved for the life of the run")
	}
}

// TestWindowsStopDoesNotWaitOutTheUnixGracePeriod pins the other half of the
// v0.8.5 regression. The graceful request on Windows is taskkill without /F,
// which never reaches a console program under ConPTY, so v0.8.5 waited out the
// full 1.5-second Unix grace period on every Stop. Closing the pseudo console is
// the graceful stop there, and it is immediate.
func TestWindowsStopDoesNotWaitOutTheUnixGracePeriod(t *testing.T) {
	manager := newWindowsTestManager(t)
	info, err := manager.Start(agent.Spec{
		ID:      "long-running",
		Name:    "long running",
		Command: []string{"cmd.exe", "/c", "ping -n 60 127.0.0.1 >nul"},
	}, 80, 24)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	// Let the child start before asking it to stop.
	time.Sleep(300 * time.Millisecond)

	started := time.Now()
	if err := manager.Stop(info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= gracefulStopTimeout {
		t.Fatalf("Stop took %s, want well under the %s grace period", elapsed, gracefulStopTimeout)
	}
}
