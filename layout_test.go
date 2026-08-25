package main

import (
	"context"
	"runtime"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
)

func TestWindowSizeMsgRecomputesViewportGeometry(t *testing.T) {
	application := newModelHarness(t)
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
			name:                     "second resize replaces prior geometry",
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
			application = updateModel(t, application, tea.WindowSizeMsg{
				Width:  test.width,
				Height: test.height,
			})

			if application.width != test.width || application.height != test.height {
				t.Fatalf(
					"model terminal size = %dx%d, want %dx%d",
					application.width,
					application.height,
					test.width,
					test.height,
				)
			}
			if application.leftWidth != test.leftWidth || application.rightWidth != test.rightWidth {
				t.Fatalf(
					"pane outer widths = %d/%d, want %d/%d",
					application.leftWidth,
					application.rightWidth,
					test.leftWidth,
					test.rightWidth,
				)
			}
			if application.leftWidth+application.rightWidth != application.width {
				t.Fatalf(
					"50/50 split loses columns: %d + %d != %d",
					application.leftWidth,
					application.rightWidth,
					application.width,
				)
			}
			widthDifference := application.rightWidth - application.leftWidth
			if widthDifference < 0 || widthDifference > 1 {
				t.Fatalf("50/50 pane width difference = %d, want 0 or 1", widthDifference)
			}
			if application.topHeight != test.topHeight || application.supervisorHeight != test.supervisorHeight {
				t.Fatalf(
					"section heights = %d/%d, want %d/%d",
					application.topHeight,
					application.supervisorHeight,
					test.topHeight,
					test.supervisorHeight,
				)
			}
			if application.panes[0].viewport.Width != test.leftViewportWidth ||
				application.panes[1].viewport.Width != test.rightViewportWidth {
				t.Fatalf(
					"agent viewport widths = %d/%d, want %d/%d",
					application.panes[0].viewport.Width,
					application.panes[1].viewport.Width,
					test.leftViewportWidth,
					test.rightViewportWidth,
				)
			}
			for index := range application.panes {
				if application.panes[index].viewport.Height != test.agentViewportHeight {
					t.Fatalf(
						"pane %d viewport height = %d, want %d",
						index,
						application.panes[index].viewport.Height,
						test.agentViewportHeight,
					)
				}
			}
			if application.supervisor.Width != test.supervisorViewportWidth ||
				application.supervisor.Height != test.supervisorViewportHeight {
				t.Fatalf(
					"supervisor viewport = %dx%d, want %dx%d",
					application.supervisor.Width,
					application.supervisor.Height,
					test.supervisorViewportWidth,
					test.supervisorViewportHeight,
				)
			}
			if application.input.Width != test.inputWidth {
				t.Fatalf("input width = %d, want %d", application.input.Width, test.inputWidth)
			}
		})
	}
}

func TestWindowSizeMsgPropagatesViewportDimensionsToPTYs(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("creack/pty integration requires a Unix PTY")
	}

	events := make(chan tea.Msg, 32)
	manager, err := NewSessionManager(
		context.Background(),
		events,
		defaultPromptPatterns,
		1024,
	)
	if err != nil {
		t.Fatalf("NewSessionManager returned an error: %v", err)
	}
	defer manager.Close()

	var sessions [2]*Session
	for index := range sessions {
		session, startErr := manager.Start(
			"resize agent",
			`trap 'exit 0' TERM HUP INT; read answer`,
			40,
			10,
		)
		if startErr != nil {
			t.Fatalf("starting session %d: %v", index, startErr)
		}
		sessions[index] = session
		assertSessionPTYSize(t, session, 40, 10)
	}

	application := newModel(manager, events, sessions)
	for _, size := range []tea.WindowSizeMsg{
		{Width: 121, Height: 40},
		{Width: 81, Height: 32},
	} {
		application = updateModel(t, application, size)
		for index, session := range sessions {
			assertSessionPTYSize(
				t,
				session,
				application.panes[index].viewport.Width,
				application.panes[index].viewport.Height,
			)
		}
	}
}

func assertSessionPTYSize(t *testing.T, session *Session, wantColumns, wantRows int) {
	t.Helper()
	session.fileMu.RLock()
	defer session.fileMu.RUnlock()
	if session.master == nil {
		t.Fatal("session PTY is closed")
	}

	size, err := pty.GetsizeFull(session.master)
	if err != nil {
		t.Fatalf("reading session PTY size: %v", err)
	}
	if gotColumns, gotRows := int(size.Cols), int(size.Rows); gotColumns != wantColumns || gotRows != wantRows {
		t.Fatalf(
			"PTY size = %dx%d columns/rows, want %dx%d",
			gotColumns,
			gotRows,
			wantColumns,
			wantRows,
		)
	}
}
