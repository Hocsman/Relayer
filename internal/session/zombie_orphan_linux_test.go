//go:build linux

package session

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
)

const nonReapingSubreaperEnv = "RELAYER_TEST_NON_REAPING_SUBREAPER"

// TestStopIsConfirmedWhenAnOrphanIsLeftAsAZombie runs in a child process that
// adopts orphans and never reaps them, as Relayer does when it is PID 1 in a
// container started without an init. The agent's shell starts a child and is
// replaced by a program that never waits for it, so once the group is killed
// that child stays in the group as a zombie. A zombie runs nothing and holds
// no descriptor, so the stop must be confirmed.
func TestStopIsConfirmedWhenAnOrphanIsLeftAsAZombie(t *testing.T) {
	if os.Getenv(nonReapingSubreaperEnv) == "" {
		child := exec.Command(os.Args[0], "-test.run=^TestStopIsConfirmedWhenAnOrphanIsLeftAsAZombie$", "-test.count=1", "-test.v", "-test.timeout=1m")
		child.Env = append(os.Environ(), nonReapingSubreaperEnv+"=1")
		output, err := child.CombinedOutput()
		if err != nil {
			t.Fatalf("the non-reaping subreaper failed: %v\n%s", err, output)
		}
		if strings.Contains(string(output), "--- SKIP") {
			t.Skipf("the non-reaping subreaper skipped:\n%s", output)
		}
		return
	}

	const prSetChildSubreaper = 36
	if _, _, errno := syscall.RawSyscall(syscall.SYS_PRCTL, prSetChildSubreaper, 1, 0); errno != 0 {
		t.Skipf("PR_SET_CHILD_SUBREAPER: %v", errno)
	}

	events := make(chan Event, 64)
	manager, err := NewManager(context.Background(), events, integrationPatterns, 1024)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	defer manager.Close()

	info, err := manager.Start(agent.Spec{
		ID:    "orphaning",
		Name:  "orphaning",
		Shell: `sleep 30 & printf 'READY\n'; exec sleep 60`,
	}, 40, 10)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.After(3 * time.Second)
	for {
		if content, _ := manager.Output(info.ID); strings.Contains(content, "READY") {
			break
		}
		select {
		case <-events:
		case <-deadline:
			t.Fatal("the agent never became ready")
		}
	}

	if err := manager.Stop(info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}
