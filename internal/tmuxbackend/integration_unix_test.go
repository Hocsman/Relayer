//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tmuxbackend

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/intercept"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

// TestTmuxHelperProcess is re-executed by a tiny test-only wrapper inside tmux.
// In the ordinary test process there is no private subcommand, so it is a no-op.
func TestTmuxHelperProcess(t *testing.T) {
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	handled, code := HelperMain(os.Args[separator+1:], os.Stderr)
	if !handled {
		return
	}
	os.Exit(code)
}

func TestTmuxIntegrationLifecyclePromptInputAndExternalSessionIsolation(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skipf("tmux integration skipped: %v", err)
	}
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	runtimeRoot := t.TempDir()
	helperWrapper := filepath.Join(runtimeRoot, "relayer test helper")
	wrapper := "#!/bin/sh\nexec " + quotePOSIX(testExecutable) + " -test.run '^TestTmuxHelperProcess$' -- \"$@\"\n"
	if err := os.WriteFile(helperWrapper, []byte(wrapper), 0o700); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	externalName := fmt.Sprintf("tmuxbackend-external-%d", os.Getpid())
	external := exec.Command(tmuxPath, "new-session", "-d", "-s", externalName, "sleep 30")
	if output, err := external.CombinedOutput(); err != nil {
		t.Fatalf("create external tmux session: %v (%s)", err, output)
	}
	t.Cleanup(func() {
		_ = exec.Command(tmuxPath, "kill-session", "-t", externalName).Run()
	})

	events := make(chan session.Event, 128)
	manager, err := NewManager(
		context.Background(),
		events,
		intercept.DefaultPatterns(),
		4096,
		Options{
			TmuxPath:      tmuxPath,
			HelperPath:    helperWrapper,
			RuntimeDir:    runtimeRoot,
			RunID:         "integration",
			PollInterval:  minimumPollInterval,
			PersistOnExit: false,
		},
	)
	if err != nil {
		t.Fatalf("NewManager: %v", err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_ = manager.Close(ctx)
	})

	script := `printf 'ready\n'; printf 'Overwrite? [Y/n]'; IFS= read -r answer; printf '\nanswer=%s\n' "$answer"`
	info, err := manager.Start(context.Background(), agent.Spec{
		ID:      "integration-agent",
		Name:    "Integration agent",
		Command: []string{"/bin/sh", "-c", script},
		Cwd:     runtimeRoot,
		Env:     map[string]string{"RELAYER_INTEGRATION": "value with spaces;$`'"},
		Adapter: agent.AdapterGeneric,
		Backend: agent.BackendTmux,
	}, terminal.Size{Columns: 72, Rows: 18})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	prompt := contractWaitForPrompt(t, events, info.ID)
	if prompt.Metadata["pattern"] != "overwrite" {
		t.Fatalf("prompt = %#v", prompt)
	}
	if err := manager.SendEvent(context.Background(), info.ID, prompt.ID, []byte("Y\r")); err != nil {
		t.Fatalf("SendEvent: %v", err)
	}
	exited := waitForExitEvent(t, events, info.ID)
	if exited.Metadata["failed"] == "true" {
		t.Fatalf("agent emitted a failed process_exit: %#v", exited)
	}
	output, err := manager.Output(info.ID)
	if err != nil || !strings.Contains(output, "answer=Y") {
		t.Fatalf("final output = %q, error %v", output, err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	if err := manager.Close(ctx); err != nil {
		cancel()
		t.Fatalf("Close: %v", err)
	}
	cancel()
	if output, err := exec.Command(tmuxPath, "has-session", "-t", externalName).CombinedOutput(); err != nil {
		t.Fatalf("Relayer interfered with external tmux session: %v (%s)", err, output)
	}
}
