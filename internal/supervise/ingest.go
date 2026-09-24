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
	s.mu.Lock()
	if taking, ingesting := s.ingesting[key]; ingesting {
		if _, pending := s.pending[key]; !pending {
			// The prompt is still being taken in, by the event loop or by a
			// reconciliation on another goroutine: nothing is pending to
			// withdraw yet. The withdrawal is left for that goroutine, which
			// sets the prompt aside instead of making it pending and journals
			// the withdrawal after the entries that took it in. Found nothing
			// and done nothing, it left the prompt pending, waiting on the
			// operator for a question the agent no longer asked.
			taking.withdrawn = true
			s.ingesting[key] = taking
			s.mu.Unlock()
			return
		}
	}
	// A pending prompt is marked as going before its withdrawal is journaled,
	// under the lock where the hand's debt is read: from then on it is
	// nobody's to act on. Taking the hand leaves it alone, a person's answer
	// to it is refused as stale, and the policy does not claim it. The hand
	// taken while the withdrawal was journaled used to ask it and journal why
	// after the journal said it was gone, and a person's answer or the
	// policy's could be journaled, and written, after it too.
	if _, pending := s.pending[key]; pending {
		s.withdrawing[key] = struct{}{}
	}
	owed := s.heldEntries[key.sessionID]
	s.mu.Unlock()
	awaitHeldEntries(owed)
	_ = s.recordAudit(eventWithdrawnEntry(event, backend, "agent_withdrew_occurrence"))

	s.mu.Lock()
	delete(s.withdrawing, key)
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
	hand, reserved := s.reserveEvent(key, event.Signature)
	if !reserved {
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
	// A prompt raised while somebody holds the terminal, who may answer it by
	// typing, or took it since the prompt started being taken in, is asked,
	// never answered, and is journaled as asked: the guard applies before the
	// evaluation entry. So is a repeat of a prompt just answered. The hand is
	// asked first, so that its reason is the one given when both apply: the
	// holder's keystrokes count as an answer to the prompts they may have
	// answered, the prompt being taken in among them, which then repeats its
	// own Signature.
	policyEvaluation := s.engine.Evaluate(event)
	evaluation := s.guardHeldSince(key.sessionID, hand, policyEvaluation)
	evaluation = s.guardRepeat(key, event, evaluation)
	if !s.recordAudit(policyAuditEntry(event, backend, evaluation)) {
		s.addFrozenEvent(event, evaluation)
		return
	}

	view := supervisionView(s.runID, event, evaluation, "pending", s.engine.SupportedDecisions(event))
	isGuardrail := evaluation.Reason == policy.ReasonDestructive ||
		evaluation.Reason == policy.ReasonExfiltration ||
		evaluation.Reason == policy.ReasonGuardrailBlocked
	s.mu.Lock()
	if s.ingesting[key].withdrawn {
		// The agent withdrew the prompt while it was being taken in: it is
		// set aside, remembered so that a late copy is refused, and its
		// withdrawal journaled after the entries that took it in. It was
		// never shown, and nothing waits on it.
		s.markResolvedLocked(key)
		s.mu.Unlock()
		_ = s.recordAudit(eventWithdrawnEntry(event, backend, "agent_withdrew_occurrence"))
		return
	}
	// The hand may have been taken since the evaluation was journaled, while
	// the prompt was not yet among those SetHolder turns into asks: it is
	// asked now, and a second entry says why. So it is when the hand was
	// taken and released again meanwhile: the holder may have typed its
	// answer, and only who held the hand now was checked, which left it the
	// policy's.
	var previous, done chan struct{}
	heldSince := evaluation.Automatic && s.handTakenSinceLocked(key.sessionID, hand)
	if heldSince {
		evaluation = askEvaluation(evaluation, ReasonOperatorAttached)
		view.Evaluation = evaluationView(evaluation)
		previous, done = s.oweHeldEntriesLocked(key.sessionID)
	}
	// A prompt the policy denies, asked all the same, offers deny alone.
	denyOnly := !evaluation.Automatic && policyDenies(policyEvaluation)
	if denyOnly {
		view.Decisions = onlyDeny(view.Decisions)
	}
	item := pendingEvent{event: event.Clone(), view: view, evaluation: evaluation, denyOnly: denyOnly}
	s.pending[key] = item
	s.setAgentWaitingLocked(event.SessionID)
	s.rebuildPendingLocked()
	s.showPromptLocked(view)

	// A notice's details are the summary the prompt is shown with, never the
	// adapter's own: a notification leaves the machine, and a webhook posts
	// it as it is. The raw summary went out even for a prompt that asked for
	// a password.
	var notice *Notice
	if isGuardrail {
		notice = &Notice{
			AgentName: s.agentNameLocked(event),
			SessionID: event.SessionID,
			Reason:    "security guardrail blocked (" + evaluation.Reason + ")",
			EventID:   event.ID,
			Kind:      NoticeGuardrailBlocked,
			Severity:  SeverityCritical,
			Details:   view.Summary,
		}
	} else if !evaluation.Automatic {
		pending := s.pendingNoticeLocked(event, evaluation, view)
		notice = &pending
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

// pendingNoticeLocked is the notice that a prompt, shown as view, waits on a
// person. Its details are the summary the prompt is shown with.
func (s *Supervisor) pendingNoticeLocked(event adapters.Event, evaluation policy.Evaluation, view View) Notice {
	reason := "confirmation required"
	// The policy's reason for a sensitive prompt is ReasonSensitive; this
	// compared it with "sensitive", which it never is.
	if requiresSecretHandling(event) || evaluation.Reason == policy.ReasonSensitive {
		reason = "sensitive input required"
	}
	return Notice{
		AgentName: s.agentNameLocked(event),
		SessionID: event.SessionID,
		Reason:    reason,
		EventID:   event.ID,
		Kind:      NoticePendingDecision,
		Severity:  SeverityWarning,
		Details:   view.Summary,
	}
}

// agentNameLocked is the name of the agent that raised event.
func (s *Supervisor) agentNameLocked(event adapters.Event) string {
	if index, found := s.agentIndex[strings.ToLower(event.SessionID)]; found {
		return s.agents[index].Name
	}
	return event.AgentID
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

// reserveEvent reserves an event for the goroutine taking it in, unless the
// core knows it already, and returns the session's hand generation at that
// moment.
func (s *Supervisor) reserveEvent(key eventKey, signature string) (hand uint64, reserved bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.resolved[key]; exists {
		return 0, false
	}
	if _, exists := s.pending[key]; exists {
		return 0, false
	}
	if _, exists := s.ingesting[key]; exists {
		return 0, false
	}
	hand = s.handGenerations[key.sessionID]
	s.ingesting[key] = ingestion{hand: hand, signature: signature}
	return hand, true
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
	s.reportEndedLocked(event.SessionID)
	s.mu.Unlock()
	s.flush()
}

// reportEndedLocked queues the report that the session's current process
// ended, as emitLocked does. The web gateway announces the process's finished
// recording on it; it did so only when Relayer lost a tmux session, never when
// a process exited, so a recording's replay was offered only once a client
// reloaded its list.
func (s *Supervisor) reportEndedLocked(sessionID string) {
	s.emitLocked(func(sink Sink) { sink.Lifecycle(sessionID, PhaseEnded) })
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
	s.reportEndedLocked(sessionID)
	s.mu.Unlock()
	s.flush()
}
