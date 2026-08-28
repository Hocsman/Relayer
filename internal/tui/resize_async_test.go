package tui

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
)

type asyncResizeBackend struct {
	*fakeBackend

	resizeMu   sync.Mutex
	calls      []resizeCall
	errors     map[string]error
	active     int
	maxActive  int
	blockFirst int
	blocked    chan struct{}
	open       chan struct{}
	started    chan resizeCall
	release    sync.Once
}

func newAsyncResizeBackend(blockFirst int) *asyncResizeBackend {
	open := make(chan struct{})
	close(open)
	return &asyncResizeBackend{
		fakeBackend: newFakeBackend(),
		errors:      make(map[string]error),
		blockFirst:  blockFirst,
		blocked:     make(chan struct{}),
		open:        open,
		started:     make(chan resizeCall, maxAgentCount*4),
	}
}

func (b *asyncResizeBackend) ResizeContext(ctx context.Context, id string, columns, rows int) error {
	call := resizeCall{id: id, columns: columns, rows: rows}
	b.resizeMu.Lock()
	callIndex := len(b.calls)
	b.calls = append(b.calls, call)
	b.active++
	if b.active > b.maxActive {
		b.maxActive = b.active
	}
	err := b.errors[id]
	gate := b.open
	if callIndex < b.blockFirst {
		gate = b.blocked
	}
	b.resizeMu.Unlock()

	b.started <- call
	select {
	case <-gate:
	case <-ctx.Done():
		err = ctx.Err()
	}

	b.resizeMu.Lock()
	b.active--
	b.resizeMu.Unlock()
	return err
}

func (b *asyncResizeBackend) releaseBlocked() {
	b.release.Do(func() { close(b.blocked) })
}

func (b *asyncResizeBackend) contextResizeSnapshot() ([]resizeCall, int) {
	b.resizeMu.Lock()
	defer b.resizeMu.Unlock()
	return append([]resizeCall(nil), b.calls...), b.maxActive
}

func (b *asyncResizeBackend) setResizeError(id string, err error) {
	b.resizeMu.Lock()
	b.errors[id] = err
	b.resizeMu.Unlock()
}

func TestContextResizeWindowUpdateReturnsImmediatelyAndRunsEightCallsConcurrently(t *testing.T) {
	backend := newAsyncResizeBackend(8)
	t.Cleanup(backend.cancel)
	t.Cleanup(backend.releaseBlocked)
	application, err := NewModel(backend, nil, testPanes(8), 80, 24, nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls, _ := backend.contextResizeSnapshot(); len(calls) != 0 {
		t.Fatalf("NewModel scheduled contextual resize calls: %#v", calls)
	}
	if calls := backend.resizeSnapshot(); len(calls) != 0 {
		t.Fatalf("NewModel used synchronous fallback for contextual backend: %#v", calls)
	}

	type updateResult struct {
		model *Model
		cmd   tea.Cmd
	}
	updated := make(chan updateResult, 1)
	go func() {
		model, command := updateModel(t, application, tea.WindowSizeMsg{Width: 121, Height: 40})
		updated <- updateResult{model: model, cmd: command}
	}()

	var result updateResult
	select {
	case result = <-updated:
	case <-time.After(250 * time.Millisecond):
		backend.releaseBlocked()
		t.Fatal("WindowSizeMsg blocked Update on backend resize I/O")
	}
	if result.cmd == nil {
		t.Fatal("WindowSizeMsg did not return the asynchronous resize command")
	}
	if calls, _ := backend.contextResizeSnapshot(); len(calls) != 0 {
		t.Fatalf("backend I/O ran inside Update: %#v", calls)
	}

	finished := make(chan tea.Msg, 1)
	go func() { finished <- result.cmd() }()
	for index := 0; index < 8; index++ {
		select {
		case <-backend.started:
		case <-time.After(time.Second):
			backend.releaseBlocked()
			t.Fatalf("only %d/8 resize calls started concurrently", index)
		}
	}
	if _, maximum := backend.contextResizeSnapshot(); maximum != 8 {
		t.Fatalf("maximum concurrent resize calls = %d, want 8", maximum)
	}
	select {
	case message := <-finished:
		t.Fatalf("resize batch completed before the blocking backend was released: %#v", message)
	default:
	}

	backend.releaseBlocked()
	var message tea.Msg
	select {
	case message = <-finished:
	case <-time.After(time.Second):
		t.Fatal("concurrent resize batch did not complete after release")
	}
	completion, ok := message.(resizeFinishedMsg)
	if !ok || completion.Generation != 1 || len(completion.Failures) != 0 {
		t.Fatalf("resize completion = %#v", message)
	}
	calls, _ := backend.contextResizeSnapshot()
	if len(calls) != 8 {
		t.Fatalf("contextual resize calls = %d, want 8", len(calls))
	}
	wantByID := make(map[string]resizeCall, len(application.panes))
	for index, pane := range application.panes {
		columns, rows := AgentViewportSize(121, 40, 8, index)
		wantByID[pane.sessionID] = resizeCall{id: pane.sessionID, columns: columns, rows: rows}
	}
	for _, call := range calls {
		if want, exists := wantByID[call.id]; !exists || call != want {
			t.Fatalf("unexpected contextual resize call %#v; want map %#v", call, wantByID)
		}
		delete(wantByID, call.id)
	}
	if len(wantByID) != 0 {
		t.Fatalf("missing contextual resize calls: %#v", wantByID)
	}
}

func TestContextResizeCoalescesQueuedWindowSizesAndAppliesLatestAfterInflight(t *testing.T) {
	backend := newAsyncResizeBackend(1)
	t.Cleanup(backend.cancel)
	t.Cleanup(backend.releaseBlocked)
	application, err := NewModel(backend, nil, testPanes(1), 70, 20, nil)
	if err != nil {
		t.Fatal(err)
	}

	application, firstCommand := updateModel(t, application, tea.WindowSizeMsg{Width: 80, Height: 24})
	if firstCommand == nil {
		t.Fatal("first WindowSizeMsg returned no resize command")
	}
	firstFinished := make(chan tea.Msg, 1)
	go func() { firstFinished <- firstCommand() }()
	select {
	case <-backend.started:
	case <-time.After(time.Second):
		t.Fatal("first resize did not start")
	}

	application, middleCommand := updateModel(t, application, tea.WindowSizeMsg{Width: 100, Height: 30})
	if middleCommand != nil {
		t.Fatal("queued resize started a second batch while one was in flight")
	}
	application, latestCommand := updateModel(t, application, tea.WindowSizeMsg{Width: 121, Height: 40})
	if latestCommand != nil {
		t.Fatal("latest queued resize started a second batch while one was in flight")
	}

	backend.releaseBlocked()
	firstMessage := waitForResizeCommand(t, firstFinished)
	firstCompletion, ok := firstMessage.(resizeFinishedMsg)
	if !ok || firstCompletion.Generation != 1 {
		t.Fatalf("first completion = %#v", firstMessage)
	}
	application, latestCommand = updateModel(t, application, firstCompletion)
	if latestCommand == nil {
		t.Fatal("stale completion did not schedule the latest queued geometry")
	}
	latestMessage := latestCommand()
	latestCompletion, ok := latestMessage.(resizeFinishedMsg)
	if !ok || latestCompletion.Generation != 3 {
		t.Fatalf("latest completion = %#v", latestMessage)
	}
	application, commandAfterLatest := updateModel(t, application, latestCompletion)
	if commandAfterLatest != nil || application.resizeInFlight {
		t.Fatalf("latest completion left resize work pending: cmd=%v inFlight=%v", commandAfterLatest != nil, application.resizeInFlight)
	}

	calls, _ := backend.contextResizeSnapshot()
	if len(calls) != 2 {
		t.Fatalf("coalesced resize calls = %#v, want only first and latest", calls)
	}
	firstColumns, firstRows := AgentViewportSize(80, 24, 1, 0)
	latestColumns, latestRows := AgentViewportSize(121, 40, 1, 0)
	want := []resizeCall{
		{id: "agent-1", columns: firstColumns, rows: firstRows},
		{id: "agent-1", columns: latestColumns, rows: latestRows},
	}
	for index := range want {
		if calls[index] != want[index] {
			t.Fatalf("resize call %d = %#v, want %#v", index, calls[index], want[index])
		}
	}
}

func TestContextResizeFailuresAreLoggedAfterCommandCompletion(t *testing.T) {
	backend := newAsyncResizeBackend(0)
	t.Cleanup(backend.cancel)
	backend.releaseBlocked()
	resizeErr := errors.New("context resize failed")
	backend.setResizeError("agent-2", resizeErr)
	application, err := NewModel(backend, nil, testPanes(2), 80, 24, nil)
	if err != nil {
		t.Fatal(err)
	}

	application, command := updateModel(t, application, tea.WindowSizeMsg{Width: 121, Height: 40})
	if command == nil {
		t.Fatal("WindowSizeMsg returned no contextual resize command")
	}
	before := strings.Join(application.logs, "\n")
	if strings.Contains(before, resizeErr.Error()) {
		t.Fatal("resize failure was logged before asynchronous completion")
	}
	message := command()
	completion, ok := message.(resizeFinishedMsg)
	if !ok || len(completion.Failures) != 1 {
		t.Fatalf("resize completion = %#v", message)
	}
	application, _ = updateModel(t, application, completion)
	logs := strings.Join(application.logs, "\n")
	if !strings.Contains(logs, "Cannot resize Agent 2: "+resizeErr.Error()) {
		t.Fatalf("resize failure missing from supervisor logs: %q", logs)
	}
}

func TestResizeFallbackRemainsSynchronousForLegacyBackend(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	application, err := NewModel(backend, nil, testPanes(2), 80, 24, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend.resetResizeCalls()

	application, command := updateModel(t, application, tea.WindowSizeMsg{Width: 121, Height: 40})
	if command != nil {
		t.Fatal("legacy backend unexpectedly received an asynchronous resize command")
	}
	calls := backend.resizeSnapshot()
	if len(calls) != 2 {
		t.Fatalf("synchronous fallback resize calls = %#v", calls)
	}
	for index, call := range calls {
		columns, rows := AgentViewportSize(121, 40, 2, index)
		if call != (resizeCall{id: application.panes[index].sessionID, columns: columns, rows: rows}) {
			t.Fatalf("fallback resize call %d = %#v", index, call)
		}
	}
}

func waitForResizeCommand(t *testing.T, result <-chan tea.Msg) tea.Msg {
	t.Helper()
	select {
	case message := <-result:
		return message
	case <-time.After(time.Second):
		t.Fatal("resize command did not complete")
		return nil
	}
}

var _ ContextResizeBackend = (*asyncResizeBackend)(nil)
