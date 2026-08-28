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
		requireTmux(t, err)
	}
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	runtimeRoot := t.TempDir()
	// Keep the socket root independent from t.TempDir: Darwin's sockaddr_un
	// limit is short enough that the testing package's test-name path can make
	// tmux fail with "File name too long" before it creates its server.
	socketRoot, err := os.MkdirTemp("", "rtmx-")
	if err != nil {
		t.Fatalf("create private tmux socket root: %v", err)
	}
	if err := os.Chmod(socketRoot, 0o700); err != nil {
		t.Fatalf("restrict private tmux socket root: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
	// Never connect this integration test to the user's personal tmux server.
	// Besides protecting unrelated sessions, a private socket makes the server
	// version and startup lifecycle deterministic across repeated test runs.
	t.Setenv("TMUX_TMPDIR", socketRoot)
	// An inherited TMUX points directly at its parent socket and takes priority
	// over TMUX_TMPDIR. Remove the ambient nesting metadata, then also pass -S in
	// the wrapper so every command has an explicit, test-owned socket target.
	//
	// These must be unset, not set to the empty string: tmux renders a format
	// differently depending on whether TMUX is present, and a Relayer launched
	// from an ordinary shell never has it. Setting it empty made this test pass
	// while the tmux backend was unable to start on tmux 3.7.
	unsetEnvForTest(t, "TMUX")
	unsetEnvForTest(t, "TMUX_PANE")
	privateSocketPath := filepath.Join(socketRoot, "relayer.sock")
	isolatedTmuxPath := filepath.Join(runtimeRoot, "isolated tmux")
	tmuxWrapper := "#!/bin/sh\nexec " + quotePOSIX(tmuxPath) + " -f /dev/null -S " + quotePOSIX(privateSocketPath) + " \"$@\"\n"
	if err := os.WriteFile(isolatedTmuxPath, []byte(tmuxWrapper), 0o700); err != nil {
		t.Fatalf("write isolated tmux wrapper: %v", err)
	}
	helperWrapper := filepath.Join(runtimeRoot, "relayer test helper")
	wrapper := "#!/bin/sh\nexec " + quotePOSIX(testExecutable) + " -test.run '^TestTmuxHelperProcess$' -- \"$@\"\n"
	if err := os.WriteFile(helperWrapper, []byte(wrapper), 0o700); err != nil {
		t.Fatalf("write helper wrapper: %v", err)
	}

	externalName := fmt.Sprintf("tmuxbackend-external-%d", os.Getpid())
	external := exec.Command(isolatedTmuxPath, "new-session", "-d", "-s", externalName, "sleep 30")
	if output, err := external.CombinedOutput(); err != nil {
		t.Fatalf("create external tmux session: %v (%s)", err, output)
	}
	socketInfo, err := os.Lstat(privateSocketPath)
	if err != nil {
		t.Fatalf("inspect private tmux socket: %v", err)
	}
	if socketInfo.Mode()&os.ModeSocket == 0 {
		t.Fatalf("private tmux socket has mode %v", socketInfo.Mode())
	}
	t.Cleanup(func() {
		_ = exec.Command(isolatedTmuxPath, "kill-session", "-t", externalName).Run()
	})

	events := make(chan session.Event, 128)
	manager, err := NewManager(
		context.Background(),
		events,
		intercept.DefaultPatterns(),
		4096,
		Options{
			TmuxPath:      isolatedTmuxPath,
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
	if output, err := exec.Command(isolatedTmuxPath, "has-session", "-t", externalName).CombinedOutput(); err != nil {
		t.Fatalf("Relayer interfered with external tmux session: %v (%s)", err, output)
	}

	// A persistent session remains owned and alive after Relayer closes, but
	// its pipe-pane helper must not survive with a private FIFO that no longer
	// exists. Exercise the real tmux state rather than only the fake runner.
	persistentEvents := make(chan session.Event, 128)
	persistentManager, err := NewManager(
		context.Background(),
		persistentEvents,
		intercept.DefaultPatterns(),
		4096,
		Options{
			TmuxPath:      isolatedTmuxPath,
			HelperPath:    helperWrapper,
			RuntimeDir:    runtimeRoot,
			RunID:         "integration-persistent",
			PollInterval:  minimumPollInterval,
			PersistOnExit: true,
		},
	)
	if err != nil {
		t.Fatalf("NewManager persistent: %v", err)
	}
	var persistentSessionID string
	t.Cleanup(func() {
		if persistentSessionID != "" {
			_ = exec.Command(isolatedTmuxPath, "kill-session", "-t", persistentSessionID).Run()
		}
	})
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		_ = persistentManager.Close(ctx)
	})

	persistentInfo, err := persistentManager.Start(context.Background(), agent.Spec{
		ID:      "persistent-agent",
		Name:    "Persistent agent",
		Command: []string{"/bin/sh", "-c", `trap 'exit 0' HUP TERM INT; printf 'persistent-ready\n'; while :; do sleep 1; done`},
		Cwd:     runtimeRoot,
		Adapter: agent.AdapterGeneric,
		Backend: agent.BackendTmux,
	}, terminal.Size{Columns: 72, Rows: 18})
	if err != nil {
		t.Fatalf("Start persistent: %v", err)
	}
	persistentTarget, err := persistentManager.session(persistentInfo.ID)
	if err != nil {
		t.Fatalf("persistent target: %v", err)
	}
	persistentSessionID = persistentTarget.sessionID
	persistentPaneID := persistentTarget.paneID

	ctx, cancel = context.WithTimeout(context.Background(), 4*time.Second)
	if err := persistentManager.Close(ctx); err != nil {
		cancel()
		t.Fatalf("Close persistent: %v", err)
	}
	cancel()

	pipeState, err := exec.Command(
		isolatedTmuxPath,
		"display-message", "-p", "-t", persistentPaneID,
		tmuxFormat("#{session_id}", "#{pane_dead}", "#{pane_pipe}"),
	).CombinedOutput()
	if err != nil {
		t.Fatalf("inspect persistent pane after Close: %v (%s)", err, pipeState)
	}
	if got, want := strings.TrimSpace(string(pipeState)), persistentSessionID+tmuxFieldSeparator+"0"+tmuxFieldSeparator+"0"; got != want {
		t.Fatalf("persistent pane state after Close = %q, want %q", got, want)
	}
	if output, err := exec.Command(isolatedTmuxPath, "has-session", "-t", persistentSessionID).CombinedOutput(); err != nil {
		t.Fatalf("persistent session did not survive Close: %v (%s)", err, output)
	}
}

// unsetEnvForTest removes a variable for the duration of the test and restores
// the ambient value afterwards. t.Setenv cannot express absence, and absence is
// the production condition for TMUX.
func unsetEnvForTest(t *testing.T, name string) {
	t.Helper()
	previous, existed := os.LookupEnv(name)
	if err := os.Unsetenv(name); err != nil {
		t.Fatalf("unset %s: %v", name, err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(name, previous)
			return
		}
		_ = os.Unsetenv(name)
	})
}

// TestTmuxFormatSeparatorSurvivesEveryAmbientTmuxVariable pins the wire contract
// every identity, ownership and snapshot parser depends on.
//
// tmux sanitizes unprintable bytes while rendering a format. On tmux 3.7 a TAB
// becomes "_" whenever TMUX is absent from the environment, so the previous
// TAB-separated formats made Manager.Start fail with "identifiants immuables
// tmux invalides" for every user running Relayer from an ordinary shell. The
// regression was invisible because the integration test set TMUX to the empty
// string, and because CI runners ship tmux 3.4.
//
// The separator must therefore survive all three ambient states, not just the
// one a test happens to create.
func TestTmuxFormatSeparatorSurvivesEveryAmbientTmuxVariable(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		requireTmux(t, err)
	}

	for _, ambient := range []struct {
		name  string
		value string
		set   bool
	}{
		{name: "absent"},
		{name: "empty", value: "", set: true},
		{name: "foreign", value: "/nonexistent/foreign.sock,1,0", set: true},
	} {
		t.Run(ambient.name, func(t *testing.T) {
			socketRoot, err := os.MkdirTemp("", "rtmx-sep-")
			if err != nil {
				t.Fatalf("create private tmux socket root: %v", err)
			}
			if err := os.Chmod(socketRoot, 0o700); err != nil {
				t.Fatalf("restrict private tmux socket root: %v", err)
			}
			t.Cleanup(func() { _ = os.RemoveAll(socketRoot) })
			t.Setenv("TMUX_TMPDIR", socketRoot)
			unsetEnvForTest(t, "TMUX_PANE")
			if ambient.set {
				t.Setenv("TMUX", ambient.value)
			} else {
				unsetEnvForTest(t, "TMUX")
			}

			socketPath := filepath.Join(socketRoot, "relayer.sock")
			wrapper := filepath.Join(socketRoot, "isolated tmux")
			script := "#!/bin/sh\nexec " + quotePOSIX(tmuxPath) + " -f /dev/null -S " + quotePOSIX(socketPath) + " \"$@\"\n"
			if err := os.WriteFile(wrapper, []byte(script), 0o700); err != nil {
				t.Fatalf("write isolated tmux wrapper: %v", err)
			}
			t.Cleanup(func() { _ = exec.Command(wrapper, "kill-server").Run() })

			output, err := exec.Command(wrapper,
				"new-session", "-d", "-P", "-F",
				tmuxFormat("#{session_id}", "#{window_id}", "#{pane_id}"),
				"-s", "relayer-separator-contract", "-x", "80", "-y", "24",
				"exec /bin/sleep 30",
			).Output()
			if err != nil {
				t.Fatalf("new-session: %v", err)
			}
			if !strings.Contains(string(output), tmuxFieldSeparator) {
				t.Fatalf("tmux rewrote the field separator: %q (tmux must pass %q through unchanged)",
					string(output), tmuxFieldSeparator)
			}
			identity, err := parseIdentity(string(output))
			if err != nil {
				t.Fatalf("parseIdentity(%q): %v", string(output), err)
			}
			if !validTmuxID(identity.sessionID, '$') || !validTmuxID(identity.windowID, '@') ||
				!validTmuxID(identity.paneID, '%') {
				t.Fatalf("identity = %#v", identity)
			}

			// The ownership marker round-trips through the same format, and its
			// hex token must not be split or rewritten either.
			if _, err := exec.Command(wrapper, "set-option", "-t", identity.sessionID,
				"@relayer_owner", "0123456789abcdef").Output(); err != nil {
				t.Fatalf("set-option: %v", err)
			}
			owned, err := exec.Command(wrapper, "display-message", "-p", "-t", identity.sessionID,
				tmuxFormat("#{session_id}", "#{@relayer_owner}")).Output()
			if err != nil {
				t.Fatalf("display-message: %v", err)
			}
			fields := splitTmuxFields(string(owned))
			if len(fields) != 2 || fields[0] != identity.sessionID || fields[1] != "0123456789abcdef" {
				t.Fatalf("ownership round trip = %q, fields %#v", string(owned), fields)
			}
		})
	}
}
