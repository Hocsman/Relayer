package tui

import (
	"context"
	"os/exec"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

type execProcessFunc func(*exec.Cmd, tea.ExecCallback) tea.Cmd

type inputDeliveredMsg struct {
	SessionID string
	Event     adapters.Event
	Err       error
}

type backendStoppedMsg struct{}

type attachFinishedMsg struct {
	SessionID string
	Err       error
}

type resyncFinishedMsg struct {
	SessionID string
	Pending   *adapters.Event
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
	event adapters.Event,
) tea.Cmd {
	return func() tea.Msg {
		message := inputDeliveredMsg{
			SessionID: sessionID,
			Event:     event.Clone(),
		}
		if decisions, ok := backend.(DecisionBackend); ok {
			message.Err = decisions.SendDecision(sessionID, event, value)
		} else {
			message.Err = backend.SendInput(sessionID, value)
		}
		return message
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
		var pending *adapters.Event
		if snapshots, ok := backend.(EventSnapshotBackend); ok {
			var err error
			pending, err = snapshots.PendingEvent(ctx, sessionID)
			if err != nil {
				return resyncFinishedMsg{SessionID: sessionID, Err: err}
			}
		}
		return resyncFinishedMsg{
			SessionID: sessionID,
			Pending:   pending,
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
