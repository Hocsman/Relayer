package tui

import (
	"context"

	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

type inputDeliveredMsg struct {
	SessionID string
	Prompt    session.PromptDetected
	Err       error
}

type backendStoppedMsg struct{}

// backendEventMsg distinguishes a completed channel subscription from other
// Bubble Tea commands. This keeps exactly one channel waiter active at a time.
type backendEventMsg struct {
	Event session.Event
}

func waitForBackendEvent(ctx context.Context, events <-chan session.Event) tea.Cmd {
	return func() tea.Msg {
		select {
		case event, ok := <-events:
			if !ok {
				return backendStoppedMsg{}
			}
			return backendEventMsg{Event: event}
		case <-ctx.Done():
			return backendStoppedMsg{}
		}
	}
}

func deliverInput(
	backend Backend,
	sessionID string,
	value string,
	prompt session.PromptDetected,
) tea.Cmd {
	return func() tea.Msg {
		return inputDeliveredMsg{
			SessionID: sessionID,
			Prompt:    prompt,
			Err:       backend.SendInput(sessionID, value),
		}
	}
}
