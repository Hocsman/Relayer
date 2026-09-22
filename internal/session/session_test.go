package session

import (
	"errors"
	"os/exec"
	"testing"
	"time"
)

func TestWaitForStopReturnsUncertainWhenLeaderDoesNotExit(t *testing.T) {
	session := &processSession{done: make(chan struct{})}
	started := time.Now()
	err := session.waitForStopWithin(time.Millisecond, time.Millisecond)
	if !errors.Is(err, ErrStopUncertain) {
		t.Fatalf("waitForStopWithin error = %v, want %v", err, ErrStopUncertain)
	}
	if elapsed := time.Since(started); elapsed > 250*time.Millisecond {
		t.Fatalf("unconfirmed stop exceeded its bounded wait: %s", elapsed)
	}
}

func TestWaitForStopReturnsUncertainWhenProcessGroupRemains(t *testing.T) {
	done := make(chan struct{})
	close(done)
	kills := 0
	session := &processSession{
		done: done,
		killGroup: func(*exec.Cmd) {
			kills++
		},
		groupExists: func(*exec.Cmd) bool {
			return true
		},
	}
	err := session.waitForStopWithin(time.Millisecond, 2*time.Millisecond)
	if !errors.Is(err, ErrStopUncertain) {
		t.Fatalf("waitForStopWithin error = %v, want %v", err, ErrStopUncertain)
	}
	if kills != 1 {
		t.Fatalf("forced process-group kills = %d, want 1", kills)
	}
}

func TestWaitForStopAcceptsConfirmedCompletion(t *testing.T) {
	done := make(chan struct{})
	close(done)
	session := &processSession{
		done:        done,
		groupExists: func(*exec.Cmd) bool { return false },
	}
	if err := session.waitForStopWithin(time.Millisecond, time.Millisecond); err != nil {
		t.Fatalf("waitForStopWithin confirmed completion: %v", err)
	}
}

// TestForcedStopNeverSignalsAReapedLeader pins the v0.8.5 regression. Closing
// the PTY at the end of the grace period can end the leader, and it can be
// reaped before the forced kill runs. v0.8.5 killed by number regardless, and on
// Windows taskkill /F then killed whatever process had just received that PID.
func TestForcedStopNeverSignalsAReapedLeader(t *testing.T) {
	kills := 0
	session := &processSession{
		done:      make(chan struct{}),
		killGroup: func(*exec.Cmd) { kills++ },
		groupExists: func(*exec.Cmd) bool {
			t.Error("the stop path probed a reaped leader's number")
			return true
		},
	}
	session.setResult(nil, nil)

	err := session.waitForStopWithin(time.Millisecond, time.Millisecond)
	if kills != 0 {
		t.Fatalf("forced kills of a reaped leader = %d, want 0", kills)
	}
	// Reaped but not settled: waitSession still owns the group, so the stop is
	// reported as unconfirmed rather than confirmed by guesswork.
	if !errors.Is(err, ErrStopUncertain) {
		t.Fatalf("waitForStopWithin error = %v, want %v", err, ErrStopUncertain)
	}
}

// TestSettledGroupIsNeverAddressedAgain covers every later Stop, Close or
// Restart of a session that has already exited: once waitSession has settled the
// group, the answer is the latch and nothing is probed or killed.
func TestSettledGroupIsNeverAddressedAgain(t *testing.T) {
	for _, leftover := range []bool{false, true} {
		done := make(chan struct{})
		close(done)
		session := &processSession{
			done: done,
			killGroup: func(*exec.Cmd) {
				t.Error("a settled group was killed by number")
			},
			groupExists: func(*exec.Cmd) bool {
				t.Error("a settled group was probed by number")
				return true
			},
		}
		session.setResult(nil, nil)
		session.settleGroup(leftover)

		session.requestStop()
		err := session.waitForStopWithin(time.Millisecond, time.Millisecond)
		if leftover && !errors.Is(err, ErrStopUncertain) {
			t.Errorf("leftover group: error = %v, want %v", err, ErrStopUncertain)
		}
		if !leftover && err != nil {
			t.Errorf("settled group: error = %v, want nil", err)
		}
	}
}

func TestSettleDescendantsLatchesWithoutKillingAnEmptyGroup(t *testing.T) {
	kills := 0
	session := &processSession{
		killGroup:   func(*exec.Cmd) { kills++ },
		groupExists: func(*exec.Cmd) bool { return false },
	}
	session.settleDescendants()
	if kills != 0 {
		t.Fatalf("kills of an empty group = %d, want 0", kills)
	}
	if settled, leftover := session.groupOutcome(); !settled || leftover {
		t.Fatalf("outcome = settled %v, leftover %v; want settled and clean", settled, leftover)
	}
}

func TestSettleDescendantsKillsAndReportsASurvivingGroup(t *testing.T) {
	kills := 0
	session := &processSession{
		killGroup:   func(*exec.Cmd) { kills++ },
		groupExists: func(*exec.Cmd) bool { return true },
	}
	session.settleDescendants()
	if kills != 1 {
		t.Fatalf("kills of a surviving group = %d, want 1", kills)
	}
	if settled, leftover := session.groupOutcome(); !settled || !leftover {
		t.Fatalf("outcome = settled %v, leftover %v; want settled with a leftover", settled, leftover)
	}
}
