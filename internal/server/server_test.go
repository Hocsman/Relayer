package server

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/config"
	"github.com/gorilla/websocket"
)

func TestServerLifecycleAndAgentAddition(t *testing.T) {
	// Create temporary configuration directory and file
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")

	// Create valid initial configuration
	loaded, err := config.LoadOrCreate(configPath)
	if err != nil {
		t.Fatalf("LoadOrCreate initial config: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct {
		serverURL string
		token     string
	}, 1)

	testToken := "test-secret-token-12345"

	opts := Options{
		Bind:        "127.0.0.1",
		Port:        0, // allocate ephemeral port
		Token:       testToken,
		ConfigPath:  configPath,
		Diagnostics: io.Discard,
		OnReady: func(serverURL string, token string) {
			readyCh <- struct {
				serverURL string
				token     string
			}{serverURL: serverURL, token: token}
		},
	}

	serverErrCh := make(chan error, 1)
	go func() {
		serverErrCh <- Serve(ctx, opts)
	}()

	var readyInfo struct {
		serverURL string
		token     string
	}
	select {
	case readyInfo = <-readyCh:
	case err := <-serverErrCh:
		t.Fatalf("Serve failed during startup: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Server startup timed out")
	}

	baseURL := readyInfo.serverURL

	// 1. Health check (no auth required)
	resp, err := http.Get(baseURL + "/api/health")
	if err != nil {
		t.Fatalf("GET /api/health: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/health returned status %d, want 200", resp.StatusCode)
	}

	// 2. Static HTML check (serves embedded SPA frontend)
	respIndex, err := http.Get(baseURL + "/")
	if err != nil {
		t.Fatalf("GET /: %v", err)
	}
	defer respIndex.Body.Close()
	if respIndex.StatusCode != http.StatusOK {
		t.Fatalf("GET / returned status %d, want 200", respIndex.StatusCode)
	}
	indexBody, _ := io.ReadAll(respIndex.Body)
	if !strings.Contains(string(indexBody), "<div id=\"root\">") {
		t.Errorf("GET / body does not contain root div: %s", string(indexBody))
	}

	// 3. Auth verification on HTTP /api/state
	// 3a. Unauthorized request (no token)
	unauthResp, err := http.Get(baseURL + "/api/state")
	if err != nil {
		t.Fatalf("GET /api/state: %v", err)
	}
	unauthResp.Body.Close()
	if unauthResp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/state without token returned %d, want 401", unauthResp.StatusCode)
	}

	// 3b. Authorized request with Bearer token
	req, err := http.NewRequest("GET", baseURL+"/api/state", nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+testToken)
	authResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/state with Bearer: %v", err)
	}
	defer authResp.Body.Close()
	if authResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/state with Bearer returned %d, want 200", authResp.StatusCode)
	}

	var initialState AppState
	if err := json.NewDecoder(authResp.Body).Decode(&initialState); err != nil {
		t.Fatalf("Decode AppState: %v", err)
	}
	if initialState.RunID == "" {
		t.Fatal("Expected non-empty RunID in initial AppState")
	}

	// 4. WebSocket connection
	// 4a. Connect without token should fail
	wsURLWithoutToken := strings.Replace(baseURL, "http://", "ws://", 1) + "/api/ws"
	_, _, err = websocket.DefaultDialer.Dial(wsURLWithoutToken, nil)
	if err == nil {
		t.Fatal("Expected WebSocket connection without token to fail")
	}

	// 4b. Connect with ?token= query parameter should succeed
	wsURLWithToken := fmt.Sprintf("%s?token=%s", wsURLWithoutToken, url.QueryEscape(testToken))
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURLWithToken, nil)
	if err != nil {
		t.Fatalf("Dial WebSocket with token: %v", err)
	}
	defer wsConn.Close()

	// Helper for sending RPC and waiting for response
	callRPC := func(method string, params any) (json.RawMessage, error) {
		paramsRaw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		reqID := fmt.Sprintf("rpc-%d", time.Now().UnixNano())
		reqMsg := wsRequest{
			ID:     reqID,
			Method: method,
			Params: paramsRaw,
		}
		if err := wsConn.WriteJSON(reqMsg); err != nil {
			return nil, err
		}

		// Read messages until we get the response matching reqID
		wsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, data, err := wsConn.ReadMessage()
			if err != nil {
				return nil, err
			}
			var resp wsResponse
			if err := json.Unmarshal(data, &resp); err == nil && resp.ID == reqID {
				if resp.Error != "" {
					return nil, fmt.Errorf("RPC error: %s", resp.Error)
				}
				resultBytes, _ := json.Marshal(resp.Result)
				return resultBytes, nil
			}
			// Might be a broadcast event, continue reading
		}
	}

	// 5. Query profiles via WebSocket RPC
	rawProfiles, err := callRPC("getAgentProfiles", map[string]any{})
	if err != nil {
		t.Fatalf("getAgentProfiles RPC failed: %v", err)
	}
	var profilesView AgentProfilesView
	if err := json.Unmarshal(rawProfiles, &profilesView); err != nil {
		t.Fatalf("Unmarshal AgentProfilesView: %v", err)
	}
	if len(profilesView.Catalog) == 0 {
		t.Fatal("Expected non-empty agent catalog")
	}
	initialAgentCount := len(profilesView.Profiles)

	// 6. Test adding a new agent (verifying user request: "Vérifie stp aussi l'ajout de l'agent")
	// We add a new agent profile to the existing profiles and call saveAgentProfilesAndRestart
	workerArgv := []string{"cmd.exe", "/c", "echo worker"}
	if runtime.GOOS != "windows" {
		workerArgv = []string{"sh", "-c", "echo worker"}
	}
	newAgentID := "agent-new-worker"
	newProfile := AgentProfileInput{
		ID:             newAgentID,
		Name:           "New Worker Agent",
		PresetID:       "custom",
		Cwd:            tempDir,
		Backend:        "auto",
		Adapter:        "auto",
		Argv:           workerArgv,
		PreserveOnSave: false,
	}

	var updatedProfiles []AgentProfileInput
	for _, p := range profilesView.Profiles {
		updatedProfiles = append(updatedProfiles, AgentProfileInput{
			ID:             p.ID,
			Name:           p.Name,
			PresetID:       p.PresetID,
			Cwd:            p.Cwd,
			Backend:        p.Backend,
			Adapter:        p.Adapter,
			Argv:           p.Argv,
			PreserveOnSave: p.PreserveOnSave,
		})
	}
	updatedProfiles = append(updatedProfiles, newProfile)

	restartReq := SaveAgentProfilesAndRestartRequest{
		ExpectedRevision: profilesView.Revision,
		ExpectedRunID:    initialState.RunID,
		Profiles:         updatedProfiles,
	}

	rawLifecycle, err := callRPC("saveAgentProfilesAndRestart", restartReq)
	if err != nil {
		t.Fatalf("saveAgentProfilesAndRestart RPC failed: %v", err)
	}
	var lifecycle LifecycleResult
	if err := json.Unmarshal(rawLifecycle, &lifecycle); err != nil {
		t.Fatalf("Unmarshal LifecycleResult: %v", err)
	}

	if lifecycle.Outcome != "restarted" && lifecycle.Outcome != "started" {
		t.Fatalf("Unexpected lifecycle outcome: %q, want 'restarted'", lifecycle.Outcome)
	}

	// Verify that the new agent is in the updated profiles
	foundInProfiles := false
	for _, p := range lifecycle.Profiles.Profiles {
		if p.ID == newAgentID {
			foundInProfiles = true
			if p.Name != "New Worker Agent" {
				t.Errorf("Profile name = %q, want 'New Worker Agent'", p.Name)
			}
			break
		}
	}
	if !foundInProfiles {
		t.Fatalf("New agent %q not found in lifecycle profiles (count=%d, previously %d)",
			newAgentID, len(lifecycle.Profiles.Profiles), initialAgentCount)
	}

	// Verify that the new agent is in the updated AppState
	foundInAppState := false
	for _, a := range lifecycle.State.Agents {
		if a.AgentID == newAgentID {
			foundInAppState = true
			break
		}
	}
	if !foundInAppState {
		t.Fatalf("New agent %q not found in lifecycle AppState.Agents", newAgentID)
	}

	// 7. Verify config persistence on disk
	reloadedCfg, err := config.LoadExisting(configPath)
	if err != nil {
		t.Fatalf("Load reloaded config from disk: %v", err)
	}
	foundOnDisk := false
	for _, agentSpec := range reloadedCfg.Agents {
		if agentSpec.ID == newAgentID {
			foundOnDisk = true
			if agentSpec.Name != "New Worker Agent" {
				t.Errorf("Disk agent spec name = %q, want 'New Worker Agent'", agentSpec.Name)
			}
			break
		}
	}
	if !foundOnDisk {
		t.Fatalf("New agent %q was not persisted to YAML on disk: %v", newAgentID, configPath)
	}

	// 8. Graceful shutdown
	cancel()
	select {
	case err := <-serverErrCh:
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("Serve returned error on shutdown: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Serve graceful shutdown timed out")
	}
	_ = loaded
}

func TestServerRPCMethods(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	if _, err := config.LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan string, 1)
	opts := Options{
		Bind:        "127.0.0.1",
		Port:        0,
		ConfigPath:  configPath,
		Diagnostics: io.Discard,
		OnReady: func(serverURL string, _ string) {
			readyCh <- serverURL
		},
	}

	go func() {
		_ = Serve(ctx, opts)
	}()

	var baseURL string
	select {
	case baseURL = <-readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("Server startup timed out")
	}

	wsURL := strings.Replace(baseURL, "http://", "ws://", 1) + "/api/ws"
	wsConn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("Dial WebSocket: %v", err)
	}
	defer wsConn.Close()

	callRPC := func(method string, params any) (json.RawMessage, error) {
		paramsRaw, err := json.Marshal(params)
		if err != nil {
			return nil, err
		}
		reqID := fmt.Sprintf("rpc-%d", time.Now().UnixNano())
		reqMsg := wsRequest{
			ID:     reqID,
			Method: method,
			Params: paramsRaw,
		}
		if err := wsConn.WriteJSON(reqMsg); err != nil {
			return nil, err
		}

		wsConn.SetReadDeadline(time.Now().Add(5 * time.Second))
		for {
			_, data, err := wsConn.ReadMessage()
			if err != nil {
				return nil, err
			}
			var resp wsResponse
			if err := json.Unmarshal(data, &resp); err == nil && resp.ID == reqID {
				if resp.Error != "" {
					return nil, fmt.Errorf("RPC error: %s", resp.Error)
				}
				resultBytes, _ := json.Marshal(resp.Result)
				return resultBytes, nil
			}
		}
	}

	// 1. getState
	rawState, err := callRPC("getState", map[string]any{})
	if err != nil {
		t.Fatalf("getState RPC failed: %v", err)
	}
	var state AppState
	if err := json.Unmarshal(rawState, &state); err != nil {
		t.Fatalf("Unmarshal AppState: %v", err)
	}
	if state.RunID == "" {
		t.Error("Expected non-empty RunID")
	}

	// 2. getFullSettings
	rawSettings, err := callRPC("getFullSettings", map[string]any{})
	if err != nil {
		t.Fatalf("getFullSettings RPC failed: %v", err)
	}
	var settings FullSettingsView
	if err := json.Unmarshal(rawSettings, &settings); err != nil {
		t.Fatalf("Unmarshal FullSettingsView: %v", err)
	}
	if settings.Revision == "" {
		t.Error("Expected non-empty settings Revision")
	}

	// 3. getTelemetrySnapshot
	rawTelemetry, err := callRPC("getTelemetrySnapshot", map[string]any{})
	if err != nil {
		t.Fatalf("getTelemetrySnapshot RPC failed: %v", err)
	}
	var telemetry TelemetrySnapshotView
	if err := json.Unmarshal(rawTelemetry, &telemetry); err != nil {
		t.Fatalf("Unmarshal TelemetrySnapshotView: %v", err)
	}
	if len(telemetry.DecisionDurations) == 0 {
		t.Error("Expected default decision duration buckets")
	}

	// 4. getAuditSummary
	rawAudit, err := callRPC("getAuditSummary", map[string]any{})
	if err != nil {
		t.Fatalf("getAuditSummary RPC failed: %v", err)
	}
	var auditSummary AuditSummaryView
	if err := json.Unmarshal(rawAudit, &auditSummary); err != nil {
		t.Fatalf("Unmarshal AuditSummaryView: %v", err)
	}

	// 5. verifyAuditJournal
	rawVerify, err := callRPC("verifyAuditJournal", map[string]any{})
	if err != nil {
		t.Fatalf("verifyAuditJournal RPC failed: %v", err)
	}
	var verifyView AuditVerificationView
	if err := json.Unmarshal(rawVerify, &verifyView); err != nil {
		t.Fatalf("Unmarshal AuditVerificationView: %v", err)
	}

	// 6. exportAuditReport
	rawExport, err := callRPC("exportAuditReport", map[string]string{"format": "csv"})
	if err != nil {
		t.Fatalf("exportAuditReport RPC failed: %v", err)
	}
	var exportStr string
	if err := json.Unmarshal(rawExport, &exportStr); err != nil {
		t.Fatalf("Unmarshal exported audit report: %v", err)
	}
	if !strings.Contains(exportStr, "sequence,timestamp") {
		t.Errorf("Unexpected CSV export content: %s", exportStr)
	}

	// 7. runPreflight
	rawPreflight, err := callRPC("runPreflight", map[string]any{})
	if err != nil {
		t.Fatalf("runPreflight RPC failed: %v", err)
	}
	var preflight PreflightReport
	if err := json.Unmarshal(rawPreflight, &preflight); err != nil {
		t.Fatalf("Unmarshal PreflightReport: %v", err)
	}
	if preflight.Status == "" {
		t.Error("Expected non-empty preflight status")
	}
}

func TestCLIServeParsing(t *testing.T) {
	// Positional arguments rejected
	err := RunServe([]string{"extra-arg"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("Expected error for positional arguments to relayer serve")
	}

	// Unknown flag rejected
	err = RunServe([]string{"--nonexistent-flag"}, io.Discard, io.Discard)
	if err == nil {
		t.Fatal("Expected error for unknown flag to relayer serve")
	}
}

