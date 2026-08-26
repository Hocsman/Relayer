package tui

import (
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/session"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

func TestKeyboardSwitchesPanelsAndCtrlCShutsDown(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	if application.focus != (FocusTarget{Kind: FocusAgent, AgentID: "agent-a"}) {
		t.Fatalf("initial focus = %#v", application.focus)
	}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlRight})
	if application.focus != (FocusTarget{Kind: FocusAgent, AgentID: "agent-b"}) {
		t.Fatalf("Ctrl+Right focus = %#v", application.focus)
	}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlRight})
	if application.focus.Kind != FocusSupervisor {
		t.Fatalf("second Ctrl+Right focus = %#v, want supervisor", application.focus)
	}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlLeft})
	if application.focus.AgentID != "agent-b" {
		t.Fatalf("Ctrl+Left focus = %#v", application.focus)
	}

	updated, command := application.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	application = updated.(*Model)
	if command == nil {
		t.Fatal("Ctrl+C returned no quit command")
	}
	backend.mu.Lock()
	shutdownCalls := backend.shutdownCalls
	backend.mu.Unlock()
	if shutdownCalls != 1 {
		t.Fatalf("BeginShutdown calls = %d, want 1", shutdownCalls)
	}
	if message := command(); message != tea.Quit() {
		t.Fatalf("Ctrl+C command returned %T, want tea.QuitMsg", message)
	}
}

func TestFocusTraversesEightAgentsAndPages(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	application, err := NewModel(backend, make(chan session.Event), testPanes(8), 120, 40, nil)
	if err != nil {
		t.Fatal(err)
	}
	for index := 1; index < 8; index++ {
		application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlRight})
		if application.focus.AgentID != application.panes[index].sessionID {
			t.Fatalf("step %d focus = %#v", index, application.focus)
		}
		if application.page != index/4 {
			t.Fatalf("step %d page = %d", index, application.page)
		}
	}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlRight})
	if application.focus.Kind != FocusSupervisor || application.page != 1 {
		t.Fatalf("supervisor focus/page = %#v/%d", application.focus, application.page)
	}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlRight})
	if application.focus.AgentID != "agent-1" || application.page != 0 {
		t.Fatalf("wrapped focus/page = %#v/%d", application.focus, application.page)
	}

	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlPgDown})
	if application.page != 1 || application.focus.AgentID != "agent-5" {
		t.Fatalf("Ctrl+PageDown = page %d focus %#v", application.page, application.focus)
	}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlPgUp})
	if application.page != 0 || application.focus.AgentID != "agent-1" {
		t.Fatalf("Ctrl+PageUp = page %d focus %#v", application.page, application.focus)
	}
}

func TestPromptQueuePreservesImmediateSecondPrompt(t *testing.T) {
	application, _, _ := newModelHarness(t)
	first := testAdapterEvent("agent-a", "confirmation", "first prompt", false)
	application, _ = updateModel(t, application, first)
	application.input.SetValue("yes")

	var delivery tea.Cmd
	application, delivery = updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if !application.writePending || application.inputTarget != "" || application.panes[0].blocked {
		t.Fatalf("old prompt was not cleared before delivery")
	}
	second := testAdapterEvent("agent-a", "confirmation", "second prompt", false)
	application, _ = updateModel(t, application, second)
	if !application.panes[0].blocked || application.inputTarget != "" {
		t.Fatal("second prompt should be queued while first write is in flight")
	}

	delivered, ok := executeCommand(t, delivery).(inputDeliveredMsg)
	if !ok {
		t.Fatalf("delivery command returned unexpected message")
	}
	application, _ = updateModel(t, application, delivered)
	if application.inputTarget != "agent-a" || !application.panes[0].blocked {
		t.Fatal("second prompt was lost when first delivery completed")
	}
	if application.panes[0].prompt.Summary != "second prompt" {
		t.Fatalf("active prompt = %#v", application.panes[0].prompt)
	}
}

func TestPromptOnHiddenPageOpensThatPageAndQueuesByStableID(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	application, err := NewModel(backend, make(chan session.Event), testPanes(8), 120, 40, nil)
	if err != nil {
		t.Fatal(err)
	}
	application, _ = updateModel(t, application, testAdapterEvent("agent-7", "confirmation", "hidden prompt", false))
	if application.page != 1 || application.focus.Kind != FocusSupervisor || application.inputTarget != "agent-7" {
		t.Fatalf("hidden prompt state = page %d focus %#v target %q", application.page, application.focus, application.inputTarget)
	}
	if len(application.pending) != 1 || application.pending[0] != "agent-7" {
		t.Fatalf("pending IDs = %#v", application.pending)
	}
	if !strings.Contains(application.View(), "Agent 7") {
		t.Fatal("prompt target page is not rendered")
	}
}

func TestPromptQueueAdvancesAcrossPagesAfterDelivery(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	application, err := NewModel(backend, make(chan session.Event), testPanes(8), 120, 40, nil)
	if err != nil {
		t.Fatal(err)
	}
	application, _ = updateModel(t, application, testAdapterEvent("agent-2", "confirmation", "first page prompt", false))
	application, _ = updateModel(t, application, testAdapterEvent("agent-7", "confirmation", "second page prompt", false))
	if application.inputTarget != "agent-2" || application.page != 0 {
		t.Fatalf("second prompt disrupted active target: target %q page %d", application.inputTarget, application.page)
	}

	application.input.SetValue("yes")
	application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	application, _ = updateModel(t, application, executeCommand(t, delivery))
	if application.inputTarget != "agent-7" || application.page != 1 || application.focus.Kind != FocusSupervisor {
		t.Fatalf("queue did not advance across pages: target %q page %d focus %#v", application.inputTarget, application.page, application.focus)
	}
	if len(application.pending) != 1 || application.pending[0] != "agent-7" {
		t.Fatalf("pending IDs after delivery = %#v", application.pending)
	}
}

func TestPasswordClearedBeforeDeliveryAndBeforeAdvancingAfterExit(t *testing.T) {
	t.Run("delivery", func(t *testing.T) {
		application, backend, _ := newModelHarness(t)
		application, _ = updateModel(t, application, testAdapterEvent("agent-a", "password", "password prompt", true))
		application.input.SetValue("top-secret")
		if strings.Contains(application.View(), "top-secret") {
			t.Fatal("sensitive input is visible in the rendered TUI")
		}
		application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
		if got := application.input.Value(); got != "" {
			t.Fatalf("password remains in model after Enter: %q", got)
		}
		if application.input.EchoMode != textinput.EchoPassword {
			t.Fatalf("unexpected interim echo mode: %v", application.input.EchoMode)
		}
		message := executeCommand(t, delivery)
		calls := backend.inputSnapshot()
		if len(calls) != 1 || calls[0] != (inputCall{id: "agent-a", value: "top-secret"}) {
			t.Fatalf("input calls = %#v", calls)
		}
		application, _ = updateModel(t, application, message)
		if application.input.EchoMode != textinput.EchoNormal {
			t.Fatalf("idle echo mode = %v, want normal", application.input.EchoMode)
		}
	})

	t.Run("target exits", func(t *testing.T) {
		application, _, _ := newModelHarness(t)
		application, _ = updateModel(t, application, testAdapterEvent("agent-a", "password", "password prompt", true))
		application.input.SetValue("top-secret")
		application, _ = updateModel(t, application, testAdapterEvent("agent-b", "confirmation", "next prompt", false))
		application, _ = updateModel(t, application, session.Exited{SessionID: "agent-a"})
		if got := application.input.Value(); got != "" {
			t.Fatalf("password survived target exit: %q", got)
		}
		if application.input.EchoMode != textinput.EchoNormal || application.inputTarget != "agent-b" {
			t.Fatalf("next prompt state = target %q, echo %v", application.inputTarget, application.input.EchoMode)
		}
	})
}

func TestDeliveryErrorRequeuesPromptWithoutRestoringSecret(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	backend.inputError = errFakeBackend
	prompt := testAdapterEvent("agent-a", "password", "credential", true)
	application, _ = updateModel(t, application, prompt)
	application.input.SetValue("secret")
	application, command := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	application, _ = updateModel(t, application, executeCommand(t, command))
	if application.inputTarget != "agent-a" || !application.panes[0].blocked {
		t.Fatalf("failed delivery was not requeued")
	}
	if got := application.input.Value(); got != "" {
		t.Fatalf("failed delivery restored secret: %q", got)
	}
	if !strings.Contains(strings.Join(application.logs, "\n"), "Échec de l'envoi") {
		t.Fatal("failed delivery was not logged")
	}
	if strings.Contains(strings.Join(application.logs, "\n"), "secret") {
		t.Fatal("sensitive input leaked into supervisor logs")
	}
}

func TestSessionEventsRefreshOutputExitAndError(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	application = publishOutput(t, application, backend, "agent-a", "hello\nworld")
	if !strings.Contains(application.panes[0].viewport.View(), "world") {
		t.Fatalf("output event did not refresh pane: %q", application.panes[0].viewport.View())
	}
	application, _ = updateModel(t, application, session.Error{SessionID: "agent-a", Err: errFakeBackend})
	if !strings.Contains(strings.Join(application.logs, "\n"), "Erreur terminal") {
		t.Fatal("session error was not logged")
	}
	application, _ = updateModel(t, application, session.Exited{SessionID: "agent-a"})
	if !application.panes[0].exited || application.panes[0].exitErr != nil {
		t.Fatalf("exit event was not recorded")
	}
}

func TestInitResubscribesToEventsAndStopsAfterBackendCancellation(t *testing.T) {
	application, backend, events := newModelHarness(t)
	backend.setOutput("agent-a", "streamed output")

	wait := application.Init()
	events <- session.OutputAvailable{SessionID: "agent-a"}
	message := executeCommand(t, wait)
	if _, ok := message.(backendEventMsg); !ok {
		t.Fatalf("Init command returned %T, want backendEventMsg", message)
	}
	application, nextWait := updateModel(t, application, message)
	if !strings.Contains(application.panes[0].viewport.View(), "streamed output") {
		t.Fatal("subscribed output event did not refresh the viewport")
	}
	if nextWait == nil {
		t.Fatal("event handling did not subscribe to the next backend event")
	}

	backend.BeginShutdown()
	stopped := executeCommand(t, nextWait)
	if _, ok := stopped.(backendStoppedMsg); !ok {
		t.Fatalf("cancelled subscription returned %T, want backendStoppedMsg", stopped)
	}
	_, quit := updateModel(t, application, stopped)
	if quit == nil {
		t.Fatal("backend stop did not request Bubble Tea shutdown")
	}
	if _, ok := executeCommand(t, quit).(tea.QuitMsg); !ok {
		t.Fatal("backend stop command is not tea.Quit")
	}
}
