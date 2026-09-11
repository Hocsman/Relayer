package ptybackend

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

// TestHelperProcessFloodOutput is invoked as a subprocess to emit a high-volume stream
// of ANSI-formatted logs through the PTY.
func TestHelperProcessFloodOutput(t *testing.T) {
	if os.Getenv("GO_WANT_HELPER_PROCESS_FLOOD") != "1" {
		return
	}
	defer os.Exit(0)

	lines := 30000
	for i := 1; i <= lines; i++ {
		fmt.Printf("\x1b[32m[STRESS-LOG]\x1b[0m line %05d - high throughput PTY test payload data padding 1234567890\r\n", i)
	}
	fmt.Println("[STRESS-LOG] COMPLETED_FLOOD_STREAM")
}

func TestPTYHighThroughputEndurance(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping endurance test in short mode")
	}

	events := make(chan session.Event, 256)
	ringCap := 8192
	manager, err := New(context.Background(), events, nil, ringCap)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	})

	// Measure baseline memory
	runtime.GC()
	var memBefore runtime.MemStats
	runtime.ReadMemStats(&memBefore)

	// Spawn helper process
	spec := agent.Spec{
		ID:      "stress-session-1",
		Name:    "Stress Worker",
		Command: []string{os.Args[0], "-test.run=TestHelperProcessFloodOutput"},
		Env:     map[string]string{"GO_WANT_HELPER_PROCESS_FLOOD": "1"},
		Backend: agent.BackendPTY,
	}

	info, err := manager.Start(context.Background(), spec, terminal.Size{Columns: 80, Rows: 24})
	if err != nil {
		t.Fatalf("manager.Start: %v", err)
	}

	// Concurrently poll snapshots and perform a resize during the flood
	stopPolling := make(chan struct{})
	var pollErrors []error
	var pollMu sync.Mutex
	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		ticker := time.NewTicker(15 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopPolling:
				return
			case <-ticker.C:
				_, err := manager.Snapshot(context.Background(), info.ID)
				if err != nil && !strings.Contains(err.Error(), "closed") {
					pollMu.Lock()
					pollErrors = append(pollErrors, err)
					pollMu.Unlock()
				}
			}
		}
	}()

	// Perform a concurrent resize during streaming
	time.Sleep(50 * time.Millisecond)
	_ = manager.Resize(context.Background(), info.ID, terminal.Size{Columns: 100, Rows: 30})

	// Wait for completion marker in output
	deadline := time.Now().Add(15 * time.Second)
	completed := false
	var lastSnapshot terminal.Snapshot
	for time.Now().Before(deadline) {
		lastSnapshot, err = manager.Snapshot(context.Background(), info.ID)
		if err == nil && strings.Contains(lastSnapshot.Output, "COMPLETED_FLOOD_STREAM") {
			completed = true
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	close(stopPolling)
	wg.Wait()

	if !completed {
		t.Fatalf("flood output did not complete within deadline; last snapshot tail: %q", lastSnapshot.Output)
	}

	if len(pollErrors) > 0 {
		t.Fatalf("errors occurred during concurrent snapshot polling: %v", pollErrors[0])
	}

	// Verify memory remains bounded: after GC, heap allocation delta must not balloon
	runtime.GC()
	var memAfter runtime.MemStats
	runtime.ReadMemStats(&memAfter)

	// In megabytes
	var heapDeltaMB int64
	if memAfter.HeapAlloc > memBefore.HeapAlloc {
		heapDeltaMB = int64(memAfter.HeapAlloc-memBefore.HeapAlloc) / (1024 * 1024)
	}
	t.Logf("Memory stats: before HeapAlloc=%d MB, after HeapAlloc=%d MB, delta=%d MB",
		memBefore.HeapAlloc/(1024*1024), memAfter.HeapAlloc/(1024*1024), heapDeltaMB)

	// The delta should remain small (< 25 MB) even after 30k lines because of bounded ring buffers
	if heapDeltaMB > 25 {
		t.Errorf("potential memory leak: heap grew by %d MB during high-throughput stream", heapDeltaMB)
	}

	// Clean shutdown within timeout
	closeCtx, cancelClose := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancelClose()
	if err := manager.Close(closeCtx); err != nil {
		t.Errorf("manager.Close failed: %v", err)
	}
}

func TestPTYConcurrentSessionsStress(t *testing.T) {
	if testing.Short() {
		t.Skip("skipping concurrent stress test in short mode")
	}

	events := make(chan session.Event, 512)
	manager, err := New(context.Background(), events, nil, 4096)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	})

	sessionCount := 3
	var sessionIDs []terminal.SessionID

	for i := 1; i <= sessionCount; i++ {
		spec := agent.Spec{
			ID:      fmt.Sprintf("concurrent-stress-%d", i),
			Name:    fmt.Sprintf("Concurrent Worker %d", i),
			Command: []string{os.Args[0], "-test.run=TestHelperProcessFloodOutput"},
			Env:     map[string]string{"GO_WANT_HELPER_PROCESS_FLOOD": "1"},
			Backend: agent.BackendPTY,
		}
		info, err := manager.Start(context.Background(), spec, terminal.Size{Columns: 60, Rows: 20})
		if err != nil {
			t.Fatalf("session %d start failed: %v", i, err)
		}
		sessionIDs = append(sessionIDs, info.ID)
	}

	// Poll all sessions concurrently until all complete
	deadline := time.Now().Add(20 * time.Second)
	completed := make(map[terminal.SessionID]bool)

	for time.Now().Before(deadline) && len(completed) < sessionCount {
		for _, id := range sessionIDs {
			if completed[id] {
				continue
			}
			snap, err := manager.Snapshot(context.Background(), id)
			if err == nil && strings.Contains(snap.Output, "COMPLETED_FLOOD_STREAM") {
				completed[id] = true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}

	if len(completed) != sessionCount {
		t.Fatalf("only %d/%d concurrent sessions completed within deadline", len(completed), sessionCount)
	}
}
