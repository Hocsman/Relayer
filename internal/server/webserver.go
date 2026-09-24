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
	"net/url"
	"os"
	"path"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
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
	// connID addresses this socket. Identity is not unique: the same operator
	// may hold several tabs, and the terminal write lock must distinguish them.
	connID string
	closed atomic.Bool
	mu     sync.Mutex

	handNoticeMu sync.Mutex
	handNotice   map[string]struct{}
}

// newConnectionID mints an unguessable per-socket identifier. It never reaches
// an audit field unredacted and is not a credential, but a predictable value
// would let one client address another's hand in a request payload.
func newConnectionID() string {
	raw := make([]byte, 8)
	if _, err := rand.Read(raw); err != nil {
		// A connection that cannot be uniquely addressed must not silently
		// share an identifier with another; the empty string is rejected by
		// every presence entry point.
		return ""
	}
	return hex.EncodeToString(raw)
}

// actor is who this connection is, as the supervision core names who made a
// decision or sent a line: the signed-in identity, the role its token gives
// it, and the connection. The journal keeps them on the person's decision and
// delivery entries; the connection is what ties an answer to the attach and
// control records around it, since one identity may be signed in from several
// tabs. The role was always written "operator", whoever answered, and the
// connection not at all.
func (c *clientConnection) actor() supervise.Actor {
	return supervise.Actor{Identity: c.identity, Role: string(c.role), ConnID: c.connID}
}

// notifyHandOnce sends one hand correction per session to a client whose
// keystrokes are being dropped, so a stale UI resynchronizes without the
// socket being flooded by one rejection per character.
func (c *clientConnection) notifyHandOnce(sessionID string, view HandView) {
	c.handNoticeMu.Lock()
	if c.handNotice == nil {
		c.handNotice = make(map[string]struct{})
	}
	key := strings.ToLower(strings.TrimSpace(sessionID))
	_, already := c.handNotice[key]
	c.handNotice[key] = struct{}{}
	c.handNoticeMu.Unlock()

	if already {
		return
	}
	if msg, err := json.Marshal(wsEventMessage{Event: eventHand, Payload: view}); err == nil {
		c.safeSend(msg)
	}
}

// clearHandNotice re-arms the correction for a session, so a client that
// reacquires and later loses the hand is told again.
func (c *clientConnection) clearHandNotice(sessionID string) {
	c.handNoticeMu.Lock()
	delete(c.handNotice, strings.ToLower(strings.TrimSpace(sessionID)))
	c.handNoticeMu.Unlock()
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

// localHostsFor lists the Host header values a tokenless gateway answers to:
// the loopback names a local browser can use to reach this port, and nothing
// else. A browser omits the default port from Host, so port 80 also admits the
// bare names.
func localHostsFor(port int) map[string]struct{} {
	names := []string{"127.0.0.1", "localhost", "[::1]"}
	hosts := make(map[string]struct{}, 2*len(names))
	for _, name := range names {
		hosts[name+":"+strconv.Itoa(port)] = struct{}{}
		if port == 80 {
			hosts[name] = struct{}{}
		}
	}
	return hosts
}

func isLoopbackRemote(remoteAddr string) bool {
	host := remoteAddr
	if h, _, err := net.SplitHostPort(remoteAddr); err == nil {
		host = h
	}
	host = strings.TrimPrefix(strings.TrimSuffix(host, "]"), "[")
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// checkSameOrigin accepts a request that carries no Origin, or one whose Origin
// names exactly the host and port it was sent to.
//
// A page on another loopback port is not the same origin. Trusting any local
// port meant anything else listening locally — a dev server, a dashboard, a
// page with an XSS hole — could drive the gateway. The web UI is served by the
// gateway itself and always connects to its own origin.
//
// Origin is compared to Host, and a DNS-rebinding page controls both, so this
// check alone does not protect the tokenless mode: gatewayHandler.ServeHTTP
// also pins Host there.
func checkSameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" {
		// Non-browser clients (CLI, curl, native GUI) do not send Origin header.
		return true
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}

var upgrader = websocket.Upgrader{
	CheckOrigin: func(r *http.Request) bool {
		return checkSameOrigin(r)
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
		// Every agent is stopped in parallel, so one stop's worst case bounds
		// them all. Five seconds let serve exit while an agent the console
		// close never reaches was still inside its grace period, and it kept
		// running after Relayer was gone.
		shutdownCtx, cancel := context.WithTimeout(context.Background(), session.StopBudget+2*time.Second)
		defer cancel()
		if err := ctrl.Close(shutdownCtx); err != nil {
			// Said, not swallowed: an agent may have outlived the shutdown.
			_, _ = fmt.Fprintf(opts.Diagnostics, "Closing the agents did not finish cleanly: %v\n", err)
		}
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

	handler := newGatewayHandler(ctrl, tokens, allowAnonymousLocal, opts.Port, opts.StaticDir, opts.Diagnostics)
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
	// localHosts is the Host allowlist of the tokenless mode, fixed once the
	// listening port is known. It is empty when tokens are configured.
	localHosts  map[string]struct{}
	staticDir   string
	diagnostics io.Writer

	clientsMu sync.RWMutex
	clients   map[*clientConnection]struct{}
}

func newGatewayHandler(ctrl *Controller, tokens map[string]AuthIdentity, allowAnonymousLocal bool, port int, staticDir string, diagnostics io.Writer) *gatewayHandler {
	gh := &gatewayHandler{
		ctrl:                ctrl,
		tokens:              tokens,
		allowAnonymousLocal: allowAnonymousLocal,
		staticDir:           staticDir,
		diagnostics:         diagnostics,
		clients:             make(map[*clientConnection]struct{}),
	}
	if allowAnonymousLocal {
		gh.localHosts = localHostsFor(port)
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
	released := make([]string, 0, len(gh.clients))
	for client := range gh.clients {
		released = append(released, client.connID)
		client.close()
	}
	gh.clientsMu.Unlock()

	// Outside clientsMu: ReleasePresence broadcasts, and the broadcast listener
	// takes clientsMu for reading.
	for _, connID := range released {
		gh.ctrl.ReleasePresence(connID)
	}
}

func (gh *gatewayHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// The tokenless mode trusts a request because it arrives over loopback. A
	// page that rebinds its own DNS name to 127.0.0.1 arrives over loopback too,
	// and controls Origin as well, so neither the socket nor the Origin check
	// can tell it apart. The Host its browser sends still names the attacker's
	// domain: pinning Host to the loopback names this gateway was started on is
	// what separates the two. It applies to every path, the static UI included,
	// so a rebinding page is never served anything by a tokenless gateway.
	if gh.allowAnonymousLocal && !gh.isLocalHost(r.Host) {
		http.Error(w, "Forbidden: unexpected Host for a tokenless gateway", http.StatusForbidden)
		return
	}

	// Health endpoint
	if r.URL.Path == "/api/health" {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
		return
	}

	// State endpoint
	if r.URL.Path == "/api/state" {
		if !checkSameOrigin(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
		identity, ok := gh.authenticate(r)
		if !ok {
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(stateForRole(gh.ctrl.GetState(), identity.Role))
		return
	}

	// WebSocket endpoint
	if r.URL.Path == "/api/ws" {
		if !checkSameOrigin(r) {
			http.Error(w, "Forbidden", http.StatusForbidden)
			return
		}
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

	// 3. Fallback for anonymous localhost if allowed. ServeHTTP has already
	// pinned Host for this mode; the checks repeat here so any future route
	// that authenticates gets them too. The anonymous mode only exists on a
	// loopback bind, so the RemoteAddr test does not refuse anything today: it
	// keeps the mode from quietly widening if the bind rules ever change.
	if gh.allowAnonymousLocal && len(gh.tokens) == 0 {
		if gh.isLocalHost(r.Host) && isLoopbackRemote(r.RemoteAddr) && checkSameOrigin(r) {
			return AuthIdentity{
				Identity: "local-operator",
				Role:     RoleOperator,
			}, true
		}
		return AuthIdentity{}, false
	}

	return AuthIdentity{}, false
}

// isLocalHost reports whether a Host header is one of the loopback names this
// tokenless gateway was started on.
func (gh *gatewayHandler) isLocalHost(host string) bool {
	_, ok := gh.localHosts[strings.ToLower(strings.TrimSpace(host))]
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
		connID:   newConnectionID(),
	}

	gh.clientsMu.Lock()
	gh.clients[client] = struct{}{}
	gh.clientsMu.Unlock()

	gh.ctrl.RegisterPresence(client.connID, client.identity, string(client.role))

	defer func() {
		gh.clientsMu.Lock()
		delete(gh.clients, client)
		gh.clientsMu.Unlock()
		client.close()
		// Released after the client lock, never under it: ReleasePresence
		// broadcasts, and the broadcast listener takes clientsMu.
		gh.ctrl.ReleasePresence(client.connID)
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
		msgType, msg, err := conn.ReadMessage()
		if err != nil {
			break
		}

		if msgType == websocket.BinaryMessage {
			if client.role == RoleViewer {
				continue
			}
			if len(msg) < 1 {
				continue
			}
			sessLen := int(msg[0])
			if len(msg) < 1+sessLen {
				continue
			}
			sessionID := string(msg[1 : 1+sessLen])
			inputBytes := msg[1+sessLen:]
			if len(inputBytes) > 0 {
				err := gh.ctrl.SendTerminalInput("", sessionID, inputBytes, client.identity, client.connID)
				// A connection that lost the hand keeps typing until its UI
				// catches up. Answering every rejected keystroke would flood the
				// socket, so correct the client once per session instead.
				if errors.Is(err, ErrNotHolder) {
					client.notifyHandOnce(sessionID, gh.ctrl.HandFor(sessionID))
				}
			}
			continue
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
	if client.role == RoleViewer && !isViewerAllowed(method) {
		return nil, errors.New("permission denied: viewer role is read-only")
	}
	if client.role == RoleViewer && method == "resizeSession" {
		// Silent no-op for viewer to avoid disruptive PTY resizes without UI errors
		return nil, nil
	}

	switch method {
	case "getUserInfo":
		return UserInfo{
			Identity: client.identity,
			ConnID:   client.connID,
			Role:     string(client.role),
			ReadOnly: client.role == RoleViewer,
		}, nil

	case "getState":
		return stateForRole(gh.ctrl.GetState(), client.role), nil

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
		return nil, gh.ctrl.SubmitDecision(p.RunID, p.SessionID, p.EventID, p.Value, client.actor())

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
		return nil, gh.ctrl.SubmitAutomaticDecision(p.RunID, p.SessionID, p.EventID, p.Decision, client.actor())

	case "submitLine":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
			Line      string `json:"line"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, gh.ctrl.SubmitLine(p.RunID, p.SessionID, p.Line, client.actor())

	case "sendTerminalInput":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
			Data      string `json:"data"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, gh.ctrl.SendTerminalInput(p.RunID, p.SessionID, []byte(p.Data), client.identity, client.connID)

	case "setInteractiveSession":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
			Active    bool   `json:"active"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		client.clearHandNotice(p.SessionID)
		return nil, gh.ctrl.SetInteractiveSession(p.RunID, p.SessionID, p.Active, client.identity, client.connID)

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
		return nil, gh.ctrl.ResizeSession(p.RunID, p.SessionID, p.Columns, p.Rows, client.connID)

	case "listPresence":
		var p struct {
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return gh.ctrl.ListPresence(p.SessionID)

	case "observeSession":
		var p struct {
			SessionID string `json:"sessionID"`
			Observing bool   `json:"observing"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return gh.ctrl.ObserveSession(client.connID, p.SessionID, p.Observing)

	case "requestControl":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		client.clearHandNotice(p.SessionID)
		return gh.ctrl.RequestControl(p.RunID, p.SessionID, client.connID, client.identity)

	case "grantControl":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
			ToConnID  string `json:"toConnID"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return gh.ctrl.GrantControl(p.RunID, p.SessionID, client.connID, client.identity, p.ToConnID)

	case "declineControl":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
			ToConnID  string `json:"toConnID"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return gh.ctrl.DeclineControl(p.RunID, p.SessionID, client.connID, client.identity, p.ToConnID)

	case "releaseControl":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return gh.ctrl.ReleaseControl(p.RunID, p.SessionID, client.connID, client.identity)

	case "forceTakeControl":
		var p struct {
			RunID     string `json:"runID"`
			SessionID string `json:"sessionID"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		client.clearHandNotice(p.SessionID)
		return gh.ctrl.ForceTakeControl(p.RunID, p.SessionID, client.connID, client.identity)

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
		view, err := gh.ctrl.GetAgentProfiles()
		if err != nil {
			if client.role != RoleOperator {
				return nil, errViewerProfiles
			}
			return nil, err
		}
		return profilesForRole(view, client.role), nil

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
		summary, err := gh.ctrl.GetAuditSummary()
		if err != nil {
			return nil, auditErrorForRole(err, client.role)
		}
		summary.Path = auditPathForRole(summary.Path, client.role)
		return summary, nil

	case "getAuditEntries":
		var filter AuditFilterInput
		_ = json.Unmarshal(params, &filter)
		entries, err := gh.ctrl.GetAuditEntries(filter)
		if err != nil {
			return nil, auditErrorForRole(err, client.role)
		}
		return entries, nil

	case "verifyAuditJournal":
		verification, err := gh.ctrl.VerifyAuditJournal()
		if err != nil {
			return nil, auditErrorForRole(err, client.role)
		}
		verification.Path = auditPathForRole(verification.Path, client.role)
		return verification, nil

	case "exportAuditReport":
		var p struct {
			Format string `json:"format"`
		}
		_ = json.Unmarshal(params, &p)
		if p.Format == "" {
			p.Format = "json"
		}
		report, err := gh.ctrl.ExportAuditReport(p.Format)
		if err != nil {
			return nil, auditErrorForRole(err, client.role)
		}
		return report, nil

	case "getTelemetrySnapshot":
		return gh.ctrl.GetTelemetrySnapshot(), nil

	case "listRecordings":
		var filter RecordingFilterInput
		_ = json.Unmarshal(params, &filter)
		recordings, err := gh.ctrl.ListRecordings(filter)
		if err != nil {
			return nil, recordingErrorForRole(err, client.role)
		}
		return recordings, nil

	case "getRecording":
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		recording, err := gh.ctrl.GetRecording(p.ID)
		if err != nil {
			return nil, recordingErrorForRole(err, client.role)
		}
		return recording, nil

	case "readRecordingChunk":
		var p struct {
			ID     string `json:"id"`
			Offset int    `json:"offset"`
			Limit  int    `json:"limit"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		chunk, err := gh.ctrl.ReadRecordingChunk(p.ID, p.Offset, p.Limit)
		if err != nil {
			return nil, recordingErrorForRole(err, client.role)
		}
		return chunk, nil

	case "exportRecording":
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return gh.ctrl.ExportRecording(p.ID, client.identity)

	case "deleteRecording":
		var p struct {
			ID string `json:"id"`
		}
		if err := json.Unmarshal(params, &p); err != nil {
			return nil, err
		}
		return nil, gh.ctrl.DeleteRecording(p.ID, client.identity)

	default:
		return nil, fmt.Errorf("unknown method %q", method)
	}
}

func isViewerAllowed(method string) bool {
	switch method {
	case "getUserInfo",
		"getState",
		"runPreflight",
		"listPresence",
		"observeSession",
		"getAgentProfiles",
		"getAuditSummary",
		"getAuditEntries",
		"verifyAuditJournal",
		"exportAuditReport",
		"getTelemetrySnapshot",
		"listRecordings",
		"getRecording",
		"readRecordingChunk",
		"resizeSession":
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
