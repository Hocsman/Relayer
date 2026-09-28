//go:build linux

package fixturecapture

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/platform"
	"golang.org/x/sys/unix"
)

const nonReapingSubreaperEnv = "RELAYER_TEST_NON_REAPING_SUBREAPER"

// inNonReapingSubreaper runs the calling test again in a child process that
// adopts orphans and never reaps them, as relayer-capture does when it is
// PID 1 in a container started without an init. It returns true in that
// child, which runs the test's body, and false in the parent once the child
// has passed.
func inNonReapingSubreaper(t *testing.T) bool {
	t.Helper()
	if os.Getenv(nonReapingSubreaperEnv) == "" {
		child := exec.Command(os.Args[0], "-test.run=^"+regexp.QuoteMeta(t.Name())+"$", "-test.count=1", "-test.v", "-test.timeout=1m")
		child.Env = append(os.Environ(), nonReapingSubreaperEnv+"=1")
		output, err := child.CombinedOutput()
		if err != nil {
			t.Fatalf("the non-reaping subreaper failed: %v\n%s", err, output)
		}
		if strings.Contains(string(output), "--- SKIP: "+t.Name()) {
			t.Skipf("the non-reaping subreaper skipped:\n%s", output)
		}
		if !strings.Contains(string(output), "--- PASS: "+t.Name()) {
			t.Fatalf("the non-reaping subreaper did not run %s:\n%s", t.Name(), output)
		}
		return false
	}
	if !platform.ProcessGroupLivenessIsExact() {
		t.Skip("/proc does not show every process here, so a zombie counts as live")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("PR_SET_CHILD_SUBREAPER: %v", err)
	}
	return true
}

// TestACaptureEndsWhenAnOrphanIsLeftAsAZombie: the captured leader exits and
// leaves a descendant, which the capture kills. It stays in the group as a
// zombie, runs nothing and holds nothing, so the capture must end cleanly.
func TestACaptureEndsWhenAnOrphanIsLeftAsAZombie(t *testing.T) {
	if !inNonReapingSubreaper(t) {
		return
	}
	directory := t.TempDir()
	options := captureOptions(t, BackendPTY, "spawn-descendant-then-exit",
		filepath.Join(directory, "descendant.pid"), filepath.Join(directory, "descendant.ready"))
	fixture, err := Capture(context.Background(), options)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if fixture.Outcome != OutcomeExited {
		t.Fatalf("outcome = %q, want %q", fixture.Outcome, OutcomeExited)
	}
}

// TestATimedOutCaptureEndsWhenAnOrphanIsLeftAsAZombie: the capture times out
// and kills the whole group while the leader still runs.
func TestATimedOutCaptureEndsWhenAnOrphanIsLeftAsAZombie(t *testing.T) {
	if !inNonReapingSubreaper(t) {
		return
	}
	options := captureOptions(t, BackendPTY, "spawn", filepath.Join(t.TempDir(), "child.pid"))
	options.Timeout = 300 * time.Millisecond
	fixture, err := Capture(context.Background(), options)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if fixture.Outcome != OutcomeTimedOut {
		t.Fatalf("outcome = %q, want %q", fixture.Outcome, OutcomeTimedOut)
	}
}

// TestATmuxCaptureEndsWhenAnOrphanIsLeftAsAZombie: with tmux the pane's group
// is killed after the private server, and the same zombie stays in it.
func TestATmuxCaptureEndsWhenAnOrphanIsLeftAsAZombie(t *testing.T) {
	if !inNonReapingSubreaper(t) {
		return
	}
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		requireTmux(t, err)
	}
	helperPath, _ := tmuxTestHelper(t)
	directory := t.TempDir()
	options := captureOptions(t, BackendTmux, "spawn-descendant-then-exit",
		filepath.Join(directory, "descendant.pid"), filepath.Join(directory, "descendant.ready"))
	options.TmuxPath = tmuxPath
	options.HelperPath = helperPath
	fixture, err := Capture(context.Background(), options)
	if err != nil {
		t.Fatalf("Capture: %v", err)
	}
	if fixture.Outcome != OutcomeExited {
		t.Fatalf("outcome = %q, want %q", fixture.Outcome, OutcomeExited)
	}
}
