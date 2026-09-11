// Package policy evaluates immutable, side-effect-free automation rules for
// semantic agent events. It deliberately does not encode or deliver a
// decision: callers must fall back to a human whenever an adapter cannot
// represent the proposed action or a delivery fails.
package policy

import (
	"sync"
	"time"
)

// Tracker manages session-level execution history for policy guardrails,
// including consecutive automatic decisions and sliding-window rate limits.
// It is thread-safe and safe for concurrent calls across different sessions.
type Tracker struct {
	mu          sync.Mutex
	clock       func() time.Time
	consecutive map[string]int
	history     map[string][]time.Time
}

// NewTracker creates a Tracker using the system clock.
func NewTracker() *Tracker {
	return NewTrackerWithClock(time.Now)
}

// NewTrackerWithClock creates a Tracker with a custom clock function,
// primarily for deterministic unit testing.
func NewTrackerWithClock(clock func() time.Time) *Tracker {
	if clock == nil {
		clock = time.Now
	}
	return &Tracker{
		clock:       clock,
		consecutive: make(map[string]int),
		history:     make(map[string][]time.Time),
	}
}

// CheckLimits checks whether an automatic decision is allowed for the given sessionID
// against the configured MaxConsecutiveAutoDecisions and RateLimitPerMinute.
// If a limit is exceeded, it returns (ActionAsk, reason, false).
// If limits are respected, it returns (ActionAllow, "", true).
func (t *Tracker) CheckLimits(sessionID string, config Config) (Action, string, bool) {
	if t == nil {
		return ActionAllow, "", true
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	// Check consecutive limit
	if config.MaxConsecutiveAutoDecisions > 0 {
		if t.consecutive[sessionID] >= config.MaxConsecutiveAutoDecisions {
			return ActionAsk, ReasonConsecutiveLimit, false
		}
	}

	// Check rate limit (sliding window of 1 minute)
	if config.RateLimitPerMinute > 0 {
		now := t.clock()
		windowStart := now.Add(-1 * time.Minute)
		recent := t.history[sessionID]
		// Prune entries older than 1 minute
		validIdx := 0
		for _, ts := range recent {
			if !ts.Before(windowStart) {
				recent[validIdx] = ts
				validIdx++
			}
		}
		recent = recent[:validIdx]
		t.history[sessionID] = recent

		if len(recent) >= config.RateLimitPerMinute {
			return ActionAsk, ReasonRateLimit, false
		}
	}

	return ActionAllow, "", true
}

// RecordAutoDecision registers that an automatic decision was delivered for the session.
// It increments the consecutive counter and appends the current timestamp to the history window.
func (t *Tracker) RecordAutoDecision(sessionID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	t.consecutive[sessionID]++
	now := t.clock()
	t.history[sessionID] = append(t.history[sessionID], now)
}

// RecordHumanDecision registers that an operator intervened for the session.
// It resets the consecutive counter to 0. Rate limiting timestamps remain intact.
func (t *Tracker) RecordHumanDecision(sessionID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	t.consecutive[sessionID] = 0
}

// Consecutive returns the current consecutive automatic decision count for the session.
func (t *Tracker) Consecutive(sessionID string) int {
	if t == nil {
		return 0
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	return t.consecutive[sessionID]
}

// Reset clears all consecutive counters and history for the specified session.
func (t *Tracker) Reset(sessionID string) {
	if t == nil {
		return
	}
	t.mu.Lock()
	defer t.mu.Unlock()

	delete(t.consecutive, sessionID)
	delete(t.history, sessionID)
}
