package main

import (
	"strings"

	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// desktopSink carries what the supervision core of one run shows to the Wails
// window, as the bridge's own DTOs. The core calls it without holding its own
// lock, so it may take a.mu and read the core; it never calls an operation
// that changes the core.
type desktopSink struct {
	app *App
	run *runGeneration
}

// current reports whether the sink's run is still the bridge's active run.
// Before the state machine moved into internal/supervise, every entry point
// checked that its run was the active one before it emitted. The core checks
// only that its run is not draining, which is the same today: a run's core is
// drained and waited for before stopGenerationLocked forgets the run, and so
// before activateRun can replace it, and the run stays the active one
// throughout its drain. That guarantee rests on the drain's ordering alone, so
// the sink checks again: this is defence in depth, and it changes nothing
// while the ordering holds.
func (s desktopSink) current() bool {
	s.app.mu.RLock()
	defer s.app.mu.RUnlock()
	return s.app.active == s.run
}

func (s desktopSink) Prompt(view supervise.View) {
	if !s.current() {
		return
	}
	s.app.emit(eventSemantic, supervisionEventFromView(view))
}

func (s desktopSink) Status(status supervise.Status) {
	if status.Scope == "audit" {
		// The journal's failure outlives the run on screen: after a shutdown
		// the last run's audit state is still what the window shows.
		s.app.mu.Lock()
		current := s.app.active == s.run
		if current {
			s.app.state.Audit.Status = status.Status
		}
		s.app.mu.Unlock()
		if !current {
			return
		}
	} else if !s.current() {
		return
	}
	s.app.emit(eventStatus, StatusEvent(status))
}

func (s desktopSink) Error(failure supervise.SafeError) {
	if !s.current() {
		return
	}
	s.app.emit(eventError, SafeErrorEvent(failure))
}

func (s desktopSink) Refresh(sessionID string) {
	s.app.refreshOutputForRun(s.run, sessionID)
}

// Lifecycle follows a new process: its predecessor's output does not carry
// over. Revision stays monotonic across process instances so the follow-up
// output snapshot is accepted by the presentation's revision guard.
func (s desktopSink) Lifecycle(sessionID string, phase supervise.Phase) {
	if phase != supervise.PhaseStarted {
		return
	}
	s.app.mu.Lock()
	defer s.app.mu.Unlock()
	if s.app.active != s.run {
		return
	}
	if index, found := s.app.agentIndex[strings.ToLower(sessionID)]; found {
		s.app.state.Agents[index].Output = ""
		s.app.state.Agents[index].Revision++
	}
}

func (s desktopSink) Notify(notice supervise.Notice) {
	// A stale run's notice is dropped for the same reason as its emissions:
	// the desktop only notified from a prompt its active run took in.
	if !s.current() {
		return
	}
	notifier := s.app.currentNotifier()
	if notifier == nil {
		return
	}
	notification := notify.Notification{
		Title:     "Relayer",
		AgentName: notice.AgentName,
		SessionID: notice.SessionID,
		Reason:    notice.Reason,
		EventID:   notice.EventID,
		Kind:      notify.KindPendingDecision,
		Severity:  notify.SeverityWarning,
		Details:   notice.Details,
	}
	if notice.Kind == supervise.NoticeGuardrailBlocked {
		notification.Title = "🛡️ Relayer Guardrail Alert"
		notification.Kind = notify.KindGuardrailBlocked
	}
	if notice.Severity == supervise.SeverityCritical {
		notification.Severity = notify.SeverityCritical
	}
	notifier.Notify(notification)
}

// withSupervision lays the core's state of an agent over the bridge's own.
func withSupervision(displayed AgentState, supervised supervise.Agent) AgentState {
	displayed.Status = supervised.Status
	displayed.Running = supervised.Running
	displayed.Attached = supervised.Attached
	displayed.InputFrozen = supervised.InputFrozen
	displayed.ExitCode = cloneInt(supervised.ExitCode)
	return displayed
}

// supervisionEventFromView is the bridge's DTO for a core view. The two have
// the same fields and the same JSON; the bridge keeps a type of its own because
// Wails generates the frontend's models from the types its methods return, and
// a core type would move them into another namespace.
func supervisionEventFromView(view supervise.View) SupervisionEvent {
	return SupervisionEvent{
		RunID:          view.RunID,
		ID:             view.ID,
		SessionID:      view.SessionID,
		AgentID:        view.AgentID,
		Adapter:        view.Adapter,
		Type:           view.Type,
		Summary:        view.Summary,
		Sensitive:      view.Sensitive,
		Risk:           view.Risk,
		Timestamp:      view.Timestamp,
		Evaluation:     PolicyEvaluation(view.Evaluation),
		DeliveryStatus: view.DeliveryStatus,
		Decisions:      view.Decisions,
	}
}
