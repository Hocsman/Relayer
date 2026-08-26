package tui

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

type resyncCall struct {
	id      string
	columns int
	rows    int
}

type fakeAttachBackend struct {
	*fakeBackend
	events chan session.Event

	name           string
	attachErr      error
	resyncErr      error
	resyncedOutput string
	resyncEvent    session.Event
	pendingEvent   *adapters.Event
	attachCalls    []string
	resyncCalls    []resyncCall
}

func newFakeAttachBackend(events chan session.Event) *fakeAttachBackend {
	return &fakeAttachBackend{
		fakeBackend: newFakeBackend(),
		events:      events,
		name:        "tmux",
	}
}

func (b *fakeAttachBackend) Name() string { return b.name }

func (b *fakeAttachBackend) AttachCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	b.mu.Lock()
	b.attachCalls = append(b.attachCalls, id)
	err := b.attachErr
	b.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return exec.CommandContext(ctx, "tmux", "attach-session", "-t", id), nil
}

func (b *fakeAttachBackend) Resync(_ context.Context, id string, columns, rows int) error {
	b.mu.Lock()
	b.resyncCalls = append(b.resyncCalls, resyncCall{id: id, columns: columns, rows: rows})
	if b.resyncedOutput != "" {
		b.outputs[id] = b.resyncedOutput
	}
	err := b.resyncErr
	event := b.resyncEvent
	b.pendingEvent = nil
	if detected, ok := event.(session.AdapterEvent); ok {
		copy := detected.Event.Clone()
		b.pendingEvent = &copy
	}
	b.mu.Unlock()
	if event != nil {
		b.events <- event
	}
	return err
}

func (b *fakeAttachBackend) PendingEvent(_ context.Context, _ string) (*adapters.Event, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.pendingEvent == nil {
		return nil, nil
	}
	copy := b.pendingEvent.Clone()
	return &copy, nil
}

func (b *fakeAttachBackend) attachSnapshot() ([]string, []resyncCall) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.attachCalls...), append([]resyncCall(nil), b.resyncCalls...)
}

func TestEnterOnTmuxAgentExecutesAttachAndResynchronizes(t *testing.T) {
	events := make(chan session.Event, 4)
	backend := newFakeAttachBackend(events)
	t.Cleanup(backend.cancel)
	backend.setOutput("tmux-agent", "before attach")
	backend.resyncedOutput = "after detach"
	backend.resyncEvent = testAdapterEvent("tmux-agent", "confirmation", "resynced prompt", false)
	application, err := NewModel(
		backend,
		events,
		[]Pane{{ID: "tmux-agent", Name: "Tmux Agent", Backend: "tmux"}},
		100,
		30,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	application.refreshPaneOutput(0)

	var executed *exec.Cmd
	application.execProcess = func(command *exec.Cmd, callback tea.ExecCallback) tea.Cmd {
		executed = command
		return func() tea.Msg { return callback(nil) }
	}
	application, attach := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if attach == nil || application.attachPending != "tmux-agent" {
		t.Fatalf("attach state = command %v pending %q", attach != nil, application.attachPending)
	}
	attachCalls, _ := backend.attachSnapshot()
	if len(attachCalls) != 1 || attachCalls[0] != "tmux-agent" {
		t.Fatalf("AttachCommand calls = %#v", attachCalls)
	}
	if executed == nil || strings.Join(executed.Args, "|") != "tmux|attach-session|-t|tmux-agent" {
		t.Fatalf("executed attach argv = %#v", executed)
	}

	returned := executeCommand(t, attach)
	if _, ok := returned.(attachFinishedMsg); !ok {
		t.Fatalf("attach callback returned %T", returned)
	}
	application, resync := updateModel(t, application, returned)
	if resync == nil || application.attachPending != "" {
		t.Fatalf("return state = resync %v pending %q", resync != nil, application.attachPending)
	}
	resynced := executeCommand(t, resync)
	application, _ = updateModel(t, application, resynced)
	_, resyncCalls := backend.attachSnapshot()
	wantColumns, wantRows := AgentViewportSize(100, 30, 1, 0)
	if len(resyncCalls) != 1 || resyncCalls[0] != (resyncCall{id: "tmux-agent", columns: wantColumns, rows: wantRows}) {
		t.Fatalf("Resync calls = %#v, want %dx%d", resyncCalls, wantColumns, wantRows)
	}
	if got := application.panes[0].viewport.View(); !strings.Contains(got, "after detach") {
		t.Fatalf("resynced output is not visible: %q", got)
	}
	if !strings.Contains(strings.Join(application.logs, "\n"), "sortie, état, prompts et taille") {
		t.Fatal("successful resynchronization is not logged")
	}

	// Resync queued the backend's freshly detected prompt. The regular event
	// subscription consumes it and restores the human-intervention state.
	eventMessage := executeCommand(t, application.Init())
	application, _ = updateModel(t, application, eventMessage)
	if application.inputTarget != "tmux-agent" || !application.panes[0].blocked || !application.input.Focused() {
		t.Fatalf("resynced prompt state = target %q blocked %t focused %t", application.inputTarget, application.panes[0].blocked, application.input.Focused())
	}
}

func TestAttachFailureStillResynchronizesAndReportsErrors(t *testing.T) {
	events := make(chan session.Event, 1)
	backend := newFakeAttachBackend(events)
	t.Cleanup(backend.cancel)
	backend.resyncErr = errors.New("capture failed")
	application, err := NewModel(
		backend,
		events,
		[]Pane{{ID: "tmux-agent", Name: "Tmux Agent", Backend: "tmux"}},
		80,
		24,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	application.execProcess = func(_ *exec.Cmd, callback tea.ExecCallback) tea.Cmd {
		return func() tea.Msg { return callback(errors.New("detach failed")) }
	}
	application, attach := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	application, resync := updateModel(t, application, executeCommand(t, attach))
	application, _ = updateModel(t, application, executeCommand(t, resync))
	logs := strings.Join(application.logs, "\n")
	if !strings.Contains(logs, "detach failed") || !strings.Contains(logs, "capture failed") {
		t.Fatalf("attachment/resync errors are not visible:\n%s", logs)
	}
}

func TestAttachResyncClearsPromptEventsAnsweredInsideTmux(t *testing.T) {
	events := make(chan session.Event, 2)
	backend := newFakeAttachBackend(events)
	t.Cleanup(backend.cancel)
	application, err := NewModel(
		backend,
		events,
		[]Pane{{ID: "tmux-agent", Name: "Tmux Agent", Backend: "tmux"}},
		100,
		30,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	application.execProcess = func(_ *exec.Cmd, callback tea.ExecCallback) tea.Cmd {
		return func() tea.Msg { return callback(nil) }
	}

	application, attach := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	application, resync := updateModel(t, application, executeCommand(t, attach))
	stale := testAdapterEvent("tmux-agent", "password", "queued before attach", true)
	backend.mu.Lock()
	copy := stale.Event.Clone()
	backend.pendingEvent = &copy
	backend.mu.Unlock()
	application, _ = updateModel(t, application, stale)
	application.input.SetValue("must-be-erased")
	if !application.panes[0].blocked {
		t.Fatal("pre-resync prompt was not represented")
	}

	// Resync observes that the prompt was answered directly in tmux and makes
	// its empty cached state authoritative over the queued event.
	application, _ = updateModel(t, application, executeCommand(t, resync))
	if application.panes[0].blocked || application.inputTarget != "" || application.input.Value() != "" {
		t.Fatalf("stale prompt survived resync: blocked=%t target=%q value=%q",
			application.panes[0].blocked, application.inputTarget, application.input.Value())
	}

	// The same event arriving after the resync must also be discarded.
	application, _ = updateModel(t, application, stale)
	if application.panes[0].blocked || application.inputTarget != "" {
		t.Fatal("late stale prompt reblocked the pane")
	}
}

func TestAttachCommandConstructionFailureDoesNotSuspendTUI(t *testing.T) {
	events := make(chan session.Event, 1)
	backend := newFakeAttachBackend(events)
	t.Cleanup(backend.cancel)
	backend.attachErr = errors.New("no owned session")
	application, err := NewModel(
		backend,
		events,
		[]Pane{{ID: "tmux-agent", Name: "Tmux Agent", Backend: "tmux"}},
		80,
		24,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	application, command := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil || application.attachPending != "" {
		t.Fatalf("failed construction suspended TUI: command=%v pending=%q", command != nil, application.attachPending)
	}
	if !strings.Contains(strings.Join(application.logs, "\n"), "no owned session") {
		t.Fatal("attach construction error was not logged")
	}
}

func TestPendingPromptEnterTakesPriorityOverAttachAndPTYNavigation(t *testing.T) {
	events := make(chan session.Event, 1)
	backend := newFakeAttachBackend(events)
	t.Cleanup(backend.cancel)
	application, err := NewModel(
		backend,
		events,
		[]Pane{
			{ID: "pty-agent", Name: "PTY Agent", Backend: "pty"},
			{ID: "tmux-agent", Name: "Tmux Agent", Backend: "tmux"},
		},
		120,
		30,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	application, _ = updateModel(t, application, testAdapterEvent("pty-agent", "confirmation", "PTY prompt", false))
	application.input.SetValue("Y")
	// Deliberately move focus onto the tmux agent. The waiting response must
	// still win over attachment when Enter is pressed.
	application.focus = FocusTarget{Kind: FocusAgent, AgentID: "tmux-agent"}
	application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if delivery == nil {
		t.Fatal("Enter did not create the PTY response command")
	}
	message := executeCommand(t, delivery)
	if _, ok := message.(inputDeliveredMsg); !ok {
		t.Fatalf("Enter returned %T, want inputDeliveredMsg", message)
	}
	inputCalls := backend.inputSnapshot()
	attachCalls, _ := backend.attachSnapshot()
	if len(inputCalls) != 1 || inputCalls[0] != (inputCall{id: "pty-agent", value: "Y"}) {
		t.Fatalf("PTY input calls = %#v", inputCalls)
	}
	if len(attachCalls) != 0 {
		t.Fatalf("prompt response unexpectedly attached tmux: %#v", attachCalls)
	}
}

func TestSelectedBackendAndTmuxAttachHelpAreVisible(t *testing.T) {
	events := make(chan session.Event, 1)
	backend := newFakeAttachBackend(events)
	t.Cleanup(backend.cancel)
	application, err := NewModel(
		backend,
		events,
		[]Pane{
			{ID: "pty-agent", Name: "PTY Agent", Backend: "pty"},
			{ID: "tmux-agent", Name: "Tmux Agent", Backend: "tmux"},
		},
		180,
		32,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	view := application.View()
	for _, text := range []string{
		"[PTY]",
		"[TMUX]",
		"BACKEND PTY/TMUX",
		"Entrée: ouvrir/répondre",
		"Ctrl+B puis D: revenir à Relayer",
	} {
		if !strings.Contains(view, text) {
			t.Fatalf("view does not expose %q:\n%s", text, view)
		}
	}
}
