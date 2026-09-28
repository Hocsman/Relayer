package main

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"
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
// A deliberate payload change regenerates the file with:
//
//	go test ./cmd/relayer-gui -run TestBridgeStateContract -update
var updateBridgeGolden = flag.Bool("update", false, "rewrite the bridge contract golden file")

func TestBridgeStateContract(t *testing.T) {
	state, err := NewApp().GetState()
	if err != nil {
		t.Fatalf("GetState on an idle application: %v", err)
	}
	payload, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		t.Fatalf("the state does not marshal: %v", err)
	}
	payload = append(payload, '\n')

	golden := filepath.Join("frontend", "src", "state", "testdata", "bridge-state.idle.golden.json")
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
