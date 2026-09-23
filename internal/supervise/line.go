package supervise

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/terminal"
)

// SubmitLine sends one ordinary application line to a detached, running
// session. The line crosses this method only as a call argument: it is never
// copied into core state, events, errors or audit entries.
func (s *Supervisor) SubmitLine(runID, sessionID, line string) error {
	if err := s.activeRun(runID); err != nil {
		return err
	}
	// Delivery admission must precede the per-session claim so lifecycle
	// shutdown cannot start waiting between those two operations.
	if !s.beginDelivery() {
		s.mu.RLock()
		frozen := s.auditFailed || s.frozen[strings.ToLower(strings.TrimSpace(sessionID))]
		s.mu.RUnlock()
		if frozen {
			return ErrDeliveryUncertain
		}
		return ErrRuntimeStopped
	}
	defer s.endDelivery()

	sessionKey := strings.ToLower(strings.TrimSpace(sessionID))
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return ErrRuntimeStopped
	}
	if s.auditFailed {
		s.mu.Unlock()
		return ErrAuditUnavailable
	}
	if s.frozen[sessionKey] {
		s.mu.Unlock()
		return ErrDeliveryUncertain
	}
	index, found := s.agentIndex[sessionKey]
	if !found {
		s.mu.Unlock()
		return ErrLineUnavailable
	}
	agent := s.agents[index]
	if s.stoppingSessions[sessionKey] {
		s.mu.Unlock()
		return ErrLineUnavailable
	}
	if _, busy := s.inFlight[sessionKey]; busy || s.hasPendingForSessionLocked(sessionKey) {
		s.mu.Unlock()
		s.reconcilePending(sessionID)
		s.emitSafeError("line_prompt_pending", "Answer the supervision request before sending a line.", sessionID)
		return ErrLinePromptPending
	}
	if !agent.Running || agent.Attached || (agent.Status != "running" && agent.Status != "detached") {
		s.mu.Unlock()
		return ErrLineUnavailable
	}
	if s.lineInFlight[sessionKey] {
		s.mu.Unlock()
		return ErrLineInFlight
	}
	s.lineInFlight[sessionKey] = true
	s.mu.Unlock()
	defer s.finishLine(sessionKey)

	if !s.recordAudit(operatorInputAuditEntry(agent, audit.OutcomeInFlight, "operator_input_started")) {
		return ErrAuditUnavailable
	}
	ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
	err := s.engine.SendLine(ctx, sessionID, line)
	line = ""
	cancel()
	if err == nil {
		if !s.recordAudit(operatorInputAuditEntry(agent, audit.OutcomeApplied, "operator_input_applied")) {
			return ErrAuditUnavailable
		}
		return nil
	}

	switch {
	case errors.Is(err, terminal.ErrEventPending):
		if !s.recordAudit(operatorInputAuditEntry(agent, audit.OutcomeFallbackStale, "operator_input_prompt_pending")) {
			return ErrAuditUnavailable
		}
		s.reconcilePending(sessionID)
		s.emitSafeError("line_prompt_pending", "A supervision request arrived before the line. No free text was sent.", sessionID)
		return ErrLinePromptPending
	case errors.Is(err, terminal.ErrInvalidLine):
		if !s.recordAudit(operatorInputAuditEntry(agent, audit.OutcomeSkipped, "operator_input_invalid")) {
			return ErrAuditUnavailable
		}
		s.emitSafeError("line_invalid", "The input must be a single UTF-8 line, with no control characters and no more than 4096 bytes.", sessionID)
		return ErrLineInvalid
	case errors.Is(err, terminal.ErrLineUnsupported):
		if !s.recordAudit(operatorInputAuditEntry(agent, audit.OutcomeSkipped, "operator_input_unsupported")) {
			return ErrAuditUnavailable
		}
		s.emitSafeError("line_unsupported", "This backend cannot send a line reliably.", sessionID)
		return ErrLineUnsupported
	case errors.Is(err, terminal.ErrClosed):
		if !s.recordAudit(operatorInputAuditEntry(agent, audit.OutcomeSkipped, "operator_input_session_unavailable")) {
			return ErrAuditUnavailable
		}
		s.markLineSessionUnavailable(sessionKey, "exited")
		s.emitSafeError("line_session_unavailable", "The session ended before delivery. No line was sent.", sessionID)
		return ErrLineUnavailable
	case errors.Is(err, terminal.ErrSessionNotFound):
		if !s.recordAudit(operatorInputAuditEntry(agent, audit.OutcomeSkipped, "operator_input_session_unavailable")) {
			return ErrAuditUnavailable
		}
		s.markLineSessionUnavailable(sessionKey, "failed")
		s.emitSafeError("line_session_unavailable", "The session is no longer available. No line was sent.", sessionID)
		return ErrLineUnavailable
	default:
		if !s.recordAudit(operatorInputAuditEntry(agent, audit.OutcomeFallbackDeliveryUncertain, "operator_input_delivery_uncertain")) {
			return ErrAuditUnavailable
		}
		s.freezeLineSession(sessionKey)
		return ErrDeliveryUncertain
	}
}

func (s *Supervisor) finishLine(sessionKey string) {
	s.mu.Lock()
	delete(s.lineInFlight, sessionKey)
	advance := !s.shuttingDown
	if index, found := s.agentIndex[sessionKey]; !found || !s.agents[index].Running {
		advance = false
	}
	s.mu.Unlock()
	if advance && s.isActiveRun() {
		s.scheduleAutomatic(sessionKey)
	}
}

func (s *Supervisor) freezeLineSession(sessionKey string) {
	s.mu.Lock()
	s.frozen[sessionKey] = true
	displaySessionID := sessionKey
	if index, found := s.agentIndex[sessionKey]; found {
		s.agents[index].InputFrozen = true
		displaySessionID = s.agents[index].SessionID
	}
	s.mu.Unlock()
	s.emitSafeError("delivery_uncertain", "Delivery is indeterminate. The session is frozen to prevent another send.", displaySessionID)
}

func (s *Supervisor) markLineSessionUnavailable(sessionKey, status string) {
	s.mu.Lock()
	displaySessionID := sessionKey
	if index, found := s.agentIndex[sessionKey]; found {
		agent := &s.agents[index]
		agent.Running = false
		agent.Attached = false
		agent.Status = status
		displaySessionID = agent.SessionID
	}
	s.mu.Unlock()
	s.sink.Status(Status{RunID: s.runID, Scope: "session", SessionID: displaySessionID, Status: status})
}
