package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/config"
	"github.com/gorilla/websocket"
)

// sharedGatewayClient is one websocket connection with the RPC and event helpers
// a sharing test needs. The gateway assigns each connection its own identifier,
// so two clients of the same operator are two distinct participants.
type sharedGatewayClient struct {
	t       *testing.T
	ws      *websocket.Conn
	connID  string
	events  chan wsEventMessage
	replies chan wsResponse
}

func dialSharedGateway(t *testing.T, baseURL, token string) *sharedGatewayClient {
	t.Helper()

	wsURL := strings.Replace(baseURL, "http://", "ws://", 1) + "/api/ws?token=" + token
	ws, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", token, err)
	}
	t.Cleanup(func() { _ = ws.Close() })

	client := &sharedGatewayClient{
		t:       t,
		ws:      ws,
		events:  make(chan wsEventMessage, 256),
		replies: make(chan wsResponse, 64),
	}

	// A single reader pump: the gateway multiplexes RPC replies and broadcasts
	// on one socket, and a test that reads inline would swallow one while
	// waiting for the other.
	go func() {
		for {
			_, data, readErr := ws.ReadMessage()
			if readErr != nil {
				close(client.events)
				return
			}
			var resp wsResponse
			if json.Unmarshal(data, &resp) == nil && resp.ID != "" {
				select {
				case client.replies <- resp:
				default:
				}
				continue
			}
			var event wsEventMessage
			if json.Unmarshal(data, &event) == nil && event.Event != "" {
				select {
				case client.events <- event:
				default:
				}
			}
		}
	}()

	var info UserInfo
	client.mustCall("getUserInfo", map[string]any{}, &info)
	if info.ConnID == "" {
		t.Fatalf("getUserInfo returned no connection id for %s", token)
	}
	client.connID = info.ConnID
	return client
}

func (c *sharedGatewayClient) call(method string, params any, out any) error {
	c.t.Helper()

	raw, _ := json.Marshal(params)
	id := fmt.Sprintf("%s-%d", method, time.Now().UnixNano())
	if err := c.ws.WriteJSON(wsRequest{ID: id, Method: method, Params: raw}); err != nil {
		return err
	}

	deadline := time.After(5 * time.Second)
	for {
		select {
		case resp := <-c.replies:
			if resp.ID != id {
				continue
			}
			if resp.Error != "" {
				return errors.New(resp.Error)
			}
			if out != nil {
				encoded, err := json.Marshal(resp.Result)
				if err != nil {
					return err
				}
				return json.Unmarshal(encoded, out)
			}
			return nil
		case <-deadline:
			return fmt.Errorf("rpc %s timed out", method)
		}
	}
}

func (c *sharedGatewayClient) mustCall(method string, params any, out any) {
	c.t.Helper()
	if err := c.call(method, params, out); err != nil {
		c.t.Fatalf("%s: %v", method, err)
	}
}

// awaitHand waits for a relayer:hand broadcast satisfying the predicate.
func (c *sharedGatewayClient) awaitHand(match func(HandView) bool) HandView {
	c.t.Helper()

	deadline := time.After(5 * time.Second)
	for {
		select {
		case event, ok := <-c.events:
			if !ok {
				c.t.Fatal("socket closed before the expected hand event")
			}
			if event.Event != eventHand {
				continue
			}
			encoded, err := json.Marshal(event.Payload)
			if err != nil {
				continue
			}
			var view HandView
			if json.Unmarshal(encoded, &view) != nil {
				continue
			}
			if match(view) {
				return view
			}
		case <-deadline:
			c.t.Fatal("no matching relayer:hand broadcast")
		}
	}
}

func startSharingGateway(t *testing.T) string {
	t.Helper()

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

	readyCh := make(chan string, 1)
	serverErrCh := make(chan error, 1)
	served := make(chan struct{})
	// Cancelling only asks the server to stop; it closes the audit journal while
	// Serve unwinds, and t.TempDir cannot remove a file still open on Windows.
	// A separate channel rather than serverErrCh, which the startup path below
	// may already have consumed.
	t.Cleanup(func() {
		cancel()
		select {
		case <-served:
		case <-time.After(10 * time.Second):
			t.Error("the server did not shut down within 10s")
		}
	})
	go func() {
		defer close(served)
		serverErrCh <- Serve(ctx, Options{
			Bind:        "127.0.0.1",
			Port:        0,
			Token:       "alice:opAlice,carol:opCarol",
			ViewerToken: "dave:viewDave",
			ConfigPath:  configPath,
			Diagnostics: io.Discard,
			OnReady:     func(serverURL, _ string) { readyCh <- serverURL },
		})
	}()

	select {
	case baseURL := <-readyCh:
		return baseURL
	case err := <-serverErrCh:
		t.Fatalf("Serve failed: %v", err)
	case <-time.After(10 * time.Second):
		t.Fatal("server startup timed out")
	}
	return ""
}

func TestSessionSharingHandLifecycle(t *testing.T) {
	baseURL := startSharingGateway(t)

	alice := dialSharedGateway(t, baseURL, "opAlice")
	carol := dialSharedGateway(t, baseURL, "opCarol")
	if alice.connID == carol.connID {
		t.Fatal("two connections share an identifier")
	}

	sessionID := firstSessionID(t, alice)

	alice.mustCall("setInteractiveSession",
		map[string]any{"sessionID": sessionID, "active": true}, nil)
	// Every connected client learns who holds the terminal, not just the holder.
	carol.awaitHand(func(view HandView) bool {
		return view.State == HandHeld && view.HolderConnID == alice.connID
	})

	// Carol must not be able to take a terminal alice holds.
	err := carol.call("setInteractiveSession",
		map[string]any{"sessionID": sessionID, "active": true}, nil)
	if err == nil {
		t.Fatal("carol took a terminal alice holds")
	}
	if !strings.Contains(err.Error(), "another operator holds the terminal") {
		t.Fatalf("carol takeover error = %v", err)
	}

	// Asking is the supported path, and the holder sees it.
	var requested HandView
	carol.mustCall("requestControl", map[string]any{"sessionID": sessionID}, &requested)
	if requested.State != HandRequested || requested.RequesterConnID != carol.connID {
		t.Fatalf("hand = %+v, want a pending request from carol", requested)
	}
	alice.awaitHand(func(view HandView) bool {
		return view.State == HandRequested && view.RequesterIdentity == "carol"
	})

	var granted HandView
	alice.mustCall("grantControl",
		map[string]any{"sessionID": sessionID, "toConnID": carol.connID}, &granted)
	if granted.HolderConnID != carol.connID {
		t.Fatalf("hand = %+v, want held by carol", granted)
	}
	carol.awaitHand(func(view HandView) bool {
		return view.State == HandHeld && view.HolderConnID == carol.connID
	})

	// Alice no longer holds it, so she cannot release it either.
	if err := alice.call("releaseControl", map[string]any{"sessionID": sessionID}, nil); err == nil {
		t.Fatal("alice released a terminal she no longer holds")
	}
}

func TestSessionSharingViewerObservesButNeverHolds(t *testing.T) {
	baseURL := startSharingGateway(t)

	alice := dialSharedGateway(t, baseURL, "opAlice")
	dave := dialSharedGateway(t, baseURL, "viewDave")

	sessionID := firstSessionID(t, alice)

	// Watching is the viewer's entire purpose and must not be refused.
	var roster PresenceView
	dave.mustCall("observeSession",
		map[string]any{"sessionID": sessionID, "observing": true}, &roster)
	if roster.ObserverCount == 0 {
		t.Fatalf("roster = %+v, want dave counted as an observer", roster)
	}

	for _, method := range []string{"requestControl", "releaseControl", "forceTakeControl"} {
		if err := dave.call(method, map[string]any{"sessionID": sessionID}, nil); err == nil {
			t.Errorf("viewer calling %s succeeded", method)
		} else if !strings.Contains(err.Error(), "permission denied") {
			t.Errorf("viewer calling %s error = %v, want permission denied", method, err)
		}
	}

	// An operator can still take the terminal while a viewer watches.
	alice.mustCall("setInteractiveSession",
		map[string]any{"sessionID": sessionID, "active": true}, nil)
	dave.awaitHand(func(view HandView) bool {
		return view.State == HandHeld && view.HolderIdentity == "alice"
	})
}

func TestSessionSharingDisconnectFreesTheTerminal(t *testing.T) {
	baseURL := startSharingGateway(t)

	alice := dialSharedGateway(t, baseURL, "opAlice")
	carol := dialSharedGateway(t, baseURL, "opCarol")

	sessionID := firstSessionID(t, alice)

	alice.mustCall("setInteractiveSession",
		map[string]any{"sessionID": sessionID, "active": true}, nil)
	carol.awaitHand(func(view HandView) bool {
		return view.State == HandHeld && view.HolderConnID == alice.connID
	})

	// A holder whose socket dies must not strand the terminal.
	_ = alice.ws.Close()
	carol.awaitHand(func(view HandView) bool {
		return view.State == HandFree
	})

	var taken HandView
	carol.mustCall("requestControl", map[string]any{"sessionID": sessionID}, &taken)
	if taken.HolderConnID != carol.connID {
		t.Fatalf("hand = %+v, want carol to have taken the freed terminal", taken)
	}
}

func TestSessionSharingSecondTabDoesNotInheritTheTerminal(t *testing.T) {
	baseURL := startSharingGateway(t)

	// The same operator token, twice: two tabs of one person.
	firstTab := dialSharedGateway(t, baseURL, "opAlice")
	secondTab := dialSharedGateway(t, baseURL, "opAlice")
	if firstTab.connID == secondTab.connID {
		t.Fatal("two tabs of one operator share a connection id")
	}

	sessionID := firstSessionID(t, firstTab)

	firstTab.mustCall("setInteractiveSession",
		map[string]any{"sessionID": sessionID, "active": true}, nil)

	// Keying the hand by identity rather than by connection would silently let
	// the second tab type into a terminal the first one holds.
	err := secondTab.call("setInteractiveSession",
		map[string]any{"sessionID": sessionID, "active": true}, nil)
	if err == nil {
		t.Fatal("the second tab inherited the first tab's terminal")
	}
	if !strings.Contains(err.Error(), "another operator holds the terminal") {
		t.Fatalf("second tab takeover error = %v", err)
	}
}

// firstSessionID returns a session the run actually started. The write lock is
// refused for a session that does not exist, so a sharing test must address a
// real one rather than invent an identifier.
func firstSessionID(t *testing.T, client *sharedGatewayClient) string {
	t.Helper()

	var state AppState
	client.mustCall("getState", map[string]any{}, &state)
	if len(state.Agents) == 0 {
		t.Skip("the default configuration started no agent on this platform")
	}
	return state.Agents[0].SessionID
}
