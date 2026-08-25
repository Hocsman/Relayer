package tui

import (
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/session"
)

func TestMouseWheelRoutesToViewportUnderCursor(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	content := viewportTestLines(0, 120)
	for index := range application.panes {
		application = publishOutput(t, application, backend, application.panes[index].sessionID, content)
	}
	setViewportContent(&application.supervisor, content)

	application.focus = FocusTarget{Kind: FocusAgent, AgentID: "agent-b"}
	leftCell := application.layout.Cells[0]
	rightCell := application.layout.Cells[1]
	leftBefore := application.panes[0].viewport.YOffset
	rightBefore := application.panes[1].viewport.YOffset
	supervisorBefore := application.supervisor.YOffset
	application, _ = updateModel(t, application, mouseWheelUp(leftCell.Outer.X+1, leftCell.Outer.Y+1))
	leftAfterUp := application.panes[0].viewport.YOffset
	if leftAfterUp >= leftBefore {
		t.Fatalf("wheel up over left moved %d to %d", leftBefore, leftAfterUp)
	}
	if got := application.panes[1].viewport.YOffset; got != rightBefore {
		t.Fatalf("left wheel changed right pane to %d", got)
	}
	if got := application.supervisor.YOffset; got != supervisorBefore {
		t.Fatalf("left wheel changed supervisor to %d", got)
	}
	if application.focus.AgentID != "agent-b" {
		t.Fatalf("wheel changed keyboard focus to %#v", application.focus)
	}

	application, _ = updateModel(t, application, mouseWheelDown(leftCell.Outer.X+1, leftCell.Outer.Y+1))
	if got := application.panes[0].viewport.YOffset; got <= leftAfterUp {
		t.Fatalf("wheel down did not move toward bottom: %d -> %d", leftAfterUp, got)
	}

	leftBeforeRightWheel := application.panes[0].viewport.YOffset
	rightBefore = application.panes[1].viewport.YOffset
	application, _ = updateModel(t, application, mouseWheelUp(rightCell.Outer.X+1, rightCell.Outer.Y+1))
	if got := application.panes[1].viewport.YOffset; got >= rightBefore {
		t.Fatalf("wheel up over right moved %d to %d", rightBefore, got)
	}
	if got := application.panes[0].viewport.YOffset; got != leftBeforeRightWheel {
		t.Fatalf("right wheel changed left pane to %d", got)
	}

	agentOffsets := []int{application.panes[0].viewport.YOffset, application.panes[1].viewport.YOffset}
	supervisorBefore = application.supervisor.YOffset
	supervisorPoint := application.layout.Supervisor
	application, _ = updateModel(t, application, mouseWheelUp(supervisorPoint.X+1, supervisorPoint.Y+1))
	supervisorAfterUp := application.supervisor.YOffset
	if supervisorAfterUp >= supervisorBefore {
		t.Fatalf("wheel up over supervisor moved %d to %d", supervisorBefore, supervisorAfterUp)
	}
	for index := range application.panes {
		if got := application.panes[index].viewport.YOffset; got != agentOffsets[index] {
			t.Fatalf("supervisor wheel changed pane %d to %d", index, got)
		}
	}
	application, _ = updateModel(t, application, mouseWheelDown(supervisorPoint.X+1, supervisorPoint.Y+1))
	if got := application.supervisor.YOffset; got <= supervisorAfterUp {
		t.Fatal("wheel down did not move supervisor toward bottom")
	}
}

func TestMouseClickSelectsEveryGridQuadrantAndSupervisor(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	application, err := NewModel(backend, make(chan session.Event), testPanes(8), 121, 40, nil)
	if err != nil {
		t.Fatal(err)
	}
	for page := 0; page < 2; page++ {
		application.setPage(page)
		for _, cell := range application.layout.Cells {
			application, _ = updateModel(t, application, mouseLeftClick(
				cell.Outer.X+cell.Outer.Width/2,
				cell.Outer.Y+cell.Outer.Height/2,
			))
			wantID := application.panes[cell.AgentIndex].sessionID
			if application.focus != (FocusTarget{Kind: FocusAgent, AgentID: wantID}) {
				t.Fatalf("page %d cell %d click focus = %#v", page, cell.AgentIndex, application.focus)
			}
		}
	}
	supervisor := application.layout.Supervisor
	application, _ = updateModel(t, application, mouseLeftClick(supervisor.X+1, supervisor.Y+1))
	if application.focus.Kind != FocusSupervisor {
		t.Fatalf("supervisor click focus = %#v", application.focus)
	}
}

func TestMouseScrolledAgentDoesNotSnapThenResumesAutoFollow(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	sessionID := application.panes[0].sessionID
	cell := application.layout.Cells[0]
	initial := viewportTestLines(0, 80)
	application = publishOutput(t, application, backend, sessionID, initial)
	application, _ = updateModel(t, application, mouseWheelUp(cell.Outer.X+1, cell.Outer.Y+1))
	if application.panes[0].viewport.AtBottom() {
		t.Fatal("wheel up left viewport at bottom")
	}
	scrolledOffset := application.panes[0].viewport.YOffset
	scrolledView := application.panes[0].viewport.View()

	withAppend := initial + "\n" + viewportTestLines(80, 10)
	application = publishOutput(t, application, backend, sessionID, withAppend)
	if got := application.panes[0].viewport.YOffset; got != scrolledOffset {
		t.Fatalf("output snapped viewport from %d to %d", scrolledOffset, got)
	}
	if got := application.panes[0].viewport.View(); got != scrolledView {
		t.Fatal("visible history changed after output while mouse-scrolled")
	}
	for attempts := 0; attempts < 100 && !application.panes[0].viewport.AtBottom(); attempts++ {
		application, _ = updateModel(t, application, mouseWheelDown(cell.Outer.X+1, cell.Outer.Y+1))
	}
	if !application.panes[0].viewport.AtBottom() {
		t.Fatal("wheel down did not return viewport to bottom")
	}
	bottomOffset := application.panes[0].viewport.YOffset
	application = publishOutput(t, application, backend, sessionID, withAppend+"\n"+viewportTestLines(90, 10))
	if !application.panes[0].viewport.AtBottom() || application.panes[0].viewport.YOffset <= bottomOffset {
		t.Fatal("viewport did not resume auto-follow")
	}
	if got := application.panes[0].viewport.View(); !strings.Contains(got, "line 099") {
		t.Fatalf("newest line missing:\n%s", got)
	}
}

func TestMouseOutsidePanelsAndNonWheelAreIgnored(t *testing.T) {
	application, _, _ := newModelHarness(t)
	beforeOffset := application.panes[0].viewport.YOffset
	beforeFocus := application.focus
	application, _ = updateModel(t, application, mouseWheelUp(-1, 5))
	if application.panes[0].viewport.YOffset != beforeOffset || application.focus != beforeFocus {
		t.Fatal("out-of-bounds mouse changed model state")
	}
	// Keep the session import exercised in this test file alongside routed
	// event tests, preventing accidental replacement with PTY-layer messages.
	var _ session.Event = session.OutputAvailable{}
}
