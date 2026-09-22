package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/tmuxbackend"
	"github.com/gorilla/websocket"
)

func TestMain(m *testing.M) {
	if handled, exitCode := tmuxbackend.HelperMain(os.Args[1:], io.Discard); handled {
		os.Exit(exitCode)
	}
	os.Exit(m.Run())
}

// startServeForTest runs the gateway until the test ends, and waits for it to
// finish unwinding before the test's temporary directory is removed.
//
// Cancelling the context only asks the server to stop. It closes the audit
// journal while Serve returns, and Windows cannot delete a file another handle
// still holds, so t.TempDir's cleanup fails on a test that merely cancels.
// Every gateway test starts here so no call site can forget the wait.
func startServeForTest(t *testing.T, ctx context.Context, cancel context.CancelFunc, opts Options) <-chan error {
	t.Helper()

	serverErrCh := make(chan error, 1)
	served := make(chan struct{})
	go func() {
		defer close(served)
		serverErrCh <- Serve(ctx, opts)
	}()

	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Error("the gateway did not shut down within 10s")
		}
	})

	return serverErrCh
}

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

	serverErrCh := startServeForTest(t, ctx, cancel, opts)

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
	req.Close = true
	authResp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/state with Bearer: %v", err)
	}
	if authResp.StatusCode != http.StatusOK {
		authResp.Body.Close()
		t.Fatalf("GET /api/state with Bearer returned %d, want 200", authResp.StatusCode)
	}

	var initialState AppState
	if err := json.NewDecoder(authResp.Body).Decode(&initialState); err != nil {
		authResp.Body.Close()
		t.Fatalf("Decode AppState: %v", err)
	}
	authResp.Body.Close()
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

		// Read messages until we get the response matching reqID. A call that
		// restarts the agents stops every one of them first, and a stop is
		// allowed session.StopBudget — five of those seconds are the grace an
		// agent gets on Windows to handle its console closing.
		wsConn.SetReadDeadline(time.Now().Add(session.StopBudget + 10*time.Second))
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
		workerArgv = []string{"sh", "-c", "sleep 10"}
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
	_ = wsConn.Close()
	http.DefaultClient.CloseIdleConnections()
	cancel()
	select {
	case err := <-serverErrCh:
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("Serve returned error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve graceful shutdown timed out")
	}
	_ = loaded
}

func TestServerRPCMethods(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("APPDATA", tempDir)
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", tempDir)
	configPath := filepath.Join(tempDir, "config.yaml")
	if _, err := config.LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())

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

	_ = startServeForTest(t, ctx, cancel, opts)

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

func TestSaveFullSettingsNotificationsAndBroadcast(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("APPDATA", tempDir)
	t.Setenv("HOME", tempDir)
	t.Setenv("XDG_CONFIG_HOME", tempDir)
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

	serverErrCh := startServeForTest(t, ctx, cancel, opts)

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

	broadcastCh := make(chan wsEventMessage, 20)
	rpcRespCh := make(chan wsResponse, 20)

	// Reader pump to split broadcast events and RPC responses
	go func() {
		for {
			_, data, err := wsConn.ReadMessage()
			if err != nil {
				return
			}
			var ev wsEventMessage
			if err := json.Unmarshal(data, &ev); err == nil && ev.Event != "" {
				broadcastCh <- ev
				continue
			}
			var resp wsResponse
			if err := json.Unmarshal(data, &resp); err == nil && resp.ID != "" {
				rpcRespCh <- resp
				continue
			}
		}
	}()

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

		timeout := time.After(5 * time.Second)
		for {
			select {
			case resp := <-rpcRespCh:
				if resp.ID == reqID {
					if resp.Error != "" {
						return nil, fmt.Errorf("RPC error: %s", resp.Error)
					}
					return json.Marshal(resp.Result)
				}
			case <-timeout:
				return nil, errors.New("RPC timeout")
			}
		}
	}

	// 1. Get initial full settings
	rawSettings, err := callRPC("getFullSettings", map[string]any{})
	if err != nil {
		t.Fatalf("getFullSettings failed: %v", err)
	}
	var fullSettings FullSettingsView
	if err := json.Unmarshal(rawSettings, &fullSettings); err != nil {
		t.Fatalf("Unmarshal FullSettingsView: %v", err)
	}

	// 2. Save new notification settings
	newNotifSettings := NotificationSettings{
		Enabled:     true,
		Bell:        false,
		Desktop:     true,
		MinSeverity: "warning",
		Webhooks: []NotificationWebhookSetting{
			{
				Name:        "team-slack",
				URL:         "https://hooks.slack.com/services/T00/B00/XXXX",
				Format:      "slack",
				MinSeverity: "warning",
				Timeout:     "5s",
			},
		},
	}

	saveReq := SaveFullSettingsRequest{
		ExpectedRevision: fullSettings.Revision,
		Notifications:    &newNotifSettings,
	}

	rawUpdated, err := callRPC("saveFullSettings", map[string]any{"request": saveReq})
	if err != nil {
		t.Fatalf("saveFullSettings failed: %v", err)
	}
	var updatedSettings FullSettingsView
	if err := json.Unmarshal(rawUpdated, &updatedSettings); err != nil {
		t.Fatalf("Unmarshal updated FullSettingsView: %v", err)
	}

	if updatedSettings.Notifications.Bell != false {
		t.Errorf("Expected Bell to be false, got %v", updatedSettings.Notifications.Bell)
	}
	if len(updatedSettings.Notifications.Webhooks) != 1 || updatedSettings.Notifications.Webhooks[0].Name != "team-slack" {
		t.Fatalf("Expected 1 webhook 'team-slack', got %+v", updatedSettings.Notifications.Webhooks)
	}

	// 3. Verify disk persistence
	diskCfg, err := config.LoadExisting(configPath)
	if err != nil {
		t.Fatalf("LoadExisting from disk: %v", err)
	}
	if diskCfg.Notifications.Bell != false {
		t.Errorf("Disk YAML Bell = true, want false")
	}
	if len(diskCfg.Notifications.Webhooks) != 1 || diskCfg.Notifications.Webhooks[0].Name != "team-slack" {
		t.Fatalf("Disk YAML webhooks mismatch: %+v", diskCfg.Notifications.Webhooks)
	}

	// 4. Test Notification RPC
	rawTest, err := callRPC("testNotification", map[string]any{})
	if err != nil {
		t.Fatalf("testNotification RPC failed: %v", err)
	}
	var testResult map[string]any
	if err := json.Unmarshal(rawTest, &testResult); err != nil || testResult["ok"] != true {
		t.Fatalf("testNotification result mismatch: %s", rawTest)
	}

	// 5. Verify broadcast event was received over WebSocket
	var notifEvent NotificationEvent
	found := false
	timeout := time.After(5 * time.Second)
	for !found {
		select {
		case ev := <-broadcastCh:
			if ev.Event == "relayer:notification" {
				payloadBytes, _ := json.Marshal(ev.Payload)
				if err := json.Unmarshal(payloadBytes, &notifEvent); err != nil {
					t.Fatalf("Unmarshal NotificationEvent: %v", err)
				}
				found = true
			}
		case <-timeout:
			t.Fatal("Timeout waiting for relayer:notification broadcast event")
		}
	}

	if notifEvent.Title != "Relayer Test Notification" {
		t.Errorf("Unexpected notification title: %s", notifEvent.Title)
	}
	if notifEvent.Severity != "info" {
		t.Errorf("Unexpected notification severity: %s", notifEvent.Severity)
	}

	// 6. Graceful shutdown
	_ = wsConn.Close()
	http.DefaultClient.CloseIdleConnections()
	cancel()
	select {
	case err := <-serverErrCh:
		if err != nil && !strings.Contains(err.Error(), "context canceled") {
			t.Fatalf("Serve returned error on shutdown: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("Serve graceful shutdown timed out")
	}
}

func TestRBACAuthenticationAndPermissions(t *testing.T) {
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
		Token:       "alice:secretOp123",
		ViewerToken: "bob:secretView456",
		ConfigPath:  configPath,
		Diagnostics: io.Discard,
		OnReady: func(serverURL, token string) {
			readyCh <- serverURL
		},
	}

	serverErrCh := startServeForTest(t, ctx, cancel, opts)

	var baseURL string
	select {
	case baseURL = <-readyCh:
	case err := <-serverErrCh:
		t.Fatalf("Serve: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Server startup timeout")
	}

	// 1. Check unauthorized requests
	respNoAuth, err := http.Get(baseURL + "/api/state")
	if err != nil {
		t.Fatalf("GET /api/state: %v", err)
	}
	defer respNoAuth.Body.Close()
	if respNoAuth.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/state without token returned %d, want 401", respNoAuth.StatusCode)
	}

	respBadToken, err := http.Get(baseURL + "/api/state?token=wrongsecret")
	if err != nil {
		t.Fatalf("GET /api/state: %v", err)
	}
	defer respBadToken.Body.Close()
	if respBadToken.StatusCode != http.StatusUnauthorized {
		t.Fatalf("GET /api/state with bad token returned %d, want 401", respBadToken.StatusCode)
	}

	// 2. Viewer access
	respViewer, err := http.Get(baseURL + "/api/state?token=secretView456")
	if err != nil {
		t.Fatalf("GET /api/state with viewer token: %v", err)
	}
	defer respViewer.Body.Close()
	if respViewer.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/state with viewer token returned %d, want 200", respViewer.StatusCode)
	}

	// 3. Viewer WebSocket RPCs
	wsViewerURL := strings.Replace(baseURL, "http://", "ws://", 1) + "/api/ws?token=secretView456"
	wsViewer, _, err := websocket.DefaultDialer.Dial(wsViewerURL, nil)
	if err != nil {
		t.Fatalf("Dial viewer WebSocket: %v", err)
	}
	defer wsViewer.Close()

	callViewerRPC := func(method string, params any) (json.RawMessage, error) {
		paramsRaw, _ := json.Marshal(params)
		reqID := fmt.Sprintf("req-%d", time.Now().UnixNano())
		if err := wsViewer.WriteJSON(wsRequest{ID: reqID, Method: method, Params: paramsRaw}); err != nil {
			return nil, err
		}
		for {
			_, data, err := wsViewer.ReadMessage()
			if err != nil {
				return nil, err
			}
			var resp wsResponse
			if err := json.Unmarshal(data, &resp); err == nil && resp.ID == reqID {
				if resp.Error != "" {
					return nil, errors.New(resp.Error)
				}
				raw, _ := json.Marshal(resp.Result)
				return raw, nil
			}
		}
	}

	// 3a. getUserInfo for Viewer
	userRaw, err := callViewerRPC("getUserInfo", map[string]any{})
	if err != nil {
		t.Fatalf("getUserInfo failed: %v", err)
	}
	var userInfo UserInfo
	if err := json.Unmarshal(userRaw, &userInfo); err != nil {
		t.Fatalf("unmarshal userInfo: %v", err)
	}
	if userInfo.Identity != "bob" || userInfo.Role != "viewer" || !userInfo.ReadOnly {
		t.Fatalf("unexpected viewer userInfo: %+v", userInfo)
	}

	// 3b. Read-only methods allowed for Viewer
	if _, err := callViewerRPC("getState", map[string]any{}); err != nil {
		t.Fatalf("Viewer getState failed: %v", err)
	}
	if _, err := callViewerRPC("getTelemetrySnapshot", map[string]any{}); err != nil {
		t.Fatalf("Viewer getTelemetrySnapshot failed: %v", err)
	}

	// 3c. resizeSession is a silent no-op for Viewer (err == nil)
	if _, err := callViewerRPC("resizeSession", map[string]any{"columns": 100, "rows": 40}); err != nil {
		t.Fatalf("Viewer resizeSession returned error, want silent no-op: %v", err)
	}

	// 3d. Mutating methods rejected with permission denied for Viewer
	mutatingTests := []struct {
		method string
		params map[string]any
	}{
		{"submitDecision", map[string]any{"runID": "test", "sessionID": "s", "eventID": "e", "value": "allow"}},
		{"submitLine", map[string]any{"runID": "test", "sessionID": "s", "line": "echo 1"}},
		{"sendTerminalInput", map[string]any{"runID": "test", "sessionID": "s", "data": "ls"}},
		{"setInteractiveSession", map[string]any{"runID": "test", "sessionID": "s", "active": true}},
		{"stopSession", map[string]any{"runID": "test", "sessionID": "s"}},
		{"startSession", map[string]any{"runID": "test", "sessionID": "s"}},
		{"restartSession", map[string]any{"runID": "test", "sessionID": "s"}},
		{"saveFullSettings", map[string]any{"runID": "test"}},
		{"testNotification", map[string]any{}},
		{"stopRun", map[string]any{"runID": "test"}},
		// Present in isMutatingMethod from the start but never asserted here.
		{"submitAutomaticDecision", map[string]any{"runID": "test", "sessionID": "s", "eventID": "e", "decision": "allow"}},
		{"saveAgentProfiles", map[string]any{"runID": "test"}},
		{"saveAgentProfilesAndRestart", map[string]any{"expectedRunID": "test"}},
		// Session sharing: a viewer may watch a terminal and may never hold it.
		{"requestControl", map[string]any{"sessionID": "s"}},
		{"grantControl", map[string]any{"sessionID": "s", "toConnID": "c"}},
		{"declineControl", map[string]any{"sessionID": "s", "toConnID": "c"}},
		{"releaseControl", map[string]any{"sessionID": "s"}},
		{"forceTakeControl", map[string]any{"sessionID": "s"}},
		// Session recording: a viewer may list and replay, never destroy.
		{"deleteRecording", map[string]any{"id": "r"}},
		// Explicit viewer allowlist: exporting raw recordings and full settings (webhooks) are forbidden
		{"exportRecording", map[string]any{"id": "r"}},
		{"getFullSettings", map[string]any{}},
		// Any unlisted or unknown method is denied by default for viewer
		{"nonExistentOrUnlistedMethod", map[string]any{}},
	}

	for _, tt := range mutatingTests {
		_, err := callViewerRPC(tt.method, tt.params)
		if err == nil {
			t.Errorf("Viewer calling mutating/restricted method %s succeeded, want permission denied", tt.method)
		} else if !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("Viewer calling %s error = %v, want 'permission denied'", tt.method, err)
		}
	}

	// Reading a roster, reading recordings, and inspecting agent profiles are allowed: observing is
	// the whole point of the viewer role, so refusing them would make the role
	// useless rather than safe.
	readableTests := []struct {
		method string
		params map[string]any
	}{
		{"listPresence", map[string]any{"sessionID": "s"}},
		{"observeSession", map[string]any{"sessionID": "s", "observing": true}},
		{"listRecordings", map[string]any{}},
		{"getAgentProfiles", map[string]any{}},
	}
	for _, tt := range readableTests {
		if _, err := callViewerRPC(tt.method, tt.params); err != nil &&
			strings.Contains(err.Error(), "permission denied") {
			t.Errorf("Viewer calling read-only method %s was denied", tt.method)
		}
	}

	// 4. Operator WebSocket RPCs
	wsOpURL := strings.Replace(baseURL, "http://", "ws://", 1) + "/api/ws?token=secretOp123"
	wsOp, _, err := websocket.DefaultDialer.Dial(wsOpURL, nil)
	if err != nil {
		t.Fatalf("Dial operator WebSocket: %v", err)
	}
	defer wsOp.Close()

	callOpRPC := func(method string, params any) (json.RawMessage, error) {
		paramsRaw, _ := json.Marshal(params)
		reqID := fmt.Sprintf("req-%d", time.Now().UnixNano())
		if err := wsOp.WriteJSON(wsRequest{ID: reqID, Method: method, Params: paramsRaw}); err != nil {
			return nil, err
		}
		for {
			_, data, err := wsOp.ReadMessage()
			if err != nil {
				return nil, err
			}
			var resp wsResponse
			if err := json.Unmarshal(data, &resp); err == nil && resp.ID == reqID {
				if resp.Error != "" {
					return nil, errors.New(resp.Error)
				}
				raw, _ := json.Marshal(resp.Result)
				return raw, nil
			}
		}
	}

	userOpRaw, err := callOpRPC("getUserInfo", map[string]any{})
	if err != nil {
		t.Fatalf("Operator getUserInfo failed: %v", err)
	}
	var userOpInfo UserInfo
	if err := json.Unmarshal(userOpRaw, &userOpInfo); err != nil {
		t.Fatalf("unmarshal userOpInfo: %v", err)
	}
	if userOpInfo.Identity != "alice" || userOpInfo.Role != "operator" || userOpInfo.ReadOnly {
		t.Fatalf("unexpected operator userInfo: %+v", userOpInfo)
	}
}

func TestRBACOperatorAuditAttribution(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("APPDATA", tempDir)
	t.Setenv("HOME", tempDir)
	t.Setenv("USERPROFILE", tempDir)
	t.Setenv("XDG_CONFIG_HOME", tempDir)
	configPath := filepath.Join(tempDir, "config.yaml")

	if _, err := config.LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	ctrl, err := NewController(configPath, io.Discard)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start controller: %v", err)
	}
	defer func() {
		shutdownCtx, sCancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer sCancel()
		_ = ctrl.Close(shutdownCtx)
	}()

	state := ctrl.GetState()
	if len(state.Agents) > 0 {
		agent := state.Agents[0]
		_ = ctrl.SubmitLineWithOperator(state.RunID, agent.SessionID, "echo hello", "alice")
	}

	entries, err := ctrl.GetAuditEntries(AuditFilterInput{Limit: 10})
	if err != nil {
		t.Fatalf("GetAuditEntries: %v", err)
	}

	foundAlice := false
	for _, e := range entries {
		if e.Operator == "alice" {
			foundAlice = true
			break
		}
	}
	if !foundAlice {
		t.Error("Did not find audit entry attributed to operator 'alice'")
	}
}

func TestInteractivePTYWebAndAudit(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("APPDATA", tempDir)
	t.Setenv("HOME", tempDir)
	t.Setenv("USERPROFILE", tempDir)
	t.Setenv("XDG_CONFIG_HOME", tempDir)
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
		Token:       "alice:secretOpInteractive",
		ViewerToken: "bob:secretViewInteractive",
		ConfigPath:  configPath,
		Diagnostics: io.Discard,
		OnReady: func(serverURL, token string) {
			readyCh <- serverURL
		},
	}

	serverErrCh := startServeForTest(t, ctx, cancel, opts)

	var baseURL string
	select {
	case baseURL = <-readyCh:
	case err := <-serverErrCh:
		t.Fatalf("Serve failed: %v", err)
	case <-time.After(5 * time.Second):
		t.Fatal("Server startup timed out")
	}

	// Connect Operator WebSocket
	wsOpURL := strings.Replace(baseURL, "http://", "ws://", 1) + "/api/ws?token=secretOpInteractive"
	wsOp, _, err := websocket.DefaultDialer.Dial(wsOpURL, nil)
	if err != nil {
		t.Fatalf("Dial operator WebSocket: %v", err)
	}
	defer wsOp.Close()

	// Connect Viewer WebSocket
	wsViewURL := strings.Replace(baseURL, "http://", "ws://", 1) + "/api/ws?token=secretViewInteractive"
	wsView, _, err := websocket.DefaultDialer.Dial(wsViewURL, nil)
	if err != nil {
		t.Fatalf("Dial viewer WebSocket: %v", err)
	}
	defer wsView.Close()

	callOpRPC := func(method string, params any) (json.RawMessage, error) {
		paramsRaw, _ := json.Marshal(params)
		reqID := fmt.Sprintf("req-%d", time.Now().UnixNano())
		if err := wsOp.WriteJSON(wsRequest{ID: reqID, Method: method, Params: paramsRaw}); err != nil {
			return nil, err
		}
		for {
			_, data, err := wsOp.ReadMessage()
			if err != nil {
				return nil, err
			}
			var resp wsResponse
			if err := json.Unmarshal(data, &resp); err == nil && resp.ID == reqID {
				if resp.Error != "" {
					return nil, errors.New(resp.Error)
				}
				raw, _ := json.Marshal(resp.Result)
				return raw, nil
			}
		}
	}

	// 1. Get state to find sessionID
	stateRaw, err := callOpRPC("getState", map[string]any{})
	if err != nil {
		t.Fatalf("getState failed: %v", err)
	}
	var state AppState
	if err := json.Unmarshal(stateRaw, &state); err != nil {
		t.Fatalf("unmarshal state: %v", err)
	}
	if len(state.Agents) == 0 {
		t.Fatal("expected at least one agent in state")
	}
	sessionID := state.Agents[0].SessionID

	// 2. Operator attaches interactively: setInteractiveSession -> true
	_, err = callOpRPC("setInteractiveSession", map[string]any{
		"sessionID": sessionID,
		"active":    true,
	})
	if err != nil {
		t.Fatalf("setInteractiveSession true failed: %v", err)
	}

	// 3. Operator sends input via RPC
	_, err = callOpRPC("sendTerminalInput", map[string]any{
		"sessionID": sessionID,
		"data":      "echo test\n",
	})
	if err != nil {
		t.Fatalf("sendTerminalInput RPC failed: %v", err)
	}

	// 4. Operator sends binary terminal input over WebSocket
	// Protocol: [1 byte: len(sessionID)][N bytes: sessionID][VT payload]
	binaryPayload := make([]byte, 0, 1+len(sessionID)+4)
	binaryPayload = append(binaryPayload, byte(len(sessionID)))
	binaryPayload = append(binaryPayload, []byte(sessionID)...)
	binaryPayload = append(binaryPayload, []byte("pwd\n")...)
	if err := wsOp.WriteMessage(websocket.BinaryMessage, binaryPayload); err != nil {
		t.Fatalf("write binary terminal input: %v", err)
	}

	// 5. Viewer attempts binary terminal input (must be silently ignored, not crash)
	if err := wsView.WriteMessage(websocket.BinaryMessage, binaryPayload); err != nil {
		t.Fatalf("viewer write binary terminal input: %v", err)
	}

	// 6. Operator detaches interactively: setInteractiveSession -> false
	_, err = callOpRPC("setInteractiveSession", map[string]any{
		"sessionID": sessionID,
		"active":    false,
	})
	if err != nil {
		t.Fatalf("setInteractiveSession false failed: %v", err)
	}

	// 7. Verify audit journal entries for attach_started and attach_finished
	auditRaw, err := callOpRPC("getAuditEntries", AuditFilterInput{Limit: 20})
	if err != nil {
		t.Fatalf("getAuditEntries failed: %v", err)
	}
	var entries []AuditEntryView
	if err := json.Unmarshal(auditRaw, &entries); err != nil {
		t.Fatalf("unmarshal audit entries: %v", err)
	}

	var foundStarted, foundFinished bool
	for _, e := range entries {
		if e.Kind == "attach_started" && e.Operator == "alice" {
			foundStarted = true
		}
		if e.Kind == "attach_finished" && e.Operator == "alice" {
			foundFinished = true
		}
	}
	if !foundStarted {
		t.Error("expected audit entry for attach_started by alice")
	}
	if !foundFinished {
		t.Error("expected audit entry for attach_finished by alice")
	}
}

func TestGatewayOriginAndLoopbackProtection(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	if _, err := config.LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct {
		serverURL string
		token     string
	}, 1)

	opts := Options{
		Bind:        "127.0.0.1",
		Port:        0,
		Token:       "op-secret-123",
		ConfigPath:  configPath,
		Diagnostics: io.Discard,
		OnReady: func(serverURL string, token string) {
			readyCh <- struct {
				serverURL string
				token     string
			}{serverURL: serverURL, token: token}
		},
	}

	_ = startServeForTest(t, ctx, cancel, opts)

	var readyInfo struct {
		serverURL string
		token     string
	}
	select {
	case readyInfo = <-readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server to become ready")
	}

	client := &http.Client{Timeout: 3 * time.Second}

	// 1. /api/state with evil Origin must be rejected with 403 Forbidden
	reqEvil, _ := http.NewRequest("GET", readyInfo.serverURL+"/api/state", nil)
	reqEvil.Header.Set("Origin", "http://evil.com")
	reqEvil.Header.Set("Authorization", "Bearer op-secret-123")
	respEvil, err := client.Do(reqEvil)
	if err != nil {
		t.Fatalf("GET /api/state with evil origin: %v", err)
	}
	_ = respEvil.Body.Close()
	if respEvil.StatusCode != http.StatusForbidden {
		t.Errorf("GET /api/state with evil origin status = %d, want %d", respEvil.StatusCode, http.StatusForbidden)
	}

	// 2. /api/state without Origin (CLI / non-browser) succeeds
	reqCLI, _ := http.NewRequest("GET", readyInfo.serverURL+"/api/state", nil)
	reqCLI.Header.Set("Authorization", "Bearer op-secret-123")
	respCLI, err := client.Do(reqCLI)
	if err != nil {
		t.Fatalf("GET /api/state without origin: %v", err)
	}
	_ = respCLI.Body.Close()
	if respCLI.StatusCode != http.StatusOK {
		t.Errorf("GET /api/state without origin status = %d, want %d", respCLI.StatusCode, http.StatusOK)
	}

	// 3. /api/state from another loopback port is a different origin. Anything
	// else listening locally (a dev server, a dashboard) must not be able to
	// drive the gateway just because it is also on localhost.
	reqLocal, _ := http.NewRequest("GET", readyInfo.serverURL+"/api/state", nil)
	reqLocal.Header.Set("Origin", "http://localhost:3000")
	reqLocal.Header.Set("Authorization", "Bearer op-secret-123")
	respLocal, err := client.Do(reqLocal)
	if err != nil {
		t.Fatalf("GET /api/state with another local origin: %v", err)
	}
	_ = respLocal.Body.Close()
	if respLocal.StatusCode != http.StatusForbidden {
		t.Errorf("GET /api/state from another loopback port status = %d, want %d", respLocal.StatusCode, http.StatusForbidden)
	}

	// 4. WebSocket dial with evil Origin must be rejected (403 Forbidden)
	wsURL := strings.Replace(readyInfo.serverURL, "http://", "ws://", 1) + "/api/ws?token=op-secret-123"
	evilDialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 3 * time.Second,
	}
	evilHeader := make(http.Header)
	evilHeader.Set("Origin", "http://evil.com")
	_, wsResp, err := evilDialer.Dial(wsURL, evilHeader)
	if err == nil {
		t.Error("WebSocket dial with evil origin succeeded, want failure")
	}
	if wsResp != nil && wsResp.StatusCode != http.StatusForbidden {
		t.Errorf("WebSocket dial with evil origin returned status = %d, want %d", wsResp.StatusCode, http.StatusForbidden)
	}

	// 5. WebSocket dial without Origin succeeds
	normalDialer := websocket.Dialer{
		Proxy:            http.ProxyFromEnvironment,
		HandshakeTimeout: 3 * time.Second,
	}
	wsConn, _, err := normalDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("WebSocket dial without origin failed: %v", err)
	}
	_ = wsConn.Close()

	// 6. WebSocket dial from another loopback port is refused; the same origin
	// the UI is served from is accepted.
	localHeader := make(http.Header)
	localHeader.Set("Origin", "http://localhost:5173")
	if conn, resp, err := normalDialer.Dial(wsURL, localHeader); err == nil {
		_ = conn.Close()
		t.Error("WebSocket dial from another loopback port succeeded, want failure")
	} else if resp != nil && resp.StatusCode != http.StatusForbidden {
		t.Errorf("WebSocket dial from another loopback port status = %d, want %d", resp.StatusCode, http.StatusForbidden)
	}
	sameHeader := make(http.Header)
	sameHeader.Set("Origin", readyInfo.serverURL)
	wsSameConn, _, err := normalDialer.Dial(wsURL, sameHeader)
	if err != nil {
		t.Fatalf("WebSocket dial with the same origin failed: %v", err)
	}
	_ = wsSameConn.Close()
}

func TestAnonymousLocalRejectsCrossOrigin(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	if _, err := config.LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	readyCh := make(chan struct {
		serverURL string
		token     string
	}, 1)

	// No tokens specified -> allowAnonymousLocal is active on loopback
	opts := Options{
		Bind:        "127.0.0.1",
		Port:        0,
		ConfigPath:  configPath,
		Diagnostics: io.Discard,
		OnReady: func(serverURL string, token string) {
			readyCh <- struct {
				serverURL string
				token     string
			}{serverURL: serverURL, token: token}
		},
	}

	_ = startServeForTest(t, ctx, cancel, opts)

	var readyInfo struct {
		serverURL string
		token     string
	}
	select {
	case readyInfo = <-readyCh:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for server to become ready")
	}

	client := &http.Client{Timeout: 3 * time.Second}

	// Malicious webpage on evil.com tries to query local /api/state anonymously
	reqEvil, _ := http.NewRequest("GET", readyInfo.serverURL+"/api/state", nil)
	reqEvil.Header.Set("Origin", "http://evil.com")
	respEvil, err := client.Do(reqEvil)
	if err != nil {
		t.Fatalf("GET /api/state with evil origin: %v", err)
	}
	_ = respEvil.Body.Close()
	if respEvil.StatusCode != http.StatusForbidden {
		t.Errorf("Anonymous /api/state with evil origin status = %d, want %d", respEvil.StatusCode, http.StatusForbidden)
	}

	// Normal local browser with loopback Origin succeeds
	reqLocal, _ := http.NewRequest("GET", readyInfo.serverURL+"/api/state", nil)
	reqLocal.Header.Set("Origin", readyInfo.serverURL)
	respLocal, err := client.Do(reqLocal)
	if err != nil {
		t.Fatalf("GET /api/state with same origin: %v", err)
	}
	_ = respLocal.Body.Close()
	if respLocal.StatusCode != http.StatusOK {
		t.Errorf("Anonymous /api/state with same origin status = %d, want %d", respLocal.StatusCode, http.StatusOK)
	}
}
