package tui

import (
	"testing"

	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

func TestCalculateLayout(t *testing.T) {
	tests := []struct {
		name                     string
		width                    int
		height                   int
		leftWidth                int
		rightWidth               int
		topHeight                int
		supervisorHeight         int
		leftViewportWidth        int
		rightViewportWidth       int
		agentViewportHeight      int
		supervisorViewportWidth  int
		supervisorViewportHeight int
		inputWidth               int
	}{
		{
			name:                     "75/25 height and exact odd width split",
			width:                    121,
			height:                   40,
			leftWidth:                60,
			rightWidth:               61,
			topHeight:                30,
			supervisorHeight:         10,
			leftViewportWidth:        58,
			rightViewportWidth:       59,
			agentViewportHeight:      27,
			supervisorViewportWidth:  119,
			supervisorViewportHeight: 5,
			inputWidth:               117,
		},
		{
			name:                     "second standard geometry",
			width:                    81,
			height:                   32,
			leftWidth:                40,
			rightWidth:               41,
			topHeight:                24,
			supervisorHeight:         8,
			leftViewportWidth:        38,
			rightViewportWidth:       39,
			agentViewportHeight:      21,
			supervisorViewportWidth:  79,
			supervisorViewportHeight: 3,
			inputWidth:               77,
		},
		{
			name:                     "tiny terminal clamps every viewport",
			width:                    9,
			height:                   5,
			leftWidth:                4,
			rightWidth:               5,
			topHeight:                2,
			supervisorHeight:         3,
			leftViewportWidth:        2,
			rightViewportWidth:       3,
			agentViewportHeight:      1,
			supervisorViewportWidth:  7,
			supervisorViewportHeight: 1,
			inputWidth:               5,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := CalculateLayout(test.width, test.height)
			if got.Width != test.width || got.Height != test.height {
				t.Fatalf("terminal size = %dx%d, want %dx%d", got.Width, got.Height, test.width, test.height)
			}
			if got.LeftWidth != test.leftWidth || got.RightWidth != test.rightWidth {
				t.Fatalf("outer widths = %d/%d, want %d/%d", got.LeftWidth, got.RightWidth, test.leftWidth, test.rightWidth)
			}
			if got.LeftWidth+got.RightWidth != got.Width {
				t.Fatalf("pane split loses columns: %d + %d != %d", got.LeftWidth, got.RightWidth, got.Width)
			}
			if got.TopHeight != test.topHeight || got.SupervisorHeight != test.supervisorHeight {
				t.Fatalf("section heights = %d/%d, want %d/%d", got.TopHeight, got.SupervisorHeight, test.topHeight, test.supervisorHeight)
			}
			if got.AgentViewportWidths != [2]int{test.leftViewportWidth, test.rightViewportWidth} {
				t.Fatalf("agent viewport widths = %v", got.AgentViewportWidths)
			}
			if got.AgentViewportHeight != test.agentViewportHeight ||
				got.SupervisorViewportWidth != test.supervisorViewportWidth ||
				got.SupervisorViewportHeight != test.supervisorViewportHeight ||
				got.InputWidth != test.inputWidth {
				t.Fatalf("unexpected inner geometry: %+v", got)
			}
		})
	}
}

func TestWindowSizeMsgAppliesGeometryAndResizesBothSessions(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	application, _ = updateModel(t, application, tea.WindowSizeMsg{Width: 121, Height: 40})
	want := CalculateLayout(121, 40)

	if application.width != want.Width || application.height != want.Height ||
		application.leftWidth != want.LeftWidth || application.rightWidth != want.RightWidth ||
		application.topHeight != want.TopHeight || application.supervisorHeight != want.SupervisorHeight {
		t.Fatalf("model geometry does not match CalculateLayout: model=%+v layout=%+v", application, want)
	}
	for index := range application.panes {
		if application.panes[index].viewport.Width != want.AgentViewportWidths[index] ||
			application.panes[index].viewport.Height != want.AgentViewportHeight {
			t.Fatalf("pane %d viewport = %dx%d, want %dx%d", index, application.panes[index].viewport.Width, application.panes[index].viewport.Height, want.AgentViewportWidths[index], want.AgentViewportHeight)
		}
	}
	if application.supervisor.Width != want.SupervisorViewportWidth ||
		application.supervisor.Height != want.SupervisorViewportHeight ||
		application.input.Width != want.InputWidth {
		t.Fatalf("supervisor/input geometry does not match layout")
	}

	calls := backend.resizeSnapshot()
	if len(calls) != 2 {
		t.Fatalf("Resize calls = %d, want 2: %#v", len(calls), calls)
	}
	wantIDs := [2]int{10, 20}
	for index, call := range calls {
		if call.id != wantIDs[index] || call.columns != want.AgentViewportWidths[index] || call.rows != want.AgentViewportHeight {
			t.Fatalf("Resize call %d = %#v", index, call)
		}
	}
}

func TestNewModelAppliesInitialGeometryAndStartupLogs(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	application := NewModel(
		backend,
		make(chan session.Event),
		[2]Pane{{ID: 1, Name: "one"}, {ID: 2, Name: "two"}},
		81,
		32,
		[]string{"configuration loaded", "mock enabled"},
	)
	want := CalculateLayout(81, 32)
	if application.width != want.Width || application.panes[0].viewport.Width != want.AgentViewportWidths[0] {
		t.Fatalf("initial geometry was not applied: %+v", application)
	}
	if len(backend.resizeSnapshot()) != 2 {
		t.Fatalf("initial resize calls = %d, want 2", len(backend.resizeSnapshot()))
	}
	if len(application.logs) != 3 {
		t.Fatalf("startup logs = %d, want base log plus 2", len(application.logs))
	}
}
