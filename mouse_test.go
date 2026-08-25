package main

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestMouseWheelRoutesToViewportUnderCursor(t *testing.T) {
	application := newModelHarness(t)
	application = updateModel(t, application, tea.WindowSizeMsg{Width: 100, Height: 40})
	content := viewportTestLines(0, 120)
	for index := range application.panes {
		application = updateModel(t, application, SessionOutputMsg{
			SessionID: application.panes[index].sessionID,
			Content:   content,
		})
	}
	setViewportContent(&application.supervisor, content)

	application.activePanel = 1
	leftBefore := application.panes[0].viewport.YOffset
	rightBefore := application.panes[1].viewport.YOffset
	supervisorBefore := application.supervisor.YOffset
	application = updateModel(t, application, mouseWheelUp(10, 5))
	leftAfterUp := application.panes[0].viewport.YOffset
	if leftAfterUp >= leftBefore {
		t.Fatalf("wheel up over left agent moved offset from %d to %d", leftBefore, leftAfterUp)
	}
	if got := application.panes[1].viewport.YOffset; got != rightBefore {
		t.Fatalf("left-agent wheel changed right agent from %d to %d", rightBefore, got)
	}
	if got := application.supervisor.YOffset; got != supervisorBefore {
		t.Fatalf("left-agent wheel changed supervisor from %d to %d", supervisorBefore, got)
	}
	if application.activePanel != 1 {
		t.Fatalf("mouse wheel changed keyboard focus to panel %d", application.activePanel)
	}

	application = updateModel(t, application, mouseWheelDown(10, 5))
	if got := application.panes[0].viewport.YOffset; got <= leftAfterUp {
		t.Fatalf("wheel down over left agent did not move toward bottom: %d -> %d", leftAfterUp, got)
	}

	leftBeforeRightWheel := application.panes[0].viewport.YOffset
	rightBefore = application.panes[1].viewport.YOffset
	application = updateModel(t, application, mouseWheelUp(application.leftWidth+5, 5))
	if got := application.panes[1].viewport.YOffset; got >= rightBefore {
		t.Fatalf("wheel up over right agent moved offset from %d to %d", rightBefore, got)
	}
	if got := application.panes[0].viewport.YOffset; got != leftBeforeRightWheel {
		t.Fatalf("right-agent wheel changed left agent from %d to %d", leftBeforeRightWheel, got)
	}

	agentOffsets := [2]int{
		application.panes[0].viewport.YOffset,
		application.panes[1].viewport.YOffset,
	}
	supervisorBefore = application.supervisor.YOffset
	application = updateModel(t, application, mouseWheelUp(50, application.topHeight))
	supervisorAfterUp := application.supervisor.YOffset
	if supervisorAfterUp >= supervisorBefore {
		t.Fatalf("wheel up over bottom supervisor moved offset from %d to %d", supervisorBefore, supervisorAfterUp)
	}
	for index := range application.panes {
		if got := application.panes[index].viewport.YOffset; got != agentOffsets[index] {
			t.Fatalf("supervisor wheel changed agent %d from %d to %d", index, agentOffsets[index], got)
		}
	}

	application = updateModel(t, application, mouseWheelDown(50, application.topHeight+1))
	if got := application.supervisor.YOffset; got <= supervisorAfterUp {
		t.Fatalf("wheel down over supervisor did not move toward bottom: %d -> %d", supervisorAfterUp, got)
	}
}

func TestMouseScrolledAgentDoesNotSnapThenResumesAutoFollowAtBottom(t *testing.T) {
	application := newModelHarness(t)
	application = updateModel(t, application, tea.WindowSizeMsg{Width: 100, Height: 40})
	sessionID := application.panes[0].sessionID
	initial := viewportTestLines(0, 80)
	application = updateModel(t, application, SessionOutputMsg{
		SessionID: sessionID,
		Content:   initial,
	})
	if !application.panes[0].viewport.AtBottom() {
		t.Fatal("initial agent output did not start at the bottom")
	}

	application = updateModel(t, application, mouseWheelUp(10, 5))
	if application.panes[0].viewport.AtBottom() {
		t.Fatal("wheel up left the agent viewport at the bottom")
	}
	scrolledOffset := application.panes[0].viewport.YOffset
	scrolledView := application.panes[0].viewport.View()

	withFirstAppend := initial + "\n" + viewportTestLines(80, 10)
	application = updateModel(t, application, SessionOutputMsg{
		SessionID: sessionID,
		Content:   withFirstAppend,
	})
	if got := application.panes[0].viewport.YOffset; got != scrolledOffset {
		t.Fatalf("new output snapped mouse-scrolled viewport from %d to %d", scrolledOffset, got)
	}
	if got := application.panes[0].viewport.View(); got != scrolledView {
		t.Fatalf("visible history changed after output while mouse-scrolled:\n--- before ---\n%s\n--- after ---\n%s", scrolledView, got)
	}
	if application.panes[0].viewport.AtBottom() {
		t.Fatal("new output forced mouse-scrolled viewport to the bottom")
	}

	for attempts := 0; attempts < 100 && !application.panes[0].viewport.AtBottom(); attempts++ {
		application = updateModel(t, application, mouseWheelDown(10, 5))
	}
	if !application.panes[0].viewport.AtBottom() {
		t.Fatal("repeated wheel down events did not return agent viewport to bottom")
	}
	bottomOffset := application.panes[0].viewport.YOffset

	application = updateModel(t, application, SessionOutputMsg{
		SessionID: sessionID,
		Content:   withFirstAppend + "\n" + viewportTestLines(90, 10),
	})
	if !application.panes[0].viewport.AtBottom() {
		t.Fatal("agent viewport did not resume auto-follow after returning to bottom")
	}
	if got := application.panes[0].viewport.YOffset; got <= bottomOffset {
		t.Fatalf("auto-follow offset did not advance: before %d, after %d", bottomOffset, got)
	}
	if got := application.panes[0].viewport.View(); !strings.Contains(got, "line 099") {
		t.Fatalf("auto-follow view does not contain newest line:\n%s", got)
	}
}

func mouseWheelUp(x, y int) tea.MouseMsg {
	return tea.MouseMsg{
		X:      x,
		Y:      y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelUp,
		Type:   tea.MouseWheelUp,
	}
}

func mouseWheelDown(x, y int) tea.MouseMsg {
	return tea.MouseMsg{
		X:      x,
		Y:      y,
		Action: tea.MouseActionPress,
		Button: tea.MouseButtonWheelDown,
		Type:   tea.MouseWheelDown,
	}
}
