package main

import (
	"context"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

func TestResolvePaneCommand(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantCommand  string
		wantUsesMock bool
	}{
		{
			name:         "absent",
			input:        "",
			wantCommand:  defaultMockCommand,
			wantUsesMock: true,
		},
		{
			name:         "blank",
			input:        " \t\n ",
			wantCommand:  defaultMockCommand,
			wantUsesMock: true,
		},
		{
			name:         "custom preserved exactly",
			input:        "  ollama run llama3.2  ",
			wantCommand:  "  ollama run llama3.2  ",
			wantUsesMock: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			command, usesMock := resolvePaneCommand(test.input)
			if command != test.wantCommand {
				t.Fatalf("resolved command = %q, want %q", command, test.wantCommand)
			}
			if usesMock != test.wantUsesMock {
				t.Fatalf("usesMock = %t, want %t", usesMock, test.wantUsesMock)
			}
		})
	}
}

func TestDefaultMockCommandRunsTwentyLinesAndRelaysAnswer(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creack/pty integration requires a Unix PTY")
	}
	if _, err := exec.LookPath("bash"); err != nil {
		t.Skipf("default mock requires bash: %v", err)
	}

	events := make(chan tea.Msg, 128)
	manager, err := NewSessionManager(
		context.Background(),
		events,
		defaultPromptPatterns,
		64*1024,
	)
	if err != nil {
		t.Fatalf("NewSessionManager returned an error: %v", err)
	}
	defer manager.Close()

	command, usesMock := resolvePaneCommand("")
	if !usesMock {
		t.Fatal("empty pane command did not select the mock")
	}
	session, err := manager.Start("default mock", command, 100, 30)
	if err != nil {
		t.Fatalf("starting default mock: %v", err)
	}

	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	latestOutput := ""
	refreshOutput := func() {
		var outputErr error
		latestOutput, outputErr = manager.Output(session.ID)
		if outputErr != nil {
			t.Fatalf("reading mock output: %v", outputErr)
		}
	}

	var detected PromptDetectedMsg
	promptSeen := false
	for !promptSeen {
		select {
		case message := <-events:
			switch msg := message.(type) {
			case SessionOutputMsg:
				if msg.SessionID == session.ID {
					refreshOutput()
				}
			case PromptDetectedMsg:
				if msg.SessionID == session.ID {
					detected = msg
					promptSeen = true
					refreshOutput()
				}
			case SessionExitedMsg:
				if msg.SessionID == session.ID {
					t.Fatalf("mock exited before validation prompt: %v", msg.Err)
				}
			case SessionErrorMsg:
				if msg.SessionID == session.ID {
					t.Fatalf("mock emitted a PTY error before validation: %v", msg.Err)
				}
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for mock overwrite prompt; output: %q", latestOutput)
		}
	}

	if detected.Pattern != "overwrite" {
		t.Fatalf("detected pattern = %q, want overwrite", detected.Pattern)
	}
	if !strings.Contains(detected.Match, "Overwrite file? [Y/n]") {
		t.Fatalf("detected match = %q", detected.Match)
	}
	wantGeneratedLines := []string{
		"Génération ligne 1...",
		"Génération ligne 2...",
		"Génération ligne 3...",
		"Génération ligne 4...",
		"Génération ligne 5...",
		"Génération ligne 6...",
		"Génération ligne 7...",
		"Génération ligne 8...",
		"Génération ligne 9...",
		"Génération ligne 10...",
		"Génération ligne 11...",
		"Génération ligne 12...",
		"Génération ligne 13...",
		"Génération ligne 14...",
		"Génération ligne 15...",
		"Génération ligne 16...",
		"Génération ligne 17...",
		"Génération ligne 18...",
		"Génération ligne 19...",
		"Génération ligne 20...",
	}
	gotGeneratedLines := mockGenerationLines(latestOutput)
	if !reflect.DeepEqual(gotGeneratedLines, wantGeneratedLines) {
		t.Fatalf(
			"generated lines mismatch:\ngot:  %#v\nwant: %#v\nfull output:\n%s",
			gotGeneratedLines,
			wantGeneratedLines,
			latestOutput,
		)
	}

	if err := manager.SendInput(session.ID, "Y"); err != nil {
		t.Fatalf("sending mock validation: %v", err)
	}

	finalText := "✅ Vous avez répondu : Y. Fin de la tâche."
	exited := false
	for !(exited && strings.Contains(latestOutput, finalText)) {
		select {
		case message := <-events:
			switch msg := message.(type) {
			case SessionOutputMsg:
				if msg.SessionID == session.ID {
					refreshOutput()
				}
			case SessionExitedMsg:
				if msg.SessionID == session.ID {
					refreshOutput()
					if msg.Err != nil {
						t.Fatalf("mock exited with an error: %v; output: %q", msg.Err, latestOutput)
					}
					exited = true
				}
			case SessionErrorMsg:
				if msg.SessionID == session.ID {
					t.Fatalf("mock emitted a PTY error: %v", msg.Err)
				}
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for mock answer and exit; output: %q", latestOutput)
		}
	}

	select {
	case <-session.done:
	default:
		t.Fatal("mock session done channel is open after SessionExitedMsg")
	}
}

func mockGenerationLines(output string) []string {
	result := make([]string, 0, 20)
	for _, line := range strings.Split(strings.ReplaceAll(output, "\r\n", "\n"), "\n") {
		line = strings.TrimSuffix(line, "\r")
		if strings.HasPrefix(line, "Génération ligne ") {
			result = append(result, line)
		}
	}
	return result
}
