//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package session

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/intercept"
	"github.com/creack/pty"
)

var integrationPatterns = []intercept.Pattern{{
	Name:        "overwrite",
	Description: "overwrite confirmation",
	Expression:  `(?i)overwrite.*\[y/n\]`,
}}

func TestDoneDoesNotWaitForFinalInvalidationConsumer(t *testing.T) {
	events := make(chan Event, 1)
	events <- OutputAvailable{SessionID: "occupied"}
	manager, err := NewManager(context.Background(), events, integrationPatterns, 4096)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(manager.Close)
	info, err := manager.Start(agent.Spec{
		ID:      "blocked-invalidation",
		Name:    "blocked invalidation",
		Command: []string{"/bin/sh", "-c", "printf final-output"},
	}, 40, 10)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	done, err := manager.Done(info.ID)
	if err != nil {
		t.Fatalf("Done: %v", err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Done waited for a consumer of the final output invalidation")
	}
	output, err := manager.Output(info.ID)
	if err != nil || !strings.Contains(output, "final-output") {
		t.Fatalf("final output = %q, error %v", output, err)
	}
	// Release the deliberately occupied queue before cleanup joins emitters.
	<-events
}

func TestManagerLifecyclePromptResizeInputAndFinalOutput(t *testing.T) {
	if _, err := exec.LookPath("stty"); err != nil {
		t.Skipf("stty is required for the PTY resize assertion: %v", err)
	}

	events := make(chan Event, 128)
	manager, err := NewManager(context.Background(), events, integrationPatterns, 4096)
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}
	defer manager.Close()

	command := `printf 'Running...\n'; printf 'Overwrite? [Y/n]'; IFS= read -r answer; printf '\nDone: %s\n' "$answer"; stty size`
	info, err := manager.Start(agent.Spec{
		ID:    "integration",
		Name:  "integration",
		Shell: command,
	}, 40, 10)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if info.ID != "integration" || info.Name != "integration" ||
		info.DisplayCommand != "[shell explicite]" || !info.Shell {
		t.Fatalf("Start info = %#v", info)
	}
	if strings.Contains(info.DisplayCommand, command) {
		t.Fatalf("shell script leaked through session metadata: %#v", info)
	}
	done, err := manager.Done(info.ID)
	if err != nil {
		t.Fatalf("Done returned an error: %v", err)
	}

	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	promptSeen := false
	latestOutput := ""
	for !promptSeen {
		select {
		case event := <-events:
			switch event := event.(type) {
			case OutputAvailable:
				if event.SessionID == info.ID {
					latestOutput, _ = manager.Output(info.ID)
				}
			case AdapterEvent:
				if event.Event.SessionID != info.ID {
					continue
				}
				if event.Event.Type == adapters.EventProcessExit {
					t.Fatalf("session exited before input: %#v", event.Event)
				}
				if event.Event.Actionable() {
					promptSeen = true
					if event.Event.Type != adapters.EventConfirmation ||
						event.Event.Metadata["pattern"] != "overwrite" ||
						!strings.Contains(event.Event.Match, "Overwrite? [Y/n]") {
						t.Fatalf("unexpected prompt event: %#v", event)
					}
				}
			case Error:
				if event.SessionID == info.ID {
					t.Fatalf("unexpected PTY error: %v", event.Err)
				}
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for prompt; output: %q", latestOutput)
		}
	}

	session, err := manager.session(info.ID)
	if err != nil {
		t.Fatalf("looking up internal session: %v", err)
	}
	if !session.processor.IsBlocked() {
		t.Fatal("processor is not blocked after AdapterEvent")
	}
	if err := manager.Resize(info.ID, 73, 19); err != nil {
		t.Fatalf("Resize returned an error: %v", err)
	}
	assertPTYSize(t, session, 73, 19)
	if err := manager.SendInput(info.ID, "yes"); err != nil {
		t.Fatalf("SendInput returned an error: %v", err)
	}
	if session.processor.IsBlocked() {
		t.Fatal("SendInput did not resolve the adapter event")
	}

	exited := false
	for !(exited && strings.Contains(latestOutput, "Done: yes") && strings.Contains(latestOutput, "19 73")) {
		select {
		case event := <-events:
			switch event := event.(type) {
			case OutputAvailable:
				if event.SessionID == info.ID {
					latestOutput, _ = manager.Output(info.ID)
				}
			case AdapterEvent:
				if event.Event.SessionID != info.ID || event.Event.Type != adapters.EventProcessExit {
					continue
				}
				// The lifecycle API promises Done before process_exit.
				select {
				case <-done:
				default:
					t.Fatal("Done is still open when process_exit was emitted")
				}
				latestOutput, _ = manager.Output(info.ID)
				_, waitErr, _, resultErr := manager.Result(info.ID)
				if resultErr != nil || waitErr != nil {
					t.Fatalf("session exit result = (%v, %v); output: %q", waitErr, resultErr, latestOutput)
				}
				exited = true
			case Error:
				if event.SessionID == info.ID {
					t.Fatalf("unexpected PTY error: %v", event.Err)
				}
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for final output and exit; output: %q", latestOutput)
		}
	}

	manager.Close()
	if err := manager.SendInput(info.ID, "late input"); !errors.Is(err, ErrClosed) {
		t.Fatalf("SendInput after Close returned %v, want %v", err, ErrClosed)
	}
	if _, err := manager.Start(agent.Spec{ID: "late", Name: "late", Command: []string{"true"}}, 40, 10); !errors.Is(err, ErrClosed) {
		t.Fatalf("Start after Close returned %v, want %v", err, ErrClosed)
	}
}

func TestManagerCloseKillsSignalIgnoringDescendants(t *testing.T) {
	events := make(chan Event, 64)
	manager, err := NewManager(context.Background(), events, integrationPatterns, 1024)
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}
	defer manager.Close()

	heartbeatPath := filepath.Join(t.TempDir(), "descendant-heartbeat")
	if err := syscall.Mkfifo(heartbeatPath, 0o600); err != nil {
		t.Fatalf("creating descendant heartbeat FIFO: %v", err)
	}
	heartbeat, err := os.OpenFile(heartbeatPath, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		t.Fatalf("opening descendant heartbeat FIFO: %v", err)
	}
	defer heartbeat.Close()

	command := `trap '' TERM HUP; (trap '' TERM HUP; exec 3>"$RELAYER_HEARTBEAT"; while :; do printf x >&3; sleep 0.01; done) & printf 'READY\n'; wait`
	info, err := manager.Start(agent.Spec{
		ID:    "stubborn-group",
		Name:  "stubborn group",
		Shell: command,
		Env:   map[string]string{"RELAYER_HEARTBEAT": heartbeatPath},
	}, 40, 10)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	done, _ := manager.Done(info.ID)

	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ready := false
	for !ready {
		select {
		case event := <-events:
			if output, ok := event.(OutputAvailable); ok && output.SessionID == info.ID {
				content, _ := manager.Output(info.ID)
				ready = strings.Contains(content, "READY")
			}
		case <-deadline.C:
			t.Fatal("timed out waiting for the stubborn child process")
		}
	}
	heartbeatByte := make([]byte, 1)
	for {
		count, readErr := heartbeat.Read(heartbeatByte)
		if count > 0 {
			break
		}
		if readErr != nil && !errors.Is(readErr, syscall.EAGAIN) && !errors.Is(readErr, io.EOF) {
			t.Fatalf("reading descendant heartbeat: %v", readErr)
		}
		select {
		case <-deadline.C:
			t.Fatal("descendant did not produce a heartbeat")
		case <-time.After(5 * time.Millisecond):
		}
	}

	started := time.Now()
	manager.Close()
	if elapsed := time.Since(started); elapsed > 3*time.Second {
		t.Fatalf("Close took %v", elapsed)
	}
	select {
	case <-done:
	default:
		t.Fatal("Done is still open after Close")
	}

	// A SIGKILLed orphan can remain in the process table as a zombie until PID 1
	// reaps it. The FIFO reaches EOF only after every writer has closed, which
	// proves that neither the descendant nor one of its children remains active.
	closedDeadline := time.NewTimer(time.Second)
	defer closedDeadline.Stop()
	for {
		count, readErr := heartbeat.Read(heartbeatByte)
		if count == 0 && (readErr == nil || errors.Is(readErr, io.EOF)) {
			break
		}
		if readErr != nil && !errors.Is(readErr, syscall.EAGAIN) {
			t.Fatalf("reading descendant heartbeat after Close: %v", readErr)
		}
		select {
		case <-closedDeadline.C:
			t.Fatal("signal-ignoring descendant retained its heartbeat writer after Close")
		case <-time.After(5 * time.Millisecond):
		}
	}
}

func TestConcurrentCloseUnblocksIOAndJoinsManagerGoroutines(t *testing.T) {
	events := make(chan Event, 128)
	manager, err := NewManager(context.Background(), events, integrationPatterns, 1024)
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}

	info, err := manager.Start(
		agent.Spec{
			ID:    "concurrent-shutdown",
			Name:  "concurrent shutdown",
			Shell: `trap '' TERM HUP; printf 'READY\n'; while :; do sleep 30; done`,
		},
		40,
		10,
	)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	done, _ := manager.Done(info.ID)

	// Give the reader goroutine an opportunity to enter its blocking PTY read.
	time.Sleep(25 * time.Millisecond)

	start := make(chan struct{})
	finished := make(chan struct{})
	var workers sync.WaitGroup
	for index := 0; index < 4; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			<-start
			manager.Close()
		}()
	}
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		_ = manager.SendInput(info.ID, strings.Repeat("x", 8<<20))
	}()
	workers.Add(1)
	go func() {
		defer workers.Done()
		<-start
		for index := 0; index < 100; index++ {
			if err := manager.Resize(info.ID, 40+index, 10+index); errors.Is(err, ErrClosed) {
				return
			}
		}
	}()
	close(start)
	go func() {
		workers.Wait()
		close(finished)
	}()

	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent Close/I/O did not finish")
	}
	select {
	case <-done:
	default:
		t.Fatal("session Done is still open after concurrent Close")
	}

	joined := make(chan struct{})
	go func() {
		manager.wg.Wait()
		close(joined)
	}()
	select {
	case <-joined:
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Manager.Close returned before its goroutines were joined")
	}
}

func TestManagerCoalescesOutputButNeverDropsPrompt(t *testing.T) {
	events := make(chan Event, 1)
	// Saturate the channel before output arrives. Output notifications are
	// intentionally best-effort, while a detected prompt must wait for room.
	events <- OutputAvailable{SessionID: "saturated"}
	manager, err := NewManager(context.Background(), events, integrationPatterns, 1024)
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}
	defer manager.Close()

	info, err := manager.Start(agent.Spec{
		ID:    "priority",
		Name:  "priority",
		Shell: `printf 'Overwrite? [Y/n]'; IFS= read -r answer`,
	}, 40, 10)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		output, outputErr := manager.Output(info.ID)
		if outputErr != nil {
			t.Fatalf("Output returned an error: %v", outputErr)
		}
		if strings.Contains(output, "Overwrite? [Y/n]") {
			break
		}
		time.Sleep(5 * time.Millisecond)
	}
	output, _ := manager.Output(info.ID)
	if !strings.Contains(output, "Overwrite? [Y/n]") {
		t.Fatalf("prompt was not consumed before deadline: %q", output)
	}

	queued := <-events
	placeholder, ok := queued.(OutputAvailable)
	if !ok || placeholder.SessionID != "saturated" {
		t.Fatalf("non-essential output displaced the saturated event: %#v", queued)
	}
	promptDeadline := time.NewTimer(time.Second)
	defer promptDeadline.Stop()
	for {
		select {
		case queued := <-events:
			switch event := queued.(type) {
			case OutputAvailable:
				// Once the placeholder is drained, an OnOutput call that was
				// already scheduled may legitimately publish a coalesced viewport
				// invalidation before the essential semantic event.
				if event.SessionID != info.ID {
					t.Fatalf("unexpected output notification before prompt: %#v", event)
				}
				continue
			case AdapterEvent:
				if event.Event.SessionID != info.ID ||
					event.Event.Metadata["pattern"] != "overwrite" {
					t.Fatalf("essential prompt event = %#v", queued)
				}
				goto promptReceived
			default:
				t.Fatalf("unexpected event before prompt: %#v", queued)
			}
		case <-promptDeadline.C:
			t.Fatal("essential prompt was dropped while the event channel was saturated")
		}
	}

promptReceived:
	if err := manager.SendInput(info.ID, "yes"); err != nil {
		t.Fatalf("SendInput returned an error: %v", err)
	}
}

func assertPTYSize(t *testing.T, session *processSession, wantColumns, wantRows int) {
	t.Helper()
	session.fileMu.RLock()
	defer session.fileMu.RUnlock()
	if session.master == nil {
		t.Fatal("session PTY is closed")
	}
	size, err := pty.GetsizeFull(session.master)
	if err != nil {
		t.Fatalf("reading PTY size: %v", err)
	}
	if gotColumns, gotRows := int(size.Cols), int(size.Rows); gotColumns != wantColumns || gotRows != wantRows {
		t.Fatalf("PTY size = %dx%d, want %dx%d", gotColumns, gotRows, wantColumns, wantRows)
	}
}
