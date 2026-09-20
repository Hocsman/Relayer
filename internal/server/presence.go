package server

import (
	"errors"
	"strings"
	"sync/atomic"
	"time"
)

// Presence and the terminal write lock ("the hand") live in the Controller
// rather than the gateway for three reasons: the Controller is the only place
// that can enforce the lock on SendTerminalInput, the only place that can audit
// it, and the owner of the single broadcast fan-out. The gateway is a thin
// registrar that reports connect and disconnect.
//
// Every state read below is derived, never cached: a caller asks for a complete
// PresenceView or HandView and receives the whole picture. Broadcasts are
// therefore self-healing, which matters because clientConnection.safeSend drops
// frames rather than blocking when a slow client's queue is full.

const (
	// HandFree means no connection may write to the session's terminal.
	HandFree = "free"
	// HandHeld means exactly one connection may write to it.
	HandHeld = "held"
	// HandRequested means the hand is held and another operator has asked for it.
	HandRequested = "requested"
)

// defaultControlRequestTimeout bounds how long a pending request blocks the
// requester's UI. A holder who never answers must not strand a colleague.
const defaultControlRequestTimeout = 30 * time.Second

// maxSessionObservers bounds the roster of a single session. The limit exists
// to keep a PresenceView small enough to broadcast on every transition, not as
// a licensing control.
const maxSessionObservers = 16

var (
	// ErrHandHeld is returned instead of silently stealing the terminal from
	// another operator. The caller is expected to request control.
	ErrHandHeld = errors.New("another operator holds the terminal")
	// ErrNotHolder is returned to a connection that tried to write to, resize,
	// or release a terminal it does not hold.
	ErrNotHolder = errors.New("this connection does not hold the terminal")
	// ErrNoPendingRequest is returned when granting or declining a request that
	// has already expired, been withdrawn, or never existed.
	ErrNoPendingRequest = errors.New("no pending control request")
	// ErrTooManyObservers is returned once a session roster is full.
	ErrTooManyObservers = errors.New("observer limit reached")
	// ErrForceDisabled is returned when force takeover is requested but the
	// deployment has not opted into it.
	ErrForceDisabled = errors.New("force takeover is disabled")
	// ErrUnknownConnection is returned for a connID the registrar never saw, or
	// that disconnected between a roster read and this call.
	ErrUnknownConnection = errors.New("unknown connection")
	// ErrViewerRole is returned when a read-only connection attempts to take or
	// request the hand. The RPC dispatcher rejects viewers first; this is the
	// second, authoritative gate.
	ErrViewerRole = errors.New("permission denied: viewer role is read-only")
)

// presenceEntry is one live websocket connection.
type presenceEntry struct {
	connID    string
	identity  string
	role      string
	since     time.Time
	observing map[string]struct{} // lowercase sessionIDs
}

// handState is the write lock for one session. The zero value means free.
type handState struct {
	holderConnID      string
	holderIdentity    string
	since             time.Time
	requesterConnID   string
	requesterIdentity string
	requestedAt       time.Time
}

func (h handState) held() bool {
	return h.holderConnID != ""
}

func (h handState) requested() bool {
	return h.requesterConnID != ""
}

// expireRequestLocked drops a control request that has outlived the timeout.
// Expiry is evaluated lazily on every read and mutation rather than from a
// per-request timer: a timer firing after Close is exactly the class of bug
// this package has fixed before, and the run event loop already ticks.
func (c *Controller) expireRequestLocked(sessionKey string, now time.Time) bool {
	hand, found := c.hands[sessionKey]
	if !found || !hand.requested() {
		return false
	}
	if now.Sub(hand.requestedAt) < c.controlRequestTimeout() {
		return false
	}
	hand.requesterConnID = ""
	hand.requesterIdentity = ""
	hand.requestedAt = time.Time{}
	c.hands[sessionKey] = hand
	return true
}

func (c *Controller) controlRequestTimeout() time.Duration {
	if c.requestTimeout > 0 {
		return c.requestTimeout
	}
	return defaultControlRequestTimeout
}

func sessionKey(sessionID string) string {
	return strings.ToLower(strings.TrimSpace(sessionID))
}

// RegisterPresence records a newly authenticated connection. It is called by
// the gateway immediately after the websocket is registered.
func (c *Controller) RegisterPresence(connID, identity, role string) {
	connID = strings.TrimSpace(connID)
	if connID == "" {
		return
	}

	c.mu.Lock()
	if c.presence == nil {
		c.presence = make(map[string]presenceEntry)
	}
	c.presence[connID] = presenceEntry{
		connID:    connID,
		identity:  strings.TrimSpace(identity),
		role:      strings.TrimSpace(role),
		since:     time.Now().UTC(),
		observing: make(map[string]struct{}),
	}
	c.mu.Unlock()
}

// ReleasePresence removes a disconnected connection and frees any hand it held.
// The gateway must call this after releasing its own client lock: broadcasting
// while the gateway holds that lock deadlocks against the broadcast listener.
func (c *Controller) ReleasePresence(connID string) {
	connID = strings.TrimSpace(connID)
	if connID == "" {
		return
	}

	c.mu.Lock()
	entry, found := c.presence[connID]
	if !found {
		c.mu.Unlock()
		return
	}
	delete(c.presence, connID)

	now := time.Now().UTC()
	touched := make(map[string]struct{})
	for key := range entry.observing {
		touched[key] = struct{}{}
	}
	for key, hand := range c.hands {
		changed := false
		if hand.holderConnID == connID {
			hand = handState{}
			changed = true
		}
		if hand.requesterConnID == connID {
			hand.requesterConnID = ""
			hand.requesterIdentity = ""
			hand.requestedAt = time.Time{}
			changed = true
		}
		if c.expireRequestLocked(key, now) {
			hand = c.hands[key]
			changed = true
		}
		if changed {
			if hand.held() || hand.requested() {
				c.hands[key] = hand
			} else {
				delete(c.hands, key)
			}
			touched[key] = struct{}{}
			c.setAttachedLocked(key, hand.held())
		}
	}

	views := c.snapshotsLocked(touched)
	c.mu.Unlock()

	c.broadcastSnapshots(views)
}

// ObserveSession adds a connection to one session's roster. Observing is a
// read-only act: viewers may observe, and observing never implies the hand.
func (c *Controller) ObserveSession(connID, sessionID string, observing bool) (PresenceView, error) {
	key := sessionKey(sessionID)
	if key == "" {
		return PresenceView{}, errors.New("session id is empty")
	}

	c.mu.Lock()
	entry, found := c.presence[strings.TrimSpace(connID)]
	if !found {
		c.mu.Unlock()
		return PresenceView{}, ErrUnknownConnection
	}
	if observing {
		if _, already := entry.observing[key]; !already {
			if c.observerCountLocked(key) >= maxSessionObservers {
				c.mu.Unlock()
				return PresenceView{}, ErrTooManyObservers
			}
			entry.observing[key] = struct{}{}
		}
	} else {
		delete(entry.observing, key)
	}
	c.presence[entry.connID] = entry

	view := c.presenceViewLocked(key)
	c.mu.Unlock()

	c.broadcast(eventPresence, view)
	return view, nil
}

// ListPresence returns the roster of one session without mutating anything.
func (c *Controller) ListPresence(sessionID string) (PresenceView, error) {
	key := sessionKey(sessionID)
	if key == "" {
		return PresenceView{}, errors.New("session id is empty")
	}

	c.mu.Lock()
	c.expireRequestLocked(key, time.Now().UTC())
	view := c.presenceViewLocked(key)
	c.mu.Unlock()

	return view, nil
}

// TakeControl acquires a free hand. It deliberately refuses to steal a held
// one: the caller is told to request control instead.
func (c *Controller) TakeControl(sessionID, connID, operator string) (HandView, error) {
	return c.mutateHand(sessionID, connID, func(hand handState, entry presenceEntry, now time.Time) (handState, error) {
		if entry.role == string(RoleViewer) {
			return hand, ErrViewerRole
		}
		if hand.held() && hand.holderConnID != connID {
			return hand, ErrHandHeld
		}
		hand.holderConnID = entry.connID
		hand.holderIdentity = entry.identity
		if hand.since.IsZero() {
			hand.since = now
		}
		return hand, nil
	})
}

// ReleaseControl frees a hand held by this connection.
func (c *Controller) ReleaseControl(sessionID, connID, operator string) (HandView, error) {
	return c.mutateHand(sessionID, connID, func(hand handState, entry presenceEntry, _ time.Time) (handState, error) {
		if !hand.held() {
			return handState{}, nil
		}
		if hand.holderConnID != entry.connID {
			return hand, ErrNotHolder
		}
		return handState{}, nil
	})
}

// RequestControl asks the current holder to hand over. A second request from
// the same connection refreshes the deadline rather than erroring, so a UI that
// retries is not punished.
func (c *Controller) RequestControl(sessionID, connID, operator string) (HandView, error) {
	return c.mutateHand(sessionID, connID, func(hand handState, entry presenceEntry, now time.Time) (handState, error) {
		if entry.role == string(RoleViewer) {
			return hand, ErrViewerRole
		}
		if !hand.held() {
			// Nothing to ask for: the hand is free, so take it directly rather
			// than leaving the requester waiting on an answer nobody will give.
			hand.holderConnID = entry.connID
			hand.holderIdentity = entry.identity
			hand.since = now
			return hand, nil
		}
		if hand.holderConnID == entry.connID {
			return hand, nil
		}
		if hand.requested() && hand.requesterConnID != entry.connID {
			return hand, ErrHandHeld
		}
		hand.requesterConnID = entry.connID
		hand.requesterIdentity = entry.identity
		hand.requestedAt = now
		return hand, nil
	})
}

// GrantControl transfers the hand to a pending requester. Only the current
// holder may grant.
func (c *Controller) GrantControl(sessionID, connID, operator, toConnID string) (HandView, error) {
	toConnID = strings.TrimSpace(toConnID)
	return c.mutateHand(sessionID, connID, func(hand handState, entry presenceEntry, now time.Time) (handState, error) {
		if !hand.held() || hand.holderConnID != entry.connID {
			return hand, ErrNotHolder
		}
		if !hand.requested() {
			return hand, ErrNoPendingRequest
		}
		if toConnID != "" && hand.requesterConnID != toConnID {
			return hand, ErrNoPendingRequest
		}
		// The requester may have disconnected between the roster read and this
		// mutation. Leaving the hand with the current holder is the safe
		// resolution; handing it to nobody is not.
		target, live := c.presence[hand.requesterConnID]
		if !live {
			hand.requesterConnID = ""
			hand.requesterIdentity = ""
			hand.requestedAt = time.Time{}
			return hand, ErrNoPendingRequest
		}
		return handState{
			holderConnID:   target.connID,
			holderIdentity: target.identity,
			since:          now,
		}, nil
	})
}

// DeclineControl refuses a pending request and leaves the hand where it is.
func (c *Controller) DeclineControl(sessionID, connID, operator, toConnID string) (HandView, error) {
	toConnID = strings.TrimSpace(toConnID)
	return c.mutateHand(sessionID, connID, func(hand handState, entry presenceEntry, _ time.Time) (handState, error) {
		if !hand.held() || hand.holderConnID != entry.connID {
			return hand, ErrNotHolder
		}
		if !hand.requested() {
			return hand, ErrNoPendingRequest
		}
		if toConnID != "" && hand.requesterConnID != toConnID {
			return hand, ErrNoPendingRequest
		}
		hand.requesterConnID = ""
		hand.requesterIdentity = ""
		hand.requestedAt = time.Time{}
		return hand, nil
	})
}

// ForceTakeControl seizes a held hand without the holder's consent. It is off
// by default: an operator typing into an agent's terminal can be interrupted
// mid-command, so the deployment must opt in.
func (c *Controller) ForceTakeControl(sessionID, connID, operator string) (HandView, error) {
	return c.mutateHand(sessionID, connID, func(hand handState, entry presenceEntry, now time.Time) (handState, error) {
		if entry.role == string(RoleViewer) {
			return hand, ErrViewerRole
		}
		if !c.allowForceTakeover {
			return hand, ErrForceDisabled
		}
		return handState{
			holderConnID:   entry.connID,
			holderIdentity: entry.identity,
			since:          now,
		}, nil
	})
}

// HoldsHand reports whether a connection may currently write to a session. A
// session nobody has claimed is writable by any operator, which preserves the
// single-operator behavior of deployments that never use sharing.
func (c *Controller) HoldsHand(sessionID, connID string) bool {
	key := sessionKey(sessionID)
	connID = strings.TrimSpace(connID)

	c.mu.Lock()
	c.expireRequestLocked(key, time.Now().UTC())
	hand, found := c.hands[key]
	c.mu.Unlock()

	if !found || !hand.held() {
		return true
	}
	return hand.holderConnID == connID
}

// HandFor returns the current write-lock snapshot for one session.
func (c *Controller) HandFor(sessionID string) HandView {
	key := sessionKey(sessionID)

	c.mu.Lock()
	c.expireRequestLocked(key, time.Now().UTC())
	view := c.handViewLocked(key)
	c.mu.Unlock()

	return view
}

// mutateHand applies one transition under a single lock acquisition, then
// broadcasts outside it. Every exported verb funnels through here so the
// lock-then-broadcast ordering is stated once rather than repeated.
func (c *Controller) mutateHand(
	sessionID, connID string,
	apply func(handState, presenceEntry, time.Time) (handState, error),
) (HandView, error) {
	key := sessionKey(sessionID)
	if key == "" {
		return HandView{}, errors.New("session id is empty")
	}
	connID = strings.TrimSpace(connID)

	c.mu.Lock()
	if atomic.LoadInt32(&c.stopped) != 0 {
		c.mu.Unlock()
		return HandView{}, errors.New("supervisor runtime not ready")
	}
	entry, found := c.presence[connID]
	if !found {
		c.mu.Unlock()
		return HandView{}, ErrUnknownConnection
	}

	now := time.Now().UTC()
	c.expireRequestLocked(key, now)

	next, err := apply(c.hands[key], entry, now)
	if err != nil {
		view := c.handViewLocked(key)
		c.mu.Unlock()
		return view, err
	}

	if c.hands == nil {
		c.hands = make(map[string]handState)
	}
	if next.held() || next.requested() {
		c.hands[key] = next
	} else {
		delete(c.hands, key)
	}
	c.setAttachedLocked(key, next.held())

	handView := c.handViewLocked(key)
	presenceView := c.presenceViewLocked(key)
	c.mu.Unlock()

	c.broadcast(eventHand, handView)
	c.broadcast(eventPresence, presenceView)
	return handView, nil
}

// setAttachedLocked keeps AgentState.Attached meaning "somebody holds this
// terminal". It is the single writer of that field on the sharing path.
func (c *Controller) setAttachedLocked(key string, attached bool) {
	idx, found := c.agentIndex[key]
	if !found || idx >= len(c.state.Agents) {
		return
	}
	c.state.Agents[idx].Attached = attached
	c.state.Agents[idx].HolderIdentity = ""
	if attached {
		c.state.Agents[idx].HolderIdentity = c.hands[key].holderIdentity
	}
	c.state.Agents[idx].ObserverCount = c.observerCountLocked(key)
}

func (c *Controller) observerCountLocked(key string) int {
	count := 0
	for _, entry := range c.presence {
		if _, watching := entry.observing[key]; watching {
			count++
		}
	}
	return count
}

func (c *Controller) presenceViewLocked(key string) PresenceView {
	hand := c.hands[key]
	members := make([]PresenceMember, 0, len(c.presence))
	observers := 0
	for _, entry := range c.presence {
		_, watching := entry.observing[key]
		if watching {
			observers++
		}
		// A connection that holds or wants the hand belongs on the roster even
		// if it never sent an explicit observe.
		if !watching && hand.holderConnID != entry.connID && hand.requesterConnID != entry.connID {
			continue
		}
		members = append(members, PresenceMember{
			ConnID:         entry.connID,
			Identity:       entry.identity,
			Role:           entry.role,
			Observing:      watching,
			HoldsHand:      hand.holderConnID == entry.connID,
			RequestingHand: hand.requesterConnID == entry.connID,
			Since:          entry.since.Format(time.RFC3339),
		})
	}
	sortPresenceMembers(members)

	return PresenceView{
		RunID:         c.runID,
		SessionID:     key,
		Members:       members,
		ObserverCount: observers,
	}
}

func (c *Controller) handViewLocked(key string) HandView {
	hand := c.hands[key]
	view := HandView{
		RunID:     c.runID,
		SessionID: key,
		State:     HandFree,
	}
	if !hand.held() {
		return view
	}

	view.State = HandHeld
	view.HolderConnID = hand.holderConnID
	view.HolderIdentity = hand.holderIdentity
	if !hand.since.IsZero() {
		view.Since = hand.since.Format(time.RFC3339)
	}
	if hand.requested() {
		view.State = HandRequested
		view.RequesterConnID = hand.requesterConnID
		view.RequesterIdentity = hand.requesterIdentity
		view.RequestExpiresAt = hand.requestedAt.Add(c.controlRequestTimeout()).Format(time.RFC3339)
	}
	return view
}

type sessionSnapshot struct {
	presence PresenceView
	hand     HandView
}

func (c *Controller) snapshotsLocked(keys map[string]struct{}) []sessionSnapshot {
	views := make([]sessionSnapshot, 0, len(keys))
	for key := range keys {
		views = append(views, sessionSnapshot{
			presence: c.presenceViewLocked(key),
			hand:     c.handViewLocked(key),
		})
	}
	return views
}

func (c *Controller) broadcastSnapshots(views []sessionSnapshot) {
	for _, view := range views {
		c.broadcast(eventHand, view.hand)
		c.broadcast(eventPresence, view.presence)
	}
}

// sortPresenceMembers gives the roster a stable order so a rerendered list does
// not reshuffle: the holder first, then the requester, then by identity.
func sortPresenceMembers(members []PresenceMember) {
	rank := func(m PresenceMember) int {
		switch {
		case m.HoldsHand:
			return 0
		case m.RequestingHand:
			return 1
		default:
			return 2
		}
	}
	for i := 1; i < len(members); i++ {
		for j := i; j > 0; j-- {
			left, right := members[j-1], members[j]
			if rank(left) < rank(right) {
				break
			}
			if rank(left) == rank(right) && left.Identity <= right.Identity {
				break
			}
			members[j-1], members[j] = right, left
		}
	}
}
