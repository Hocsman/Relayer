package tui

import (
	"fmt"
	"testing"

	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

// TestCalculateLayout keeps the original two-pane geometry contract while the
// broader table below exercises every dynamic cardinality and page.
func TestCalculateLayout(t *testing.T) {
	tests := []struct {
		name                     string
		width                    int
		height                   int
		leftWidth                int
		rightWidth               int
		agentHeight              int
		supervisorHeight         int
		leftViewportWidth        int
		rightViewportWidth       int
		agentViewportHeight      int
		supervisorViewportWidth  int
		supervisorViewportHeight int
		inputWidth               int
	}{
		{"75/25 height and exact odd width split", 121, 40, 60, 61, 30, 10, 58, 59, 27, 119, 5, 117},
		{"second standard geometry", 81, 32, 40, 41, 24, 8, 38, 39, 21, 79, 3, 77},
		{"tiny terminal clamps every viewport", 9, 5, 4, 5, 2, 3, 2, 3, 1, 7, 1, 5},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := CalculateLayout(test.width, test.height, 2, 0)
			if got.Width != test.width || got.Height != test.height || len(got.Cells) != 2 {
				t.Fatalf("terminal/cells = %+v", got)
			}
			if got.Cells[0].Outer.Width != test.leftWidth || got.Cells[1].Outer.Width != test.rightWidth ||
				got.Cells[0].Outer.Width+got.Cells[1].Outer.Width != got.Width {
				t.Fatalf("outer widths = %+v", got.Cells)
			}
			if got.AgentArea.Height != test.agentHeight || got.Supervisor.Height != test.supervisorHeight {
				t.Fatalf("section heights = %d/%d", got.AgentArea.Height, got.Supervisor.Height)
			}
			if got.Cells[0].ViewportWidth != test.leftViewportWidth ||
				got.Cells[1].ViewportWidth != test.rightViewportWidth ||
				got.Cells[0].ViewportHeight != test.agentViewportHeight ||
				got.Cells[1].ViewportHeight != test.agentViewportHeight ||
				got.SupervisorViewportWidth != test.supervisorViewportWidth ||
				got.SupervisorViewportHeight != test.supervisorViewportHeight ||
				got.InputWidth != test.inputWidth {
				t.Fatalf("unexpected inner geometry: %+v", got)
			}
		})
	}
}

func TestCalculateLayoutForOneThroughEightAgents(t *testing.T) {
	for agentCount := 1; agentCount <= 8; agentCount++ {
		t.Run(fmt.Sprintf("%d agents", agentCount), func(t *testing.T) {
			pages := pageCount(agentCount)
			for page := 0; page < pages; page++ {
				got := CalculateLayout(121, 40, agentCount, page)
				if got.Width != 121 || got.Height != 40 {
					t.Fatalf("terminal size = %dx%d", got.Width, got.Height)
				}
				if got.AgentArea.Height != 30 || got.Supervisor.Height != 10 || got.AgentArea.Height+got.Supervisor.Height != got.Height {
					t.Fatalf("section split = %+v", got)
				}
				if got.Page != page || got.PageCount != pages {
					t.Fatalf("page = %d/%d, want %d/%d", got.Page, got.PageCount, page, pages)
				}
				wantVisible := minInt(4, agentCount-page*4)
				if len(got.Cells) != wantVisible {
					t.Fatalf("visible cells = %d, want %d", len(got.Cells), wantVisible)
				}
				area := 0
				for cellOffset, cell := range got.Cells {
					wantIndex := page*4 + cellOffset
					if cell.AgentIndex != wantIndex {
						t.Fatalf("cell agent = %d, want %d", cell.AgentIndex, wantIndex)
					}
					if cell.Outer.Width < 1 || cell.Outer.Height < 1 || cell.ViewportWidth < 1 || cell.ViewportHeight < 1 {
						t.Fatalf("non-positive cell: %+v", cell)
					}
					area += cell.Outer.Width * cell.Outer.Height
					columns, rows := AgentViewportSize(121, 40, agentCount, cell.AgentIndex)
					if columns != cell.ViewportWidth || rows != cell.ViewportHeight {
						t.Fatalf("agent %d size = %dx%d, cell = %dx%d", cell.AgentIndex, columns, rows, cell.ViewportWidth, cell.ViewportHeight)
					}
				}
				if area != got.AgentArea.Width*got.AgentArea.Height {
					t.Fatalf("cells cover %d cells, agent area has %d", area, got.AgentArea.Width*got.AgentArea.Height)
				}
			}
		})
	}
}

func TestCalculateLayoutShapes(t *testing.T) {
	tests := []struct {
		count int
		want  []Rect
	}{
		{count: 1, want: []Rect{{Width: 121, Height: 30}}},
		{count: 2, want: []Rect{{Width: 60, Height: 30}, {X: 60, Width: 61, Height: 30}}},
		{count: 3, want: []Rect{{Width: 60, Height: 15}, {X: 60, Width: 61, Height: 15}, {Y: 15, Width: 121, Height: 15}}},
		{count: 4, want: []Rect{{Width: 60, Height: 15}, {X: 60, Width: 61, Height: 15}, {Y: 15, Width: 60, Height: 15}, {X: 60, Y: 15, Width: 61, Height: 15}}},
	}
	for _, test := range tests {
		got := CalculateLayout(121, 40, test.count, 0)
		for index, cell := range got.Cells {
			if cell.Outer != test.want[index] {
				t.Fatalf("count %d cell %d = %+v, want %+v", test.count, index, cell.Outer, test.want[index])
			}
		}
	}
}

func TestTinyLayoutNeverProducesNegativeDimensions(t *testing.T) {
	for width := -2; width <= 12; width++ {
		for height := -2; height <= 8; height++ {
			for count := 1; count <= 8; count++ {
				layout := CalculateLayout(width, height, count, 0)
				if layout.AgentArea.Width < 0 || layout.AgentArea.Height < 0 ||
					layout.Supervisor.Width < 0 || layout.Supervisor.Height < 0 {
					t.Fatalf("negative section at %dx%d: %+v", width, height, layout)
				}
				for _, cell := range layout.Cells {
					if cell.Outer.Width < 0 || cell.Outer.Height < 0 {
						t.Fatalf("negative cell at %dx%d: %+v", width, height, cell)
					}
				}
				for index := 0; index < count; index++ {
					columns, rows := AgentViewportSize(width, height, count, index)
					if columns < 1 || rows < 1 {
						t.Fatalf("%dx%d count %d index %d -> %dx%d", width, height, count, index, columns, rows)
					}
				}
			}
		}
	}
}

func TestTinyWindowResizesAllHiddenAndVisibleSessionsSafely(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	application, err := NewModel(backend, make(chan session.Event), testPanes(8), 120, 40, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend.resetResizeCalls()
	_, _ = updateModel(t, application, tea.WindowSizeMsg{Width: 1, Height: 1})
	calls := backend.resizeSnapshot()
	if len(calls) != 8 {
		t.Fatalf("tiny resize calls = %d, want 8", len(calls))
	}
	for _, call := range calls {
		if call.columns < 1 || call.rows < 1 {
			t.Fatalf("unsafe PTY resize: %#v", call)
		}
	}
}

func TestWindowSizeMsgAppliesGeometryAndResizesEverySession(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	panes := testPanes(8)
	application, err := NewModel(backend, make(chan session.Event), panes, 80, 24, nil)
	if err != nil {
		t.Fatal(err)
	}
	backend.resetResizeCalls()
	application, _ = updateModel(t, application, tea.WindowSizeMsg{Width: 121, Height: 40})

	if application.width != 121 || application.height != 40 || application.layout.PageCount != 2 {
		t.Fatalf("model geometry = %+v", application.layout)
	}
	for index := range application.panes {
		columns, rows := AgentViewportSize(121, 40, 8, index)
		if application.panes[index].viewport.Width != columns || application.panes[index].viewport.Height != rows {
			t.Fatalf("pane %d viewport = %dx%d, want %dx%d", index, application.panes[index].viewport.Width, application.panes[index].viewport.Height, columns, rows)
		}
	}
	calls := backend.resizeSnapshot()
	if len(calls) != 8 {
		t.Fatalf("Resize calls = %d, want 8: %#v", len(calls), calls)
	}
	for index, call := range calls {
		columns, rows := AgentViewportSize(121, 40, 8, index)
		if call.id != panes[index].ID || call.columns != columns || call.rows != rows {
			t.Fatalf("Resize call %d = %#v, want %s %dx%d", index, call, panes[index].ID, columns, rows)
		}
	}
}

// TestWindowSizeMsgAppliesGeometryAndResizesBothSessions preserves the
// original two-session regression test alongside the eight-session variant.
func TestWindowSizeMsgAppliesGeometryAndResizesBothSessions(t *testing.T) {
	application, backend, _ := newModelHarness(t)
	application, _ = updateModel(t, application, tea.WindowSizeMsg{Width: 121, Height: 40})
	want := CalculateLayout(121, 40, 2, 0)

	if application.width != want.Width || application.height != want.Height || application.layout.Page != 0 {
		t.Fatalf("model geometry does not match layout: model=%+v layout=%+v", application.layout, want)
	}
	for index, cell := range want.Cells {
		if application.panes[index].viewport.Width != cell.ViewportWidth ||
			application.panes[index].viewport.Height != cell.ViewportHeight {
			t.Fatalf("pane %d viewport = %dx%d, want %dx%d", index, application.panes[index].viewport.Width, application.panes[index].viewport.Height, cell.ViewportWidth, cell.ViewportHeight)
		}
	}
	if application.supervisor.Width != want.SupervisorViewportWidth ||
		application.supervisor.Height != want.SupervisorViewportHeight ||
		application.input.Width != want.InputWidth {
		t.Fatal("supervisor/input geometry does not match layout")
	}

	calls := backend.resizeSnapshot()
	if len(calls) != 2 {
		t.Fatalf("Resize calls = %d, want 2: %#v", len(calls), calls)
	}
	for index, call := range calls {
		cell := want.Cells[index]
		if call.id != application.panes[index].sessionID || call.columns != cell.ViewportWidth || call.rows != cell.ViewportHeight {
			t.Fatalf("Resize call %d = %#v", index, call)
		}
	}
}

func TestNewModelAppliesInitialGeometryAndStartupLogs(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	application, err := NewModel(
		backend,
		make(chan session.Event),
		[]Pane{{ID: "one", Name: "one"}, {ID: "two", Name: "two"}},
		81,
		32,
		[]string{"configuration loaded", "mock enabled"},
	)
	if err != nil {
		t.Fatal(err)
	}
	columns, _ := AgentViewportSize(81, 32, 2, 0)
	if application.width != 81 || application.panes[0].viewport.Width != columns {
		t.Fatalf("initial geometry was not applied: %+v", application)
	}
	if len(backend.resizeSnapshot()) != 2 {
		t.Fatalf("initial resize calls = %d, want 2", len(backend.resizeSnapshot()))
	}
	if len(application.logs) != 3 {
		t.Fatalf("startup logs = %d, want base log plus 2", len(application.logs))
	}
}

func TestNewModelRejectsInvalidPaneSlices(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	for _, panes := range [][]Pane{
		nil,
		{{ID: ""}},
		{{ID: "same"}, {ID: "SAME"}},
		testPanes(9),
	} {
		if _, err := NewModel(backend, make(chan session.Event), panes, 80, 24, nil); err == nil {
			t.Fatalf("NewModel accepted invalid panes: %#v", panes)
		}
	}
}

func TestNewModelCopiesTheDynamicPaneSlice(t *testing.T) {
	backend := newFakeBackend()
	t.Cleanup(backend.cancel)
	panes := testPanes(3)
	application, err := NewModel(backend, make(chan session.Event), panes, 100, 30, nil)
	if err != nil {
		t.Fatal(err)
	}
	panes[0].ID = "mutated"
	panes[1].Name = "mutated"
	if len(application.panes) != 3 || application.panes[0].sessionID != "agent-1" || application.panes[1].name != "Agent 2" {
		t.Fatalf("model aliases caller pane slice: %#v", application.panes)
	}
}

func testPanes(count int) []Pane {
	panes := make([]Pane, count)
	for index := range panes {
		panes[index] = Pane{ID: fmt.Sprintf("agent-%d", index+1), Name: fmt.Sprintf("Agent %d", index+1)}
	}
	return panes
}
