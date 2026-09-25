package tui

import (
	"context"
	"errors"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/charmbracelet/bubbles/cursor"
	tea "github.com/charmbracelet/bubbletea"
)

// This file drives the supervision scenarios on the Model as it supervises
// today: its own policy evaluation, consecutive-decision tracker and journal,
// over a backend that answers as the application's router does. It goes once
// the TUI supervises through the shared core, whose driver runs the same
// table, so that every change of the journal shows as a changed row.

// errScenarioTransport is a write that failed after its bytes may have
// reached the agent, the failure whose outcome nobody can know.
var errScenarioTransport = errors.New("scenario transport failure")

// scenarioBackend has every capability the Model looks for, and answers as the
// router does: a decision is written only to the prompt the agent still
// holds, encoded by that prompt's own adapter, and a line only while the
// agent holds none. It holds one prompt per session at a time, as an agent's
// output processor does.
type scenarioBackend struct {
	*fakeBackend

	registry *adapters.Registry

	stateMu        sync.Mutex
	pending        map[string]*adapters.Event
	writes         []write
	rawWrites      []string
	lifecycle      []string
	failNextWrite  bool
	staleNextWrite bool
}

func newScenarioBackend(t *testing.T) *scenarioBackend {
	t.Helper()
	registry, err := adapters.NewRegistry(adapters.DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	backend := &scenarioBackend{
		fakeBackend: newFakeBackend(),
		registry:    registry,
		pending:     make(map[string]*adapters.Event),
	}
	t.Cleanup(backend.cancel)
	return backend
}

func (b *scenarioBackend) Name() string { return "pty" }

// holdPrompt is the processor taking a prompt in. It never replaces one it
// still holds, which is why a scenario that raises a second prompt over a
// first one is a mistake in the scenario.
func (b *scenarioBackend) holdPrompt(t *testing.T, event adapters.Event) {
	t.Helper()
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	key := strings.ToLower(event.SessionID)
	if held := b.pending[key]; held != nil {
		t.Fatalf("%s raised %s while its processor still held %s", event.SessionID, event.ID, held.ID)
	}
	copy := event.Clone()
	b.pending[key] = &copy
}

// dropPrompt is the processor letting go of the prompt it holds, if it is
// the one named: the agent withdrew it, or its process ended.
func (b *scenarioBackend) dropPrompt(sessionID, eventID string) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	key := strings.ToLower(sessionID)
	if held := b.pending[key]; held != nil && (eventID == "" || held.ID == eventID) {
		delete(b.pending, key)
	}
}

func (b *scenarioBackend) failNext() {
	b.stateMu.Lock()
	b.failNextWrite = true
	b.stateMu.Unlock()
}

func (b *scenarioBackend) staleNext() {
	b.stateMu.Lock()
	b.staleNextWrite = true
	b.stateMu.Unlock()
}

func (b *scenarioBackend) PendingEvent(_ context.Context, sessionID string) (*adapters.Event, error) {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	held := b.pending[strings.ToLower(sessionID)]
	if held == nil {
		return nil, nil
	}
	copy := held.Clone()
	return &copy, nil
}

// SendInput is the raw write the Model must never use for supervision: it
// is recorded apart so that a scenario sees it.
func (b *scenarioBackend) SendInput(id string, value string) error {
	b.stateMu.Lock()
	b.rawWrites = append(b.rawWrites, id)
	b.stateMu.Unlock()
	return b.fakeBackend.SendInput(id, value)
}

func (b *scenarioBackend) SendDecision(id string, event adapters.Event, value string) error {
	return b.applyDecision(id, event, adapters.DecisionManual, value)
}

func (b *scenarioBackend) SendAutomaticDecision(id string, event adapters.Event, decision adapters.Decision) error {
	return b.applyDecision(id, event, decision, "")
}

// applyDecision is the router's ApplyDecision: the prompt the agent holds is
// compared with the one answered, the answer is encoded by its adapter, and
// only then written. A write that fails in transport keeps the prompt, as an
// output processor acknowledges one only once its write has succeeded
// (internal/adapters/processor.go:627-631).
func (b *scenarioBackend) applyDecision(id string, event adapters.Event, decision adapters.Decision, value string) error {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	key := strings.ToLower(id)
	if b.staleNextWrite {
		b.staleNextWrite = false
		delete(b.pending, key)
	}
	held := b.pending[key]
	if held == nil || held.ID != event.ID {
		return adapters.ErrEventMismatch
	}
	adapter, _, err := b.registry.Resolve(held.Adapter, "")
	if err != nil {
		return err
	}
	if _, err := adapter.EncodeDecision(held.Clone(), decision, value); err != nil {
		return err
	}
	b.writes = append(b.writes, write{Session: id, EventID: held.ID, Decision: decision, Value: value})
	if b.failNextWrite {
		b.failNextWrite = false
		return errScenarioTransport
	}
	delete(b.pending, key)
	return nil
}

func (b *scenarioBackend) SendLine(id, value string) error {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	if b.pending[strings.ToLower(id)] != nil {
		return terminal.ErrEventPending
	}
	b.writes = append(b.writes, write{Session: id, Line: true, Value: value})
	if b.failNextWrite {
		b.failNextWrite = false
		return errScenarioTransport
	}
	return nil
}

func (b *scenarioBackend) ResizeContext(_ context.Context, id string, columns, rows int) error {
	return b.Resize(id, columns, rows)
}

func (b *scenarioBackend) StopSession(id string) error {
	b.recordLifecycle("stop " + id)
	return nil
}

func (b *scenarioBackend) StartSession(id string) error {
	b.recordLifecycle("start " + id)
	b.dropPrompt(id, "")
	return nil
}

func (b *scenarioBackend) RestartSession(id string) error {
	b.recordLifecycle("restart " + id)
	b.dropPrompt(id, "")
	return nil
}

func (b *scenarioBackend) MarkSessionExited(id string) {
	b.recordLifecycle("exited " + id)
}

func (b *scenarioBackend) AttachCommand(ctx context.Context, id string) (*exec.Cmd, error) {
	b.recordLifecycle("attach " + id)
	return exec.CommandContext(ctx, "tmux", "attach-session", "-t", id), nil
}

func (b *scenarioBackend) Resync(context.Context, string, int, int) error { return nil }

func (b *scenarioBackend) recordLifecycle(call string) {
	b.stateMu.Lock()
	b.lifecycle = append(b.lifecycle, call)
	b.stateMu.Unlock()
}

func (b *scenarioBackend) writeSnapshot() []write {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return append([]write(nil), b.writes...)
}

func (b *scenarioBackend) rawWriteSnapshot() []string {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return append([]string(nil), b.rawWrites...)
}

func (b *scenarioBackend) lifecycleSnapshot() []string {
	b.stateMu.Lock()
	defer b.stateMu.Unlock()
	return append([]string(nil), b.lifecycle...)
}

var (
	_ Backend                  = (*scenarioBackend)(nil)
	_ DecisionBackend          = (*scenarioBackend)(nil)
	_ AutomaticDecisionBackend = (*scenarioBackend)(nil)
	_ LineInputBackend         = (*scenarioBackend)(nil)
	_ EventSnapshotBackend     = (*scenarioBackend)(nil)
	_ ContextResizeBackend     = (*scenarioBackend)(nil)
	_ AttachableBackend        = (*scenarioBackend)(nil)
	_ SessionLifecycleBackend  = (*scenarioBackend)(nil)
	_ SessionExitObserver      = (*scenarioBackend)(nil)
)

// legacyDriver feeds a scenario's steps to the Model the way Bubble Tea
// would: raw session messages and keys go to Update, and every command Update
// returns is run, with the message it produces fed back, until none is left.
// The journal is a real one, in a directory the recorder creates itself.
type legacyDriver struct {
	t        *testing.T
	model    *Model
	backend  *scenarioBackend
	recorder *audit.Recorder
	path     string
}

func newLegacyDriver(t *testing.T, panes []Pane, configuration policy.Config) *legacyDriver {
	t.Helper()
	engine, err := policy.New(configuration)
	if err != nil {
		t.Fatal(err)
	}
	backend := newScenarioBackend(t)
	auditConfig := audit.DefaultConfig()
	auditConfig.Path = filepath.Join(t.TempDir(), "journal", "audit.jsonl")
	recorder, err := audit.Open(auditConfig)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recorder.Close() })
	model, err := NewModelWithPolicyAndAudit(
		backend,
		make(chan session.Event, 1),
		panes,
		120,
		36,
		nil,
		engine,
		recorder,
	)
	if err != nil {
		t.Fatal(err)
	}
	// A blinking cursor answers its focus with a timer command, which would
	// make every step wait for a tick that means nothing here.
	model.input.Cursor.SetMode(cursor.CursorStatic)
	return &legacyDriver{t: t, model: model, backend: backend, recorder: recorder, path: auditConfig.Path}
}

// update hands one message to the Model and runs what it asks for.
func (d *legacyDriver) update(message tea.Msg) {
	d.t.Helper()
	updated, command := d.model.Update(message)
	model, ok := updated.(*Model)
	if !ok {
		d.t.Fatalf("Update returned model type %T", updated)
	}
	d.model = model
	d.run(command)
}

// run executes a command as Bubble Tea's loop would, a batch as each of its
// commands, and feeds back the message it returns.
func (d *legacyDriver) run(command tea.Cmd) {
	d.t.Helper()
	if command == nil {
		return
	}
	switch message := command().(type) {
	case nil:
	case tea.BatchMsg:
		for _, inner := range message {
			d.run(inner)
		}
	case tea.QuitMsg:
		d.t.Fatal("a supervision step quit the program")
	default:
		d.update(message)
	}
}

func (d *legacyDriver) key(message tea.KeyMsg) { d.update(message) }

func (d *legacyDriver) runes(text string) {
	d.key(tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(text)})
}

// focusAgent moves the keyboard to the agent's pane with the focus keys, as
// an operator must before a pane key does anything.
func (d *legacyDriver) focusAgent(sessionID string) {
	d.t.Helper()
	for range len(d.model.panes) + 1 {
		if d.model.focus.Kind == FocusAgent && d.model.focus.AgentID == sessionID {
			return
		}
		d.key(tea.KeyMsg{Type: tea.KeyCtrlRight})
	}
	d.t.Fatalf("the focus keys never reached %s", sessionID)
}

// focusSupervisor moves the keyboard to the supervisor, where an answer is
// typed.
func (d *legacyDriver) focusSupervisor() {
	d.t.Helper()
	for range len(d.model.panes) + 1 {
		if d.model.focus.Kind == FocusSupervisor {
			return
		}
		d.key(tea.KeyMsg{Type: tea.KeyCtrlRight})
	}
	d.t.Fatal("the focus keys never reached the supervisor")
}

func (d *legacyDriver) do(current step) {
	d.t.Helper()
	switch current.kind {
	case stepRaise:
		d.backend.holdPrompt(d.t, current.event)
		d.update(session.AdapterEvent{Event: current.event.Clone()})
	case stepWithdraw:
		d.backend.dropPrompt(current.event.SessionID, current.event.ID)
		d.update(session.AdapterEventWithdrawn{Event: current.event.Clone()})
	case stepAnswer:
		d.focusSupervisor()
		d.runes(current.text)
		d.key(tea.KeyMsg{Type: tea.KeyEnter})
	case stepAllow:
		d.key(tea.KeyMsg{Type: tea.KeyF2})
	case stepDeny:
		d.key(tea.KeyMsg{Type: tea.KeyF3})
	case stepLine:
		d.focusAgent(current.sessionID)
		d.runes("i")
		d.runes(current.text)
		d.key(tea.KeyMsg{Type: tea.KeyEnter})
	case stepStop:
		d.confirmLifecycle(current.sessionID, "x", "stop "+current.sessionID)
	case stepRestart:
		d.confirmLifecycle(current.sessionID, "r", "restart "+current.sessionID)
	case stepExit:
		// The Model cannot tell a replaced process's exit from its current
		// one's: both reach it the same way, which is what a late exit pins.
		if !current.stale {
			d.backend.dropPrompt(current.event.SessionID, "")
		}
		d.update(session.AdapterEvent{Event: current.event.Clone()})
	case stepFailNextWrite:
		d.backend.failNext()
	case stepStaleNextWrite:
		d.backend.staleNext()
	default:
		d.t.Fatalf("unknown step kind %d", current.kind)
	}
}

// confirmLifecycle presses key twice on the agent's pane and fails unless
// exactly call reached the backend. A scenario's journal and screen do not
// show it: they come from the exit that follows a stop, or the late exit
// after a restart, so a lifecycle key that did nothing, or the wrong thing,
// would pass unseen.
func (d *legacyDriver) confirmLifecycle(sessionID, key, call string) {
	d.t.Helper()
	before := len(d.backend.lifecycleSnapshot())
	d.focusAgent(sessionID)
	d.runes(key)
	d.runes(key)
	if calls := d.backend.lifecycleSnapshot()[before:]; !reflect.DeepEqual(calls, []string{call}) {
		d.t.Fatalf("%s %s on %s reached the backend as %v, want exactly %q", key, key, sessionID, calls, call)
	}
}

// journal is what the recorder synchronized to disk, read back once it is
// closed.
func (d *legacyDriver) journal() []row {
	d.t.Helper()
	if err := d.recorder.Close(); err != nil {
		d.t.Fatalf("close the journal: %v", err)
	}
	entries, err := audit.ReadEntriesFromFile(d.path, audit.Filter{})
	if err != nil {
		d.t.Fatalf("read the journal: %v", err)
	}
	rows := make([]row, 0, len(entries))
	for _, entry := range entries {
		rows = append(rows, rowOf(entry))
	}
	return rows
}

func (d *legacyDriver) writes() []write {
	if raw := d.backend.rawWriteSnapshot(); len(raw) != 0 {
		d.t.Fatalf("supervision wrote raw input to %v", raw)
	}
	return d.backend.writeSnapshot()
}

// shown is what an operator sees of the agent: the prompts that wait on a
// person, in the order they are answered, the one the input field answers,
// and the agent's pane.
func (d *legacyDriver) shown(sessionID string) shown {
	d.t.Helper()
	result := shown{}
	for _, waiting := range d.model.pending {
		if index := d.model.paneIndex(waiting); index >= 0 {
			result.Awaiting = append(result.Awaiting, d.model.panes[index].prompt.ID)
		}
	}
	if index := d.model.paneIndex(d.model.inputTarget); index >= 0 {
		result.Target = d.model.panes[index].prompt.ID
	}
	result.Status, result.Tag = paneTitle(d.t, d.model.View(), d.model.panes[d.model.paneIndex(sessionID)].name)
	return result
}

// paneTitle reads an agent's status and policy tag from its rendered title,
// whose parts are separated by two spaces.
func paneTitle(t *testing.T, view, name string) (status, tag string) {
	t.Helper()
	parts := strings.Split(paneTitleText(t, view, name), "  ")
	status = parts[0]
	for _, part := range parts[1:] {
		if value, found := strings.CutPrefix(strings.TrimSpace(part), "POLICY "); found {
			tag = value
		}
	}
	return status, tag
}

// paneTitleText is an agent's rendered title from its status on, up to its
// pane's border, so that the title of a pane beside it is not read with it.
// That border is rounded, but double on a blocked pane, one whose prompt
// waits on a person or whose delivery is uncertain, and on every pane once
// the journal has failed (agentPanelStyle), so either side rune ends it.
func paneTitleText(t *testing.T, view, name string) string {
	t.Helper()
	marker := name + "  ● "
	for _, line := range strings.Split(view, "\n") {
		start := strings.Index(line, marker)
		if start < 0 {
			continue
		}
		title := line[start+len(marker):]
		if end := strings.IndexAny(title, "│║"); end >= 0 {
			title = title[:end]
		}
		return strings.TrimRight(title, " ")
	}
	t.Fatalf("no title for %s in the view:\n%s", name, view)
	return ""
}
