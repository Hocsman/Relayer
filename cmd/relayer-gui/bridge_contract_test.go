package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
)

// The empty window of v0.8.9, v0.8.10 and v0.8.11 was a contract breach
// between the two sides of this bridge: Go sent an idle application's agents
// as null, and the interface read them as a list before its first render.
// Each side's tests passed against its own hand-written fixtures, so nothing
// exercised the payload one side actually produces against the code the other
// side actually runs.
//
// The golden file is that payload: the exact bytes GetState returns for a
// freshly launched, idle application. This test fails when Go changes what it
// emits; the interface's bridgeContract.test.ts consumes the same file
// through its real reducer and fails when the interface can no longer read
// what Go emits. Neither side is ever tested against a fixture the other side
// does not see.
//
// A deliberate payload change regenerates the files with:
//
//	go test ./cmd/relayer-gui -run 'BridgeStateContract' -update
var updateBridgeGolden = flag.Bool("update", false, "rewrite the bridge contract golden file")

func TestBridgeStateContract(t *testing.T) {
	state, err := NewApp().GetState()
	if err != nil {
		t.Fatalf("GetState on an idle application: %v", err)
	}
	assertBridgeGolden(t, "bridge-state.idle.golden.json", state)
}

// TestRunningBridgeStateContract pins the payload of a run in progress, the
// state an operator spends most of their time in: one agent waiting on a
// prompt, one running, one whose process has exited with a code, and the
// pending prompt itself. The idle payload has no agent and no prompt, so it
// could not catch a field of either that one side stopped sending or the
// other stopped reading.
func TestRunningBridgeStateContract(t *testing.T) {
	engine := newFakeDesktopEngine("Agent-A", "Agent-B", "Agent-C")
	application := newBridgeForTest(engine)
	application.handleAdapterEvent(bridgeEvent("Agent-A", "prompt-1"))
	exitCode := 3
	exit := adapters.NewProcessExitEvent("Agent-C", "Agent-C", "generic", 1, &exitCode, true)
	exit.Timestamp = time.Date(2026, time.August, 27, 10, 5, 0, 0, time.UTC)
	application.handleAdapterEvent(exit)

	state, err := application.GetState()
	if err != nil {
		t.Fatalf("GetState on a running application: %v", err)
	}
	// The start time is the moment the fake run began; it is the one field
	// that changes from run to run.
	if state.StartedAt == "" {
		t.Fatal("a running application sent no start time")
	}
	state.StartedAt = "2026-08-27T09:59:00Z"
	assertBridgeGolden(t, "bridge-state.running.golden.json", state)
}

func assertBridgeGolden(t *testing.T, name string, state AppState) {
	t.Helper()
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("the state does not marshal: %v", err)
	}
	payload = append(payload, '\n')

	golden := filepath.Join("frontend", "src", "state", "testdata", name)
	if *updateBridgeGolden {
		if err := os.WriteFile(golden, payload, 0o644); err != nil {
			t.Fatalf("rewrite the golden file: %v", err)
		}
	}
	committed, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("read the golden file: %v", err)
	}
	if string(committed) != string(payload) {
		t.Fatalf("the bridge payload changed; if the change is deliberate, regenerate with -update and review the interface side:\ncommitted:\n%s\nactual:\n%s", committed, payload)
	}
}
