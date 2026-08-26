package tui

import (
	"fmt"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func TestActiveAgentViewportHandlesNavigationKeys(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	content := viewportTestLines(0, 120)
	for index := range application.panes {
		application = publishOutput(t, application, backend, application.panes[index].sessionID, content)
		application.panes[index].viewport.GotoTop()
	}

	application.focus = FocusTarget{Kind: FocusAgent, AgentID: "agent-a"}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyDown})
	downOffset := application.panes[0].viewport.YOffset
	if downOffset <= 0 {
		t.Fatalf("Down did not move active viewport: %d", downOffset)
	}
	if got := application.panes[1].viewport.YOffset; got != 0 {
		t.Fatalf("Down moved inactive pane to %d", got)
	}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyUp})
	if got := application.panes[0].viewport.YOffset; got >= downOffset {
		t.Fatalf("Up did not move toward top: %d", got)
	}

	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyPgDown})
	pageDownOffset := application.panes[0].viewport.YOffset
	if pageDownOffset <= downOffset {
		t.Fatalf("PageDown offset = %d, want beyond %d", pageDownOffset, downOffset)
	}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyPgUp})
	if got := application.panes[0].viewport.YOffset; got >= pageDownOffset {
		t.Fatalf("PageUp did not move toward top: %d", got)
	}

	firstPaneOffset := application.panes[0].viewport.YOffset
	application.focus = FocusTarget{Kind: FocusAgent, AgentID: "agent-b"}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyDown})
	if got := application.panes[1].viewport.YOffset; got <= 0 {
		t.Fatalf("Down did not move pane 1: %d", got)
	}
	if got := application.panes[0].viewport.YOffset; got != firstPaneOffset {
		t.Fatalf("pane 1 navigation changed pane 0 from %d to %d", firstPaneOffset, got)
	}
}

func TestAgentViewportPreservesScrollAndResumesAutoFollow(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	sessionID := application.panes[0].sessionID
	initial := viewportTestLines(0, 80)
	application = publishOutput(t, application, backend, sessionID, initial)
	if !application.panes[0].viewport.AtBottom() {
		t.Fatal("initial output did not start at bottom")
	}

	application.focus = FocusTarget{Kind: FocusAgent, AgentID: "agent-a"}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyPgUp})
	if application.panes[0].viewport.AtBottom() {
		t.Fatal("PageUp left viewport at bottom")
	}
	beforeOffset := application.panes[0].viewport.YOffset
	beforeView := application.panes[0].viewport.View()
	withAppend := initial + "\n" + viewportTestLines(80, 10)
	application = publishOutput(t, application, backend, sessionID, withAppend)
	if got := application.panes[0].viewport.YOffset; got != beforeOffset {
		t.Fatalf("new output moved scrolled viewport from %d to %d", beforeOffset, got)
	}
	if got := application.panes[0].viewport.View(); got != beforeView {
		t.Fatalf("visible history changed while scrolled")
	}

	application.panes[0].viewport.GotoBottom()
	bottomOffset := application.panes[0].viewport.YOffset
	application = publishOutput(t, application, backend, sessionID, withAppend+"\n"+viewportTestLines(90, 10))
	if !application.panes[0].viewport.AtBottom() {
		t.Fatal("viewport did not resume auto-follow")
	}
	if got := application.panes[0].viewport.YOffset; got <= bottomOffset {
		t.Fatalf("auto-follow offset did not advance: %d -> %d", bottomOffset, got)
	}
	if got := application.panes[0].viewport.View(); !strings.Contains(got, "line 099") {
		t.Fatalf("newest line is not visible:\n%s", got)
	}
}

func TestAgentViewportPreservesScrollPositionAcrossResize(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	application = publishOutput(t, application, backend, "agent-a", viewportTestLines(0, 120))
	application.focus = FocusTarget{Kind: FocusAgent, AgentID: "agent-a"}
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyPgUp})
	beforeOffset := application.panes[0].viewport.YOffset
	if application.panes[0].viewport.AtBottom() {
		t.Fatal("test precondition failed: viewport is at bottom")
	}
	application, _ = updateModel(t, application, tea.WindowSizeMsg{Width: 121, Height: 40})
	if got := application.panes[0].viewport.YOffset; got != beforeOffset {
		t.Fatalf("resize moved viewport from %d to %d", beforeOffset, got)
	}
	if application.panes[0].viewport.AtBottom() {
		t.Fatal("resize forced viewport to bottom")
	}
}

func TestSupervisorViewportHandlesPageKeysWhileInputFocused(t *testing.T) {
	application, _, _ := newModelHarness(t)
	for index := 0; index < 80; index++ {
		application.appendLog(fmt.Sprintf("supervisor event %03d", index))
	}
	application, _ = updateModel(t, application, testAdapterEvent("agent-a", "confirmation", "manual approval", false))
	if application.focus.Kind != FocusSupervisor || application.inputTarget != "agent-a" || !application.supervisor.AtBottom() {
		t.Fatal("supervisor prompt precondition failed")
	}
	application.input.SetValue("answer-in-progress")
	application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyPgUp})
	if application.supervisor.AtBottom() {
		t.Fatal("PageUp did not scroll supervisor")
	}
	beforeOffset := application.supervisor.YOffset
	application.appendLog("new event while reading history")
	if got := application.supervisor.YOffset; got != beforeOffset {
		t.Fatalf("new log moved supervisor from %d to %d", beforeOffset, got)
	}
	if got := application.input.Value(); got != "answer-in-progress" {
		t.Fatalf("navigation changed input to %q", got)
	}
	for attempts := 0; attempts < 100 && !application.supervisor.AtBottom(); attempts++ {
		application, _ = updateModel(t, application, tea.KeyMsg{Type: tea.KeyPgDown})
	}
	if !application.supervisor.AtBottom() {
		t.Fatal("PageDown did not return supervisor to bottom")
	}
	application.appendLog("latest supervisor event")
	if !application.supervisor.AtBottom() {
		t.Fatal("supervisor did not resume auto-follow")
	}
}

func TestOutputFailureLeavesExistingViewportUntouched(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	application = publishOutput(t, application, backend, "agent-a", "existing")
	backend.mu.Lock()
	backend.outputErrors["agent-a"] = errFakeBackend
	backend.outputs["agent-a"] = "replacement"
	backend.mu.Unlock()
	application, _ = updateModel(t, application, session.OutputAvailable{SessionID: "agent-a"})
	if got := application.panes[0].viewport.View(); !strings.Contains(got, "existing") || strings.Contains(got, "replacement") {
		t.Fatalf("failed Output changed viewport: %q", got)
	}
}
