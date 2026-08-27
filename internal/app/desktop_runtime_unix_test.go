//go:build darwin || linux

package app

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

func TestDesktopPrepareHasNoRuntimeSideEffectsAndStartUsesReservedRunID(t *testing.T) {
	directory := t.TempDir()
	configurationPath := filepath.Join(directory, "config.yaml")
	marker := filepath.Join(directory, "started")
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
  - id: prepared
    name: Prepared
    command:
      - /bin/sh
      - -c
      - 'printf started > "$1"; sleep 30'
      - relayer-preflight
      - %q
    cwd: .
    backend: pty
    adapter: generic
intercept_patterns:
  - pattern: '(?i)confirm'
    description: confirmation
`, marker)
	if err := os.WriteFile(configurationPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}

	plan, err := PrepareDesktopRuntime(DesktopOptions{ConfigPath: configurationPath})
	if err != nil {
		t.Fatalf("PrepareDesktopRuntime: %v", err)
	}
	if _, err := os.Stat(marker); !os.IsNotExist(err) {
		t.Fatalf("prepare launched the agent: stat error = %v", err)
	}

	runtime, err := StartDesktopRuntime(context.Background(), plan, "desktop-run-explicit")
	if err != nil {
		t.Fatalf("StartDesktopRuntime: %v", err)
	}
	if got := runtime.Metadata().RunID; got != "desktop-run-explicit" {
		t.Fatalf("Metadata RunID = %q", got)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(marker); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("started runtime never produced its marker")
		}
		time.Sleep(10 * time.Millisecond)
	}

	stopCtx, stopCancel := context.WithTimeout(context.Background(), 6*time.Second)
	if err := runtime.BeginRestart(stopCtx); err != nil {
		stopCancel()
		t.Fatalf("BeginRestart: %v", err)
	}
	stopCancel()
	closeCtx, closeCancel := context.WithTimeout(context.Background(), 6*time.Second)
	if err := runtime.Close(closeCtx); err != nil {
		closeCancel()
		t.Fatalf("Close: %v", err)
	}
	closeCancel()
}

func TestDesktopCommandLabelNeverExposesArguments(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		shell   string
		want    string
	}{
		{name: "argv", command: []string{"/opt/homebrew/bin/claude", "--token", "fixture-secret"}, want: "claude"},
		{name: "shell", shell: "echo fixture-secret", want: "[shell explicite]"},
		{name: "missing", want: "[commande argv]"},
		{name: "sensitive executable label", command: []string{"password=fixture-secret"}, want: "[commande argv]"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := desktopCommandLabel(test.command, test.shell); got != test.want {
				t.Fatalf("desktopCommandLabel() = %q, want %q", got, test.want)
			}
		})
	}
}

func TestDesktopRuntimeRunsDefaultMocksAndRelaysExactEvents(t *testing.T) {
	configurationPath := filepath.Join(t.TempDir(), "config.yaml")
	configuration := `version: 1
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
agents: []
intercept_patterns:
  - pattern: '(?i)overwrite.*\[y/n\]'
    description: overwrite confirmation
`
	if err := os.WriteFile(configurationPath, []byte(configuration), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	runtime, err := NewDesktopRuntime(ctx, DesktopOptions{
		ConfigPath:  configurationPath,
		InitialSize: terminal.Size{Columns: 72, Rows: 16},
	})
	if err != nil {
		t.Fatalf("NewDesktopRuntime: %v", err)
	}
	closed := false
	defer func() {
		if !closed {
			closeCtx, closeCancel := context.WithTimeout(context.Background(), 8*time.Second)
			_ = runtime.Close(closeCtx)
			closeCancel()
		}
	}()

	sessions := runtime.Sessions()
	if len(sessions) != 2 {
		t.Fatalf("sessions = %#v, want two fallback mocks", sessions)
	}
	for _, item := range sessions {
		if item.Command != "bash" || item.Backend != "pty" || item.Adapter != "generic" {
			t.Fatalf("unsafe or unexpected desktop metadata: %#v", item)
		}
	}

	answered := make(map[string]bool, len(sessions))
	exited := make(map[string]bool, len(sessions))
	for len(exited) < len(sessions) {
		select {
		case <-ctx.Done():
			t.Fatalf("desktop runtime timed out: answered=%v exited=%v", answered, exited)
		case message := <-runtime.Events():
			eventMessage, ok := message.(session.AdapterEvent)
			if !ok {
				continue
			}
			event := eventMessage.Event
			if event.Type == adapters.EventProcessExit {
				exited[strings.ToLower(event.SessionID)] = true
				continue
			}
			if !event.Actionable() || answered[strings.ToLower(event.SessionID)] {
				continue
			}
			decisionCtx, decisionCancel := context.WithTimeout(ctx, 3*time.Second)
			err := runtime.ApplyDecision(decisionCtx, event.SessionID, event, adapters.DecisionManual, "n")
			decisionCancel()
			if err != nil {
				t.Fatalf("ApplyDecision(%s): %v", event.SessionID, err)
			}
			answered[strings.ToLower(event.SessionID)] = true
		}
	}

	for _, item := range sessions {
		output, err := runtime.Output(item.ID)
		if err != nil {
			t.Fatalf("Output(%s): %v", item.ID, err)
		}
		if !strings.Contains(output, "Vous avez répondu : n") {
			t.Fatalf("final output for %s did not contain relayed answer: %q", item.ID, output)
		}
	}

	closeCtx, closeCancel := context.WithTimeout(context.Background(), 8*time.Second)
	if err := runtime.Close(closeCtx); err != nil {
		closeCancel()
		t.Fatalf("Close: %v", err)
	}
	closeCancel()
	closed = true
}
