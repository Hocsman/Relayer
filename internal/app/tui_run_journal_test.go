package app

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/Hocsman/Relayer/internal/tmuxbackend"
	"github.com/Hocsman/Relayer/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

// tuiRunHarness takes the terminal interface's command through run() from end
// to end: fake backends stand in for the agents, a fake program for Bubble Tea,
// and the journal is a real one in the test's own directory, so the entries a
// whole run leaves can be pinned without a terminal.
type tuiRunHarness struct {
	t          *testing.T
	configPath string
	journal    string
	pty        *routerFakeBackend
	tmux       *routerFakeBackend
	program    *tuiRunProgram

	columns, rows int

	mu          sync.Mutex
	tmuxOptions []tmuxbackend.Options
}

// newTUIRunHarness writes a configuration with one agent per backend given,
// named agent-1, agent-2 and so on in that order.
func newTUIRunHarness(t *testing.T, persistOnExit bool, backends ...string) *tuiRunHarness {
	t.Helper()
	directory := t.TempDir()
	h := &tuiRunHarness{
		t:          t,
		configPath: filepath.Join(directory, "config.yaml"),
		// The recorder creates the journal's directory itself, private, rather
		// than trusting whatever permissions the temporary root was given.
		journal: filepath.Join(directory, "journal", "audit.jsonl"),
		pty:     newRouterFakeBackend(agent.BackendPTY),
		tmux:    newRouterFakeBackend(agent.BackendTmux),
		columns: 120,
		rows:    40,
	}
	h.program = &tuiRunProgram{journal: h.journal}
	// The real backends name the adapter each session runs, and the run's
	// session entries carry it, so the fakes must name it too for a pin to see
	// it.
	h.pty.reportAdapter = true
	h.tmux.reportAdapter = true

	// Notifications are on when the block is absent, and a test must never
	// raise a real desktop notification.
	var content strings.Builder
	content.WriteString("version: 1\n" +
		"backend: pty\n" +
		"sessions:\n" +
		"  persist_on_exit: " + boolText(persistOnExit) + "\n" +
		"  cleanup_on_success: true\n" +
		"notifications:\n" +
		"  enabled: false\n" +
		"audit:\n" +
		"  enabled: true\n" +
		"  mode: metadata\n" +
		"  path: " + strconv.Quote(h.journal) + "\n" +
		"  max_file_size_mb: 1\n" +
		"  max_files: 1\n" +
		"agents:\n")
	for index, backend := range backends {
		fmt.Fprintf(&content,
			"  - id: agent-%[1]d\n"+
				"    name: Agent %[1]d\n"+
				"    command: [runner, argument-%[1]d]\n"+
				"    backend: %[2]s\n"+
				"    adapter: generic\n",
			index+1, backend)
	}
	content.WriteString("intercept_patterns:\n" +
		"  - pattern: continue\n" +
		"    description: Continue\n")
	if err := os.WriteFile(h.configPath, []byte(content.String()), 0o600); err != nil {
		t.Fatalf("write the run's configuration: %v", err)
	}
	return h
}

func (h *tuiRunHarness) run(arguments ...string) error {
	h.t.Helper()
	return run(append([]string{"--config", h.configPath}, arguments...), io.Discard, h.dependencies())
}

func (h *tuiRunHarness) dependencies() backendDependencies {
	return backendDependencies{
		lookup: func(name string) (string, error) {
			if name != "tmux" {
				return "", fmt.Errorf("unexpected lookup of %q", name)
			}
			return "/fake/bin/tmux", nil
		},
		probeTmux: func(context.Context, string) error { return nil },
		newAudit: func(configuration audit.Config) (*audit.Recorder, error) {
			if err := h.ownJournal(configuration); err != nil {
				return nil, err
			}
			return audit.Open(configuration)
		},
		newAuditForRun: func(configuration audit.Config, runID string) (*audit.Recorder, error) {
			if err := h.ownJournal(configuration); err != nil {
				return nil, err
			}
			return audit.Open(configuration, audit.WithRunID(runID))
		},
		newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
			return h.pty, nil
		},
		newTmux: func(_ context.Context, _ chan<- session.Event, _ *adapters.Registry, _ int, options tmuxbackend.Options) (terminal.Backend, error) {
			h.mu.Lock()
			h.tmuxOptions = append(h.tmuxOptions, options)
			h.mu.Unlock()
			return h.tmux, nil
		},
		newProgram: func(model tea.Model, options ...tea.ProgramOption) uiProgram {
			h.program.build(model, options)
			return h.program
		},
		terminalSize: func() (int, int) { return h.columns, h.rows },
	}
}

// ownJournal refuses every journal but the test's own. An audit block the
// loader did not take in would fall back to the profile's default path, which
// is the real journal of whoever runs the tests.
func (h *tuiRunHarness) ownJournal(configuration audit.Config) error {
	if !configuration.Enabled || filepath.Clean(configuration.Path) != filepath.Clean(h.journal) {
		h.t.Errorf("the run opened the journal %q (enabled %t), want %q",
			configuration.Path, configuration.Enabled, h.journal)
		return errors.New("the run did not open the test's journal")
	}
	return nil
}

func (h *tuiRunHarness) entries() []audit.Entry {
	h.t.Helper()
	entries, err := audit.ReadEntriesFromFile(h.journal, audit.Filter{})
	if err != nil {
		h.t.Fatalf("read the run's journal: %v", err)
	}
	return entries
}

func (h *tuiRunHarness) builtTmuxOptions() []tmuxbackend.Options {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]tmuxbackend.Options(nil), h.tmuxOptions...)
}

func (h *tuiRunHarness) starts(backend *routerFakeBackend) []routerStartCall {
	backend.mu.Lock()
	defer backend.mu.Unlock()
	return append([]routerStartCall(nil), backend.starts...)
}

// tuiRunProgram stands in for Bubble Tea. Its Run returns at once, as if the
// person quit the moment the interface came up, and keeps the journal as it
// stood then: what comes before is the run's start, what comes after its end.
// It keeps the options run built it with, which it cannot apply itself, so
// that a test can hand them to a real program.
type tuiRunProgram struct {
	journal string

	mu       sync.Mutex
	runErr   error
	model    tea.Model
	options  []tea.ProgramOption
	builds   int
	runs     int
	atRun    []audit.Entry
	atRunErr error
}

func (p *tuiRunProgram) build(model tea.Model, options []tea.ProgramOption) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.builds++
	p.model = model
	p.options = append([]tea.ProgramOption(nil), options...)
}

func (p *tuiRunProgram) builtOptions() []tea.ProgramOption {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]tea.ProgramOption(nil), p.options...)
}

func (p *tuiRunProgram) Run() (tea.Model, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.runs++
	p.atRun, p.atRunErr = audit.ReadEntriesFromFile(p.journal, audit.Filter{})
	return p.model, p.runErr
}

// Send drops the message, as a program that has already quit does.
func (*tuiRunProgram) Send(tea.Msg) {}

func (p *tuiRunProgram) snapshot() (builds, runs int, model tea.Model, atRun []audit.Entry, atRunErr error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.builds, p.runs, p.model, append([]audit.Entry(nil), p.atRun...), p.atRunErr
}

// tuiJournalRow is the part of an entry a run-level pin compares. Every entry
// of these runs is the system's; assertTUIJournal checks that apart.
type tuiJournalRow struct {
	Kind      audit.Kind
	Outcome   audit.Outcome
	Reason    string
	SessionID string
	AgentID   string
	Backend   string
	Adapter   string
}

func assertTUIJournal(t *testing.T, when string, entries []audit.Entry, want []tuiJournalRow) {
	t.Helper()
	got := make([]tuiJournalRow, len(entries))
	for index, entry := range entries {
		got[index] = tuiJournalRow{
			Kind:      entry.Kind,
			Outcome:   entry.Outcome,
			Reason:    entry.Reason,
			SessionID: entry.SessionID,
			AgentID:   entry.AgentID,
			Backend:   entry.Backend,
			Adapter:   entry.Adapter,
		}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("journal %s:\n%s\nwant:\n%s", when, formatTUIJournalRows(got), formatTUIJournalRows(want))
	}
	for index, entry := range entries {
		if entry.RunID == "" || entry.RunID != entries[0].RunID {
			t.Fatalf("entry %d (%s) has run_id %q, want the run's one %q", index+1, entry.Kind, entry.RunID, entries[0].RunID)
		}
		if entry.DecisionBy != audit.DecisionBySystem {
			t.Fatalf("entry %d (%s) is decided by %q, want the system", index+1, entry.Kind, entry.DecisionBy)
		}
	}
}

func formatTUIJournalRows(rows []tuiJournalRow) string {
	lines := make([]string, len(rows))
	for index, row := range rows {
		lines[index] = fmt.Sprintf("  %+v", row)
	}
	return strings.Join(lines, "\n")
}

// cleanTUIRunStart and cleanTUIRunEnd are the journal of a run of one PTY and
// one tmux agent that starts and ends without a fault.
func cleanTUIRunStart() []tuiJournalRow {
	return []tuiJournalRow{
		{Kind: audit.KindRunStarted, Outcome: audit.OutcomeStarted},
		{Kind: audit.KindSessionStarted, Outcome: audit.OutcomeStarted, SessionID: "agent-1", AgentID: "agent-1", Backend: agent.BackendPTY, Adapter: agent.AdapterGeneric},
		{Kind: audit.KindSessionStarted, Outcome: audit.OutcomeStarted, SessionID: "agent-2", AgentID: "agent-2", Backend: agent.BackendTmux, Adapter: agent.AdapterGeneric},
	}
}

func cleanTUIRunEnd(runOutcome audit.Outcome) []tuiJournalRow {
	return []tuiJournalRow{
		{Kind: audit.KindSupervisionFinished, Outcome: audit.OutcomeFinished, Reason: "supervision_ended", SessionID: "agent-1", AgentID: "agent-1", Backend: agent.BackendPTY, Adapter: agent.AdapterGeneric},
		{Kind: audit.KindSupervisionFinished, Outcome: audit.OutcomeFinished, Reason: "supervision_ended", SessionID: "agent-2", AgentID: "agent-2", Backend: agent.BackendTmux, Adapter: agent.AdapterGeneric},
		{Kind: audit.KindSessionCleanup, Outcome: audit.OutcomeSucceeded, Reason: "backend_cleanup_completed", SessionID: "agent-1", AgentID: "agent-1", Backend: agent.BackendPTY, Adapter: agent.AdapterGeneric},
		{Kind: audit.KindSessionCleanup, Outcome: audit.OutcomeSucceeded, Reason: "backend_cleanup_completed", SessionID: "agent-2", AgentID: "agent-2", Backend: agent.BackendTmux, Adapter: agent.AdapterGeneric},
		{Kind: audit.KindRunFinished, Outcome: runOutcome},
	}
}

// The terminal interface's run-level journal, entry by entry, so that any change
// to how the command is wired shows here as a change to the test.
func TestATUIRunJournalsItsStartAndEnd(t *testing.T) {
	h := newTUIRunHarness(t, false, agent.BackendPTY, agent.BackendTmux)

	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	builds, runs, model, atRun, atRunErr := h.program.snapshot()
	if builds != 1 || runs != 1 {
		t.Fatalf("the interface was built %d time(s) and run %d time(s), want once each", builds, runs)
	}
	if _, ok := model.(*tui.Model); !ok {
		t.Fatalf("the program was given %T, want the interface's model", model)
	}
	if atRunErr != nil {
		t.Fatalf("read the journal while the interface ran: %v", atRunErr)
	}
	assertTUIJournal(t, "while the interface ran", atRun, cleanTUIRunStart())
	assertTUIJournal(t, "after the run", h.entries(), append(cleanTUIRunStart(), cleanTUIRunEnd(audit.OutcomeSucceeded)...))

	// The tmux backend is built without the run's ID today, so the names of
	// its sessions do not carry the journal's run_id.
	options := h.builtTmuxOptions()
	if len(options) != 1 {
		t.Fatalf("the tmux backend was built %d time(s), want once", len(options))
	}
	if options[0].RunID != "" {
		t.Fatalf("tmux RunID = %q, want empty", options[0].RunID)
	}
}

// A failure of the interface itself is the run's failure: run() returns it,
// and the journal says so in its last entry. The agents are still ended and
// cleaned up exactly as in a clean run.
func TestATUIRunWhoseInterfaceFailsIsJournaledFailed(t *testing.T) {
	h := newTUIRunHarness(t, false, agent.BackendPTY, agent.BackendTmux)
	failure := errors.New("planned interface failure")
	h.program.runErr = failure

	if err := h.run(); !errors.Is(err, failure) {
		t.Fatalf("run error = %v, want the interface's failure", err)
	}

	entries := h.entries()
	if len(entries) == 0 {
		t.Fatal("the run left an empty journal")
	}
	if last := entries[len(entries)-1]; last.Kind != audit.KindRunFinished || last.Outcome != audit.OutcomeFailed {
		t.Fatalf("the journal ends with %s %s, want %s %s",
			last.Kind, last.Outcome, audit.KindRunFinished, audit.OutcomeFailed)
	}
	assertTUIJournal(t, "after the run", entries, append(cleanTUIRunStart(), cleanTUIRunEnd(audit.OutcomeFailed)...))
}

// A startup that fails part-way: agent 1 runs on tmux with persist_on_exit,
// agent 2 does not start. The interface never comes up, and the journal is
// pinned as the terminal command writes it today.
func TestATUIRunWhoseSecondAgentFailsToStart(t *testing.T) {
	h := newTUIRunHarness(t, true, agent.BackendTmux, agent.BackendTmux)
	failure := errors.New("planned start failure")
	h.tmux.startErrs = map[string]error{"agent-2": failure}

	if err := h.run(); !errors.Is(err, failure) {
		t.Fatalf("run error = %v, want it to wrap the start failure", err)
	}

	if builds, runs, _, _, _ := h.program.snapshot(); builds != 0 || runs != 0 {
		t.Fatalf("the interface was built %d time(s) and run %d time(s) for a run that never started", builds, runs)
	}
	assertTUIJournal(t, "after the run", h.entries(), []tuiJournalRow{
		{Kind: audit.KindRunStarted, Outcome: audit.OutcomeStarted},
		{Kind: audit.KindSessionStarted, Outcome: audit.OutcomeStarted, SessionID: "agent-1", AgentID: "agent-1", Backend: agent.BackendTmux, Adapter: agent.AdapterGeneric},
		// The command's own startup failure entry names no agent, backend or
		// adapter.
		{Kind: audit.KindBackendError, Outcome: audit.OutcomeFailed, Reason: "session_start_failed"},
		{Kind: audit.KindSupervisionFinished, Outcome: audit.OutcomeFinished, Reason: "supervision_ended", SessionID: "agent-1", AgentID: "agent-1", Backend: agent.BackendTmux, Adapter: agent.AdapterGeneric},
		// persist_on_exit holds even for a run that failed to start: the tmux
		// session is left behind.
		{Kind: audit.KindSessionCleanup, Outcome: audit.OutcomeSkipped, Reason: "persistence_requested", SessionID: "agent-1", AgentID: "agent-1", Backend: agent.BackendTmux, Adapter: agent.AdapterGeneric},
		{Kind: audit.KindRunFinished, Outcome: audit.OutcomeFailed},
	})
}

// Each agent starts at the size of its own pane in the first layout, not at
// the whole terminal's, so a fast agent's first screen is already drawn for
// the pane it is shown in.
func TestATUIRunStartsEachAgentAtItsPaneSize(t *testing.T) {
	h := newTUIRunHarness(t, false, agent.BackendPTY, agent.BackendPTY, agent.BackendPTY)
	h.columns, h.rows = 160, 48

	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	starts := h.starts(h.pty)
	if len(starts) != 3 {
		t.Fatalf("starts = %#v, want three", starts)
	}
	for index, start := range starts {
		columns, rows := tui.AgentViewportSize(160, 48, 3, index)
		want := terminal.Size{Columns: columns, Rows: rows}
		// Were the pane the same at the fallback 80x24, the test could not tell
		// the injected terminal from a console that was never asked.
		fallbackColumns, fallbackRows := tui.AgentViewportSize(80, 24, 3, index)
		if want == (terminal.Size{Columns: fallbackColumns, Rows: fallbackRows}) {
			t.Fatalf("agent %d gets the same pane at 160x48 as at 80x24", index+1)
		}
		if id := fmt.Sprintf("agent-%d", index+1); start.spec.ID != id || start.size != want {
			t.Fatalf("start %d = %s at %+v, want %s at %+v", index+1, start.spec.ID, start.size, id, want)
		}
	}
}

// The deprecated --pane1 still replaces the first agent's command, as a direct
// argv, and leaves the others as configured.
func TestATUIRunHonoursThePaneOverrides(t *testing.T) {
	h := newTUIRunHarness(t, false, agent.BackendPTY, agent.BackendPTY)

	if err := h.run("--pane1", `replacement --flag "two words"`); err != nil {
		t.Fatalf("run: %v", err)
	}

	starts := h.starts(h.pty)
	if len(starts) != 2 {
		t.Fatalf("starts = %#v, want two", starts)
	}
	if got, want := starts[0].spec.Command, []string{"replacement", "--flag", "two words"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("agent 1 command = %q, want %q", got, want)
	}
	if starts[0].spec.Shell != "" {
		t.Fatalf("agent 1 runs through a shell: %q", starts[0].spec.Shell)
	}
	if got, want := starts[1].spec.Command, []string{"runner", "argument-2"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("agent 2 command = %q, want its configured %q", got, want)
	}
}

// The interface takes the terminal's alternate screen and has the mouse
// reported as it moves from cell to cell. The stand-in cannot show what the
// options run gave it do, so they are handed to a real Bubble Tea program,
// built by the very newProgram the command hands run. That program reads one
// key from a string instead of the console, writes into a buffer, and quits on
// that key.
func TestATUIRunBuildsItsProgramOnTheAltScreenWithTheMouse(t *testing.T) {
	h := newTUIRunHarness(t, false, agent.BackendPTY)
	if err := h.run(); err != nil {
		t.Fatalf("run: %v", err)
	}

	var output bytes.Buffer
	options := append(h.program.builtOptions(),
		// The program must never take the test's own console, nor its signals.
		// The key it quits on comes through the input it is given, so a factory
		// that replaced that input would leave it running until the bound below.
		tea.WithInput(strings.NewReader(tuiQuitKey)),
		tea.WithOutput(&output),
		tea.WithoutSignalHandler(),
		// A context run gives its program is the run's own and ends with it, and
		// a program started on an ended context is killed at once. This one is
		// started after run has returned, so it runs on a context of its own.
		tea.WithContext(context.Background()),
	)
	newProgram := productionBackendDependencies().newProgram
	if newProgram == nil {
		t.Fatal("the command hands run no way to build its program")
	}
	// A nil *tea.Program inside the interface is not a nil interface, and its
	// Run would panic on the goroutine below, which ends the whole test binary
	// rather than this test.
	program, ok := newProgram(tuiQuitOnKeyModel{}, options...).(*tea.Program)
	if !ok || program == nil {
		t.Fatal("the command builds no Bubble Tea program")
	}
	finished := make(chan error, 1)
	go func() {
		_, err := program.Run()
		finished <- err
	}()
	select {
	case err := <-finished:
		if err != nil {
			t.Fatalf("run the real program: %v", err)
		}
	case <-time.After(10 * time.Second):
		// A program built without the input above never reads the key it quits
		// on: it reads the console, or nothing at all. Killing it fails the
		// test now rather than at the package's timeout.
		go program.Kill()
		t.Fatal("the real program did not quit within 10s; one built without the input it was given never reads the key it quits on")
	}

	written := output.String()
	for _, want := range []struct {
		what     string
		sequence string
	}{
		{"enter the alternate screen", "\x1b[?1049h"},
		{"report the mouse's motion from cell to cell", "\x1b[?1002h"},
	} {
		if !strings.Contains(written, want.sequence) {
			t.Errorf("the program did not %s (%q); it wrote %q", want.what, want.sequence, written)
		}
	}
}

// tuiQuitKey is the one key the real program is given to read.
const tuiQuitKey = "q"

// tuiQuitOnKeyModel asks to quit when it reads tuiQuitKey, and on nothing else,
// so a real program goes through its startup, reads the input it was given,
// and shuts down. It never quits from Init: a program whose input was taken
// away would then pass as well.
type tuiQuitOnKeyModel struct{}

func (tuiQuitOnKeyModel) Init() tea.Cmd { return nil }

func (m tuiQuitOnKeyModel) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	if key, ok := msg.(tea.KeyMsg); ok && key.String() == tuiQuitKey {
		return m, tea.Quit
	}
	return m, nil
}

func (tuiQuitOnKeyModel) View() string { return "" }
