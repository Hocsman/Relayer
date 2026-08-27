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
