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
	// An agent that exits on the close event stops at once; the grace period
	// only bounds one that is still handling it.
	const unixGrace = 1500 * time.Millisecond
	if elapsed := time.Since(started); elapsed >= unixGrace {
		t.Fatalf("Stop took %s, want well under %s", elapsed, unixGrace)
	}
}

// TestAnAgentThatExitsWith259IsConfirmedStopped: 259 is the value of
// STILL_ACTIVE, and liveness was read from the exit code. With the session now
// holding the process object open, an agent that exited with 259 looked alive
// for good and every Stop of it reported ErrStopUncertain.
func TestAnAgentThatExitsWith259IsConfirmedStopped(t *testing.T) {
	manager := newWindowsTestManager(t)
	info, err := manager.Start(agent.Spec{
		ID:      "exits-with-259",
		Name:    "exits with 259",
		Command: []string{"cmd.exe", "/c", "exit 259"},
	}, 80, 24)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	waitDone(t, manager, info.ID)
	if err := manager.Stop(info.ID); err != nil {
		t.Fatalf("Stop of an agent that exited with 259: %v", err)
	}
}

// TestStoppingAnAgentReportsNoStreamError: closing the pseudo console freed the
// output pipe's handle while the reader was still blocked on it, and the next
// read used the freed number. Every Stop then reported "invalid handle" as a
// backend failure — the desktop journaled it and marked the agent failed — and
// when Windows had already given the number to another pipe, the reader read
// that pipe's bytes into the agent's output and never ended.
func TestStoppingAnAgentReportsNoStreamError(t *testing.T) {
	for _, agentCase := range []struct {
		id      string
		command []string
	}{
		{"quiet", []string{"cmd.exe", "/c", "ping -n 60 127.0.0.1 >nul"}},
		{"chatty", []string{"cmd.exe", "/c", "for /l %i in (1,1,100000) do @echo line %i"}},
	} {
		t.Run(agentCase.id, func(t *testing.T) {
			events := make(chan Event, 1024)
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
			streamErrors := make(chan error, 16)
			go func() {
				for {
					select {
					case event := <-events:
						if failure, ok := event.(Error); ok {
							streamErrors <- failure.Err
						}
					case <-ctx.Done():
						return
					}
				}
			}()
			t.Cleanup(func() {
				manager.Close()
				cancel()
			})

			info, err := manager.Start(agent.Spec{ID: agentCase.id, Name: agentCase.id, Command: agentCase.command}, 80, 24)
			if err != nil {
				t.Fatalf("Start: %v", err)
			}
			time.Sleep(700 * time.Millisecond)
			if err := manager.Stop(info.ID); err != nil {
				t.Fatalf("Stop: %v", err)
			}
			waitDone(t, manager, info.ID)
			select {
			case err := <-streamErrors:
				t.Fatalf("stopping the agent reported a stream failure: %v", err)
			case <-time.After(300 * time.Millisecond):
			}
		})
	}
}
