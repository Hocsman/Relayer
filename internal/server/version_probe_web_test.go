package server

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
)

// copyAsVendorExecutable copies this test binary under a vendor tool's name.
// The version probe runs argv[0] only when its base name is the adapter's own
// executable, so an agent the probe may inspect has to look like one.
func copyAsVendorExecutable(t *testing.T, name string) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read the test binary: %v", err)
	}
	target := filepath.Join(t.TempDir(), name)
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	if err := os.WriteFile(target, raw, 0o755); err != nil {
		t.Fatalf("write the vendor executable: %v", err)
	}
	return target
}

// Starting a run probes each vendor agent's own binary. The probe used to run
// inside the gateway's state lock, one agent at a time, so every client's
// GetState waited for the sum of the tools' answer times — and a tool that
// ignored the deadline held the lock with it. The run is now prepared and
// started before the lock is taken, and a state read answers while a probe is
// still running.
func TestAStateReadAnswersWhileARunIsStarting(t *testing.T) {
	configPath, _, _ := writeWebRunConfig(t, webRun{
		agents: []webAgent{{
			id:         "vendor",
			mode:       webAgentListen,
			adapter:    "claude",
			executable: copyAsVendorExecutable(t, "claude"),
		}},
	})

	ctrl, err := NewController(configPath, io.Discard)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}

	probing := make(chan struct{}, 1)
	var releaseOnce sync.Once
	release := make(chan struct{})
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	ctrl.versionInspector = func(ctx context.Context, _ agent.Spec) (string, error) {
		select {
		case probing <- struct{}{}:
		default:
		}
		select {
		case <-release:
			return "3.0.0", nil
		case <-ctx.Done():
			return "", ctx.Err()
		}
	}

	ctx, cancel := context.WithCancel(context.Background())
	var (
		startMu   sync.Mutex
		startErr  error
		startDone = make(chan struct{})
	)
	go func() {
		err := ctrl.Start(ctx)
		startMu.Lock()
		startErr = err
		startMu.Unlock()
		close(startDone)
	}()
	readStartErr := func() error {
		startMu.Lock()
		defer startMu.Unlock()
		return startErr
	}
	t.Cleanup(func() {
		releaseProbe()
		select {
		case <-startDone:
		case <-time.After(20 * time.Second):
			t.Error("Start did not return")
		}
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer closeCancel()
		_ = ctrl.Close(closeCtx)
		cancel()
	})

	select {
	case <-probing:
	case <-startDone:
		t.Fatalf("Start returned before any probe ran: %v", readStartErr())
	case <-time.After(20 * time.Second):
		t.Fatal("the version probe never ran for the vendor agent")
	}

	// The probe is inside the run's start. A state read must not wait for it.
	stateRead := make(chan AppState, 1)
	go func() { stateRead <- ctrl.GetState() }()
	select {
	case <-stateRead:
	case <-time.After(3 * time.Second):
		releaseProbe()
		t.Fatal("GetState waited for the version probe: the run is started under the state lock")
	}

	releaseProbe()
	select {
	case <-startDone:
		if err := readStartErr(); err != nil {
			t.Fatalf("Start: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Start did not return after the probe was released")
	}

	state := ctrl.GetState()
	if len(state.Agents) != 1 || state.Agents[0].InstalledVersion != "3.0.0" || !state.Agents[0].UnverifiedVersion {
		t.Fatalf("the started run reports %+v, want the probed 3.0.0 as unverified", state.Agents)
	}
}

// What each role receives for a probed agent, through the gateway a browser
// talks to. The operator gets the version and the reason; the viewer gets the
// flag alone. The version names the host's tooling, and the reason quotes the
// version, so one without the other would leak it — and the startup notices
// carry the same reason. The flag stays: a viewer watching a prompt should see
// that the agent's version is not one this build was verified against.
func TestAViewerGetsTheVersionWarningWithoutTheHostsVersion(t *testing.T) {
	gateway := startWebRun(t, webRun{
		agents: []webAgent{{
			id:         "vendor",
			mode:       webAgentListen,
			adapter:    "claude",
			executable: copyAsVendorExecutable(t, "claude"),
		}},
		versionInspector: func(context.Context, agent.Spec) (string, error) {
			return "3.0.0", nil
		},
	})

	if agents := gateway.ctrl.GetState().Agents; len(agents) != 1 ||
		agents[0].InstalledVersion != "3.0.0" || !agents[0].UnverifiedVersion || agents[0].UnverifiedReason == "" {
		t.Fatalf("the run does not carry the probe's answer: %+v", agents)
	}

	baseURL := gateway.serve()
	operator := dialSharedGateway(t, baseURL, "opAlice")
	viewer := dialSharedGateway(t, baseURL, "viewDave")

	var operatorState, viewerState AppState
	operator.mustCall("getState", map[string]any{}, &operatorState)
	viewer.mustCall("getState", map[string]any{}, &viewerState)

	if len(operatorState.Agents) != 1 {
		t.Fatalf("the operator's agents = %+v", operatorState.Agents)
	}
	operatorAgent := operatorState.Agents[0]
	if operatorAgent.InstalledVersion != "3.0.0" || !operatorAgent.UnverifiedVersion || operatorAgent.UnverifiedReason == "" {
		t.Errorf("the operator lost the probe's answer: %+v", operatorAgent)
	}

	if len(viewerState.Agents) != 1 {
		t.Fatalf("the viewer's agents = %+v", viewerState.Agents)
	}
	viewerAgent := viewerState.Agents[0]
	if !viewerAgent.UnverifiedVersion {
		t.Error("the viewer lost the warning that the version is not verified")
	}
	if viewerAgent.InstalledVersion != "" {
		t.Errorf("the viewer received the host's version %q", viewerAgent.InstalledVersion)
	}
	if viewerAgent.UnverifiedReason != "" {
		t.Errorf("the viewer received the reason, which quotes the version: %q", viewerAgent.UnverifiedReason)
	}
	if text := stateText(t, viewerState); strings.Contains(text, "3.0.0") {
		t.Errorf("the viewer's state names the version somewhere: %s", text)
	}
}
