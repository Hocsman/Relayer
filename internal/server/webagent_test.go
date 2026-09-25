package server

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// The gateway's supervision tests run real agents: this test binary, run again
// inside the run's terminal, ConPTY on Windows and a PTY elsewhere, asking a
// real question and reporting on its screen every answer it reads. A synthetic
// event proves what the gateway does with an event; only a real agent proves
// what reached it, and the gateway's defects were in what reached it: the
// policy's answer never did, a refused answer lost the prompt, and two answers
// could be written into one terminal.
const (
	webAgentModeEnv = "RELAYER_TEST_WEB_AGENT"
	// webAgentDelayEnv is how long the agent waits before it asks.
	webAgentDelayEnv = "RELAYER_TEST_WEB_AGENT_DELAY"
	// webAgentListenEnv is how long, once answered, it listens for an extra
	// answer before it reports that it is done.
	webAgentListenEnv = "RELAYER_TEST_WEB_AGENT_LISTEN"
	// webAgentExitEnv is the code it exits with once done.
	webAgentExitEnv = "RELAYER_TEST_WEB_AGENT_EXIT"
	// webAgentGateEnv names a file the agent waits for before its next step:
	// asking, or taking its question back. A test opens the gate once it has
	// done what must come first, rather than racing a delay.
	webAgentGateEnv = "RELAYER_TEST_WEB_AGENT_GATE"

	// The modes: the question the agent asks, and so the adapter it needs.
	webAgentAider    = "aider"    // an Aider question, adapter aider
	webAgentGeneric  = "generic"  // an overwrite question, adapter generic
	webAgentToolCall = "toolcall" // a question about an MCP tool call, adapter generic
	webAgentWithdraw = "withdraw" // an overwrite question, taken back before any answer
	webAgentListen   = "listen"   // no question: it reports every line it reads
	webAgentExit     = "exit"     // no question: it exits at once

	// The agent's reports. Everything it reads after its first answer, for as
	// long as it listens, is an extra answer.
	webAgentAnswer   = "WEB-AGENT-ANSWER:"
	webAgentExtra    = "WEB-AGENT-EXTRA:"
	webAgentLine     = "WEB-AGENT-LINE:"
	webAgentDone     = "WEB-AGENT-DONE"
	webAgentNoAnswer = "WEB-AGENT-NO-ANSWER"
	webAgentWithdrew = "the agent changed its mind"

	webAgentAiderQuestion   = "Apply changes? (Y)es/(N)o/(D)escribe [Yes]: "
	webAgentGenericQuestion = "Overwrite file probe.txt? [y/n] "
	// webAgentToken is a credential the tool-call agent hands to its tool.
	webAgentToken = "ghp_abcdefghijklmnop1234"
)

// TestHelperProcessWebAgent is the agent, run as a child of this test binary.
// It does nothing when the test binary runs its tests. Its mode comes from its
// environment, which a save through the settings keeps: until it did, an
// agent saved there came back with none, and the mode was passed as an
// argument instead.
func TestHelperProcessWebAgent(t *testing.T) {
	mode := os.Getenv(webAgentModeEnv)
	if mode == "" {
		return
	}
	os.Exit(runWebAgent(mode))
}

// runWebAgent is the agent's life. It clears the screen and homes the cursor
// first, which is what ConPTY's first frame does to every Windows session; on
// a Unix terminal it is what puts detection on the rendered screen, as it is
// on Windows. The terminal stays in cooked mode, so an answer is echoed onto
// the question's line, as a real agent's is.
func runWebAgent(mode string) int {
	fmt.Print("\x1b[?25l\x1b[2J\x1b[m\x1b[Hagent ready\r\n\x1b[?25h")
	lines := make(chan string, 16)
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
	delay := 300 * time.Millisecond
	if value, err := time.ParseDuration(os.Getenv(webAgentDelayEnv)); err == nil {
		delay = value
	}
	listen := 1500 * time.Millisecond
	if value, err := time.ParseDuration(os.Getenv(webAgentListenEnv)); err == nil {
		listen = value
	}
	exitCode := 0
	if value, err := strconv.Atoi(os.Getenv(webAgentExitEnv)); err == nil {
		exitCode = value
	}
	time.Sleep(delay)

	switch mode {
	case webAgentExit:
		fmt.Print(webAgentDone + "\r\n")
		time.Sleep(200 * time.Millisecond)
		return exitCode
	case webAgentListen:
		stop := time.After(90 * time.Second)
		for {
			select {
			case line := <-lines:
				fmt.Printf("%s%q\r\n", webAgentLine, line)
			case <-stop:
				return exitCode
			}
		}
	case webAgentWithdraw:
		fmt.Print(webAgentGenericQuestion)
		if !awaitWebAgentGate() {
			time.Sleep(1500 * time.Millisecond)
		}
		// The question's row painted with something else: the processor
		// reads that as the question taken back.
		fmt.Print("\r\x1b[2K" + webAgentWithdrew + "\r\n")
		stop := time.After(listen)
		for {
			select {
			case line := <-lines:
				fmt.Printf("%s%q\r\n", webAgentExtra, line)
			case <-stop:
				fmt.Print(webAgentDone + "\r\n")
				time.Sleep(200 * time.Millisecond)
				return exitCode
			}
		}
	case webAgentAider:
		awaitWebAgentGate()
		fmt.Print(webAgentAiderQuestion)
	case webAgentGeneric:
		awaitWebAgentGate()
		fmt.Print(webAgentGenericQuestion)
	case webAgentToolCall:
		awaitWebAgentGate()
		fmt.Print("Calling mcp__github__create_issue\r\n" +
			"  repo: Hocsman/Relayer\r\n" +
			"  auth: " + webAgentToken + "\r\n" +
			"Do you want to continue? [y/n] ")
	default:
		return 9
	}

	var first string
	select {
	case first = <-lines:
	case <-time.After(15 * time.Second):
		fmt.Print("\r\n" + webAgentNoAnswer + "\r\n")
		time.Sleep(200 * time.Millisecond)
		return 3
	}
	// A real agent works on the answer before it prints anything, while the
	// terminal echoes it: the write an adapter used to read the question again
	// from.
	time.Sleep(500 * time.Millisecond)
	fmt.Printf("\r\n%s%q\r\n", webAgentAnswer, first)
	stop := time.After(listen)
	for {
		select {
		case line := <-lines:
			fmt.Printf("%s%q\r\n", webAgentExtra, line)
		case <-stop:
			fmt.Print(webAgentDone + "\r\n")
			time.Sleep(200 * time.Millisecond)
			return exitCode
		}
	}
}

// awaitWebAgentGate waits until the agent's gate file exists, when it has
// one, and reports whether it had one.
func awaitWebAgentGate() bool {
	path := os.Getenv(webAgentGateEnv)
	if path == "" {
		return false
	}
	for deadline := time.Now().Add(60 * time.Second); time.Now().Before(deadline); {
		if _, err := os.Stat(path); err == nil {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return true
}

// webAgentGate is a gate for one agent: the environment that gives it the
// gate, and the function that opens it.
func webAgentGate(t *testing.T) (map[string]string, func()) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gate")
	return map[string]string{webAgentGateEnv: path}, func() {
		t.Helper()
		if err := os.WriteFile(path, []byte("open"), 0o600); err != nil {
			t.Fatalf("open the agent's gate: %v", err)
		}
	}
}

// webAgent is one agent of a test run: its session ID, its name, what it does
// and the adapter that reads it.
type webAgent struct {
	id, name, mode, adapter string
	env                     map[string]string
}

// webRun configures one gateway run of helper agents. policy is the default
// action, ask when empty; rules is a YAML list of policy rules, indented as
// the policies block's items. The audit journal is on, in the mode auditMode
// names, in the test's directory.
type webRun struct {
	policy        string
	rules         string
	notifications bool
	// auditMode is the journal's mode, metadata when empty: detailed keeps
	// the role and connection of whoever answered.
	auditMode string
	recording bool
	agents    []webAgent
	// engineWrap is the Controller's seam, set before the run starts.
	engineWrap func(supervise.Engine) supervise.Engine
}

// webFrame is one frame the gateway broadcast to its clients.
type webFrame struct {
	at      time.Time
	event   string
	payload any
}

// webGateway is a started gateway run and what it broadcast.
type webGateway struct {
	t            *testing.T
	ctrl         *Controller
	auditPath    string
	recordingDir string
	// cancelStart cancels the context the run was started with, as a signal
	// to Serve does.
	cancelStart context.CancelFunc

	mu     sync.Mutex
	frames []webFrame
	// screens keeps, per session, the last screen that carried an agent's
	// report: a finished session's screen is not what these tests are about.
	screens map[string]string
}

func webYAMLQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "''") + "'"
}

// startWebRun writes the run's configuration and starts a real Controller on
// it, as Serve does. The run is closed when the test ends, before its
// directory is removed: Windows cannot remove a journal still open.
func startWebRun(t *testing.T, run webRun) *webGateway {
	t.Helper()
	configPath, auditPath, recordingDir := writeWebRunConfig(t, run)

	ctrl, err := NewController(configPath, io.Discard)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	ctrl.engineWrap = run.engineWrap
	gateway := &webGateway{t: t, ctrl: ctrl, auditPath: auditPath, recordingDir: recordingDir, screens: map[string]string{}}
	ctrl.Subscribe(func(event string, payload any) {
		gateway.mu.Lock()
		gateway.frames = append(gateway.frames, webFrame{at: time.Now(), event: event, payload: payload})
		gateway.mu.Unlock()
	})
	ctx, cancel := context.WithCancel(context.Background())
	gateway.cancelStart = cancel
	if err := ctrl.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		closeCtx, closeCancel := context.WithTimeout(context.Background(), session.StopBudget+5*time.Second)
		defer closeCancel()
		_ = ctrl.Close(closeCtx)
		cancel()
	})
	return gateway
}

// writeWebRunConfig writes the run's configuration into a directory of its
// own, which also holds its journal and recordings, and points the user's
// directories there, so nothing reaches the real ones.
func writeWebRunConfig(t *testing.T, run webRun) (configPath, auditPath, recordingDir string) {
	t.Helper()
	dir := t.TempDir()
	for _, name := range []string{"APPDATA", "LOCALAPPDATA", "HOME", "USERPROFILE", "XDG_CONFIG_HOME"} {
		t.Setenv(name, dir)
	}
	auditPath = filepath.Join(dir, "audit", "journal.jsonl")
	if err := os.MkdirAll(filepath.Dir(auditPath), 0o700); err != nil {
		t.Fatal(err)
	}
	recordingDir = filepath.Join(dir, "recordings")
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	policy := run.policy
	if policy == "" {
		policy = "ask"
	}
	var b strings.Builder
	b.WriteString("version: 1\nbackend: pty\n")
	b.WriteString("sessions:\n  persist_on_exit: false\n  cleanup_on_success: true\n")
	b.WriteString("policies:\n  default_action: " + policy + "\n  dry_run: false\n")
	if strings.TrimSpace(run.rules) == "" {
		b.WriteString("  rules: []\n")
	} else {
		b.WriteString("  rules:\n" + run.rules)
	}
	auditMode := run.auditMode
	if auditMode == "" {
		auditMode = "metadata"
	}
	b.WriteString("audit:\n  enabled: true\n  mode: " + auditMode + "\n  path: " + webYAMLQuote(auditPath) + "\n  max_file_size_mb: 10\n  max_files: 5\n")
	if run.recording {
		b.WriteString("recording:\n  enabled: true\n  path: " + webYAMLQuote(recordingDir) + "\n")
	}
	b.WriteString("agents:\n")
	for _, agent := range run.agents {
		name := agent.name
		if name == "" {
			name = agent.id
		}
		b.WriteString("  - id: " + agent.id + "\n")
		b.WriteString("    name: " + webYAMLQuote(name) + "\n")
		b.WriteString("    command: [" + webYAMLQuote(executable) + ", " + webYAMLQuote("-test.run=^TestHelperProcessWebAgent$") + "]\n")
		b.WriteString("    cwd: " + webYAMLQuote(dir) + "\n")
		b.WriteString("    env:\n      " + webAgentModeEnv + ": " + agent.mode + "\n")
		keys := make([]string, 0, len(agent.env))
		for key := range agent.env {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			b.WriteString("      " + key + ": " + webYAMLQuote(agent.env[key]) + "\n")
		}
		b.WriteString("    adapter: " + agent.adapter + "\n")
		b.WriteString("    backend: pty\n")
	}
	b.WriteString(`intercept_patterns:
  - pattern: (?i)overwrite.*\[y/n\]
    description: overwrite confirmation
  - pattern: (?i)\[[yn]/[yn]\]
    description: yes/no confirmation
  - pattern: (?im)password:[[:space:]]*$
    description: password entry
  - pattern: (?i)do you want to continue
    description: continue confirmation
`)
	enabled := strconv.FormatBool(run.notifications)
	b.WriteString("notifications:\n  enabled: " + enabled + "\n  bell: false\n  desktop: false\n")
	configPath = filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte(b.String()), 0o600); err != nil {
		t.Fatal(err)
	}
	return configPath, auditPath, recordingDir
}

// runID is the run's ID, as every client reads it.
func (g *webGateway) runID() string {
	return g.ctrl.GetState().RunID
}

// agent is the agent's state as every client reads it.
func (g *webGateway) agent(sessionID string) AgentState {
	for _, agent := range g.ctrl.GetState().Agents {
		if strings.EqualFold(agent.SessionID, sessionID) {
			return agent
		}
	}
	g.t.Fatalf("no agent %s in the run", sessionID)
	return AgentState{}
}

// awaitAgent waits until the agent's state satisfies want.
func (g *webGateway) awaitAgent(sessionID string, within time.Duration, want func(AgentState) bool) AgentState {
	g.t.Helper()
	deadline := time.Now().Add(within)
	var last AgentState
	for time.Now().Before(deadline) {
		if last = g.agent(sessionID); want(last) {
			return last
		}
		time.Sleep(20 * time.Millisecond)
	}
	g.t.Fatalf("agent %s never reached the expected state; last %+v", sessionID, last)
	return last
}

// pending is the session's prompts as every client reads them.
func (g *webGateway) pending(sessionID string) []SupervisionEvent {
	var prompts []SupervisionEvent
	for _, prompt := range g.ctrl.GetState().PendingEvents {
		if strings.EqualFold(prompt.SessionID, sessionID) {
			prompts = append(prompts, prompt)
		}
	}
	return prompts
}

// awaitPending waits for a prompt of the session.
func (g *webGateway) awaitPending(sessionID string, within time.Duration) SupervisionEvent {
	g.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		if prompts := g.pending(sessionID); len(prompts) > 0 {
			return prompts[0]
		}
		time.Sleep(20 * time.Millisecond)
	}
	g.t.Fatalf("no prompt of %s within %s; screen: %q; journal:\n%s", sessionID, within, g.screen(sessionID), journalTrace(g.sessionJournal(sessionID)))
	return SupervisionEvent{}
}

// screen is the session's rendered screen now, as plain text, or the last one
// that carried a report of the agent's once it is gone.
func (g *webGateway) screen(sessionID string) string {
	g.ctrl.mu.RLock()
	rt := g.ctrl.runtime
	g.ctrl.mu.RUnlock()
	output := ""
	if rt != nil {
		output, _ = rt.Output(sessionID)
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	key := strings.ToLower(sessionID)
	// A screen with a report is never replaced by one without: a finished
	// session's screen may be blank, or partly repainted.
	if output != "" && (strings.Contains(output, "WEB-AGENT-") || !strings.Contains(g.screens[key], "WEB-AGENT-")) {
		g.screens[key] = output
	}
	return g.screens[key]
}

// awaitScreen waits until the session's screen carries text.
func (g *webGateway) awaitScreen(sessionID, text string, within time.Duration) string {
	g.t.Helper()
	deadline := time.Now().Add(within)
	screen := ""
	for time.Now().Before(deadline) {
		if screen = g.screen(sessionID); strings.Contains(screen, text) {
			return screen
		}
		time.Sleep(50 * time.Millisecond)
	}
	g.t.Fatalf("the screen of %s never showed %q within %s; screen:\n%s", sessionID, text, within, screen)
	return screen
}

// answers is what the agent reported receiving: its first answer, and how
// many extra ones followed.
func answers(screen string) (first string, count, extras int) {
	count = strings.Count(screen, webAgentAnswer)
	extras = strings.Count(screen, webAgentExtra)
	if index := strings.Index(screen, webAgentAnswer); index >= 0 {
		first = screen[index+len(webAgentAnswer):]
		if end := strings.IndexAny(first, "\r\n"); end >= 0 {
			first = first[:end]
		}
	}
	return first, count, extras
}

// broadcast returns the frames of one event the gateway broadcast so far.
func (g *webGateway) broadcast(event string) []webFrame {
	g.mu.Lock()
	defer g.mu.Unlock()
	var frames []webFrame
	for _, frame := range g.frames {
		if frame.event == event {
			frames = append(frames, frame)
		}
	}
	return frames
}

// awaitBroadcast reports whether a frame of event that match accepts is
// broadcast within the time given. The state every client reads changes under
// the core's lock, and the frame that tells the clients follows once the lock
// is released, possibly from another goroutine: a test that found the state
// changed has not necessarily seen the frame yet.
func (g *webGateway) awaitBroadcast(event string, within time.Duration, match func(any) bool) bool {
	deadline := time.Now().Add(within)
	for {
		for _, frame := range g.broadcast(event) {
			if match(frame.payload) {
				return true
			}
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// broadcastText is every frame the gateway broadcast so far but the output
// snapshots, as its clients received them, for leak checks. A snapshot is the
// agent's own screen, which every client watches as the agent painted it; the
// other frames are what the gateway says about the agent, and are its own.
func (g *webGateway) broadcastText() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	var text strings.Builder
	for _, frame := range g.frames {
		if frame.event == eventSnapshot {
			continue
		}
		encoded, _ := json.Marshal(wsEventMessage{Event: frame.event, Payload: frame.payload})
		text.Write(encoded)
		text.WriteByte('\n')
	}
	return text.String()
}

// journal reads every entry of the run's audit journal, in order.
func (g *webGateway) journal() []audit.Entry {
	g.t.Helper()
	raw, err := os.ReadFile(g.auditPath)
	if err != nil {
		g.t.Fatalf("read the journal: %v", err)
	}
	var entries []audit.Entry
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var entry audit.Entry
		if err := json.Unmarshal([]byte(line), &entry); err != nil {
			g.t.Fatalf("decode a journal line %q: %v", line, err)
		}
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Sequence < entries[j].Sequence })
	return entries
}

// sessionJournal is the journal's entries about one session.
func (g *webGateway) sessionJournal(sessionID string) []audit.Entry {
	var entries []audit.Entry
	for _, entry := range g.journal() {
		if strings.EqualFold(entry.SessionID, sessionID) {
			entries = append(entries, entry)
		}
	}
	return entries
}

// awaitJournal waits until an entry of the session satisfies want.
func (g *webGateway) awaitJournal(sessionID string, within time.Duration, want func(audit.Entry) bool) audit.Entry {
	g.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, entry := range g.sessionJournal(sessionID) {
			if want(entry) {
				return entry
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	g.t.Fatalf("no such journal entry for %s within %s; journal:\n%s", sessionID, within, journalTrace(g.sessionJournal(sessionID)))
	return audit.Entry{}
}

// journalTrace names each entry by its kind, who decided and its outcome.
func journalTrace(entries []audit.Entry) string {
	var trace strings.Builder
	for _, entry := range entries {
		fmt.Fprintf(&trace, "  #%d %s event=%s decision=%s by=%s operator=%q outcome=%s reason=%s metadata=%v\n",
			entry.Sequence, entry.Kind, entry.EventID, entry.Decision, entry.DecisionBy, entry.Operator, entry.Outcome, entry.Reason, entry.Metadata)
	}
	return trace.String()
}

// inject takes in one event of the run's session stream, as the event loop
// does, for what no real agent can be made to produce on demand: a backend
// stream error, a lost tmux session.
func (g *webGateway) inject(event session.Event) {
	injectEvent(g.ctrl, event)
}

// stateText is the state as a client receives it, for leak checks.
func stateText(t *testing.T, state AppState) string {
	t.Helper()
	encoded, err := json.Marshal(state)
	if err != nil {
		t.Fatalf("marshal the state: %v", err)
	}
	return string(encoded)
}

// webOperator is the operator the tests answer as, from one connection.
var webOperator = supervise.Actor{Identity: "alice", Role: supervise.RoleOperator, ConnID: "conn-alice"}

// serve puts the gateway's websocket in front of the run, as Serve does, with
// two operators, alice and carol, and a viewer, dave, each by their token,
// and returns its address. A test that goes through it goes through what a
// browser does: the RPC dispatch, the viewer's list of calls and the actor
// each connection answers as.
func (g *webGateway) serve() string {
	g.t.Helper()
	handler := newGatewayHandler(g.ctrl, map[string]AuthIdentity{
		"opAlice":  {Identity: "alice", Role: RoleOperator},
		"opCarol":  {Identity: "carol", Role: RoleOperator},
		"viewDave": {Identity: "dave", Role: RoleViewer},
	}, false, 0, "", io.Discard)
	server := httptest.NewServer(handler)
	g.t.Cleanup(func() {
		handler.closeClients()
		server.Close()
	})
	return server.URL
}
