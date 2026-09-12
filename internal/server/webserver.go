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

// Options configures the headless Relayer web server.
type Options struct {
	Bind        string
	Port        int
	Token       string
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
	ws     *websocket.Conn
	send   chan []byte
	closed atomic.Bool
	mu     sync.Mutex
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

	token := strings.TrimSpace(opts.Token)
	if token == "" && opts.Bind != "127.0.0.1" && opts.Bind != "localhost" {
		randomBytes := make([]byte, 16)
		_, _ = rand.Read(randomBytes)
		token = hex.EncodeToString(randomBytes)
		_, _ = fmt.Fprintf(opts.Diagnostics, "Generated security token for %s: %s\n", opts.Bind, token)
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

	handler := newGatewayHandler(ctrl, token, opts.StaticDir, opts.Diagnostics)
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
	if token != "" {
		_, _ = fmt.Fprintf(opts.Diagnostics, "  URL:   http://%s:%d/?token=%s\n", displayHost, opts.Port, token)
		_, _ = fmt.Fprintf(opts.Diagnostics, "  Token: %s\n", token)
	} else {
		_, _ = fmt.Fprintf(opts.Diagnostics, "  URL:   http://%s:%d/\n", displayHost, opts.Port)
	}
	_, _ = fmt.Fprintf(opts.Diagnostics, "  Config: %s\n", ctrl.configPath)
	_, _ = fmt.Fprintln(opts.Diagnostics, "=======================================================")
	_, _ = fmt.Fprintln(opts.Diagnostics, "")

	if opts.OnReady != nil {
		serverURL := fmt.Sprintf("http://%s:%d", displayHost, opts.Port)
		opts.OnReady(serverURL, token)
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
	ctrl        *Controller
	token       string
	staticDir   string
	diagnostics io.Writer

	clientsMu sync.RWMutex
	clients   map[*clientConnection]struct{}
}

func newGatewayHandler(ctrl *Controller, token, staticDir string, diagnostics io.Writer) *gatewayHandler {
	gh := &gatewayHandler{
		ctrl:        ctrl,
		token:       token,
		staticDir:   staticDir,
		diagnostics: diagnostics,
		clients:     make(map[*clientConnection]struct{}),
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
		if !gh.isAuthorized(r) {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		gh.handleWebSocket(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/api/") {
		http.NotFound(w, r)
		return
	}

	// Static SPA file serving
	gh.serveStatic(w, r)
}

func (gh *gatewayHandler) isAuthorized(r *http.Request) bool {
	if gh.token == "" {
		return true
	}

	// 1. Check query parameter `?token=...`
	if qToken := r.URL.Query().Get("token"); qToken != "" {
		if subtle.ConstantTimeCompare([]byte(qToken), []byte(gh.token)) == 1 {
			return true
		}
	}

	// 2. Check Authorization header `Bearer ...`
	authHeader := r.Header.Get("Authorization")
	if strings.HasPrefix(authHeader, "Bearer ") {
		token := strings.TrimPrefix(authHeader, "Bearer ")
		if subtle.ConstantTimeCompare([]byte(token), []byte(gh.token)) == 1 {
			return true
		}
	}

	return false
}

func (gh *gatewayHandler) handleWebSocket(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		_, _ = fmt.Fprintf(gh.diagnostics, "websocket upgrade error: %v\n", err)
		return
	}

	client := &clientConnection{
		ws:   conn,
		send: make(chan []byte, 256),
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
	result, err := gh.executeMethod(req.Method, req.Params)

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

func (gh *gatewayHandler) executeMethod(method string, params json.RawMessage) (any, error) {
	switch method {
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
		return nil, gh.ctrl.SubmitDecision(p.RunID, p.SessionID, p.EventID, p.Value)

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
		return nil, gh.ctrl.SubmitAutomaticDecision(p.RunID, p.SessionID, p.EventID, p.Decision)

	case "submitLine":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
			Line      string `json:"line"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, gh.ctrl.SubmitLine(p.RunID, p.SessionID, p.Line)

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
