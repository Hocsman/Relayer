package main

import (
	"fmt"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
)

func TestActiveAgentViewportHandlesNavigationKeys(t *testing.T) {
	application := newModelHarness(t)
	application = updateModel(t, application, tea.WindowSizeMsg{Width: 100, Height: 30})
	content := viewportTestLines(0, 120)
	for index := range application.panes {
		application = updateModel(t, application, SessionOutputMsg{
			SessionID: application.panes[index].sessionID,
			Content:   content,
		})
		application.panes[index].viewport.GotoTop()
	}

	application.activePanel = 0
	application = updateModel(t, application, tea.KeyMsg{Type: tea.KeyDown})
	downOffset := application.panes[0].viewport.YOffset
	if downOffset <= 0 {
		t.Fatalf("Down did not move the active viewport: offset = %d", downOffset)
	}
	if got := application.panes[1].viewport.YOffset; got != 0 {
		t.Fatalf("Down moved inactive pane 1 to offset %d", got)
	}

	application = updateModel(t, application, tea.KeyMsg{Type: tea.KeyUp})
	if got := application.panes[0].viewport.YOffset; got >= downOffset {
		t.Fatalf("Up did not move toward the top: offset = %d, previous = %d", got, downOffset)
	}

	application = updateModel(t, application, tea.KeyMsg{Type: tea.KeyPgDown})
	pageDownOffset := application.panes[0].viewport.YOffset
	if pageDownOffset <= downOffset {
		t.Fatalf("PageDown offset = %d, want a page jump beyond Down offset %d", pageDownOffset, downOffset)
	}
	if got := application.panes[1].viewport.YOffset; got != 0 {
		t.Fatalf("PageDown moved inactive pane 1 to offset %d", got)
	}

	application = updateModel(t, application, tea.KeyMsg{Type: tea.KeyPgUp})
	if got := application.panes[0].viewport.YOffset; got >= pageDownOffset {
		t.Fatalf("PageUp did not move toward the top: offset = %d, previous = %d", got, pageDownOffset)
	}

	firstPaneOffset := application.panes[0].viewport.YOffset
	application.activePanel = 1
	application = updateModel(t, application, tea.KeyMsg{Type: tea.KeyDown})
	if got := application.panes[1].viewport.YOffset; got <= 0 {
		t.Fatalf("Down did not move newly active pane 1: offset = %d", got)
	}
	if got := application.panes[0].viewport.YOffset; got != firstPaneOffset {
		t.Fatalf("navigation of pane 1 changed inactive pane 0 from %d to %d", firstPaneOffset, got)
	}
}

func TestAgentViewportPreservesScrollPositionWhenOutputArrives(t *testing.T) {
	application := newModelHarness(t)
	application = updateModel(t, application, tea.WindowSizeMsg{Width: 100, Height: 30})
	sessionID := application.panes[0].sessionID
	initial := viewportTestLines(0, 80)
	application = updateModel(t, application, SessionOutputMsg{
		SessionID: sessionID,
		Content:   initial,
	})
	if !application.panes[0].viewport.AtBottom() {
		t.Fatal("initial output did not start at the bottom")
	}

	application.activePanel = 0
	application = updateModel(t, application, tea.KeyMsg{Type: tea.KeyPgUp})
	if application.panes[0].viewport.AtBottom() {
		t.Fatal("PageUp left the viewport at the bottom")
	}
	beforeOffset := application.panes[0].viewport.YOffset
	beforeView := application.panes[0].viewport.View()

	application = updateModel(t, application, SessionOutputMsg{
		SessionID: sessionID,
		Content:   initial + "\n" + viewportTestLines(80, 10),
	})
	if got := application.panes[0].viewport.YOffset; got != beforeOffset {
		t.Fatalf("new output moved a scrolled viewport from offset %d to %d", beforeOffset, got)
	}
	if application.panes[0].viewport.AtBottom() {
		t.Fatal("new output forced a manually scrolled viewport back to the bottom")
	}
	if got := application.panes[0].viewport.View(); got != beforeView {
		t.Fatalf("visible content changed while user was scrolled:\n--- before ---\n%s\n--- after ---\n%s", beforeView, got)
	}
}

func TestAgentViewportFollowsOutputWhenAlreadyAtBottom(t *testing.T) {
	application := newModelHarness(t)
	application = updateModel(t, application, tea.WindowSizeMsg{Width: 100, Height: 30})
	sessionID := application.panes[0].sessionID
	initial := viewportTestLines(0, 40)
	application = updateModel(t, application, SessionOutputMsg{
		SessionID: sessionID,
		Content:   initial,
	})
	if !application.panes[0].viewport.AtBottom() {
		t.Fatal("initial output did not start at the bottom")
	}
	beforeOffset := application.panes[0].viewport.YOffset

	application = updateModel(t, application, SessionOutputMsg{
		SessionID: sessionID,
		Content:   initial + "\n" + viewportTestLines(40, 10),
	})
	if !application.panes[0].viewport.AtBottom() {
		t.Fatal("viewport stopped following output despite already being at the bottom")
	}
	if got := application.panes[0].viewport.YOffset; got <= beforeOffset {
		t.Fatalf("bottom offset did not advance after appended output: before %d, after %d", beforeOffset, got)
	}
	if got := application.panes[0].viewport.View(); !strings.Contains(got, "line 049") {
		t.Fatalf("bottom viewport does not contain newest line:\n%s", got)
	}
}

func TestAgentViewportPreservesScrollPositionAcrossResize(t *testing.T) {
	application := newModelHarness(t)
	application = updateModel(t, application, tea.WindowSizeMsg{Width: 100, Height: 30})
	application = updateModel(t, application, SessionOutputMsg{
		SessionID: application.panes[0].sessionID,
		Content:   viewportTestLines(0, 120),
	})
	application.activePanel = 0
	application = updateModel(t, application, tea.KeyMsg{Type: tea.KeyPgUp})
	beforeOffset := application.panes[0].viewport.YOffset
	if application.panes[0].viewport.AtBottom() {
		t.Fatal("test precondition failed: PageUp did not leave the bottom")
	}

	application = updateModel(t, application, tea.WindowSizeMsg{Width: 121, Height: 40})
	if got := application.panes[0].viewport.YOffset; got != beforeOffset {
		t.Fatalf("resize moved a scrolled viewport from offset %d to %d", beforeOffset, got)
	}
	if application.panes[0].viewport.AtBottom() {
		t.Fatal("resize forced a manually scrolled viewport back to the bottom")
	}
}

func TestSupervisorViewportHandlesPageKeysWhileInputIsFocused(t *testing.T) {
	application := newModelHarness(t)
	application = updateModel(t, application, tea.WindowSizeMsg{Width: 100, Height: 30})
	for index := 0; index < 80; index++ {
		application.appendLog(fmt.Sprintf("supervisor event %03d", index))
	}
	application = updateModel(t, application, PromptDetectedMsg{
		SessionID:   application.panes[0].sessionID,
		Pattern:     "confirmation",
		Description: "manual approval",
	})
	if application.activePanel != 2 || application.inputTarget != 0 {
		t.Fatal("test precondition failed: supervisor input is not focused")
	}
	if !application.supervisor.AtBottom() {
		t.Fatal("supervisor did not initially follow logs")
	}

	application.input.SetValue("answer-in-progress")
	application = updateModel(t, application, tea.KeyMsg{Type: tea.KeyPgUp})
	if application.supervisor.AtBottom() {
		t.Fatal("PageUp did not scroll supervisor history while input was focused")
	}
	beforeOffset := application.supervisor.YOffset
	application.appendLog("new event while reading history")
	if got := application.supervisor.YOffset; got != beforeOffset {
		t.Fatalf("new log moved supervisor from offset %d to %d", beforeOffset, got)
	}
	if got := application.input.Value(); got != "answer-in-progress" {
		t.Fatalf("supervisor navigation changed input value to %q", got)
	}

	for !application.supervisor.AtBottom() {
		application = updateModel(t, application, tea.KeyMsg{Type: tea.KeyPgDown})
	}
	application.appendLog("latest supervisor event")
	if !application.supervisor.AtBottom() {
		t.Fatal("supervisor did not resume automatic following at the bottom")
	}
}

func TestStreamingOutputSnapshotsRemainRingBounded(t *testing.T) {
	const capacity = 64
	outputNotifications := 0
	interceptor, err := NewInterceptor(
		91,
		nil,
		capacity,
		func(message tea.Msg, _ bool) bool {
			if _, ok := message.(SessionOutputMsg); ok {
				outputNotifications++
			}
			return true
		},
	)
	if err != nil {
		t.Fatalf("NewInterceptor returned an error: %v", err)
	}

	stream := strings.Repeat("0123456789", 20)
	for offset := 0; offset < len(stream); offset += 17 {
		end := minInt(offset+17, len(stream))
		interceptor.Consume([]byte(stream[offset:end]))
		if got := interceptor.output.Len(); got > capacity {
			t.Fatalf("ring grew to %d bytes after offset %d, capacity is %d", got, offset, capacity)
		}
	}

	if outputNotifications == 0 {
		t.Fatal("stream emitted no output notifications")
	}
	if got := interceptor.output.Len(); got != capacity {
		t.Fatalf("ring length = %d, want capacity %d", got, capacity)
	}
	want := stream[len(stream)-capacity:]
	if got := interceptor.output.String(); got != want {
		t.Fatalf("ring retained %q, want newest bytes %q", got, want)
	}
}

func viewportTestLines(start, count int) string {
	lines := make([]string, count)
	for index := range lines {
		lines[index] = fmt.Sprintf("line %03d", start+index)
	}
	return strings.Join(lines, "\n")
}
