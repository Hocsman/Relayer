package tui

import (
	"context"
	"errors"
	"os/exec"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	tea "github.com/charmbracelet/bubbletea"
)

type execProcessFunc func(*exec.Cmd, tea.ExecCallback) tea.Cmd

type inputDeliveredMsg struct {
	SessionID string
	Event     adapters.Event
	// Decision is what the operator expressed. Free-form text is recorded as
	// an ask; the semantic answers carry their own value so the journal can
	// show a refusal made by a person.
	Decision audit.Decision
	Err      error
}

// lineInputDeliveredMsg intentionally carries no submitted value or length.
// Pending contains only the core's canonical semantic occurrence used to
// reconcile a CAS refusal.
type lineInputDeliveredMsg struct {
	SessionID    string
	Err          error
	Pending      *adapters.Event
	PendingKnown bool
}

var (
	errAutomaticDecisionBackendUnavailable = errors.New("automatic decision backend unavailable")
	errDecisionBackendUnavailable          = errors.New("manual decision backend unavailable")
	errEventSnapshotBackendUnavailable     = errors.New("event snapshot backend unavailable")
	errLineDeliveryUncertain               = errors.New("direct instruction delivery uncertain")
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
	SessionID string
	Name      string
	Err       error
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
	gates ...*deliveryGate,
) tea.Cmd {
	return func() tea.Msg {
		message := inputDeliveredMsg{
			SessionID: sessionID,
			Event:     event.Clone(),
			Decision:  audit.DecisionAsk,
		}
		if len(gates) > 0 {
			if !gates[0].beginOperation() {
				message.Err = errAuditUnavailable
				return message
			}
			defer gates[0].endOperation()
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

func deliverLineInput(
	backend Backend,
	sessionID string,
	value string,
	gates ...*deliveryGate,
) tea.Cmd {
	return func() tea.Msg {
		message := lineInputDeliveredMsg{SessionID: sessionID}
		if len(gates) > 0 {
			if !gates[0].beginOperation() {
				message.Err = errAuditUnavailable
				return message
			}
			defer gates[0].endOperation()
		}
		lines, ok := backend.(LineInputBackend)
		if !ok {
			message.Err = terminal.ErrLineUnsupported
			return message
		}
		deliveryErr := lines.SendLine(sessionID, value)
		// Drop the command closure's last reference as soon as the synchronous
		// backend call returns. The result message never carries the value.
		value = ""
		// Keep backend error strings out of Bubble Tea messages: a faulty
		// transport must not be able to echo operator input into retained UI
		// state. Only the public semantic sentinels cross this boundary.
		switch {
		case deliveryErr == nil:
			message.Err = nil
		case errors.Is(deliveryErr, terminal.ErrEventPending):
			message.Err = terminal.ErrEventPending
		case errors.Is(deliveryErr, terminal.ErrInvalidLine):
			message.Err = terminal.ErrInvalidLine
		case errors.Is(deliveryErr, terminal.ErrLineUnsupported):
			message.Err = terminal.ErrLineUnsupported
		default:
			message.Err = errLineDeliveryUncertain
		}
		if !errors.Is(deliveryErr, terminal.ErrEventPending) {
			return message
		}
		if snapshots, ok := backend.(EventSnapshotBackend); ok {
			pending, err := snapshots.PendingEvent(backend.Context(), sessionID)
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

func deliverAutomaticDecision(
	backend Backend,
	event adapters.Event,
	evaluation policy.Evaluation,
	decision adapters.Decision,
	gates ...*deliveryGate,
) tea.Cmd {
	return func() tea.Msg {
		message := automaticDecisionFinishedMsg{
			SessionID:  event.SessionID,
			Event:      event.Clone(),
			Evaluation: evaluation,
		}
		if len(gates) > 0 {
			if !gates[0].beginOperation() {
				message.Err = errAuditUnavailable
				return message
			}
			defer gates[0].endOperation()
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
				failures = append(failures, resizeFailure{
					SessionID: copied[index].SessionID,
					Name:      copied[index].Name,
					Err:       err,
				})
			}
		}
		return resizeFinishedMsg{Generation: generation, Failures: failures}
	}
}

// deliverHumanDecision encodes a semantic answer the operator chose and
// delivers it through the adapter that produced the exact occurrence.
//
// It shares the automatic path's transport because the encoding is the
// adapter's either way; only the attribution differs. An adapter that cannot
// represent the answer reports ErrDecisionUnsupported and nothing is written,
// which keeps the occurrence pending for a typed reply instead of inventing
// terminal bytes on the operator's behalf.
func deliverHumanDecision(
	backend Backend,
	sessionID string,
	event adapters.Event,
	decision adapters.Decision,
	gates ...*deliveryGate,
) tea.Cmd {
	return func() tea.Msg {
		message := inputDeliveredMsg{
			SessionID: sessionID,
			Event:     event.Clone(),
			Decision:  decisionForAdapter(decision),
		}
		if len(gates) > 0 {
			if !gates[0].beginOperation() {
				message.Err = errAuditUnavailable
				return message
			}
			defer gates[0].endOperation()
		}
		automatic, ok := backend.(AutomaticDecisionBackend)
		if !ok {
			message.Err = errAutomaticDecisionBackendUnavailable
			return message
		}
		message.Err = automatic.SendAutomaticDecision(sessionID, event.Clone(), decision)
		return message
	}
}
