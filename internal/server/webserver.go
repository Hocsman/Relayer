package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
)

// UserRole defines the privilege level of an authenticated client.
type UserRole string

const (
	RoleOperator UserRole = "operator"
	RoleViewer   UserRole = "viewer"
)

type AuthIdentity struct {
	Identity string
	Role     UserRole
}

func parseTokens(input string, role UserRole) map[string]AuthIdentity {
	result := make(map[string]AuthIdentity)
	if strings.TrimSpace(input) == "" {
		return result
	}
	parts := strings.Split(input, ",")
	for _, part := range parts {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		if idx := strings.Index(part, ":"); idx > 0 {
			username := strings.TrimSpace(part[:idx])
			secret := strings.TrimSpace(part[idx+1:])
			if secret != "" {
				result[secret] = AuthIdentity{
					Identity: username,
					Role:     role,
				}
			}
		} else {
			result[part] = AuthIdentity{
				Identity: string(role),
				Role:     role,
			}
		}
	}
	return result
}

// Options configures the headless Relayer web server.
type Options struct {
	Bind        string
	Port        int
	Token       string
	ViewerToken string
	ConfigPath  string
	StaticDir   string
	Diagnostics io.Writer
	OnReady     func(serverURL string, token string)
}

type wsRequest struct {
	ID     string          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params"`
}

type wsResponse struct {
	ID     string `json:"id"`
	Result any    `json:"result,omitempty"`
	Error  string `json:"error,omitempty"`
}

type wsEventMessage struct {
	Event   string `json:"event"`
	Payload any    `json:"payload"`
}

type clientConnection struct {
	ws       *websocket.Conn
	send     chan []byte
	identity string
	role     UserRole
	closed   atomic.Bool
	mu       sync.Mutex
}

func (c *clientConnection) safeSend(msg []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Load() {
		return
	}
	select {
	case c.send <- msg:
	default:
	}
}

func (c *clientConnection) close() {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed.Swap(true) {
		return
	}
	_ = c.ws.Close()
	close(c.send)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return true // Allowed for headless gateway
	},
	ReadBufferSize:  1024 * 64,
	WriteBufferSize: 1024 * 64,
}

// Serve initializes and runs the Relayer Web Gateway until context cancellation.
func Serve(ctx context.Context, opts Options) error {
	if opts.Port < 0 {
		opts.Port = 8080
	}
	if strings.TrimSpace(opts.Bind) == "" {
		opts.Bind = "127.0.0.1"
	}
	if opts.Diagnostics == nil {
		opts.Diagnostics = os.Stderr
	}

	opTokens := parseTokens(opts.Token, RoleOperator)
	viewTokens := parseTokens(opts.ViewerToken, RoleViewer)

	tokens := make(map[string]AuthIdentity)
	for k, v := range opTokens {
		tokens[k] = v
	}
	for k, v := range viewTokens {
		tokens[k] = v
	}

	allowAnonymousLocal := false
	var displayOpToken, displayViewToken string

	for k, v := range opTokens {
		if v.Role == RoleOperator {
			displayOpToken = k
			break
		}
	}
	for k, v := range viewTokens {
		if v.Role == RoleViewer {
			displayViewToken = k
			break
		}
	}

	if len(tokens) == 0 {
		if opts.Bind != "127.0.0.1" && opts.Bind != "localhost" {
			randomBytesOp := make([]byte, 16)
			_, _ = rand.Read(randomBytesOp)
			displayOpToken = hex.EncodeToString(randomBytesOp)
			tokens[displayOpToken] = AuthIdentity{Identity: "operator", Role: RoleOperator}

			randomBytesView := make([]byte, 16)
			_, _ = rand.Read(randomBytesView)
			displayViewToken = hex.EncodeToString(randomBytesView)
			tokens[displayViewToken] = AuthIdentity{Identity: "viewer", Role: RoleViewer}

			_, _ = fmt.Fprintf(opts.Diagnostics, "Generated security tokens for %s (Operator & Viewer)\n", opts.Bind)
		} else {
			allowAnonymousLocal = true
		}
	}

	ctrl, err := NewController(opts.ConfigPath, opts.Diagnostics)
	if err != nil {
		return fmt.Errorf("creating supervisor controller: %w", err)
	}

	if err := ctrl.Start(ctx); err != nil {
		return fmt.Errorf("starting supervisor controller: %w", err)
	}
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = ctrl.Close(shutdownCtx)
	}()

	addr := fmt.Sprintf("%s:%d", opts.Bind, opts.Port)
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("binding server to %s: %w", addr, err)
	}
	defer listener.Close()

	if tcpAddr, ok := listener.Addr().(*net.TCPAddr); ok {
		opts.Port = tcpAddr.Port
	}

	handler := newGatewayHandler(ctrl, tokens, allowAnonymousLocal, opts.StaticDir, opts.Diagnostics)
	httpServer := &http.Server{
		Handler:      handler,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 30 * time.Second,
	}
	httpServer.RegisterOnShutdown(handler.closeClients)

	displayHost := opts.Bind
	if displayHost == "0.0.0.0" {
		displayHost = "127.0.0.1"
	}

	_, _ = fmt.Fprintln(opts.Diagnostics, "")
	_, _ = fmt.Fprintln(opts.Diagnostics, "=======================================================")
	_, _ = fmt.Fprintln(opts.Diagnostics, "  🚀 Relayer Web Gateway is active")
	if displayOpToken != "" {
		_, _ = fmt.Fprintf(opts.Diagnostics, "  Operator URL:   http://%s:%d/?token=%s\n", displayHost, opts.Port, displayOpToken)
		_, _ = fmt.Fprintf(opts.Diagnostics, "  Operator Token: %s\n", displayOpToken)
	} else if allowAnonymousLocal {
		_, _ = fmt.Fprintf(opts.Diagnostics, "  Operator URL:   http://%s:%d/\n", displayHost, opts.Port)
	}
	if displayViewToken != "" {
		_, _ = fmt.Fprintf(opts.Diagnostics, "  Viewer URL:     http://%s:%d/?token=%s\n", displayHost, opts.Port, displayViewToken)
		_, _ = fmt.Fprintf(opts.Diagnostics, "  Viewer Token:   %s\n", displayViewToken)
	}
	_, _ = fmt.Fprintf(opts.Diagnostics, "  Config: %s\n", ctrl.configPath)
	_, _ = fmt.Fprintln(opts.Diagnostics, "=======================================================")
	_, _ = fmt.Fprintln(opts.Diagnostics, "")

	if opts.OnReady != nil {
		serverURL := fmt.Sprintf("http://%s:%d", displayHost, opts.Port)
		opts.OnReady(serverURL, displayOpToken)
	}

	serverErrChan := make(chan error, 1)
	go func() {
		if err := httpServer.Serve(listener); err != nil && !errors.Is(err, http.ErrServerClosed) {
			serverErrChan <- err
		}
		close(serverErrChan)
	}()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpServer.Shutdown(shutdownCtx)
	case err := <-serverErrChan:
		return err
	}
}

type gatewayHandler struct {
	ctrl                *Controller
	tokens              map[string]AuthIdentity
	allowAnonymousLocal bool
	staticDir           string
	diagnostics         io.Writer

	clientsMu sync.RWMutex
	clients   map[*clientConnection]struct{}
}

func newGatewayHandler(ctrl *Controller, tokens map[string]AuthIdentity, allowAnonymousLocal bool, staticDir string, diagnostics io.Writer) *gatewayHandler {
	gh := &gatewayHandler{
		ctrl:                ctrl,
		tokens:              tokens,
		allowAnonymousLocal: allowAnonymousLocal,
		staticDir:           staticDir,
		diagnostics:         diagnostics,
		clients:             make(map[*clientConnection]struct{}),
	}

	// Subscribe to controller events and broadcast to all connected WebSocket clients
	ctrl.Subscribe(func(event string, payload any) {
		msgBytes, err := json.Marshal(wsEventMessage{
			Event:   event,
			Payload: payload,
		})
		if err != nil {
			return
		}

		gh.clientsMu.RLock()
		defer gh.clientsMu.RUnlock()
		for client := range gh.clients {
			client.safeSend(msgBytes)
		}
	})

	return gh
}

func (gh *gatewayHandler) closeClients() {
	gh.clientsMu.Lock()
	defer gh.clientsMu.Unlock()
	for client := range gh.clients {
		client.close()
	}
}

func (gh *gatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Health endpoint
	if r.URL.Path == "/api/health" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}

	// State endpoint
	if r.URL.Path == "/api/state" {
		if !gh.isAuthorized(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(gh.ctrl.GetState())
		return
	}

	// WebSocket endpoint
	if r.URL.Path == "/api/ws" {
		identity, ok := gh.authenticate(r)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		gh.handleWebSocket(w, r, identity)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}

	// Static SPA file serving
	gh.serveStatic(w, r)
}

func (gh *gatewayHandler) authenticate(r *http.Request) (AuthIdentity, bool) {
	// 1. Check query parameter `?token=...`
	if qToken := r.URL.Query().Get("token"); qToken != "" {
		for secret, id := range gh.tokens {
			if subtle.ConstantTimeCompare([]byte(qToken), []byte(secret)) == 1 {
				return id, true
			}
		}
		return AuthIdentity{}, false
	}

	// 2. Check Authorization header `Bearer ...`
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		for secret, id := range gh.tokens {
			if subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1 {
				return id, true
			}
		}
		return AuthIdentity{}, false
	}

	// 3. Fallback for anonymous localhost if allowed
	if gh.allowAnonymousLocal && len(gh.tokens) == 0 {
		return AuthIdentity{
			Identity: "local-operator",
			Role:     RoleOperator,
		}, true
	}

	return AuthIdentity{}, false
}

func (gh *gatewayHandler) isAuthorized(r *http.Request) bool {
	_, ok := gh.authenticate(r)
	return ok
}

func (gh *gatewayHandler) handleWebSocket(w http.ResponseWriter, r *http.Request, identity AuthIdentity) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		_, _ = fmt.Fprintf(gh.diagnostics, "websocket upgrade error: %v\n", err)
		return
	}

	client := &clientConnection{
		ws:       conn,
		send:     make(chan []byte, 256),
		identity: identity.Identity,
		role:     identity.Role,
	}

	gh.clientsMu.Lock()
	gh.clients[client] = struct{}{}
	gh.clientsMu.Unlock()

	defer func() {
		gh.clientsMu.Lock()
		delete(gh.clients, client)
		gh.clientsMu.Unlock()
		client.close()
	}()

	// Write pump
	go func() {
		for msg := range client.send {
			_ = conn.SetWriteDeadline(time.Now().Add(10 * time.Second))
			if err := conn.WriteMessage(websocket.TextMessage, msg); err != nil {
				return
			}
		}
	}()

	// Read pump & RPC dispatcher
	conn.SetReadLimit(1024 * 1024)
	for {
		_, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}

		var req wsRequest
		if err := json.Unmarshal(msg, &req); err != nil {
			continue
		}

		go gh.dispatchRPC(client, req)
	}
}

func (gh *gatewayHandler) dispatchRPC(client *clientConnection, req wsRequest) {
	result, err := gh.executeMethod(client, req.Method, req.Params)

	resp := wsResponse{ID: req.ID}
	if err != nil {
		resp.Error = err.Error()
	} else {
		resp.Result = result
	}

	respBytes, err := json.Marshal(resp)
	if err == nil {
		client.safeSend(respBytes)
	}
}

func (gh *gatewayHandler) executeMethod(client *clientConnection, method string, params json.RawMessage) (any, error) {
	if client.role == RoleViewer && isMutatingMethod(method) {
		if method == "resizeSession" {
			// Silent no-op for viewer to avoid disruptive PTY resizes without UI errors
			return nil, nil
		}
		return nil, errors.New("permission denied: viewer role is read-only")
	}

	switch method {
	case "getUserInfo":
		return UserInfo{
			Identity: client.identity,
			Role:     string(client.role),
			ReadOnly: client.role == RoleViewer,
		}, nil

	case "getState":
		return gh.ctrl.GetState(), nil

	case "runPreflight":
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		return gh.ctrl.RunPreflight(ctx)

	case "submitDecision":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
			EventID   string `json:"eventID"`
			Value     string `json:"value"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, gh.ctrl.SubmitDecisionWithOperator(p.RunID, p.SessionID, p.EventID, p.Value, client.identity)

	case "submitAutomaticDecision":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
			EventID   string `json:"eventID"`
			Decision  string `json:"decision"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, gh.ctrl.SubmitDecisionWithOperator(p.RunID, p.SessionID, p.EventID, p.Decision, client.identity)

	case "submitLine":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
			Line      string `json:"line"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, gh.ctrl.SubmitLineWithOperator(p.RunID, p.SessionID, p.Line, client.identity)

	case "resizeSession":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
			Columns   int    `json:"columns"`
			Rows      int    `json:"rows"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, gh.ctrl.ResizeSession(p.RunID, p.SessionID, p.Columns, p.Rows)

	case "stopSession":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, gh.ctrl.StopSession(p.RunID, p.SessionID)

	case "startSession":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, gh.ctrl.StartSession(p.RunID, p.SessionID)

	case "restartSession":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, gh.ctrl.RestartSession(p.RunID, p.SessionID)

	case "getAgentProfiles":
		return gh.ctrl.GetAgentProfiles()

	case "saveAgentProfiles":
		var p struct {
			RunID   string                   `json:"runID"`
			Request SaveAgentProfilesRequest `json:"request"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return gh.ctrl.SaveAgentProfiles(p.RunID, p.Request)

	case "saveAgentProfilesAndRestart":
		var req SaveAgentProfilesAndRestartRequest
		if err := json.Unmarshal(params, &req); err != nil {
			return nil, err
		}
		return gh.ctrl.SaveAgentProfilesAndRestart(req)

	case "getFullSettings":
		return gh.ctrl.GetFullSettings()

	case "saveFullSettings":
		var p struct {
			RunID   string                  `json:"runID"`
			Request SaveFullSettingsRequest `json:"request"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return gh.ctrl.SaveFullSettings(p.RunID, p.Request)

	case "testNotification":
		return map[string]any{"ok": true}, gh.ctrl.TestNotification()

	case "stopRun":
		var p struct {
			RunID string `json:"runID"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return gh.ctrl.StopRun(p.RunID)

	case "getAuditSummary":
		return gh.ctrl.GetAuditSummary()

	case "getAuditEntries":
		var filter AuditFilterInput
		_ = json.Unmarshal(params, &filter)
		return gh.ctrl.GetAuditEntries(filter)

	case "verifyAuditJournal":
		return gh.ctrl.VerifyAuditJournal()

	case "exportAuditReport":
		var p struct {
			Format string `json:"format"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Format == "" {
			p.Format = "json"
		}
		return gh.ctrl.ExportAuditReport(p.Format)

	case "getTelemetrySnapshot":
		return gh.ctrl.GetTelemetrySnapshot(), nil

	default:
		return nil, fmt.Errorf("unknown method %q", method)
	}
}

func isMutatingMethod(method string) bool {
	switch method {
	case "submitDecision",
		"submitAutomaticDecision",
		"submitLine",
		"resizeSession",
		"stopSession",
		"startSession",
		"restartSession",
		"saveAgentProfiles",
		"saveAgentProfilesAndRestart",
		"saveFullSettings",
		"testNotification",
		"stopRun":
		return true
	default:
		return false
	}
}

func (gh *gatewayHandler) serveStatic(w http.ResponseWriter, r *http.Request) {
	// If explicit staticDir is given, serve from disk
	if gh.staticDir != "" {
		http.FileServer(http.Dir(gh.staticDir)).ServeHTTP(w, r)
		return
	}

	// Serve embedded assets
	subFs, err := fs.Sub(DistAssets, "dist")
	if err != nil {
		http.Error(w, "Asset bundle unavailable", http.StatusInternalServerError)
		return
	}

	cleanPath := strings.TrimPrefix(path.Clean(r.URL.Path), "/")
	if cleanPath == "" || cleanPath == "." {
		cleanPath = "index.html"
	}

	file, err := subFs.Open(cleanPath)
	if err != nil {
		// SPA fallback: return index.html for unrecognized routes
		indexFile, indexErr := subFs.Open("index.html")
		if indexErr != nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<!DOCTYPE html><html><head><title>Relayer Web Gateway</title></head><body style="background:#080c14;color:#fff;font-family:sans-serif;padding:40px;text-align:center"><h1>Relayer Web Gateway</h1><p>Frontend assets are ready. Run <code>npm run build</code> in <code>cmd/relayer-gui/frontend</code> to embed the full visual dashboard.</p></body></html>`))
			return
		}
		defer indexFile.Close()
		stat, _ := indexFile.Stat()
		http.ServeContent(w, r, "index.html", stat.ModTime(), indexFile.(io.ReadSeeker))
		return
	}
	defer file.Close()

	stat, _ := file.Stat()
	http.ServeContent(w, r, cleanPath, stat.ModTime(), file.(io.ReadSeeker))
}
