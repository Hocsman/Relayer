package supervisetest

import (
	"testing"
	"time"
)

// WaitFor returns once condition holds, and fails t, naming what it waited
// for, when it still does not once timeout has passed. condition is checked at
// least once, then every millisecond. A test waits on what the core does, not
// for how long it may take: a sleep long enough on one machine is too short
// on a loaded one. Like t.Fatalf, it must be called from the test's goroutine.
func WaitFor(t testing.TB, timeout time.Duration, what string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if !time.Now().Before(deadline) {
			t.Fatalf("timed out after %v waiting for %s", timeout, what)
			return
		}
		time.Sleep(time.Millisecond)
	}
}
