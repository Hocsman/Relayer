package main

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	appcore "github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/audit"
)

const (
	askOnceHelperEnv = "RELAYER_TEST_ASK_ONCE_AGENT"
	// askOnceAfterEnv names what the helper does once it has the answer, before
	// it prints anything of its own. Empty, it works on the answer for a while.
	askOnceAfterEnv = "RELAYER_TEST_ASK_ONCE_AFTER"
	// askOnceFullScreen runs a full-screen program first: an editor or a pager
	// on the alternate screen, which gives the primary one back unchanged.
	askOnceFullScreen = "full-screen"

	// The helper's reports. Everything it reads after the first answer, for as
	// long as it keeps listening, is an extra answer.
	askOnceAnswer   = "ASK-ONCE-ANSWER:"
	askOnceExtra    = "ASK-ONCE-EXTRA:"
	askOnceDone     = "ASK-ONCE-DONE"
	askOnceNoAnswer = "ASK-ONCE-NO-ANSWER"
)

// TestHelperProcessAskOnceAgent is the agent the test below supervises, run as
// a child of this test binary. It asks one Aider question and reports every
// line it reads: the first as the answer, and any other that arrives in the
// next 1.5 seconds as an extra one.
//
// It clears the screen and homes the cursor before anything else, which is
// what ConPTY's first frame does to every Windows session. On Windows that is
// redundant; on a Unix terminal it is what puts detection on the rendered
// screen, where the answered question stays painted, as a full-screen agent
// does. The terminal is left in cooked mode, so the answer is echoed onto the
// question's line on both.
func TestHelperProcessAskOnceAgent(t *testing.T) {
	if os.Getenv(askOnceHelperEnv) != "1" {
		return
	}
	fmt.Print("\x1b[?25l\x1b[2J\x1b[m\x1b[Hagent ready\r\n\x1b[?25h")
	time.Sleep(300 * time.Millisecond)
	fmt.Print("Apply changes? (Y)es/(N)o/(D)escribe [Yes]: ")

	lines := make(chan string, 8)
	go func() {
		reader := bufio.NewReader(os.Stdin)
		for {
			line, err := reader.ReadString('\n')
			if line != "" {
				lines <- line
			}
			if err != nil {
				return
			}
		}
	}()
	var first string
	select {
	case first = <-lines:
	case <-time.After(15 * time.Second):
		fmt.Print("\r\n" + askOnceNoAnswer + "\r\n")
		time.Sleep(200 * time.Millisecond)
		os.Exit(3)
	}
	if os.Getenv(askOnceAfterEnv) == askOnceFullScreen {
		// The alternate screen, then back to the primary one, which still
		// shows the answered question. ConPTY passes the switch through and
		// repaints the primary screen after it; a Unix PTY passes the bytes as
		// they are, and the terminal restores the primary screen itself. The
		// switch is written by the agent, as an editor does, and ConPTY honours
		// it without the agent enabling anything on its console.
		time.Sleep(200 * time.Millisecond)
		fmt.Print("\x1b[?1049h\x1b[H\x1b[2Jeditor contents\r\n~\r\n~")
		time.Sleep(400 * time.Millisecond)
		fmt.Print("\x1b[?1049l")
		// Long enough for a repeat of the question to be raised, allowed and
		// typed, so that the listening below would read it.
		time.Sleep(1500 * time.Millisecond)
	} else {
		// A real agent works on the answer before printing anything. Meanwhile
		// the terminal has echoed it, and ConPTY flushes that echo as a write of
		// its own: the write that used to raise the question a second time.
		time.Sleep(500 * time.Millisecond)
	}
	fmt.Printf("\r\n%s%q\r\n", askOnceAnswer, first)
	listen := time.After(1500 * time.Millisecond)
	for {
		select {
		case line := <-lines:
			fmt.Printf("%s%q\r\n", askOnceExtra, line)
		case <-listen:
			fmt.Print(askOnceDone + "\r\n")
			time.Sleep(200 * time.Millisecond)
			os.Exit(0)
		}
	}
}

// The desktop answers an Aider question once under an automatic policy.
//
// After the answer the question stays painted, with the echo after it, and the
// Aider adapter read it again under a new ID on the next write. The desktop
// deduplicates by ID, so it evaluated the repeat as a new question, allowed it
// and typed a second "y" into an agent that had already consumed the first:
// whatever the agent asked next received an answer nobody gave. This runs the
// real desktop runtime against a real terminal, ConPTY on Windows and a PTY
// elsewhere, and counts the answers where they land, in the agent.
//
// A full-screen program run after the answer did the same by another road: the
// answered question's row was parked with the primary screen, taken for a row
// that was gone, and forgotten, and the primary screen came back with the
// question still on it.
func TestTheDesktopAnswersAnAutomaticQuestionOnce(t *testing.T) {
	if !desktopAgentExecutionSupported() {
		t.Skip(desktopUnsupportedReason())
	}
	t.Run("the echo of the answer", func(t *testing.T) {
		desktopAnswersOnce(t, "")
	})
	t.Run("a full-screen program after the answer", func(t *testing.T) {
		desktopAnswersOnce(t, askOnceFullScreen)
	})
}

// desktopAnswersOnce runs the helper agent under the desktop runtime, with after
// telling the helper what to do once it has the answer, and asserts that the
// agent received exactly one answer.
func desktopAnswersOnce(t *testing.T, after string) {
	dir := t.TempDir()
	for _, name := range []string{"APPDATA", "LOCALAPPDATA", "HOME", "USERPROFILE", "XDG_CONFIG_HOME"} {
		t.Setenv(name, dir)
	}
	auditPath := filepath.Join(dir, "audit", "journal.jsonl")
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		t.Fatal(err)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	quote := func(value string) string { return "'" + strings.ReplaceAll(value, "'", "''") + "'" }
	const agentID = "ask-once"
	environment := "      " + askOnceHelperEnv + ": '1'\n"
	if after != "" {
		environment += "      " + askOnceAfterEnv + ": " + quote(after) + "\n"
	}
	configuration := "version: 1\nbackend: pty\n" +
		"sessions:\n  persist_on_exit: false\n  cleanup_on_success: true\n" +
		"policies:\n  default_action: allow\n  dry_run: false\n  rules: []\n" +
		"audit:\n  enabled: true\n  mode: metadata\n  path: " + quote(auditPath) +
		"\n  max_file_size_mb: 10\n  max_files: 5\n" +
		"agents:\n  - id: " + agentID + "\n    name: " + agentID + "\n" +
		"    command: [" + quote(executable) + ", " + quote("-test.run=^TestHelperProcessAskOnceAgent$") + "]\n" +
		"    cwd: " + quote(dir) + "\n    env:\n" + environment +
		"    adapter: aider\n    backend: pty\n" +
		"intercept_patterns:\n" +
		"  - pattern: (?i)overwrite.*\\[y/n\\]\n    description: overwrite confirmation\n" +
		"  - pattern: (?i)\\[[yn]/[yn]\\]\n    description: yes/no confirmation\n" +
		"  - pattern: (?im)password:[[:space:]]*$\n    description: password entry\n" +
		"  - pattern: (?i)do you want to continue\n    description: continue confirmation\n" +
		"notifications:\n  enabled: false\n  bell: false\n  desktop: false\n"
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatal(err)
	}

	application := NewApp()
	started := time.Now()
	var (
		emittedMu sync.Mutex
		emitted   []string
	)
	application.ctx = context.Background()
	application.emitFn = func(_ context.Context, name string, payload ...interface{}) {
		if len(payload) == 0 {
			return
		}
		event, ok := payload[0].(SupervisionEvent)
		if !ok {
			return
		}
		emittedMu.Lock()
		emitted = append(emitted, fmt.Sprintf("+%s %s %s delivery=%s automatic=%v",
			time.Since(started).Round(time.Millisecond), name, event.ID, event.DeliveryStatus, event.Evaluation.Automatic))
		emittedMu.Unlock()
	}
	plan, err := appcore.PrepareDesktopRuntime(application.desktopOptions(configPath))
	if err != nil {
		t.Fatalf("prepare the desktop runtime: %v", err)
	}
	application.lifecycleMu.Lock()
	run, err := application.startGenerationLocked(plan)
	application.lifecycleMu.Unlock()
	if err != nil {
		t.Fatalf("start the desktop runtime: %v", err)
	}
	t.Cleanup(func() { _ = application.Shutdown() })

	// The last screen read is kept: the agent exits shortly after its report,
	// and what a finished session still returns is not this test's subject.
	screen := ""
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if output, err := run.engine.Output(agentID); err == nil && output != "" {
			screen = output
		}
		if strings.Contains(screen, askOnceDone) || strings.Contains(screen, askOnceNoAnswer) {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	emittedMu.Lock()
	for _, line := range emitted {
		t.Log(line)
	}
	emittedMu.Unlock()

	answers := strings.Count(screen, askOnceAnswer)
	extras := strings.Count(screen, askOnceExtra)
	if !strings.Contains(screen, askOnceDone) || answers != 1 || extras != 0 {
		t.Fatalf("the agent received %d answer(s) and %d extra one(s), want exactly one answer; screen:\n%s",
			answers, extras, screen)
	}
	if applied := appliedDeliveries(t, auditPath); applied != 1 {
		t.Fatalf("the journal records %d applied deliveries, want 1", applied)
	}
}

// appliedDeliveries counts the deliveries the journal records as applied.
func appliedDeliveries(t *testing.T, path string) int {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	applied := 0
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry audit.Entry
		if json.Unmarshal([]byte(line), &entry) != nil {
			continue
		}
		if entry.Kind == audit.KindDelivery && entry.Outcome == audit.OutcomeApplied {
			applied++
		}
	}
	return applied
}
