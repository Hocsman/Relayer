package server

import (
	"errors"
	"sync"
	"testing"
	"time"
)

// newPresenceController builds a Controller with only the state the presence
// machine touches. It deliberately has no runtime: the write lock is a gateway
// concern and must be testable without starting an agent.
func newPresenceController() *Controller {
	return &Controller{
		runID:       "run-test",
		agentIndex:  map[string]int{"alpha": 0, "beta": 1},
		subscribers: make(map[uint64]func(event string, payload any)),
		state: AppState{
			RunID: "run-test",
			Agents: []AgentState{
				{SessionID: "alpha", AgentID: "alpha", Status: "running", Running: true},
				{SessionID: "beta", AgentID: "beta", Status: "running", Running: true},
			},
		},
	}
}

// recordBroadcasts captures every event a transition publishes.
func recordBroadcasts(t *testing.T, c *Controller) func() []string {
	t.Helper()

	var mu sync.Mutex
	var events []string
	unsubscribe := c.Subscribe(func(event string, _ any) {
		mu.Lock()
		events = append(events, event)
		mu.Unlock()
	})
	t.Cleanup(unsubscribe)

	return func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), events...)
	}
}

func TestPresenceRegisterAndRelease(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))

	view, err := c.ObserveSession("conn-a", "alpha", true)
	if err != nil {
		t.Fatalf("ObserveSession: %v", err)
	}
	if view.ObserverCount != 1 || len(view.Members) != 1 {
		t.Fatalf("roster = %+v, want one observing member", view)
	}
	if view.Members[0].Identity != "alice" || !view.Members[0].Observing {
		t.Fatalf("member = %+v", view.Members[0])
	}

	c.ReleasePresence("conn-a")

	after, err := c.ListPresence("alpha")
	if err != nil {
		t.Fatalf("ListPresence: %v", err)
	}
	if after.ObserverCount != 0 || len(after.Members) != 0 {
		t.Fatalf("roster after release = %+v, want empty", after)
	}
}

func TestPresenceUnknownConnectionIsRejected(t *testing.T) {
	c := newPresenceController()

	if _, err := c.ObserveSession("ghost", "alpha", true); !errors.Is(err, ErrUnknownConnection) {
		t.Fatalf("ObserveSession error = %v, want ErrUnknownConnection", err)
	}
	if _, err := c.TakeControl("alpha", "ghost", "nobody"); !errors.Is(err, ErrUnknownConnection) {
		t.Fatalf("TakeControl error = %v, want ErrUnknownConnection", err)
	}
}

func TestHandTakeIsExclusive(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))
	c.RegisterPresence("conn-b", "bob", string(RoleOperator))

	view, err := c.TakeControl("alpha", "conn-a", "alice")
	if err != nil {
		t.Fatalf("alice TakeControl: %v", err)
	}
	if view.State != HandHeld || view.HolderConnID != "conn-a" || view.HolderIdentity != "alice" {
		t.Fatalf("hand = %+v, want held by alice", view)
	}

	if _, err := c.TakeControl("alpha", "conn-b", "bob"); !errors.Is(err, ErrHandHeld) {
		t.Fatalf("bob TakeControl error = %v, want ErrHandHeld", err)
	}

	// The refusal must not have disturbed the holder.
	if got := c.HandFor("alpha"); got.HolderConnID != "conn-a" {
		t.Fatalf("hand after refused takeover = %+v", got)
	}
	if !c.HoldsHand("alpha", "conn-a") {
		t.Fatal("alice should hold alpha")
	}
	if c.HoldsHand("alpha", "conn-b") {
		t.Fatal("bob must not hold alpha")
	}
}

func TestHandFreeSessionIsWritableByAnyone(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))

	// A deployment that never uses sharing must keep working: an unclaimed
	// session is writable, so single-operator behavior is unchanged.
	if !c.HoldsHand("alpha", "conn-a") {
		t.Fatal("unclaimed session should be writable")
	}
	if !c.HoldsHand("alpha", "") {
		t.Fatal("unclaimed session should be writable by an unregistered caller")
	}
}

func TestHandRequestGrantTransfers(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))
	c.RegisterPresence("conn-b", "bob", string(RoleOperator))

	if _, err := c.TakeControl("alpha", "conn-a", "alice"); err != nil {
		t.Fatalf("alice TakeControl: %v", err)
	}

	requested, err := c.RequestControl("alpha", "conn-b", "bob")
	if err != nil {
		t.Fatalf("bob RequestControl: %v", err)
	}
	if requested.State != HandRequested || requested.RequesterIdentity != "bob" {
		t.Fatalf("hand = %+v, want requested by bob", requested)
	}
	if requested.RequestExpiresAt == "" {
		t.Fatal("a pending request must carry a deadline")
	}
	if !c.HoldsHand("alpha", "conn-a") || c.HoldsHand("alpha", "conn-b") {
		t.Fatal("a pending request must not transfer the hand on its own")
	}

	granted, err := c.GrantControl("alpha", "conn-a", "alice", "conn-b")
	if err != nil {
		t.Fatalf("alice GrantControl: %v", err)
	}
	if granted.State != HandHeld || granted.HolderConnID != "conn-b" {
		t.Fatalf("hand = %+v, want held by bob", granted)
	}
	if granted.RequesterConnID != "" {
		t.Fatalf("granting must clear the request, got %+v", granted)
	}
	if !c.HoldsHand("alpha", "conn-b") || c.HoldsHand("alpha", "conn-a") {
		t.Fatal("hand did not transfer")
	}
}

func TestHandDeclineKeepsHolder(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))
	c.RegisterPresence("conn-b", "bob", string(RoleOperator))

	if _, err := c.TakeControl("alpha", "conn-a", "alice"); err != nil {
		t.Fatalf("alice TakeControl: %v", err)
	}
	if _, err := c.RequestControl("alpha", "conn-b", "bob"); err != nil {
		t.Fatalf("bob RequestControl: %v", err)
	}

	declined, err := c.DeclineControl("alpha", "conn-a", "alice", "conn-b")
	if err != nil {
		t.Fatalf("alice DeclineControl: %v", err)
	}
	if declined.State != HandHeld || declined.HolderConnID != "conn-a" {
		t.Fatalf("hand = %+v, want still held by alice", declined)
	}
	if declined.RequesterConnID != "" {
		t.Fatal("decline must clear the pending request")
	}
}

func TestHandGrantRequiresHolderAndRequest(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))
	c.RegisterPresence("conn-b", "bob", string(RoleOperator))

	if _, err := c.TakeControl("alpha", "conn-a", "alice"); err != nil {
		t.Fatalf("alice TakeControl: %v", err)
	}

	if _, err := c.GrantControl("alpha", "conn-b", "bob", "conn-b"); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("non-holder grant error = %v, want ErrNotHolder", err)
	}
	if _, err := c.GrantControl("alpha", "conn-a", "alice", "conn-b"); !errors.Is(err, ErrNoPendingRequest) {
		t.Fatalf("grant with no request error = %v, want ErrNoPendingRequest", err)
	}
}

func TestHandGrantToDisconnectedRequesterKeepsHolder(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))
	c.RegisterPresence("conn-b", "bob", string(RoleOperator))

	if _, err := c.TakeControl("alpha", "conn-a", "alice"); err != nil {
		t.Fatalf("alice TakeControl: %v", err)
	}
	if _, err := c.RequestControl("alpha", "conn-b", "bob"); err != nil {
		t.Fatalf("bob RequestControl: %v", err)
	}

	// Bob's socket dies between the roster read and the grant. The hand must
	// stay with alice rather than be handed to nobody.
	c.ReleasePresence("conn-b")

	if _, err := c.GrantControl("alpha", "conn-a", "alice", ""); !errors.Is(err, ErrNoPendingRequest) {
		t.Fatalf("grant to departed requester error = %v, want ErrNoPendingRequest", err)
	}
	if got := c.HandFor("alpha"); got.HolderConnID != "conn-a" {
		t.Fatalf("hand = %+v, want still held by alice", got)
	}
}

func TestHandReleaseAndDisconnectFreeTheTerminal(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))
	c.RegisterPresence("conn-b", "bob", string(RoleOperator))

	if _, err := c.TakeControl("alpha", "conn-a", "alice"); err != nil {
		t.Fatalf("alice TakeControl: %v", err)
	}
	released, err := c.ReleaseControl("alpha", "conn-a", "alice")
	if err != nil {
		t.Fatalf("alice ReleaseControl: %v", err)
	}
	if released.State != HandFree {
		t.Fatalf("hand = %+v, want free", released)
	}

	if _, err := c.TakeControl("alpha", "conn-b", "bob"); err != nil {
		t.Fatalf("bob TakeControl after release: %v", err)
	}
	c.ReleasePresence("conn-b")
	if got := c.HandFor("alpha"); got.State != HandFree {
		t.Fatalf("hand after holder disconnect = %+v, want free", got)
	}
}

func TestHandReleaseByNonHolderIsRejected(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))
	c.RegisterPresence("conn-b", "bob", string(RoleOperator))

	if _, err := c.TakeControl("alpha", "conn-a", "alice"); err != nil {
		t.Fatalf("alice TakeControl: %v", err)
	}
	if _, err := c.ReleaseControl("alpha", "conn-b", "bob"); !errors.Is(err, ErrNotHolder) {
		t.Fatalf("bob ReleaseControl error = %v, want ErrNotHolder", err)
	}
	if !c.HoldsHand("alpha", "conn-a") {
		t.Fatal("a rejected release must not free the hand")
	}
}

func TestHandRequestExpires(t *testing.T) {
	c := newPresenceController()
	c.requestTimeout = 10 * time.Millisecond
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))
	c.RegisterPresence("conn-b", "bob", string(RoleOperator))

	if _, err := c.TakeControl("alpha", "conn-a", "alice"); err != nil {
		t.Fatalf("alice TakeControl: %v", err)
	}
	if _, err := c.RequestControl("alpha", "conn-b", "bob"); err != nil {
		t.Fatalf("bob RequestControl: %v", err)
	}

	time.Sleep(25 * time.Millisecond)

	got := c.HandFor("alpha")
	if got.State != HandHeld {
		t.Fatalf("hand = %+v, want held with the request expired", got)
	}
	if got.RequesterConnID != "" {
		t.Fatalf("expired request survived: %+v", got)
	}
}

func TestHandRequestOnFreeSessionTakesItDirectly(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-b", "bob", string(RoleOperator))

	// Asking nobody for a hand nobody holds would strand the requester, so the
	// request resolves immediately into a take.
	view, err := c.RequestControl("alpha", "conn-b", "bob")
	if err != nil {
		t.Fatalf("RequestControl on free session: %v", err)
	}
	if view.State != HandHeld || view.HolderConnID != "conn-b" {
		t.Fatalf("hand = %+v, want held by bob", view)
	}
}

func TestHandForceTakeoverRequiresOptIn(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))
	c.RegisterPresence("conn-b", "bob", string(RoleOperator))

	if _, err := c.TakeControl("alpha", "conn-a", "alice"); err != nil {
		t.Fatalf("alice TakeControl: %v", err)
	}
	if _, err := c.ForceTakeControl("alpha", "conn-b", "bob"); !errors.Is(err, ErrForceDisabled) {
		t.Fatalf("force takeover error = %v, want ErrForceDisabled", err)
	}
	if !c.HoldsHand("alpha", "conn-a") {
		t.Fatal("a refused force takeover must leave the holder alone")
	}

	c.allowForceTakeover = true
	forced, err := c.ForceTakeControl("alpha", "conn-b", "bob")
	if err != nil {
		t.Fatalf("ForceTakeControl with opt-in: %v", err)
	}
	if forced.HolderConnID != "conn-b" {
		t.Fatalf("hand = %+v, want seized by bob", forced)
	}
}

func TestHandViewerCannotTakeOrRequest(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-v", "watcher", string(RoleViewer))

	if _, err := c.TakeControl("alpha", "conn-v", "watcher"); !errors.Is(err, ErrViewerRole) {
		t.Fatalf("viewer TakeControl error = %v, want ErrViewerRole", err)
	}
	if _, err := c.RequestControl("alpha", "conn-v", "watcher"); !errors.Is(err, ErrViewerRole) {
		t.Fatalf("viewer RequestControl error = %v, want ErrViewerRole", err)
	}

	// Observing stays open to viewers: watching is the whole point of the role.
	if _, err := c.ObserveSession("conn-v", "alpha", true); err != nil {
		t.Fatalf("viewer ObserveSession: %v", err)
	}
}

func TestHandIsPerSession(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))
	c.RegisterPresence("conn-b", "bob", string(RoleOperator))

	if _, err := c.TakeControl("alpha", "conn-a", "alice"); err != nil {
		t.Fatalf("alice TakeControl alpha: %v", err)
	}
	if _, err := c.TakeControl("beta", "conn-b", "bob"); err != nil {
		t.Fatalf("bob TakeControl beta: %v", err)
	}

	if !c.HoldsHand("alpha", "conn-a") || !c.HoldsHand("beta", "conn-b") {
		t.Fatal("hands must be independent per session")
	}
	if c.HoldsHand("alpha", "conn-b") || c.HoldsHand("beta", "conn-a") {
		t.Fatal("a hand on one session must not grant another")
	}
}

func TestHandKeyedByConnectionNotIdentity(t *testing.T) {
	c := newPresenceController()
	// The same operator in two browser tabs. A release from one tab must not
	// revoke the other, and the second tab must not inherit the first's hand.
	c.RegisterPresence("tab-1", "alice", string(RoleOperator))
	c.RegisterPresence("tab-2", "alice", string(RoleOperator))

	if _, err := c.TakeControl("alpha", "tab-1", "alice"); err != nil {
		t.Fatalf("tab-1 TakeControl: %v", err)
	}
	if c.HoldsHand("alpha", "tab-2") {
		t.Fatal("a second tab must not inherit the hand")
	}
	if _, err := c.TakeControl("alpha", "tab-2", "alice"); !errors.Is(err, ErrHandHeld) {
		t.Fatalf("tab-2 TakeControl error = %v, want ErrHandHeld", err)
	}
}

func TestPresenceSessionIDIsCaseInsensitive(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))

	if _, err := c.TakeControl("ALPHA", "conn-a", "alice"); err != nil {
		t.Fatalf("TakeControl: %v", err)
	}
	if !c.HoldsHand("alpha", "conn-a") {
		t.Fatal("session ids must match case-insensitively")
	}
}

func TestPresenceObserverLimit(t *testing.T) {
	c := newPresenceController()
	for i := 0; i < maxSessionObservers; i++ {
		id := "conn-" + string(rune('a'+i))
		c.RegisterPresence(id, id, string(RoleViewer))
		if _, err := c.ObserveSession(id, "alpha", true); err != nil {
			t.Fatalf("observer %d: %v", i, err)
		}
	}

	c.RegisterPresence("conn-overflow", "overflow", string(RoleViewer))
	if _, err := c.ObserveSession("conn-overflow", "alpha", false); err != nil {
		t.Fatalf("leaving a full session must always be allowed: %v", err)
	}
	if _, err := c.ObserveSession("conn-overflow", "alpha", true); !errors.Is(err, ErrTooManyObservers) {
		t.Fatalf("overflow observer error = %v, want ErrTooManyObservers", err)
	}
}

func TestPresenceTransitionsBroadcast(t *testing.T) {
	c := newPresenceController()
	events := recordBroadcasts(t, c)

	c.RegisterPresence("conn-a", "alice", string(RoleOperator))
	if _, err := c.TakeControl("alpha", "conn-a", "alice"); err != nil {
		t.Fatalf("TakeControl: %v", err)
	}

	got := events()
	var hands, rosters int
	for _, event := range got {
		switch event {
		case eventHand:
			hands++
		case eventPresence:
			rosters++
		}
	}
	if hands == 0 || rosters == 0 {
		t.Fatalf("events = %v, want both a hand and a presence broadcast", got)
	}
}

func TestPresenceAgentStateTracksHolder(t *testing.T) {
	c := newPresenceController()
	c.RegisterPresence("conn-a", "alice", string(RoleOperator))

	if _, err := c.TakeControl("alpha", "conn-a", "alice"); err != nil {
		t.Fatalf("TakeControl: %v", err)
	}

	c.mu.RLock()
	agent := c.state.Agents[0]
	c.mu.RUnlock()
	if !agent.Attached || agent.HolderIdentity != "alice" {
		t.Fatalf("agent = %+v, want attached and attributed to alice", agent)
	}

	if _, err := c.ReleaseControl("alpha", "conn-a", "alice"); err != nil {
		t.Fatalf("ReleaseControl: %v", err)
	}

	c.mu.RLock()
	agent = c.state.Agents[0]
	c.mu.RUnlock()
	if agent.Attached || agent.HolderIdentity != "" {
		t.Fatalf("agent = %+v, want detached", agent)
	}
}

// TestHandConcurrentTransitions is the test that justifies the design: under
// contention there must be at most one holder at every observable instant.
func TestHandConcurrentTransitions(t *testing.T) {
	c := newPresenceController()
	const connections = 8
	ids := make([]string, connections)
	for i := range ids {
		ids[i] = "conn-" + string(rune('a'+i))
		c.RegisterPresence(ids[i], ids[i], string(RoleOperator))
	}

	var wg sync.WaitGroup
	for _, id := range ids {
		wg.Add(1)
		go func(connID string) {
			defer wg.Done()
			for i := 0; i < 200; i++ {
				_, _ = c.TakeControl("alpha", connID, connID)
				_, _ = c.RequestControl("alpha", connID, connID)
				_, _ = c.ObserveSession(connID, "alpha", i%2 == 0)
				_, _ = c.GrantControl("alpha", connID, connID, "")
				_, _ = c.ReleaseControl("alpha", connID, connID)
				c.HoldsHand("alpha", connID)
				c.HandFor("alpha")
			}
		}(id)
	}

	// A single snapshot is the unit of observation: comparing two successive
	// reads would only prove the hand moved between them, which it is supposed
	// to do. What must never appear is one torn snapshot.
	stop := make(chan struct{})
	watcher := make(chan struct{})
	go func() {
		defer close(watcher)
		for {
			select {
			case <-stop:
				return
			default:
			}
			view := c.HandFor("alpha")
			switch view.State {
			case HandFree:
				if view.HolderConnID != "" || view.RequesterConnID != "" {
					t.Errorf("free hand still names participants: %+v", view)
					return
				}
			case HandHeld:
				if view.HolderConnID == "" || view.RequesterConnID != "" {
					t.Errorf("held hand is inconsistent: %+v", view)
					return
				}
			case HandRequested:
				if view.HolderConnID == "" || view.RequesterConnID == "" {
					t.Errorf("requested hand is inconsistent: %+v", view)
					return
				}
				if view.HolderConnID == view.RequesterConnID {
					t.Errorf("holder is its own requester: %+v", view)
					return
				}
			default:
				t.Errorf("unknown hand state %q", view.State)
				return
			}
		}
	}()

	wg.Wait()
	close(stop)
	<-watcher

	// Every connection leaves; the terminal must end unclaimed.
	for _, id := range ids {
		c.ReleasePresence(id)
	}
	if view := c.HandFor("alpha"); view.State != HandFree {
		t.Fatalf("hand after everyone left = %+v, want free", view)
	}
}
