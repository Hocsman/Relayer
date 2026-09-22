package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/config"
	"github.com/gorilla/websocket"
)

// startAnonymousGateway boots a gateway with no token on loopback: the mode in
// which every request that passes the checks is granted the operator role.
func startAnonymousGateway(t *testing.T) (serverURL string, port string) {
	t.Helper()

	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")
	if _, err := config.LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	readyCh := make(chan string, 1)
	serverErrCh := startServeForTest(t, ctx, cancel, Options{
		Bind:        "127.0.0.1",
		Port:        0,
		ConfigPath:  configPath,
		Diagnostics: io.Discard,
		OnReady:     func(url, _ string) { readyCh <- url },
	})

	select {
	case serverURL = <-readyCh:
	case err := <-serverErrCh:
		t.Fatalf("Serve failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server startup timed out")
	}
	parsed, err := url.Parse(serverURL)
	if err != nil {
		t.Fatalf("parse server URL: %v", err)
	}
	return serverURL, parsed.Port()
}

// anonymousStatus sends one request to the gateway with the given Host and
// Origin, and returns the status code.
func anonymousStatus(t *testing.T, serverURL, path, host, origin string) int {
	t.Helper()

	request, err := http.NewRequest(http.MethodGet, serverURL+path, nil)
	if err != nil {
		t.Fatalf("NewRequest: %v", err)
	}
	request.Host = host
	if origin != "" {
		request.Header.Set("Origin", origin)
	}
	response, err := (&http.Client{Timeout: 3 * time.Second}).Do(request)
	if err != nil {
		t.Fatalf("GET %s (Host %s): %v", path, host, err)
	}
	_ = response.Body.Close()
	return response.StatusCode
}

// dialAnonymous opens the websocket on 127.0.0.1 while presenting the given
// Host and Origin, the way a browser does after a DNS name rebinds to loopback.
func dialAnonymous(serverURL, host, origin string) (*websocket.Conn, *http.Response, error) {
	header := make(http.Header)
	header.Set("Host", host)
	if origin != "" {
		header.Set("Origin", origin)
	}
	dialer := websocket.Dialer{HandshakeTimeout: 3 * time.Second}
	wsURL := strings.Replace(serverURL, "http://", "ws://", 1) + "/api/ws"
	return dialer.Dial(wsURL, header)
}

// TestAnonymousGatewayRefusesDNSRebinding is the reason a tokenless gateway pins
// Host. v0.8.4 added an Origin check, but compared Origin to Host, and a page
// that rebinds its own name to 127.0.0.1 controls both: its browser sends
// Origin http://rebind.attacker.example:PORT and Host rebind.attacker.example:PORT,
// they match, the socket is loopback, and the page was granted the operator
// role — enough to rewrite an agent's argv and restart it.
func TestAnonymousGatewayRefusesDNSRebinding(t *testing.T) {
	serverURL, port := startAnonymousGateway(t)
	attacker := "rebind.attacker.example:" + port
	attackerOrigin := "http://" + attacker

	// The rebinding page, with and without Origin, on every kind of path. A
	// tokenless gateway serves such a page nothing at all.
	for _, path := range []string{"/api/state", "/api/health", "/"} {
		for _, origin := range []string{attackerOrigin, ""} {
			if status := anonymousStatus(t, serverURL, path, attacker, origin); status != http.StatusForbidden {
				t.Errorf("GET %s with Host %s and Origin %q = %d, want %d",
					path, attacker, origin, status, http.StatusForbidden)
			}
		}
	}

	conn, response, err := dialAnonymous(serverURL, attacker, attackerOrigin)
	if err == nil {
		_ = conn.Close()
		t.Fatal("a DNS-rebinding page opened the operator websocket")
	}
	if response == nil || response.StatusCode != http.StatusForbidden {
		t.Fatalf("rebinding websocket response = %v, want %d", response, http.StatusForbidden)
	}

	// Another local port is another origin, even on a local Host.
	local := "127.0.0.1:" + port
	if status := anonymousStatus(t, serverURL, "/api/state", local, "http://localhost:3000"); status != http.StatusForbidden {
		t.Errorf("GET /api/state from http://localhost:3000 = %d, want %d", status, http.StatusForbidden)
	}
	if conn, _, err := dialAnonymous(serverURL, local, "http://localhost:3000"); err == nil {
		_ = conn.Close()
		t.Error("a page on another local port opened the operator websocket")
	}
}

// TestAnonymousGatewayServesItsOwnOrigin guards the other side: the UI a local
// browser loads from the gateway, under any of its loopback names, still works
// and is still the operator.
func TestAnonymousGatewayServesItsOwnOrigin(t *testing.T) {
	serverURL, port := startAnonymousGateway(t)

	for _, host := range []string{"127.0.0.1:" + port, "localhost:" + port, "LOCALHOST:" + port} {
		origin := "http://" + strings.ToLower(host)
		if status := anonymousStatus(t, serverURL, "/api/state", host, origin); status != http.StatusOK {
			t.Errorf("GET /api/state with Host %s and its own Origin = %d, want %d", host, status, http.StatusOK)
		}
		// A non-browser client sends no Origin.
		if status := anonymousStatus(t, serverURL, "/api/state", host, ""); status != http.StatusOK {
			t.Errorf("GET /api/state with Host %s and no Origin = %d, want %d", host, status, http.StatusOK)
		}

		conn, _, err := dialAnonymous(serverURL, host, origin)
		if err != nil {
			t.Fatalf("same-origin websocket on %s: %v", host, err)
		}
		request, _ := json.Marshal(wsRequest{ID: "who", Method: "getUserInfo", Params: json.RawMessage(`{}`)})
		if err := conn.WriteMessage(websocket.TextMessage, request); err != nil {
			t.Fatalf("write getUserInfo: %v", err)
		}
		var info UserInfo
		for info.Role == "" {
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, data, err := conn.ReadMessage()
			if err != nil {
				t.Fatalf("read getUserInfo on %s: %v", host, err)
			}
			var response wsResponse
			if json.Unmarshal(data, &response) != nil || response.ID != "who" {
				continue
			}
			raw, _ := json.Marshal(response.Result)
			if err := json.Unmarshal(raw, &info); err != nil {
				t.Fatalf("decode UserInfo: %v", err)
			}
		}
		_ = conn.Close()
		if info.Role != string(RoleOperator) {
			t.Errorf("same-origin anonymous role on %s = %q, want %q", host, info.Role, RoleOperator)
		}
	}
}

func TestLocalHostsForCoversTheDefaultPort(t *testing.T) {
	hosts := localHostsFor(80)
	for _, host := range []string{"127.0.0.1", "localhost", "[::1]", "127.0.0.1:80", "[::1]:80"} {
		if _, ok := hosts[host]; !ok {
			t.Errorf("port 80 allowlist is missing %q", host)
		}
	}
	if _, ok := localHostsFor(8080)["localhost"]; ok {
		t.Error("a non-default port admitted the bare name, which a browser never sends for it")
	}
	if len(localHostsFor(8080)) != 3 {
		t.Errorf("allowlist for 8080 = %v, want exactly the three loopback names", localHostsFor(8080))
	}
}
