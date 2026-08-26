package tui

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	tea "github.com/charmbracelet/bubbletea"
)

type execProcessFunc func(*exec.Cmd, tea.ExecCallback) tea.Cmd

type inputDeliveredMsg struct {
	SessionID string
	Event     adapters.Event
	Err       error
}

var (
	errAutomaticDecisionBackendUnavailable = errors.New("backend de décision automatique indisponible")
	errDecisionBackendUnavailable          = errors.New("backend de décision manuelle indisponible")
	errEventSnapshotBackendUnavailable     = errors.New("backend de snapshot d'événement indisponible")
)

type automaticDecisionFinishedMsg struct {
	SessionID    string
	Event        adapters.Event
	Evaluation   policy.Evaluation
	Err          error
	Pending      *adapters.Event
	PendingKnown bool
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
		decisions, ok := backend.(DecisionBackend)
		if !ok {
			// A raw write cannot prove which pending occurrence it resolves and
			// would bypass adapter-specific encoding. Keep the event pending so
			// the user can retry through a capable backend instead.
			message.Err = errDecisionBackendUnavailable
			return message
		}
		message.Err = decisions.SendDecision(sessionID, event, value)
		return message
	}
}

func deliverAutomaticDecision(
	backend Backend,
	event adapters.Event,
	evaluation policy.Evaluation,
	decision adapters.Decision,
) tea.Cmd {
	return func() tea.Msg {
		message := automaticDecisionFinishedMsg{
			SessionID:  event.SessionID,
			Event:      event.Clone(),
			Evaluation: evaluation,
		}
		automatic, ok := backend.(AutomaticDecisionBackend)
		if !ok {
			message.Err = errAutomaticDecisionBackendUnavailable
		} else {
			message.Err = automatic.SendAutomaticDecision(event.SessionID, event.Clone(), decision)
		}
		if message.Err == nil {
			return message
		}
		if snapshots, ok := backend.(EventSnapshotBackend); ok {
			pending, err := snapshots.PendingEvent(backend.Context(), event.SessionID)
			if err == nil {
				message.PendingKnown = true
				if pending != nil {
					copy := pending.Clone()
					message.Pending = &copy
				}
			}
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
		snapshots, ok := backend.(EventSnapshotBackend)
		if !ok {
			return resyncFinishedMsg{SessionID: sessionID, Err: errEventSnapshotBackendUnavailable}
		}
		pending, err := snapshots.PendingEvent(ctx, sessionID)
		if err != nil {
			return resyncFinishedMsg{SessionID: sessionID, Err: err}
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
