//go:build windows

package session

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
	"golang.org/x/sys/windows"
)

// ctrlCloseHelperEnv turns the test binary into an agent that needs three
// seconds to save its state when its console closes.
const ctrlCloseHelperEnv = "RELAYER_TEST_CTRL_CLOSE_MARKER"

// detachHelperEnv turns the test binary into an agent that leaves its console,
// so closing the console never reaches it.
const detachHelperEnv = "RELAYER_TEST_DETACH_FROM_CONSOLE"

func TestMain(m *testing.M) {
	if marker := os.Getenv(ctrlCloseHelperEnv); marker != "" {
		runCtrlCloseHelper(marker)
		return
	}
	if os.Getenv(detachHelperEnv) != "" {
		fmt.Println("READY")
		_, _, _ = windows.NewLazySystemDLL("kernel32.dll").NewProc("FreeConsole").Call()
		time.Sleep(time.Hour)
	}
	os.Exit(m.Run())
}

// runCtrlCloseHelper waits for CTRL_CLOSE_EVENT, which Go delivers as SIGTERM
// and keeps the process alive for while it is handled, then saves and exits.
func runCtrlCloseHelper(marker string) {
	closing := make(chan os.Signal, 1)
	signal.Notify(closing, syscall.SIGTERM)
	fmt.Println("READY")
	<-closing
	time.Sleep(3 * time.Second)
	_ = os.WriteFile(marker, []byte("saved"), 0o600)
	os.Exit(0)
}

// TestAnAgentGetsTimeToHandleItsConsoleClosing: once closing the console
// stopped blocking, a Stop killed an agent 1.5 seconds into its close handler,
// the Unix SIGTERM grace, where v0.8.5 let it finish within Windows' own limit.
func TestAnAgentGetsTimeToHandleItsConsoleClosing(t *testing.T) {
	manager := newWindowsTestManager(t)
	marker := filepath.Join(t.TempDir(), "saved")
	info, err := manager.Start(agent.Spec{
		ID:      "saves-on-close",
		Name:    "saves on close",
		Command: []string{os.Args[0], "-test.run=^$"},
		Env:     map[string]string{ctrlCloseHelperEnv: marker},
	}, 80, 24)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if output, _ := manager.Output(info.ID); strings.Contains(output, "READY") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	started := time.Now()
	if err := manager.Stop(info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	elapsed := time.Since(started)
	if data, err := os.ReadFile(marker); err != nil || string(data) != "saved" {
		t.Fatalf("the agent was killed %s into its close handler, before it could save (marker: %v)", elapsed, err)
	}
}

// TestStopBudgetCoversAnAgentTheCloseNeverReaches: an agent that left its
// console gets no close event and is killed only when the grace runs out.
// Callers budgeted five seconds for a Stop, less than that worst case, and
// every such stop through the web gateway ended as stop_uncertain.
func TestStopBudgetCoversAnAgentTheCloseNeverReaches(t *testing.T) {
	manager := newWindowsTestManager(t)
	info, err := manager.Start(agent.Spec{
		ID:      "detached",
		Name:    "detached",
		Command: []string{os.Args[0], "-test.run=^$"},
		Env:     map[string]string{detachHelperEnv: "1"},
	}, 80, 24)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		if output, _ := manager.Output(info.ID); strings.Contains(output, "READY") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("the agent never became ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)

	started := time.Now()
	if err := manager.Stop(info.ID); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	if elapsed := time.Since(started); elapsed >= StopBudget {
		t.Fatalf("Stop took %s, beyond the %s budget callers give it", elapsed, StopBudget)
	}
}
