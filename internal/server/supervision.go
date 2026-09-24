package server

import (
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// The gateway supervises its prompts through internal/supervise, the core the
// desktop moved its state machine into. It kept a second copy of that machine
// until v0.8.8, which never delivered the policy's automatic decisions,
// journaled no detection, evaluation, withdrawal, natural exit or stream
// error, went on writing to agents once the journal had failed, dropped a
// prompt before its answer was written, and wrote two answers to one terminal
// at once. What decides whether a byte reaches an agent is now the core's;
// the gateway brings the websocket and what only it has: the output, the
// presence of its clients and the hand.

// gatewaySink carries what one run's supervision core shows to every client of
// the gateway, as the gateway's own frames. Each frame goes to every client,
// viewers included, since the fan-out does not filter by role: what the core
// shows is display-safe by construction, and the sink adds nothing that is not.
//
// The core calls the sink without holding its own lock, so the sink may take
// c.mu, and it does. The gateway therefore never holds c.mu while it calls an
// operation that changes the core, or Wait: the core may show, on that
// goroutine, what another queued, and wait for a sink call blocked on c.mu.
// It may hold c.mu while it reads the core or calls SetHolder or BeginDrain,
// none of which calls the sink on the caller's goroutine.
type gatewaySink struct {
	c  *Controller
	rt *app.DesktopRuntime
	// sup is the core this sink shows, set under c.mu before the run takes in
	// any event: it is how the sink knows its run is still the gateway's.
	sup *supervise.Supervisor
}

// current returns the sink's core when its run is still the gateway's run. A
// run a profile restart replaced may still finish a write, and what it shows
// then is the previous run's: the clients have moved on, and a notice about it
// would reach a webhook about an agent that is gone.
func (s *gatewaySink) current() (*supervise.Supervisor, bool) {
	s.c.mu.RLock()
	defer s.c.mu.RUnlock()
	return s.sup, s.sup != nil && s.c.sup == s.sup
}

func (s *gatewaySink) Prompt(view supervise.View) {
	if _, current := s.current(); !current {
		return
	}
	s.c.broadcast(eventSemantic, supervisionEventFromView(view))
}

func (s *gatewaySink) Status(status supervise.Status) {
	if _, current := s.current(); !current {
		return
	}
	s.c.broadcast(eventStatus, StatusEvent(status))
}

// Error shows a failure by the core's fixed code and message. The gateway sent
// a backend stream error's own text to every client, viewers included, and
// that text can name a path or echo what the backend was given.
func (s *gatewaySink) Error(failure supervise.SafeError) {
	if _, current := s.current(); !current {
		return
	}
	s.c.broadcast(eventError, SafeErrorEvent(failure))
}

func (s *gatewaySink) Refresh(sessionID string) {
	sup, current := s.current()
	if !current {
		return
	}
	s.c.refreshOutput(s.rt, sup, sessionID)
}

// Lifecycle follows a new process: its predecessor's output does not carry
// over. The revision stays monotonic, so the next snapshot is accepted by the
// interface's revision guard.
func (s *gatewaySink) Lifecycle(sessionID string, phase supervise.Phase) {
	if phase != supervise.PhaseStarted {
		return
	}
	s.c.mu.Lock()
	defer s.c.mu.Unlock()
	if s.sup == nil || s.c.sup != s.sup {
		return
	}
	if index, found := s.c.agentIndex[strings.ToLower(sessionID)]; found && index < len(s.c.state.Agents) {
		s.c.state.Agents[index].Output = ""
		s.c.state.Agents[index].Revision++
	}
}

// Notify tells the operators about a prompt by the core's rules: a guardrail
// is critical even when the policy decided alone, and a prompt waits on a
// person only when the policy does not answer it. The gateway notified every
// prompt as awaiting a decision, the policy's own included, by the agent's ID,
// with the adapter's raw summary, which a webhook posted as it was; the notice
// carries the agent's name and the summary the prompt is shown with.
func (s *gatewaySink) Notify(notice supervise.Notice) {
	if _, current := s.current(); !current {
		return
	}
	s.c.mu.RLock()
	notifier := s.c.notifier
	config := s.c.notificationConfig
	s.c.mu.RUnlock()

	notification := notify.Notification{
		Title:     "Relayer Arbitration Required",
		AgentName: notice.AgentName,
		SessionID: notice.SessionID,
		Reason:    notice.Reason,
		EventID:   notice.EventID,
		Kind:      notify.KindPendingDecision,
		Severity:  notify.SeverityWarning,
		Details:   notice.Details,
		Timestamp: time.Now().UTC(),
	}
	if notice.Kind == supervise.NoticeGuardrailBlocked {
		notification.Title = "Relayer Guardrail Alert"
		notification.Kind = notify.KindGuardrailBlocked
	}
	if notice.Severity == supervise.SeverityCritical {
		notification.Severity = notify.SeverityCritical
	}
	if notifier != nil {
		notifier.Notify(notification)
	}
	if config.Enabled && notify.SeverityMeetsThreshold(notification.Severity, config.MinSeverity) {
		s.c.broadcast(eventNotification, NotificationEvent{
			Title:     notification.Title,
			Body:      notice.Details,
			AgentName: notice.AgentName,
			SessionID: notice.SessionID,
			EventID:   notice.EventID,
			Kind:      notification.Kind,
			Severity:  notification.Severity,
			Reason:    notice.Reason,
			Timestamp: notification.Timestamp.Format(time.RFC3339),
		})
	}
}

// supervisionEventFromView is the gateway's frame for a core view. The two
// have the same fields and JSON, and the gateway adds the tool-call badge the
// desktop has no place for, in the form the core allows it to be shown.
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
		ToolCall:       toolCallView(view.ToolCall()),
	}
}

// withSupervision lays the core's state of an agent over the gateway's own.
// Attached stays the gateway's: it is who holds the hand, which the gateway
// owns and tells the core (SetHolder), and the two agree.
func withSupervision(displayed AgentState, supervised supervise.Agent) AgentState {
	displayed.Status = supervised.Status
	displayed.Running = supervised.Running
	displayed.InputFrozen = supervised.InputFrozen
	displayed.ExitCode = cloneExitCode(supervised.ExitCode)
	return displayed
}

func cloneExitCode(value *int) *int {
	if value == nil {
		return nil
	}
	code := *value
	return &code
}
