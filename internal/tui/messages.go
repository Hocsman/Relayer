package tui

import (
	"context"
	"os/exec"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

type execProcessFunc func(*exec.Cmd, tea.ExecCallback) tea.Cmd

type inputDeliveredMsg struct {
	SessionID string
	Prompt    session.PromptDetected
	Err       error
}

type backendStoppedMsg struct{}

type attachFinishedMsg struct {
	SessionID string
	Err       error
}

type resyncFinishedMsg struct {
	SessionID string
	Prompt    *session.PromptDetected
	Err       error
}

type resizeRequest struct {
	SessionID string
	Name      string
	Columns   int
	Rows      int
}

type resizeFailure struct {
	Name string
	Err  error
}

type resizeFinishedMsg struct {
	Generation uint64
	Failures   []resizeFailure
}

const resizeBatchTimeout = 3 * time.Second

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

func resyncAttachedSession(
	ctx context.Context,
	backend AttachableBackend,
	sessionID string,
	columns int,
	rows int,
) tea.Cmd {
	return func() tea.Msg {
		if err := backend.Resync(ctx, sessionID, columns, rows); err != nil {
			return resyncFinishedMsg{SessionID: sessionID, Err: err}
		}
		var prompt *session.PromptDetected
		if snapshots, ok := backend.(PromptSnapshotBackend); ok {
			var err error
			prompt, err = snapshots.PendingPrompt(ctx, sessionID)
			if err != nil {
				return resyncFinishedMsg{SessionID: sessionID, Err: err}
			}
		}
		return resyncFinishedMsg{
			SessionID: sessionID,
			Prompt:    prompt,
		}
	}
}

// resizeSessions applies one geometry generation concurrently. tmux needs two
// short subprocesses per pane (identity check and resize), so doing this work
// in Update would freeze keyboard handling for seconds when tmux is unhealthy.
func resizeSessions(
	ctx context.Context,
	backend ContextResizeBackend,
	generation uint64,
	requests []resizeRequest,
) tea.Cmd {
	copied := append([]resizeRequest(nil), requests...)
	return func() tea.Msg {
		if ctx == nil {
			ctx = context.Background()
		}
		bounded, cancel := context.WithTimeout(ctx, resizeBatchTimeout)
		defer cancel()

		errorsByIndex := make([]error, len(copied))
		var group sync.WaitGroup
		for index, request := range copied {
			index, request := index, request
			group.Add(1)
			go func() {
				defer group.Done()
				errorsByIndex[index] = backend.ResizeContext(
					bounded,
					request.SessionID,
					request.Columns,
					request.Rows,
				)
			}()
		}
		group.Wait()

		failures := make([]resizeFailure, 0)
		for index, err := range errorsByIndex {
			if err != nil {
				failures = append(failures, resizeFailure{Name: copied[index].Name, Err: err})
			}
		}
		return resizeFinishedMsg{Generation: generation, Failures: failures}
	}
}
