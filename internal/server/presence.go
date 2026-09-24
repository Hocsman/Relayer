package server

import (
	"errors"
	"strings"
	"sync/atomic"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
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
//
// A hand freed this way is journaled as control_released by the system, so a
// journal never shows somebody taking a terminal and then simply stops. A
// withdrawn or expired request moves no terminal and is not journaled.
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
	var dropped []handTransition
	for key, hand := range c.hands {
		changed := false
		if hand.holderConnID == connID {
			if idx, known := c.agentIndex[key]; known && idx < len(c.state.Agents) {
				dropped = append(dropped, handTransition{
					actor:  entry,
					agent:  c.state.Agents[idx],
					before: hand,
				})
			}
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
	for _, change := range dropped {
		c.recordControlAudit(change, controlRecord{
			kind:    audit.KindControlReleased,
			by:      audit.DecisionBySystem,
			outcome: audit.OutcomeApplied,
			reason:  "control_released_disconnect",
		})
	}
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
//
// It journals nothing itself: its only caller is the attach verb, which
// records attach_started, so one action never produces two records.
func (c *Controller) TakeControl(sessionID, connID, operator string) (HandView, error) {
	view, _, err := c.mutateHand(sessionID, connID, func(hand handState, entry presenceEntry, now time.Time) (handState, error) {
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
	return view, err
}

// ReleaseControl frees a hand held by this connection and journals it as
// control_released. Releasing a hand nobody holds changes nothing and is not
// journaled.
func (c *Controller) ReleaseControl(sessionID, connID, operator string) (HandView, error) {
	view, change, err := c.releaseHand(sessionID, connID)
	if err == nil && change.known() && change.before.held() {
		c.recordControlAudit(change, controlRecord{
			kind:    audit.KindControlReleased,
			by:      audit.DecisionByHuman,
			outcome: audit.OutcomeApplied,
			reason:  "control_released",
		})
	}
	return view, err
}

// releaseHand is ReleaseControl without the journal entry. The attach verb
// releases through it and records attach_finished instead.
func (c *Controller) releaseHand(sessionID, connID string) (HandView, handTransition, error) {
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
// retries is not punished, and is not journaled a second time either.
func (c *Controller) RequestControl(sessionID, connID, operator string) (HandView, error) {
	view, change, err := c.mutateHand(sessionID, connID, func(hand handState, entry presenceEntry, now time.Time) (handState, error) {
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
	if err != nil || !change.known() {
		return view, err
	}

	actor := change.actor.connID
	switch {
	case !change.before.held() && change.after.holderConnID == actor:
		// The hand was free and the request took it at once. The record says
		// so rather than posing as a request somebody could still answer.
		c.recordControlAudit(change, controlRecord{
			kind:    audit.KindControlRequested,
			by:      audit.DecisionByHuman,
			outcome: audit.OutcomeApplied,
			reason:  "control_taken_free",
		})
	case change.before.held() && change.before.requesterConnID != actor && change.after.requesterConnID == actor:
		c.recordControlAudit(change, controlRecord{
			kind:           audit.KindControlRequested,
			by:             audit.DecisionByHuman,
			outcome:        audit.OutcomePending,
			reason:         "control_requested",
			targetConnID:   change.before.holderConnID,
			targetIdentity: change.before.holderIdentity,
		})
	}
	return view, nil
}

// GrantControl transfers the hand to a pending requester. Only the current
// holder may grant.
func (c *Controller) GrantControl(sessionID, connID, operator, toConnID string) (HandView, error) {
	toConnID = strings.TrimSpace(toConnID)
	view, change, err := c.mutateHand(sessionID, connID, func(hand handState, entry presenceEntry, now time.Time) (handState, error) {
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
	if err == nil && change.known() {
		c.recordControlAudit(change, controlRecord{
			kind:           audit.KindControlGranted,
			by:             audit.DecisionByHuman,
			outcome:        audit.OutcomeApplied,
			reason:         "control_granted",
			targetConnID:   change.after.holderConnID,
			targetIdentity: change.after.holderIdentity,
		})
	}
	return view, err
}

// DeclineControl refuses a pending request and leaves the hand where it is.
func (c *Controller) DeclineControl(sessionID, connID, operator, toConnID string) (HandView, error) {
	toConnID = strings.TrimSpace(toConnID)
	view, change, err := c.mutateHand(sessionID, connID, func(hand handState, entry presenceEntry, _ time.Time) (handState, error) {
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
	if err == nil && change.known() {
		c.recordControlAudit(change, controlRecord{
			kind:           audit.KindControlDeclined,
			by:             audit.DecisionByHuman,
			outcome:        audit.OutcomeApplied,
			reason:         "control_declined",
			targetConnID:   change.before.requesterConnID,
			targetIdentity: change.before.requesterIdentity,
		})
	}
	return view, err
}

// ForceTakeControl seizes a held hand without the holder's consent. It is off
// by default: an operator typing into an agent's terminal can be interrupted
// mid-command, so the deployment must opt in.
//
// Every attempt by a known connection is journaled as control_forced, refused
// ones included: trying to seize a colleague's terminal is the event worth
// finding later, whether or not the deployment allowed it.
func (c *Controller) ForceTakeControl(sessionID, connID, operator string) (HandView, error) {
	view, change, err := c.mutateHand(sessionID, connID, func(hand handState, entry presenceEntry, now time.Time) (handState, error) {
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
	if !change.known() {
		return view, err
	}

	forced := controlRecord{
		kind:    audit.KindControlForced,
		by:      audit.DecisionByHuman,
		outcome: audit.OutcomeApplied,
		reason:  "control_forced",
	}
	switch {
	case errors.Is(err, ErrForceDisabled):
		forced.outcome, forced.reason = audit.OutcomeFailed, "control_force_disabled"
	case errors.Is(err, ErrViewerRole):
		forced.outcome, forced.reason = audit.OutcomeFailed, "control_force_viewer_denied"
	case err != nil:
		forced.outcome, forced.reason = audit.OutcomeFailed, "control_force_failed"
	}
	// The displaced holder, not the operator's own earlier hold.
	if change.before.held() && change.before.holderConnID != change.actor.connID {
		forced.targetConnID = change.before.holderConnID
		forced.targetIdentity = change.before.holderIdentity
	}
	c.recordControlAudit(change, forced)
	return view, err
}

// HoldsHand reports whether a connection may currently resize a session's
// terminal. A session nobody has claimed may be resized by any operator: a
// resize writes nothing the agent reads. Keystrokes are stricter, and need the
// hand held by the connection that types them (SendTerminalInput).
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

// handTransition is what one verb found and left, captured under the same lock
// acquisition as the change itself, so a journal entry describes the state the
// verb actually acted on rather than a later re-read of it.
type handTransition struct {
	actor  presenceEntry
	agent  AgentState
	before handState
	after  handState
}

// known reports whether the verb reached a real session through a registered
// connection. Anything short of that changed nothing and names nothing worth
// journaling.
func (t handTransition) known() bool {
	return t.actor.connID != ""
}

// mutateHand applies one transition under a single lock acquisition, then
// broadcasts outside it. Every exported verb funnels through here so the
// lock-then-broadcast ordering is stated once rather than repeated.
//
// A session the run never started is refused, as the attach verb already
// refused it: holding a terminal that does not exist means nothing, and an
// unchecked session ID would let a client grow the hand table without bound.
func (c *Controller) mutateHand(
	sessionID, connID string,
	apply func(handState, presenceEntry, time.Time) (handState, error),
) (HandView, handTransition, error) {
	key := sessionKey(sessionID)
	if key == "" {
		return HandView{}, handTransition{}, errors.New("session id is empty")
	}
	connID = strings.TrimSpace(connID)

	c.mu.Lock()
	if atomic.LoadInt32(&c.stopped) != 0 {
		c.mu.Unlock()
		return HandView{}, handTransition{}, errors.New("supervisor runtime not ready")
	}
	entry, found := c.presence[connID]
	if !found {
		c.mu.Unlock()
		return HandView{}, handTransition{}, ErrUnknownConnection
	}
	idx, known := c.agentIndex[key]
	if !known || idx >= len(c.state.Agents) {
		c.mu.Unlock()
		return HandView{}, handTransition{}, errUnknownSession
	}

	now := time.Now().UTC()
	c.expireRequestLocked(key, now)

	change := handTransition{
		actor:  entry,
		agent:  c.state.Agents[idx],
		before: c.hands[key],
	}
	next, err := apply(change.before, entry, now)
	if err != nil {
		change.after = change.before
		view := c.handViewLocked(key)
		c.mu.Unlock()
		return view, change, err
	}
	change.after = next

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
	return handView, change, nil
}

// controlRecord is one journaled hand transition. The target is the other
// party to it: the holder asked, the operator granted to or declined, or the
// holder displaced.
type controlRecord struct {
	kind           audit.Kind
	by             audit.DecisionBy
	outcome        audit.Outcome
	reason         string
	targetConnID   string
	targetIdentity string
}

// recordControlAudit journals a transition after the lock is released and the
// snapshots are out: an audit write is synchronous file I/O, and nothing that
// holds c.mu may wait on a disk.
//
// A write failure is not reported to the operator, matching the attach and
// recording records. The hand has already moved by then, and refusing the
// answer would leave the interface disagreeing with the server about who
// holds the terminal.
func (c *Controller) recordControlAudit(change handTransition, rec controlRecord) {
	c.mu.RLock()
	rt := c.runtime
	c.mu.RUnlock()
	if rt == nil {
		return
	}

	operator := change.actor.identity
	if operator == "" {
		operator = "operator"
	}
	role := change.actor.role
	if role == "" {
		role = string(RoleOperator)
	}
	metadata := map[string]string{
		"operator": operator,
		"role":     role,
		"conn_id":  change.actor.connID,
	}
	if rec.targetConnID != "" {
		target := rec.targetIdentity
		if target == "" {
			target = "operator"
		}
		metadata["target_operator"] = target
		metadata["target_conn_id"] = rec.targetConnID
	}

	_ = rt.RecordAudit(audit.Entry{
		Kind:       rec.kind,
		SessionID:  change.agent.SessionID,
		AgentID:    change.agent.AgentID,
		Backend:    strings.ToLower(strings.TrimSpace(change.agent.Backend)),
		Adapter:    strings.ToLower(strings.TrimSpace(change.agent.Adapter)),
		DecisionBy: rec.by,
		Operator:   operator,
		Outcome:    rec.outcome,
		Reason:     rec.reason,
		Metadata:   metadata,
	})
}

// setAttachedLocked keeps AgentState.Attached meaning "somebody holds this
// terminal". It is the single writer of that field on the sharing path, and
// so where the run's supervision core learns who holds the hand: under the
// lock the hand changed under, before any other goroutine can see it moved.
//
// The holder types into the agent directly, and raw keystrokes never resolve
// the runtime's pending prompt. The core answers nothing automatically on a
// terminal somebody holds, and never again a prompt that was pending while it
// was held; told late, or not at all, it answered a prompt its holder had
// just answered by typing, and the agent received a second answer. SetHolder
// never calls the sink on its caller's goroutine, which is what makes it safe
// under c.mu.
func (c *Controller) setAttachedLocked(key string, attached bool) {
	idx, found := c.agentIndex[key]
	if !found || idx >= len(c.state.Agents) {
		return
	}
	c.state.Agents[idx].Attached = attached
	c.state.Agents[idx].HolderIdentity = ""
	holder := ""
	if attached {
		c.state.Agents[idx].HolderIdentity = c.hands[key].holderIdentity
		holder = c.hands[key].holderConnID
	}
	c.state.Agents[idx].ObserverCount = c.observerCountLocked(key)
	c.sup.SetHolder(c.state.Agents[idx].SessionID, holder)
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
