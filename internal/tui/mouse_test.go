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

	application.activePanel = 1
	leftBefore := application.panes[0].viewport.YOffset
	rightBefore := application.panes[1].viewport.YOffset
	supervisorBefore := application.supervisor.YOffset
	application, _ = updateModel(t, application, mouseWheelUp(10, 5))
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
	if application.activePanel != 1 {
		t.Fatalf("wheel changed keyboard focus to %d", application.activePanel)
	}

	application, _ = updateModel(t, application, mouseWheelDown(10, 5))
	if got := application.panes[0].viewport.YOffset; got <= leftAfterUp {
		t.Fatalf("wheel down did not move toward bottom: %d -> %d", leftAfterUp, got)
	}

	leftBeforeRightWheel := application.panes[0].viewport.YOffset
	rightBefore = application.panes[1].viewport.YOffset
	application, _ = updateModel(t, application, mouseWheelUp(application.leftWidth+5, 5))
	if got := application.panes[1].viewport.YOffset; got >= rightBefore {
		t.Fatalf("wheel up over right moved %d to %d", rightBefore, got)
	}
	if got := application.panes[0].viewport.YOffset; got != leftBeforeRightWheel {
		t.Fatalf("right wheel changed left pane to %d", got)
	}

	agentOffsets := [2]int{application.panes[0].viewport.YOffset, application.panes[1].viewport.YOffset}
	supervisorBefore = application.supervisor.YOffset
	application, _ = updateModel(t, application, mouseWheelUp(50, application.topHeight))
	supervisorAfterUp := application.supervisor.YOffset
	if supervisorAfterUp >= supervisorBefore {
		t.Fatalf("wheel up over supervisor moved %d to %d", supervisorBefore, supervisorAfterUp)
	}
	for index := range application.panes {
		if got := application.panes[index].viewport.YOffset; got != agentOffsets[index] {
			t.Fatalf("supervisor wheel changed pane %d to %d", index, got)
		}
	}
	application, _ = updateModel(t, application, mouseWheelDown(50, application.topHeight+1))
	if got := application.supervisor.YOffset; got <= supervisorAfterUp {
		t.Fatalf("wheel down did not move supervisor toward bottom")
	}
}

func TestMouseScrolledAgentDoesNotSnapThenResumesAutoFollow(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	sessionID := application.panes[0].sessionID
	initial := viewportTestLines(0, 80)
	application = publishOutput(t, application, backend, sessionID, initial)
	application, _ = updateModel(t, application, mouseWheelUp(10, 5))
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
		application, _ = updateModel(t, application, mouseWheelDown(10, 5))
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
	before := application
	application, _ = updateModel(t, application, mouseWheelUp(-1, 5))
	if application.panes[0].viewport.YOffset != before.panes[0].viewport.YOffset {
		t.Fatal("out-of-bounds mouse changed viewport")
	}
	// Keep the session import exercised in this test file alongside routed
	// event tests, preventing accidental replacement with PTY-layer messages.
	var _ session.Event = session.OutputAvailable{}
}
