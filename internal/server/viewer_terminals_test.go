package server

import (
	"encoding/json"
	"io"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// servedWithHiddenTerminals serves the run to an operator and a viewer, with
// terminals hidden from viewers as --viewer-terminals hidden does.
func servedWithHiddenTerminals(g *webGateway) string {
	g.t.Helper()
	handler := newGatewayHandler(g.ctrl, map[string]AuthIdentity{
		"opAlice":  {Identity: "alice", Role: RoleOperator},
		"viewDave": {Identity: "dave", Role: RoleViewer},
	}, false, 0, "", io.Discard, true)
	server := httptest.NewServer(handler)
	g.t.Cleanup(func() {
		handler.closeClients()
		server.Close()
	})
	return server.URL
}

// TestAViewerSeesNoTerminalWhenTerminalsAreHidden: the agent prints a token.
// The operator's snapshots carry it; the viewer's carry the agent's state and
// prompt card but none of its output, in the snapshots and in getState, and a
// recording's contents are refused to it.
func TestAViewerSeesNoTerminalWhenTerminalsAreHidden(t *testing.T) {
	g := startWebRun(t, webRun{
		recording: true,
		agents:    []webAgent{{id: "tooling", mode: webAgentToolCall, adapter: "generic"}},
	})
	baseURL := servedWithHiddenTerminals(g)
	viewer := dialSharedGateway(t, baseURL, "viewDave")
	operator := dialSharedGateway(t, baseURL, "opAlice")

	var info UserInfo
	viewer.mustCall("getUserInfo", map[string]any{}, &info)
	if !info.ReadOnly || !info.TerminalsHidden {
		t.Fatalf("viewer info = %+v, want read-only with terminals hidden", info)
	}
	var operatorInfo UserInfo
	operator.mustCall("getUserInfo", map[string]any{}, &operatorInfo)
	if operatorInfo.TerminalsHidden {
		t.Fatalf("operator info = %+v, want terminals shown", operatorInfo)
	}

	// The operator sees the token, so the agent has printed it.
	deadline := time.After(30 * time.Second)
	for seen := false; !seen; {
		select {
		case event, ok := <-operator.events:
			if !ok {
				t.Fatal("the operator's socket closed")
			}
			if payload, _ := json.Marshal(event.Payload); event.Event == eventSnapshot && strings.Contains(string(payload), webAgentToken) {
				seen = true
			}
		case <-deadline:
			t.Fatal("the operator never saw the agent's output")
		}
	}
	g.awaitPending("tooling", 10*time.Second)

	snapshots := 0
	for drained := false; !drained; {
		select {
		case event, ok := <-viewer.events:
			if !ok {
				drained = true
				break
			}
			payload, _ := json.Marshal(event.Payload)
			if strings.Contains(string(payload), webAgentToken) {
				t.Fatalf("the viewer received the agent's output in %s: %s", event.Event, payload)
			}
			if event.Event == eventSnapshot {
				snapshots++
			}
		case <-time.After(500 * time.Millisecond):
			drained = true
		}
	}
	if snapshots == 0 {
		t.Fatal("the viewer received no snapshot at all; it should still follow the agent's state")
	}

	var state AppState
	viewer.mustCall("getState", map[string]any{}, &state)
	if len(state.Agents) != 1 || state.Agents[0].Output != "" || state.Agents[0].Status == "" {
		t.Fatalf("viewer state agents = %+v, want the agent's state without its output", state.Agents)
	}
	if len(state.PendingEvents) != 1 {
		t.Fatalf("viewer pending = %+v, want the prompt card", state.PendingEvents)
	}
	operator.mustCall("getState", map[string]any{}, &state)
	if !strings.Contains(state.Agents[0].Output, webAgentToken) {
		t.Fatalf("the operator's state lost the agent's output: %q", state.Agents[0].Output)
	}

	var recordings []RecordingView
	operator.mustCall("listRecordings", map[string]any{}, &recordings)
	if len(recordings) == 0 {
		t.Fatal("no recording to read")
	}
	err := viewer.call("readRecordingChunk", map[string]any{"id": recordings[0].ID, "offset": 0, "limit": 4096}, nil)
	if err == nil || !strings.Contains(err.Error(), errViewerTerminals.Error()) {
		t.Fatalf("a viewer's readRecordingChunk = %v, want %v", err, errViewerTerminals)
	}
}

func TestViewerTerminalsAcceptsOnlyShownOrHidden(t *testing.T) {
	for value, hidden := range map[string]bool{"": false, "shown": false, "Hidden": true, " hidden ": true} {
		got, err := parseViewerTerminals(value)
		if err != nil || got != hidden {
			t.Errorf("parseViewerTerminals(%q) = %v, %v; want %v", value, got, err, hidden)
		}
	}
	if _, err := parseViewerTerminals("masked"); err == nil {
		t.Error("parseViewerTerminals accepted masked")
	}
}
