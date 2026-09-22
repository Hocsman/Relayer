package app

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestAfterLockWaitGivesTheOperationItsBudget covers the context an operation
// runs under once it holds the quiescence lock: the caller's deadline is moved
// later by the wait, a caller that gave up first is refused at once, and a nil
// context is tolerated as every other runtime entry point tolerates it.
func TestAfterLockWaitGivesTheOperationItsBudget(t *testing.T) {
	nilled, release := afterLockWait(nil, time.Second)
	release()
	if nilled == nil {
		t.Fatal("a nil context was not replaced")
	}

	caller, giveUp := context.WithTimeout(context.Background(), time.Hour)
	giveUp()
	owned, done := afterLockWait(caller, time.Second)
	if owned.Err() == nil {
		t.Fatal("an operation began under a context its caller had already cancelled")
	}
	done()

	short, expire := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer expire()
	time.Sleep(40 * time.Millisecond)
	moved, stop := afterLockWait(short, 2*time.Second)
	defer stop()
	if moved.Err() != nil {
		t.Fatalf("the deadline was not moved past the lock wait: %v", moved.Err())
	}

	live, cancelLive := context.WithTimeout(context.Background(), time.Hour)
	defer cancelLive()
	running, end := afterLockWait(live, time.Second)
	defer end()
	cancelLive()
	deadline := time.Now().Add(time.Second)
	for running.Err() == nil && time.Now().Before(deadline) {
		time.Sleep(time.Millisecond)
	}
	if !errors.Is(running.Err(), context.Canceled) {
		t.Fatalf("a cancellation did not end the operation: %v", running.Err())
	}
}
