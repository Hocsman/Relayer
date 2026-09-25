package server

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
)

// Stopping relayer serve ends the run while its clients are still connected:
// a terminal a client held is let go with the run, and the journal says so.
// Serve used to shut its HTTP server down first, which closed every WebSocket,
// so each held terminal was journaled as let go by a disconnect before the
// run's end had even begun.
func TestStoppingServeLetsAHeldTerminalGoWithTheRun(t *testing.T) {
	configPath, auditPath, _ := writeWebRunConfig(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ready := make(chan string, 1)
	served := make(chan error, 1)
	go func() {
		served <- Serve(ctx, Options{
			Bind:        "127.0.0.1",
			Port:        0,
			Token:       "alice:opsecret",
			ConfigPath:  configPath,
			Diagnostics: io.Discard,
			OnReady:     func(url, _ string) { ready <- url },
		})
	}()
	var baseURL string
	select {
	case baseURL = <-ready:
	case err := <-served:
		t.Fatalf("Serve failed during startup: %v", err)
	case <-time.After(30 * time.Second):
		t.Fatal("the gateway did not start")
	}

	alice := dialSharedGateway(t, baseURL, "opsecret")
	var state AppState
	deadline := time.Now().Add(30 * time.Second)
	for {
		alice.mustCall("getState", map[string]any{}, &state)
		running := false
		for _, agent := range state.Agents {
			running = running || (agent.SessionID == "web-listen" && agent.Running)
		}
		if running || time.Now().After(deadline) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	alice.mustCall("setInteractiveSession", map[string]any{"runID": state.RunID, "sessionID": "web-listen", "active": true}, nil)

	cancel()
	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(60 * time.Second):
		t.Fatal("the gateway did not shut down")
	}

	raw, err := os.ReadFile(auditPath)
	if err != nil {
		t.Fatalf("read the journal: %v", err)
	}
	var released []string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry audit.Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			t.Fatalf("decode a journal line: %v", err)
		}
		if entry.Kind == audit.KindControlReleased && entry.SessionID == "web-listen" {
			released = append(released, entry.Reason)
		}
	}
	if len(released) != 1 || released[0] != "control_released_run_end" {
		t.Fatalf("the held terminal was let go as %v, want once, with the run's end", released)
	}
}
