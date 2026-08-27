package tui

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/policy"
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
	automaticCalls []string
	resyncCalls    []resyncCall
}

type fakeAttachWithoutSnapshot struct {
	*fakeBackend
	attachCalls []string
}

func (b *fakeAttachWithoutSnapshot) Name() string { return "tmux" }

func (b *fakeAttachWithoutSnapshot) AttachCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	b.mu.Lock()
	b.attachCalls = append(b.attachCalls, id)
	b.mu.Unlock()
	return exec.CommandContext(ctx, "tmux", "attach-session", "-t", id), nil
}

func (b *fakeAttachWithoutSnapshot) Resync(context.Context, string, int, int) error { return nil }

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

func (b *fakeAttachBackend) SendAutomaticDecision(
	_ string,
	event adapters.Event,
	_ adapters.Decision,
) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.automaticCalls = append(b.automaticCalls, event.ID)
	if b.pendingEvent == nil || b.pendingEvent.ID != event.ID {
		return adapters.ErrEventMismatch
	}
	b.pendingEvent = nil
	return nil
}

func (b *fakeAttachBackend) attachSnapshot() ([]string, []resyncCall) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.attachCalls...), append([]resyncCall(nil), b.resyncCalls...)
}

func (b *fakeAttachBackend) automaticSnapshot() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]string(nil), b.automaticCalls...)
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
	if resync == nil || application.attachPending != "tmux-agent" {
		t.Fatalf("return state = resync %v pending %q", resync != nil, application.attachPending)
	}
	resynced := executeCommand(t, resync)
	application, _ = updateModel(t, application, resynced)
	if application.attachPending != "" {
		t.Fatalf("resync left attach pending = %q", application.attachPending)
	}
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

func TestPolicyFrozenTmuxAgentRefusesAttach(t *testing.T) {
	events := make(chan session.Event, 1)
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
	application.panes[0].policyFrozen = true
	application.panes[0].policyTag = "LIVRAISON INCERTAINE"

	application, command := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil {
		t.Fatal("frozen pane returned an attach command")
	}
	attachCalls, _ := backend.attachSnapshot()
	if len(attachCalls) != 0 {
		t.Fatalf("AttachCommand calls = %#v, want none", attachCalls)
	}
	if application.attachPending != "" {
		t.Fatalf("attach pending = %q", application.attachPending)
	}
	logs := strings.Join(application.logs, "\n")
	if !strings.Contains(logs, "arrêt requis") {
		t.Fatalf("frozen attach refusal was not logged safely:\n%s", logs)
	}
}

func TestAutomaticDecisionInFlightRefusesTmuxAttach(t *testing.T) {
	events := make(chan session.Event, 1)
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
	key := semanticEventKey("tmux-agent", "event-1")
	application.automaticBySession[key.sessionID] = key

	application, command := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil {
		t.Fatal("automatic decision in flight returned an attach command")
	}
	attachCalls, _ := backend.attachSnapshot()
	if len(attachCalls) != 0 {
		t.Fatalf("AttachCommand calls = %#v, want none", attachCalls)
	}
	if application.attachPending != "" {
		t.Fatalf("attach pending = %q", application.attachPending)
	}
	logs := strings.Join(application.logs, "\n")
	if !strings.Contains(logs, "décision automatique en cours") {
		t.Fatalf("in-flight attach refusal was not logged safely:\n%s", logs)
	}
}

func TestManualDecisionInFlightRefusesTmuxAttach(t *testing.T) {
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
		100,
		30,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	detected := testAdapterEvent("pty-agent", "confirmation", "PTY prompt", false)
	pending := detected.Event.Clone()
	backend.pendingEvent = &pending
	application, _ = updateModel(t, application, detected)
	application.input.SetValue("Y")
	application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if delivery == nil || !application.writePending {
		t.Fatalf("manual delivery state = command %v pending %t", delivery != nil, application.writePending)
	}
	application.focus = FocusTarget{Kind: FocusAgent, AgentID: "tmux-agent"}
	application, command := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil {
		t.Fatal("manual decision in flight returned an attach command")
	}
	attachCalls, _ := backend.attachSnapshot()
	if len(attachCalls) != 0 {
		t.Fatalf("AttachCommand calls = %#v, want none", attachCalls)
	}
	if !strings.Contains(strings.Join(application.logs, "\n"), "réponse manuelle en cours") {
		t.Fatal("manual in-flight attach refusal was not logged")
	}
}

func TestTmuxAttachRequiresEventSnapshots(t *testing.T) {
	backend := &fakeAttachWithoutSnapshot{fakeBackend: newFakeBackend()}
	t.Cleanup(backend.cancel)
	application, err := NewModel(
		backend,
		make(chan session.Event, 1),
		[]Pane{{ID: "tmux-agent", Name: "Tmux Agent", Backend: "tmux"}},
		100,
		30,
		nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	application, command := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if command != nil {
		t.Fatal("backend without snapshots returned an attach command")
	}
	backend.mu.Lock()
	attachCalls := append([]string(nil), backend.attachCalls...)
	backend.mu.Unlock()
	if len(attachCalls) != 0 {
		t.Fatalf("AttachCommand calls = %#v, want none", attachCalls)
	}
	if !strings.Contains(strings.Join(application.logs, "\n"), "snapshot d'événement indisponible") {
		t.Fatal("missing snapshot capability was not reported")
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
	if !application.panes[0].policyFrozen || application.attachPending != "" {
		t.Fatalf("failed resync state = frozen %t pending %q", application.panes[0].policyFrozen, application.attachPending)
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
	if application.attachPending != "tmux-agent" {
		t.Fatalf("attach phase ended before resync: %q", application.attachPending)
	}
	stale := testAdapterEvent("tmux-agent", "password", "queued before attach", true)
	backend.mu.Lock()
	copy := stale.Event.Clone()
	backend.pendingEvent = &copy
	backend.mu.Unlock()
	application, _ = updateModel(t, application, stale)
	if application.panes[0].blocked || application.inputTarget != "" {
		t.Fatal("pre-resync stale prompt was allowed to reach the supervisor")
	}

	// Resync observes that the prompt was answered directly in tmux and makes
	// its empty cached state authoritative over the queued event.
	application, _ = updateModel(t, application, executeCommand(t, resync))
	if application.panes[0].blocked || application.inputTarget != "" || application.input.Value() != "" || application.attachPending != "" {
		t.Fatalf("stale prompt survived resync: blocked=%t target=%q value=%q",
			application.panes[0].blocked, application.inputTarget, application.input.Value())
	}

	// The same event arriving after the resync must also be discarded.
	application, _ = updateModel(t, application, stale)
	if application.panes[0].blocked || application.inputTarget != "" {
		t.Fatal("late stale prompt reblocked the pane")
	}
}

func TestQueuedEventDuringAttachUsesOnlyResynchronizedPendingOccurrence(t *testing.T) {
	events := make(chan session.Event, 2)
	backend := newFakeAttachBackend(events)
	t.Cleanup(backend.cancel)
	engine, err := policy.New(policy.Config{
		DefaultAction: policy.ActionAsk,
		Rules: []policy.Rule{{
			Name: "allow-low-risk-confirmation",
			Match: policy.Match{
				EventTypes: []adapters.EventType{adapters.EventConfirmation},
			},
			Action: policy.ActionAllow,
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	application, err := NewModelWithPolicy(
		backend,
		events,
		[]Pane{{ID: "tmux-agent", Name: "Tmux Agent", Backend: "tmux", Adapter: adapters.GenericID}},
		100,
		30,
		nil,
		engine,
	)
	if err != nil {
		t.Fatal(err)
	}
	application.execProcess = func(_ *exec.Cmd, callback tea.ExecCallback) tea.Cmd {
		return func() tea.Msg { return callback(nil) }
	}

	application, attach := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	application, resync := updateModel(t, application, executeCommand(t, attach))
	stale := testAdapterEvent("tmux-agent", "confirmation", "stale prompt", false)
	stale.Event.ID = "event-stale"
	stale.Event.Risk = adapters.RiskLow
	backend.mu.Lock()
	copy := stale.Event.Clone()
	backend.pendingEvent = &copy
	backend.mu.Unlock()
	application, command := updateModel(t, application, stale)
	if command != nil || len(backend.automaticSnapshot()) != 0 {
		t.Fatal("queued stale event triggered automation during attach/resync")
	}

	fresh := testAdapterEvent("tmux-agent", "confirmation", "fresh prompt", false)
	fresh.Event.ID = "event-fresh"
	fresh.Event.Risk = adapters.RiskLow
	backend.resyncEvent = fresh
	application, automatic := updateModel(t, application, executeCommand(t, resync))
	if automatic == nil || application.attachPending != "" {
		t.Fatalf("resynchronized event state = command %v pending %q", automatic != nil, application.attachPending)
	}
	_, _ = updateModel(t, application, executeCommand(t, automatic))
	if got := backend.automaticSnapshot(); len(got) != 1 || got[0] != "event-fresh" {
		t.Fatalf("automatic decisions = %#v, want only event-fresh", got)
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
	detected := testAdapterEvent("pty-agent", "confirmation", "PTY prompt", false)
	pending := detected.Event.Clone()
	backend.pendingEvent = &pending
	application, _ = updateModel(t, application, detected)
	application.input.SetValue("Y")
	// Deliberately move focus onto the tmux agent. The waiting response must
	// still win over attachment when Enter is pressed.
	application.focus = FocusTarget{Kind: FocusAgent, AgentID: "tmux-agent"}
	_, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
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
