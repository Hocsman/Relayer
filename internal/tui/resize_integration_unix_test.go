//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package tui_test

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/tui"
	tea "github.com/charmbracelet/bubbletea"
)

func TestWindowSizeMsgPropagatesThroughManagerToRealPTYs(t *testing.T) {
	if _, err := exec.LookPath("stty"); err != nil {
		t.Skipf("stty is required for the PTY resize assertion: %v", err)
	}

	events := make(chan session.Event, 128)
	manager, err := session.NewManager(context.Background(), events, nil, 4096)
	if err != nil {
		t.Fatalf("session.NewManager returned an error: %v", err)
	}
	defer manager.Close()

	command := `printf 'READY\n'; while IFS= read -r line; do printf 'SIZE '; stty size; done`
	var infos [2]session.Info
	for index := range infos {
		infos[index], err = manager.Start(fmt.Sprintf("resize agent %d", index), command, 40, 10)
		if err != nil {
			t.Fatalf("starting session %d: %v", index, err)
		}
	}
	waitForSessionOutput(t, manager, infos, func(_ int, output string) bool {
		return strings.Contains(output, "READY")
	})

	application := tui.NewModel(
		manager,
		events,
		[2]tui.Pane{
			{ID: infos[0].ID, Name: infos[0].Name, Command: infos[0].Command},
			{ID: infos[1].ID, Name: infos[1].Name, Command: infos[1].Command},
		},
		80,
		24,
		nil,
	)
	const width = 121
	const height = 40
	_, _ = application.Update(tea.WindowSizeMsg{Width: width, Height: height})
	for index, info := range infos {
		if err := manager.SendInput(info.ID, "probe"); err != nil {
			t.Fatalf("probing resized session %d: %v", index, err)
		}
	}

	geometry := tui.CalculateLayout(width, height)
	waitForSessionOutput(t, manager, infos, func(index int, output string) bool {
		want := fmt.Sprintf("SIZE %d %d", geometry.AgentViewportHeight, geometry.AgentViewportWidths[index])
		return strings.Contains(output, want)
	})
}

func waitForSessionOutput(
	t *testing.T,
	manager *session.Manager,
	infos [2]session.Info,
	ready func(index int, output string) bool,
) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		allReady := true
		for index, info := range infos {
			output, err := manager.Output(info.ID)
			if err != nil {
				t.Fatalf("reading session %d output: %v", index, err)
			}
			if !ready(index, output) {
				allReady = false
			}
		}
		if allReady {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}

	outputs := make([]string, len(infos))
	for index, info := range infos {
		outputs[index], _ = manager.Output(info.ID)
	}
	t.Fatalf("timed out waiting for PTY output: %#v", outputs)
}
