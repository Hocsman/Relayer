package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/terminal"
)

// DesktopOptions.VersionInspector existed and nothing ever set it: the runtime
// always ran the real probe. This starts a real run — two agents, this test
// binary in both — with an inspector of its own, and asserts what the runtime
// does with it:
//
//   - the agent whose argv[0] is the vendor executable is inspected, and the
//     answer reaches the session and the startup log;
//   - the agent whose argv[0] is anything else is not inspected at all, and
//     shows no version and no warning.
//
// Before the probe was gated on argv[0], both agents were inspected: a
// launcher or a wrapper reported its own version as the agent's.
func TestTheDesktopRuntimeUsesTheInjectedVersionInspector(t *testing.T) {
	directory := t.TempDir()
	vendorExecutable := versionHelperExecutable(t, directory, "claude")
	plainExecutable, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}

	var (
		mu        sync.Mutex
		calls     []string
		inspector VersionInspector = func(_ context.Context, spec agent.Spec) (string, error) {
			mu.Lock()
			calls = append(calls, spec.ID)
			mu.Unlock()
			return "9.9.9", nil
		}
	)

	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	configuration := fmt.Sprintf(`version: 1
backend: pty
sessions:
  persist_on_exit: false
  cleanup_on_success: true
policies:
  default_action: ask
  dry_run: false
  rules: []
audit:
  enabled: false
  mode: off
  path: ""
  max_file_size_mb: 1
  max_files: 1
agents:
  - id: vendor
    name: Vendor
    command: [%s]
    cwd: %s
    env:
      RELAYER_VERSION_HELPER: agent
      TERM: screen
    adapter: claude
    backend: pty
  - id: wrapped
    name: Wrapped
    command: [%s]
    cwd: %s
    env:
      RELAYER_VERSION_HELPER: agent
      TERM: screen
    adapter: claude
    backend: pty
intercept_patterns:
  - pattern: '(?i)continue'
    description: continue prompt
`, quote(vendorExecutable), quote(directory), quote(plainExecutable), quote(directory))

	configPath := filepath.Join(directory, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatalf("write the configuration: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	runtime, err := NewDesktopRuntime(ctx, DesktopOptions{
		ConfigPath:       configPath,
		InitialSize:      terminal.Size{Columns: 72, Rows: 16},
		VersionInspector: inspector,
	})
	if err != nil {
		t.Fatalf("NewDesktopRuntime: %v", err)
	}
	defer func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer closeCancel()
		_ = runtime.Close(closeCtx)
	}()

	mu.Lock()
	inspected := append([]string(nil), calls...)
	mu.Unlock()
	if len(inspected) != 1 || inspected[0] != "vendor" {
		t.Fatalf("the inspector ran for %v, want only the agent whose argv[0] is the vendor executable", inspected)
	}

	sessions := runtime.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("sessions = %+v, want the two configured agents", sessions)
	}
	byID := make(map[string]DesktopSession, len(sessions))
	for _, session := range sessions {
		byID[session.ID] = session
	}

	vendor := byID["vendor"]
	if vendor.InstalledVersion != "9.9.9" {
		t.Errorf("the vendor agent's session carries version %q, want the inspector's 9.9.9", vendor.InstalledVersion)
	}
	if !vendor.UnverifiedVersion || !strings.Contains(vendor.UnverifiedReason, "9.9.9") {
		t.Errorf("the vendor agent's session carries %+v, want an unverified 9.9.9 with a reason", vendor)
	}

	wrapped := byID["wrapped"]
	if wrapped.InstalledVersion != "" || wrapped.UnverifiedVersion || wrapped.UnverifiedReason != "" {
		t.Errorf("an agent the probe may not run reported %+v, want nothing", wrapped)
	}

	logs := strings.Join(runtime.StartupLogs(), "\n")
	if !strings.Contains(logs, "Warning [Vendor]: version 9.9.9 is not verified") {
		t.Errorf("the startup log does not warn about the vendor agent:\n%s", logs)
	}
	if strings.Contains(logs, "[Wrapped]") {
		t.Errorf("the startup log warns about an agent that was never probed:\n%s", logs)
	}
}
