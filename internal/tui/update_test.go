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
	if application.activePanel != 0 {
		t.Fatalf("initial active panel = %d", application.activePanel)
	}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlRight})
	if application.activePanel != 1 {
		t.Fatalf("Ctrl+Right active panel = %d, want 1", application.activePanel)
	}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyCtrlLeft})
	if application.activePanel != 0 {
		t.Fatalf("Ctrl+Left active panel = %d, want 0", application.activePanel)
	}

	updated, command := application.Update(tea.KeyMsg{Type: tea.KeyCtrlC})
	application = updated.(Model)
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

func TestPromptQueuePreservesImmediateSecondPrompt(t *testing.T) {
	application, _, _ := newModelHarness(t)
	first := session.PromptDetected{SessionID: 10, Pattern: "confirmation", Description: "first prompt"}
	application, _ = updateModel(t, application, first)
	application.input.SetValue("yes")

	var delivery tea.Cmd
	application, delivery = updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	if !application.writePending || application.inputTarget != -1 || application.panes[0].blocked {
		t.Fatalf("old prompt was not cleared before delivery")
	}
	second := session.PromptDetected{SessionID: 10, Pattern: "confirmation", Description: "second prompt"}
	application, _ = updateModel(t, application, second)
	if !application.panes[0].blocked || application.inputTarget != -1 {
		t.Fatal("second prompt should be queued while first write is in flight")
	}

	delivered, ok := executeCommand(t, delivery).(inputDeliveredMsg)
	if !ok {
		t.Fatalf("delivery command returned unexpected message")
	}
	application, _ = updateModel(t, application, delivered)
	if application.inputTarget != 0 || !application.panes[0].blocked {
		t.Fatal("second prompt was lost when first delivery completed")
	}
	if application.panes[0].prompt.Description != "second prompt" {
		t.Fatalf("active prompt = %#v", application.panes[0].prompt)
	}
}

func TestPasswordClearedBeforeDeliveryAndBeforeAdvancingAfterExit(t *testing.T) {
	t.Run("delivery", func(t *testing.T) {
		application, backend, _ := newModelHarness(t)
		application, _ = updateModel(t, application, session.PromptDetected{
			SessionID: 10, Pattern: "password", Description: "password prompt", Sensitive: true,
		})
		application.input.SetValue("top-secret")
		if strings.Contains(application.View(), "top-secret") {
			t.Fatal("sensitive input is visible in the rendered TUI")
		}
		application, delivery := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
		if got := application.input.Value(); got != "" {
			t.Fatalf("password remains in model after Enter: %q", got)
		}
		if application.input.EchoMode != textinput.EchoPassword {
			// Echo mode resets when the delivery completes or another prompt is
			// activated; before that the value itself is the sensitive state.
			t.Fatalf("unexpected interim echo mode: %v", application.input.EchoMode)
		}
		message := executeCommand(t, delivery)
		calls := backend.inputSnapshot()
		if len(calls) != 1 || calls[0] != (inputCall{id: 10, value: "top-secret"}) {
			t.Fatalf("input calls = %#v", calls)
		}
		application, _ = updateModel(t, application, message)
		if application.input.EchoMode != textinput.EchoNormal {
			t.Fatalf("idle echo mode = %v, want normal", application.input.EchoMode)
		}
	})

	t.Run("target exits", func(t *testing.T) {
		application, _, _ := newModelHarness(t)
		application, _ = updateModel(t, application, session.PromptDetected{
			SessionID: 10, Pattern: "password", Description: "password prompt", Sensitive: true,
		})
		application.input.SetValue("top-secret")
		application, _ = updateModel(t, application, session.PromptDetected{
			SessionID: 20, Pattern: "confirmation", Description: "next prompt",
		})
		application, _ = updateModel(t, application, session.Exited{SessionID: 10})
		if got := application.input.Value(); got != "" {
			t.Fatalf("password survived target exit: %q", got)
		}
		if application.input.EchoMode != textinput.EchoNormal || application.inputTarget != 1 {
			t.Fatalf("next prompt state = target %d, echo %v", application.inputTarget, application.input.EchoMode)
		}
	})
}

func TestDeliveryErrorRequeuesPromptWithoutRestoringSecret(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	backend.inputError = errFakeBackend
	prompt := session.PromptDetected{SessionID: 10, Pattern: "password", Description: "credential", Sensitive: true}
	application, _ = updateModel(t, application, prompt)
	application.input.SetValue("secret")
	application, command := updateModel(t, application, tea.KeyMsg{Type: tea.KeyEnter})
	application, _ = updateModel(t, application, executeCommand(t, command))
	if application.inputTarget != 0 || !application.panes[0].blocked {
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
	application = publishOutput(t, application, backend, 10, "hello\nworld")
	if !strings.Contains(application.panes[0].viewport.View(), "world") {
		t.Fatalf("output event did not refresh pane: %q", application.panes[0].viewport.View())
	}
	application, _ = updateModel(t, application, session.Error{SessionID: 10, Err: errFakeBackend})
	if !strings.Contains(strings.Join(application.logs, "\n"), "Erreur PTY") {
		t.Fatal("session error was not logged")
	}
	application, _ = updateModel(t, application, session.Exited{SessionID: 10})
	if !application.panes[0].exited || application.panes[0].exitErr != nil {
		t.Fatalf("exit event was not recorded")
	}
}

func TestInitResubscribesToEventsAndStopsAfterBackendCancellation(t *testing.T) {
	application, backend, events := newModelHarness(t)
	backend.setOutput(10, "streamed output")

	wait := application.Init()
	events <- session.OutputAvailable{SessionID: 10}
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
