package app

import (
	"bytes"
	"context"
	"os/exec"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/session"
)

func TestParseOptionsPreservesPublicCLIFlags(t *testing.T) {
	var diagnostics bytes.Buffer
	options, err := parseOptions([]string{
		"--pane1", "claude",
		"--pane2", "ollama run llama3.2",
		"--config", "custom.yaml",
	}, &diagnostics)
	if err != nil {
		t.Fatalf("parseOptions returned an error: %v", err)
	}
	if options.pane1 != "claude" || options.pane2 != "ollama run llama3.2" || options.configPath != "custom.yaml" {
		t.Fatalf("parsed options = %#v", options)
	}
	if !options.pane1Set || !options.pane2Set {
		t.Fatalf("explicit pane flags were not tracked: %#v", options)
	}
	if diagnostics.Len() != 0 {
		t.Fatalf("successful parsing wrote diagnostics: %q", diagnostics.String())
	}
}

func TestParseOptionsDistinguishesOmittedAndExplicitEmptyPaneFlags(t *testing.T) {
	var diagnostics bytes.Buffer
	omitted, err := parseOptions(nil, &diagnostics)
	if err != nil {
		t.Fatalf("parseOptions with omitted flags: %v", err)
	}
	if omitted.pane1Set || omitted.pane2Set {
		t.Fatalf("omitted pane flags were reported as present: %#v", omitted)
	}

	explicit, err := parseOptions([]string{"--pane1", "", "--pane2="}, &diagnostics)
	if err != nil {
		t.Fatalf("parseOptions with explicit empty flags: %v", err)
	}
	if !explicit.pane1Set || !explicit.pane2Set {
		t.Fatalf("explicit empty pane flags were not reported as present: %#v", explicit)
	}
}

func TestSplitLegacyCommandPreservesQuotedArgumentsWithoutShellExpansion(t *testing.T) {
	input := `tool "argument with spaces" "O'Reilly" '$HOME' 'a|b;c&d*e' "" plain\ value`
	want := []string{
		"tool",
		"argument with spaces",
		"O'Reilly",
		"$HOME",
		"a|b;c&d*e",
		"",
		"plain value",
	}

	got, err := splitLegacyCommand(input)
	if err != nil {
		t.Fatalf("splitLegacyCommand returned an error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %#v, want %#v", got, want)
	}
}

func TestSplitLegacyCommandTreatsUnquotedShellMetacharactersAsLiteralArguments(t *testing.T) {
	input := `tool $HOME $(whoami) a|b ; redirection>file "" '' escaped\ space`
	want := []string{
		"tool",
		"$HOME",
		"$(whoami)",
		"a|b",
		";",
		"redirection>file",
		"",
		"",
		"escaped space",
	}

	got, err := splitLegacyCommand(input)
	if err != nil {
		t.Fatalf("splitLegacyCommand returned an error: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("arguments = %#v, want %#v", got, want)
	}
}

func TestResolveAgentPlansRejectsNULInLegacyOverrideBeforeStartup(t *testing.T) {
	_, err := resolveAgentPlans(config.Result{
		Backend: "pty",
		Agents:  configuredAgentSpecs(1),
	}, options{pane1Set: true, pane1: "runner argument\x00suffix"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("resolveAgentPlans error = %v, want NUL validation error", err)
	}
}

func TestSplitLegacyCommandRejectsIncompleteQuoting(t *testing.T) {
	for _, input := range []string{`tool "unfinished`, `tool trailing\`} {
		if _, err := splitLegacyCommand(input); err == nil {
			t.Fatalf("splitLegacyCommand accepted %q", input)
		}
	}
}

func TestResolvePaneCommand(t *testing.T) {
	tests := []struct {
		name         string
		input        string
		wantCommand  []string
		wantUsesMock bool
	}{
		{
			name:         "absent",
			input:        "",
			wantCommand:  mockCommand(),
			wantUsesMock: true,
		},
		{
			name:         "blank",
			input:        " \t\n ",
			wantCommand:  mockCommand(),
			wantUsesMock: true,
		},
		{
			name:         "custom becomes direct argv",
			input:        "  ollama run llama3.2  ",
			wantCommand:  []string{"ollama", "run", "llama3.2"},
			wantUsesMock: false,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			configured := configuredAgentSpecs(1)
			resolution, err := resolveAgentPlans(config.Result{
				Backend: "pty",
				Agents:  configured,
			}, options{pane1Set: true, pane1: test.input}, t.TempDir())
			if err != nil {
				t.Fatalf("resolveAgentPlans: %v", err)
			}
			command := resolution.Specs[0].Command
			usesMock := len(resolution.MockAgentNames) == 1
			if !reflect.DeepEqual(command, test.wantCommand) {
				t.Fatalf("resolved command = %#v, want %#v", command, test.wantCommand)
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

	events := make(chan session.Event, 128)
	manager, err := session.NewManager(
		context.Background(),
		events,
		config.DefaultPatterns(),
		64*1024,
	)
	if err != nil {
		t.Fatalf("NewSessionManager returned an error: %v", err)
	}
	defer manager.Close()

	resolution, err := resolveAgentPlans(config.Result{Backend: "pty"}, options{}, t.TempDir())
	if err != nil {
		t.Fatalf("resolving default agents: %v", err)
	}
	if len(resolution.MockAgentNames) != 2 {
		t.Fatal("empty agent configuration did not select the mocks")
	}
	startedAgent, err := manager.Start(resolution.Specs[0], 100, 30)
	if err != nil {
		t.Fatalf("starting default mock: %v", err)
	}

	deadline := time.NewTimer(12 * time.Second)
	defer deadline.Stop()
	latestOutput := ""
	refreshOutput := func() {
		var outputErr error
		latestOutput, outputErr = manager.Output(startedAgent.ID)
		if outputErr != nil {
			t.Fatalf("reading mock output: %v", outputErr)
		}
	}

	var detected session.PromptDetected
	promptSeen := false
	for !promptSeen {
		select {
		case message := <-events:
			switch msg := message.(type) {
			case session.OutputAvailable:
				if msg.SessionID == startedAgent.ID {
					refreshOutput()
				}
			case session.PromptDetected:
				if msg.SessionID == startedAgent.ID {
					detected = msg
					promptSeen = true
					refreshOutput()
				}
			case session.Exited:
				if msg.SessionID == startedAgent.ID {
					t.Fatalf("mock exited before validation prompt: %v", msg.Err)
				}
			case session.Error:
				if msg.SessionID == startedAgent.ID {
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

	if err := manager.SendInput(startedAgent.ID, "Y"); err != nil {
		t.Fatalf("sending mock validation: %v", err)
	}

	finalText := "✅ Vous avez répondu : Y. Fin de la tâche."
	exited := false
	for !(exited && strings.Contains(latestOutput, finalText)) {
		select {
		case message := <-events:
			switch msg := message.(type) {
			case session.OutputAvailable:
				if msg.SessionID == startedAgent.ID {
					refreshOutput()
				}
			case session.Exited:
				if msg.SessionID == startedAgent.ID {
					refreshOutput()
					if msg.Err != nil {
						t.Fatalf("mock exited with an error: %v; output: %q", msg.Err, latestOutput)
					}
					exited = true
				}
			case session.Error:
				if msg.SessionID == startedAgent.ID {
					t.Fatalf("mock emitted a PTY error: %v", msg.Err)
				}
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for mock answer and exit; output: %q", latestOutput)
		}
	}

	done, err := manager.Done(startedAgent.ID)
	if err != nil {
		t.Fatalf("reading mock completion channel: %v", err)
	}
	select {
	case <-done:
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
