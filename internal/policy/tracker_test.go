package policy

import (
	"sync"
	"testing"
	"time"
)

func TestTrackerConsecutiveLimit(t *testing.T) {
	now := time.Now()
	tracker := NewTrackerWithClock(func() time.Time { return now })
	config := Config{
		MaxConsecutiveAutoDecisions: 3,
	}

	sessionID := "session-1"

	// Initial check should be allowed
	action, reason, ok := tracker.CheckLimits(sessionID, config)
	if !ok || action != ActionAllow || reason != "" {
		t.Fatalf("expected allowed, got action=%v, reason=%v, ok=%v", action, reason, ok)
	}

	// 1st auto decision
	tracker.RecordAutoDecision(sessionID)
	if count := tracker.Consecutive(sessionID); count != 1 {
		t.Fatalf("expected consecutive=1, got %d", count)
	}
	_, _, ok = tracker.CheckLimits(sessionID, config)
	if !ok {
		t.Fatal("expected allowed after 1st auto decision")
	}

	// 2nd auto decision
	tracker.RecordAutoDecision(sessionID)
	if count := tracker.Consecutive(sessionID); count != 2 {
		t.Fatalf("expected consecutive=2, got %d", count)
	}
	_, _, ok = tracker.CheckLimits(sessionID, config)
	if !ok {
		t.Fatal("expected allowed after 2nd auto decision")
	}

	// 3rd auto decision
	tracker.RecordAutoDecision(sessionID)
	if count := tracker.Consecutive(sessionID); count != 3 {
		t.Fatalf("expected consecutive=3, got %d", count)
	}

	// Now check limits - should trigger consecutive limit
	action, reason, ok = tracker.CheckLimits(sessionID, config)
	if ok || action != ActionAsk || reason != ReasonConsecutiveLimit {
		t.Fatalf("expected blocked by consecutive limit, got action=%v, reason=%v, ok=%v", action, reason, ok)
	}

	// Human intervenes
	tracker.RecordHumanDecision(sessionID)
	if count := tracker.Consecutive(sessionID); count != 0 {
		t.Fatalf("expected consecutive=0 after human decision, got %d", count)
	}

	// Should be allowed again
	action, reason, ok = tracker.CheckLimits(sessionID, config)
	if !ok || action != ActionAllow || reason != "" {
		t.Fatalf("expected allowed after human reset, got action=%v, reason=%v, ok=%v", action, reason, ok)
	}
}

func TestTrackerRateLimit(t *testing.T) {
	currentTime := time.Date(2026, 1, 1, 12, 0, 0, 0, time.UTC)
	tracker := NewTrackerWithClock(func() time.Time { return currentTime })
	config := Config{
		RateLimitPerMinute: 2,
	}

	sessionID := "session-rate"

	// 1st auto decision at T=0
	tracker.RecordAutoDecision(sessionID)
	action, reason, ok := tracker.CheckLimits(sessionID, config)
	if !ok {
		t.Fatalf("expected allowed, got %v, %v, %v", action, reason, ok)
	}

	// 2nd auto decision at T=10s
	currentTime = currentTime.Add(10 * time.Second)
	tracker.RecordAutoDecision(sessionID)

	// Now limit is reached (2 decisions within 1 minute window)
	action, reason, ok = tracker.CheckLimits(sessionID, config)
	if ok || action != ActionAsk || reason != ReasonRateLimit {
		t.Fatalf("expected rate limit exceeded, got action=%v, reason=%v, ok=%v", action, reason, ok)
	}

	// Advance time by 51s (total 61s from 1st decision)
	// 1st decision at T=0 should now expire, leaving only the 2nd decision at T=10s
	currentTime = currentTime.Add(51 * time.Second)
	action, reason, ok = tracker.CheckLimits(sessionID, config)
	if !ok || action != ActionAllow {
		t.Fatalf("expected allowed after window slid, got action=%v, reason=%v, ok=%v", action, reason, ok)
	}

	// Human decision does NOT reset rate limit history
	tracker.RecordHumanDecision(sessionID)
	tracker.RecordAutoDecision(sessionID) // 2nd decision in current window
	action, reason, ok = tracker.CheckLimits(sessionID, config)
	if ok || action != ActionAsk || reason != ReasonRateLimit {
		t.Fatalf("expected rate limit to still hold after human intervention, got %v, %v, %v", action, reason, ok)
	}
}

func TestTrackerMultiSessionAndReset(t *testing.T) {
	tracker := NewTracker()
	config := Config{
		MaxConsecutiveAutoDecisions: 2,
	}

	sessionA := "session-A"
	sessionB := "session-B"

	tracker.RecordAutoDecision(sessionA)
	tracker.RecordAutoDecision(sessionA)

	// sessionA reached limit
	_, _, okA := tracker.CheckLimits(sessionA, config)
	if okA {
		t.Fatal("sessionA should be blocked")
	}

	// sessionB should be unaffected
	_, _, okB := tracker.CheckLimits(sessionB, config)
	if !okB {
		t.Fatal("sessionB should be allowed")
	}

	// Reset sessionA
	tracker.Reset(sessionA)
	if count := tracker.Consecutive(sessionA); count != 0 {
		t.Fatalf("expected 0 after reset, got %d", count)
	}
	_, _, okA = tracker.CheckLimits(sessionA, config)
	if !okA {
		t.Fatal("sessionA should be allowed after reset")
	}
}

func TestTrackerConcurrency(t *testing.T) {
	tracker := NewTracker()
	config := Config{
		MaxConsecutiveAutoDecisions: 100,
		RateLimitPerMinute:          200,
	}

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		sessionID := "session-concurrent"
		go func() {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				tracker.RecordAutoDecision(sessionID)
				tracker.CheckLimits(sessionID, config)
				if j%10 == 0 {
					tracker.RecordHumanDecision(sessionID)
				}
			}
		}()
	}
	wg.Wait()
}
