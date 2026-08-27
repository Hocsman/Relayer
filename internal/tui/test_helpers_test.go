package tui

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

type resizeCall struct {
	id      string
	columns int
	rows    int
}

type inputCall struct {
	id    string
	value string
}

type fakeBackend struct {
	ctx    context.Context
	cancel context.CancelFunc

	mu            sync.Mutex
	outputs       map[string]string
	outputErrors  map[string]error
	resizeCalls   []resizeCall
	inputCalls    []inputCall
	resizeError   error
	inputError    error
	shutdownCalls int
}

func newFakeBackend() *fakeBackend {
	ctx, cancel := context.WithCancel(context.Background())
	return &fakeBackend{
		ctx:          ctx,
		cancel:       cancel,
		outputs:      make(map[string]string),
		outputErrors: make(map[string]error),
	}
}

func (b *fakeBackend) Context() context.Context { return b.ctx }

func (b *fakeBackend) Output(id string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.outputs[id], b.outputErrors[id]
}

func (b *fakeBackend) SendInput(id string, value string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.inputCalls = append(b.inputCalls, inputCall{id: id, value: value})
	return b.inputError
}

func (b *fakeBackend) SendDecision(id string, _ adapters.Event, value string) error {
	return b.SendInput(id, value)
}

func (b *fakeBackend) Resize(id string, columns, rows int) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.resizeCalls = append(b.resizeCalls, resizeCall{id: id, columns: columns, rows: rows})
	return b.resizeError
}

func (b *fakeBackend) BeginShutdown() {
	b.mu.Lock()
	b.shutdownCalls++
	b.mu.Unlock()
	b.cancel()
}

func (b *fakeBackend) setOutput(id string, output string) {
	b.mu.Lock()
	b.outputs[id] = output
	b.mu.Unlock()
}

func (b *fakeBackend) resetResizeCalls() {
	b.mu.Lock()
	b.resizeCalls = nil
	b.mu.Unlock()
}

func (b *fakeBackend) resizeSnapshot() []resizeCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]resizeCall(nil), b.resizeCalls...)
}

func (b *fakeBackend) inputSnapshot() []inputCall {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]inputCall(nil), b.inputCalls...)
}

func newModelHarness(t *testing.T) (*Model, *fakeBackend, chan session.Event) {
	t.Helper()
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	events := make(chan session.Event, 16)
	application, err := NewModel(
		backend,
		events,
		[]Pane{
			{ID: "agent-a", Name: "Agent A", Command: "agent-a"},
			{ID: "agent-b", Name: "Agent B", Command: "agent-b"},
		},
		100,
		30,
		nil,
	)
	if err != nil {
		t.Fatalf("NewModel returned an error: %v", err)
	}
	backend.resetResizeCalls()
	return application, backend, events
}

func updateModel(t *testing.T, application *Model, message tea.Msg) (*Model, tea.Cmd) {
	t.Helper()
	updated, command := application.Update(message)
	result, ok := updated.(*Model)
	if !ok {
		t.Fatalf("Update returned model type %T", updated)
	}
	return result, command
}

func publishOutput(t *testing.T, application *Model, backend *fakeBackend, sessionID string, content string) *Model {
	t.Helper()
	backend.setOutput(sessionID, content)
	updated, _ := updateModel(t, application, session.OutputAvailable{SessionID: sessionID})
	return updated
}

func executeCommand(t *testing.T, command tea.Cmd) tea.Msg {
	t.Helper()
	if command == nil {
		t.Fatal("expected a Bubble Tea command")
	}
	return command()
}

func viewportTestLines(start, count int) string {
	lines := make([]string, count)
	for index := range lines {
		lines[index] = fmt.Sprintf("line %03d", start+index)
	}
	return strings.Join(lines, "\n")
}

func mouseWheelUp(x, y int) tea.MouseMsg {
	return tea.MouseMsg{
		X:      x,
		Y:      y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelUp,
	}
}

func mouseWheelDown(x, y int) tea.MouseMsg {
	return tea.MouseMsg{
		X:      x,
		Y:      y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelDown,
	}
}

func mouseLeftClick(x, y int) tea.MouseMsg {
	return tea.MouseMsg{
		X:      x,
		Y:      y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonLeft,
	}
}

var errFakeBackend = errors.New("fake backend error")

var testEventSequence uint64

func testAdapterEvent(sessionID, pattern, summary string, sensitive bool) session.AdapterEvent {
	sequence := atomic.AddUint64(&testEventSequence, 1)
	eventType := adapters.EventConfirmation
	risk := adapters.RiskUnknown
	if sensitive {
		eventType = adapters.EventCredential
		risk = adapters.RiskHigh
	}
	return session.AdapterEvent{Event: adapters.Event{
		ID:        fmt.Sprintf("test-event-%d", sequence),
		Signature: fmt.Sprintf("%s:%s", sessionID, pattern),
		Sequence:  sequence,
		SessionID: sessionID,
		AgentID:   sessionID,
		Adapter:   adapters.GenericID,
		Type:      eventType,
		Summary:   summary,
		Sensitive: sensitive,
		Risk:      risk,
		Metadata:  map[string]string{"pattern": pattern},
	}}
}
