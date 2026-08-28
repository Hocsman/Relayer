//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tmuxbackend

import (
	"os"
	"testing"
)

// requireTmux decides what an absent tmux means.
//
// On a developer machine without tmux the test has nothing to exercise and
// skipping is right. In CI that installs tmux, a lookup failure means the
// installation or the PATH broke, and skipping would turn that into a green
// run that verified nothing. RELAYER_REQUIRE_TMUX tells the two apart.
func requireTmux(t *testing.T, cause error) {
	t.Helper()
	if os.Getenv("RELAYER_REQUIRE_TMUX") == "1" {
		t.Fatalf("tmux is required in this environment but was not found: %v", cause)
	}
	t.Skipf("tmux is not installed: %v", cause)
}
