//go:build linux

package session

import (
	"context"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/platform"
	"golang.org/x/sys/unix"
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
		return
	}

	if !platform.ProcessGroupLivenessIsExact() {
		t.Skip("/proc does not show every process here, so a zombie counts as live")
	}
	if err := unix.Prctl(unix.PR_SET_CHILD_SUBREAPER, 1, 0, 0, 0); err != nil {
		t.Skipf("PR_SET_CHILD_SUBREAPER: %v", err)
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
	// Nothing but Relayer would ever reap the orphans it adopted, so a Stop
	// that left them would leave them in the process table for good.
	if zombies := zombieChildren(t); len(zombies) != 0 {
		t.Fatalf("the stop left zombie children behind: %v", zombies)
	}
}

// zombieChildren lists this process's children that have exited and not been
// waited for.
func zombieChildren(t *testing.T) []string {
	t.Helper()
	entries, err := os.ReadDir("/proc")
	if err != nil {
		t.Fatalf("list /proc: %v", err)
	}
	self := strconv.Itoa(os.Getpid())
	var zombies []string
	for _, entry := range entries {
		stat, err := os.ReadFile("/proc/" + entry.Name() + "/stat")
		if err != nil {
			continue
		}
		name := strings.LastIndexByte(string(stat), ')')
		if name < 0 {
			continue
		}
		// state ppid ...
		fields := strings.Fields(string(stat[name+1:]))
		if len(fields) >= 2 && fields[0] == "Z" && fields[1] == self {
			zombies = append(zombies, strings.TrimSpace(string(stat)))
		}
	}
	return zombies
}
