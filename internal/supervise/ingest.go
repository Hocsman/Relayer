package supervise

import (
	"strconv"
	"strings"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
)

// Handle takes in one event of the run's session stream: a prompt, its
// withdrawal, a backend stream error or a legacy exit. OutputAvailable stays
// with the front end, which coalesces output refreshes; the core ignores it.
// Events are handled synchronously, on the caller's goroutine.
func (s *Supervisor) Handle(message session.Event) {
	switch value := message.(type) {
	case session.AdapterEvent:
		s.handleAdapterEvent(value.Event.Clone())
	case session.AdapterEventWithdrawn:
		s.handleAdapterEventWithdrawn(value.Event.Clone())
	case session.Error:
		s.markSessionError(value.SessionID, "backend_stream_failed")
	case session.Exited:
		s.markLegacyExit(value.SessionID)
	}
}

func (s *Supervisor) handleAdapterEventWithdrawn(event adapters.Event) {
	if !s.isActiveRun() {
		return
	}
	key := makeEventKey(event.SessionID, event.ID)
	if key.sessionID == "" || key.eventID == "" {
		return
	}

	backend := s.backendFor(event.SessionID)
	s.mu.RLock()
	owed := s.heldEntries[key.sessionID]
	s.mu.RUnlock()
	awaitHeldEntries(owed)
	_ = s.recordAudit(eventWithdrawnEntry(event, backend, "agent_withdrew_occurrence"))

	s.mu.Lock()
	if _, duplicate := s.resolved[key]; duplicate {
		s.mu.Unlock()
		return
	}
	pending, found := s.pending[key]
	if !found {
		s.mu.Unlock()
		return
	}
	delete(s.pending, key)
	sessionKey := strings.ToLower(event.SessionID)
	// The prompt goes, but a write of its answer may still be in progress:
	// the session's claim stays with that write, which releases it when it
	// returns (finishDecision) and then considers the next automatic prompt.
	// Released here, the claim let the next answer be written into the same
	// terminal while the first still was.
	s.markResolvedLocked(key)

	hasOtherPending := false
	for otherKey := range s.pending {
		if strings.ToLower(otherKey.sessionID) == sessionKey {
			hasOtherPending = true
			break
		}
	}
	currentStatus := "running"
	if index, foundAgent := s.agentIndex[sessionKey]; foundAgent {
		if !hasOtherPending && s.agents[index].Status == "waiting" {
			s.agents[index].Status = "running"
		}
		currentStatus = s.agents[index].Status
	}
	s.rebuildPendingLocked()
	view := pending.view
	view.DeliveryStatus = "delivered"
	s.showPromptLocked(view)
	s.showStatusLocked(Status{RunID: s.runID, Scope: "session", SessionID: event.SessionID, Status: currentStatus})
	s.emitLocked(func(sink Sink) { sink.Refresh(event.SessionID) })
	s.mu.Unlock()
	s.flush()
	s.scheduleAutomatic(event.SessionID)
}

func (s *Supervisor) handleAdapterEvent(event adapters.Event) {
	if !s.isActiveRun() {
		return
	}
	key := makeEventKey(event.SessionID, event.ID)
	if key.sessionID == "" || key.eventID == "" {
		s.emitSafeError("invalid_event", "An invalid event was ignored.", event.SessionID)
		return
	}
	if !s.reserveEvent(key) {
		return
	}
	defer s.releaseEventReservation(key)

	backend := s.backendFor(event.SessionID)
	if event.Type == adapters.EventProcessExit {
		s.handleProcessExit(event, backend)
		return
	}
	if !event.Actionable() {
		return
	}
	if !s.sessionRunning(event.SessionID) && !s.sessionStarting(key.sessionID) {
		s.mu.Lock()
		s.markResolvedLocked(key)
		s.mu.Unlock()
		return
	}
	// OutputAvailable is intentionally coalescable. Refreshing here guarantees
	// that an essential semantic event still brings the latest bounded tail to
	// the WebView even when its preceding output invalidation was dropped.
	s.emit(func(sink Sink) { sink.Refresh(event.SessionID) })
	if !s.recordAudit(eventDetectedEntry(event, backend)) {
		s.addFrozenEvent(event, policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionAsk, Reason: policy.ReasonNoEngine})
		return
	}
	// A repeat of a prompt just answered is asked, never answered, and is
	// journaled as asked: the guard applies before the evaluation entry.
	evaluation := s.guardRepeat(key, event, s.engine.Evaluate(event))
	// So is a prompt raised while somebody holds the terminal, who may answer
	// it by typing.
	evaluation = s.guardHeld(key.sessionID, evaluation)
	if !s.recordAudit(policyAuditEntry(event, backend, evaluation)) {
		s.addFrozenEvent(event, evaluation)
		return
	}

	view := supervisionView(s.runID, event, evaluation, "pending", s.engine.SupportedDecisions(event))
	isGuardrail := evaluation.Reason == policy.ReasonDestructive ||
		evaluation.Reason == policy.ReasonExfiltration ||
		evaluation.Reason == policy.ReasonGuardrailBlocked
	s.mu.Lock()
	// The hand may have been taken since the evaluation was journaled, while
	// the prompt was not yet among those SetHolder turns into asks: it is
	// asked now, and a second entry says why.
	var previous, done chan struct{}
	heldSince := evaluation.Automatic && s.holders[key.sessionID] != ""
	if heldSince {
		evaluation = askEvaluation(evaluation, ReasonOperatorAttached)
		view.Evaluation = evaluationView(evaluation)
		previous, done = s.oweHeldEntriesLocked(key.sessionID)
	}
	item := pendingEvent{event: event.Clone(), view: view, evaluation: evaluation}
	s.pending[key] = item
	s.setAgentWaitingLocked(event.SessionID)
	s.rebuildPendingLocked()
	s.showPromptLocked(view)
	agentName := event.AgentID
	if index, found := s.agentIndex[strings.ToLower(event.SessionID)]; found {
		agentName = s.agents[index].Name
	}

	// A notice's details are the summary the prompt is shown with, never the
	// adapter's own: a notification leaves the machine, and a webhook posts
	// it as it is. The raw summary went out even for a prompt that asked for
	// a password.
	var notice *Notice
	if isGuardrail {
		notice = &Notice{
			AgentName: agentName,
			SessionID: event.SessionID,
			Reason:    "security guardrail blocked (" + evaluation.Reason + ")",
			EventID:   event.ID,
			Kind:      NoticeGuardrailBlocked,
			Severity:  SeverityCritical,
			Details:   view.Summary,
		}
	} else if !evaluation.Automatic {
		reason := "confirmation required"
		// The policy's reason for a sensitive prompt is ReasonSensitive; this
		// compared it with "sensitive", which it never is.
		if requiresSecretHandling(event) || evaluation.Reason == policy.ReasonSensitive {
			reason = "sensitive input required"
		}
		notice = &Notice{
			AgentName: agentName,
			SessionID: event.SessionID,
			Reason:    reason,
			EventID:   event.ID,
			Kind:      NoticePendingDecision,
			Severity:  SeverityWarning,
			Details:   view.Summary,
		}
	}
	if notice != nil {
		shown := *notice
		s.emitLocked(func(sink Sink) { sink.Notify(shown) })
	}
	s.mu.Unlock()
	if heldSince {
		s.journalHeldEntries(key.sessionID, []pendingEvent{item}, previous, done)
	}
	s.flush()
	s.scheduleAutomatic(event.SessionID)
}

func (s *Supervisor) sessionRunning(sessionID string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	index, found := s.agentIndex[strings.ToLower(strings.TrimSpace(sessionID))]
	return found && s.agents[index].Running
}

func (s *Supervisor) sessionStarting(sessionKey string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.startingSessions[sessionKey]
}

func (s *Supervisor) reserveEvent(key eventKey) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.resolved[key]; exists {
		return false
	}
	if _, exists := s.pending[key]; exists {
		return false
	}
	if _, exists := s.ingesting[key]; exists {
		return false
	}
	s.ingesting[key] = struct{}{}
	return true
}

func (s *Supervisor) releaseEventReservation(key eventKey) {
	s.mu.Lock()
	delete(s.ingesting, key)
	s.mu.Unlock()
}

func (s *Supervisor) handleProcessExit(event adapters.Event, backend string) {
	current := s.engine.MarkProcessExited(event.SessionID)
	key := makeEventKey(event.SessionID, event.ID)
	finished := eventAuditEntry(audit.KindSessionFinished, event, backend)
	finished.Outcome = audit.OutcomeFinished
	if event.Metadata["failed"] == "true" {
		finished.Outcome = audit.OutcomeFailed
	}
	finished.Reason = "process_exit"
	if !current {
		// A replacement already runs: this is the exit of the process before
		// it, which can be emitted after the replacement started. It is still
		// a finished session and is journaled with its real outcome, but under
		// a reason of its own: the session it ends is not the running one, and
		// the telemetry read as the replacement's end dropped the
		// replacement's pending prompts and counted it inactive. Showing the
		// agent stopped would hide a live process and offer to start a second
		// one.
		finished.Reason = audit.ReasonProcessExitStale
		_ = s.recordAudit(eventDetectedEntry(event, backend))
		_ = s.recordAudit(finished)
		s.mu.Lock()
		s.markResolvedLocked(key)
		s.mu.Unlock()
		return
	}
	// Lifecycle state still has to converge even when audit has failed, so the
	// result is deliberately ignored rather than short-circuiting the exit.
	_ = s.recordAudit(eventDetectedEntry(event, backend))
	_ = s.recordAudit(finished)

	s.mu.Lock()
	if _, duplicate := s.resolved[key]; duplicate {
		s.mu.Unlock()
		return
	}
	s.markResolvedLocked(key)
	index, found := s.agentIndex[strings.ToLower(event.SessionID)]
	if found {
		agent := &s.agents[index]
		agent.Running = false
		agent.Status = "exited"
		if event.Metadata["failed"] == "true" {
			agent.Status = "failed"
		}
		if value := strings.TrimSpace(event.Metadata["exit_code"]); value != "" {
			if code, err := strconv.Atoi(value); err == nil {
				agent.ExitCode = &code
			}
		}
	}
	s.clearSessionPendingLocked(event.SessionID)
	s.rebuildPendingLocked()
	status := "exited"
	if found {
		status = s.agents[index].Status
	}
	s.emitLocked(func(sink Sink) { sink.Refresh(event.SessionID) })
	s.showStatusLocked(Status{RunID: s.runID, Scope: "session", SessionID: event.SessionID, Status: status, ClearedBefore: clearedAll()})
	s.mu.Unlock()
	s.flush()
}

func (s *Supervisor) markSessionError(sessionID, reason string) {
	backend := s.backendFor(sessionID)
	_ = s.recordAudit(audit.Entry{
		Kind:       audit.KindBackendError,
		SessionID:  sessionID,
		AgentID:    sessionID,
		Backend:    backend,
		DecisionBy: audit.DecisionBySystem,
		Outcome:    audit.OutcomeFailed,
		Reason:     reason,
	})
	s.mu.Lock()
	if index, found := s.agentIndex[strings.ToLower(sessionID)]; found {
		s.agents[index].Status = "failed"
	}
	s.showStatusLocked(Status{RunID: s.runID, Scope: "session", SessionID: sessionID, Status: "failed"})
	s.emitSafeErrorLocked("backend_stream_failed", "The backend stream failed.", sessionID)
	s.mu.Unlock()
	s.flush()
}

func (s *Supervisor) markLegacyExit(sessionID string) {
	s.mu.Lock()
	if index, found := s.agentIndex[strings.ToLower(sessionID)]; found {
		s.agents[index].Status = "failed"
		s.agents[index].Running = false
	}
	s.clearSessionPendingLocked(sessionID)
	s.rebuildPendingLocked()
	s.showStatusLocked(Status{RunID: s.runID, Scope: "session", SessionID: sessionID, Status: "failed", ClearedBefore: clearedAll()})
	s.mu.Unlock()
	s.flush()
}
