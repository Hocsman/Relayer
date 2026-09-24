package supervise

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/session"
)

// StopSession strictly stops one agent's process while its siblings keep
// running. Its prompts stay until the process's exit arrives. It is refused
// while anything is being written to the session (ErrLineInFlight for a
// line, ErrDecisionInFlight for an answer or the holder's keystrokes): the
// write's outcome would be lost with the process. Keystrokes are refused like
// an answer rather than cut short, although a Stop takes nothing more to the
// agent: they are bounded and their write returns at once, and the session's
// one write slot is simpler to reason about when nothing overtakes it.
func (s *Supervisor) StopSession(runID, sessionID string) error {
	if err := s.activeRun(runID); err != nil {
		return err
	}
	if !s.beginDelivery() {
		return ErrRuntimeStopped
	}
	defer s.endDelivery()
	sessionKey := strings.ToLower(strings.TrimSpace(sessionID))
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return ErrRuntimeStopped
	}
	if s.lineInFlight[sessionKey] {
		s.mu.Unlock()
		return ErrLineInFlight
	}
	if _, busy := s.inFlight[sessionKey]; busy || s.rawInFlight[sessionKey] {
		s.mu.Unlock()
		return ErrDecisionInFlight
	}
	if s.stoppingSessions[sessionKey] {
		s.mu.Unlock()
		return ErrLineUnavailable
	}
	index, found := s.agentIndex[sessionKey]
	if !found || !s.agents[index].Running {
		s.mu.Unlock()
		return ErrLineUnavailable
	}
	s.stoppingSessions[sessionKey] = true
	s.agents[index].Status = "stopping"
	s.showStatusLocked(Status{RunID: s.runID, Scope: "session", SessionID: s.agents[index].SessionID, Status: "stopping"})
	s.mu.Unlock()
	s.flush()
	ctx, cancel := context.WithTimeout(s.ctx, session.StopBudget+2*time.Second)
	err := s.engine.StopAgent(ctx, sessionID)
	cancel()
	if err != nil {
		s.mu.Lock()
		delete(s.stoppingSessions, sessionKey)
		s.mu.Unlock()
		s.freezeLineSession(sessionKey)
		s.emitSafeError("stop_failed", "session could not be stopped cleanly", sessionID)
		return errors.New("session could not be stopped cleanly")
	}
	s.mu.Lock()
	delete(s.stoppingSessions, sessionKey)
	s.mu.Unlock()
	s.markLineSessionUnavailable(sessionKey, "exited")
	return nil
}

// StartSession launches a fresh process for one stopped or exited agent under
// its unchanged identity and plan specification, without interrupting the
// other agents of the run. It is refused (ErrDecisionInFlight) while a write
// to the previous process has not returned, an answer, a line or the
// holder's keystrokes: what it still sends would reach the replacement, and
// the replacement's first answer would be written beside it. A process that
// exits during a write leaves the session to that write, which releases it
// when it returns.
func (s *Supervisor) StartSession(runID, sessionID string) error {
	if err := s.activeRun(runID); err != nil {
		return err
	}
	if !s.beginDelivery() {
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
	if s.stoppingSessions[sessionKey] {
		s.mu.Unlock()
		return ErrLineUnavailable
	}
	index, found := s.agentIndex[sessionKey]
	if !found {
		s.mu.Unlock()
		return ErrAgentUnknown
	}
	if s.agents[index].Running {
		s.mu.Unlock()
		return ErrAgentStillRunning
	}
	if _, busy := s.inFlight[sessionKey]; busy || s.lineInFlight[sessionKey] || s.rawInFlight[sessionKey] {
		s.mu.Unlock()
		return ErrDecisionInFlight
	}
	s.stoppingSessions[sessionKey] = true
	s.startingSessions[sessionKey] = true
	s.agents[index].Status = "starting"
	s.dropSessionPendingLocked(sessionKey)
	s.showStatusLocked(Status{RunID: s.runID, Scope: "session", SessionID: s.agents[index].SessionID, Status: "starting", ClearedBefore: clearedAll()})
	s.mu.Unlock()
	s.flush()
	startedAt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(s.ctx, 10*time.Second)
	err := s.engine.StartAgent(ctx, sessionID)
	cancel()
	if err != nil {
		s.mu.Lock()
		delete(s.stoppingSessions, sessionKey)
		delete(s.startingSessions, sessionKey)
		if index, found := s.agentIndex[sessionKey]; found {
			s.agents[index].Status = "failed"
		}
		s.showStatusLocked(Status{RunID: s.runID, Scope: "session", SessionID: sessionID, Status: "failed"})
		s.emitSafeErrorLocked("start_failed", "session could not be started", sessionID)
		s.mu.Unlock()
		s.flush()
		return errors.New("session could not be started")
	}
	s.completeAgentStart(sessionKey, startedAt)
	return nil
}

// RestartSession transactionally stops then starts one agent in place. An
// unconfirmed stop never produces a replacement process; a failed start
// leaves the agent down with its identity locked for an explicit retry. It is
// refused, as a Stop is, while anything is being written to the session: the
// old process's keystrokes could otherwise reach its replacement.
func (s *Supervisor) RestartSession(runID, sessionID string) error {
	if err := s.activeRun(runID); err != nil {
		return err
	}
	if !s.beginDelivery() {
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
	if s.lineInFlight[sessionKey] {
		s.mu.Unlock()
		return ErrLineInFlight
	}
	if _, busy := s.inFlight[sessionKey]; busy || s.rawInFlight[sessionKey] {
		s.mu.Unlock()
		return ErrDecisionInFlight
	}
	if s.stoppingSessions[sessionKey] {
		s.mu.Unlock()
		return ErrLineUnavailable
	}
	index, found := s.agentIndex[sessionKey]
	if !found {
		s.mu.Unlock()
		return ErrAgentUnknown
	}
	s.stoppingSessions[sessionKey] = true
	s.startingSessions[sessionKey] = true
	s.agents[index].Status = "stopping"
	s.dropSessionPendingLocked(sessionKey)
	s.showStatusLocked(Status{RunID: s.runID, Scope: "session", SessionID: s.agents[index].SessionID, Status: "stopping", ClearedBefore: clearedAll()})
	s.mu.Unlock()
	s.flush()
	startedAt := time.Now().UTC()
	ctx, cancel := context.WithTimeout(s.ctx, session.StopBudget+8*time.Second)
	err := s.engine.RestartAgent(ctx, sessionID)
	cancel()
	if err != nil {
		s.mu.Lock()
		delete(s.stoppingSessions, sessionKey)
		delete(s.startingSessions, sessionKey)
		if index, found := s.agentIndex[sessionKey]; found {
			s.agents[index].Status = "failed"
		}
		s.showStatusLocked(Status{RunID: s.runID, Scope: "session", SessionID: sessionID, Status: "failed"})
		s.mu.Unlock()
		s.flush()
		s.freezeLineSession(sessionKey)
		s.emitSafeError("restart_failed", "session could not be restarted cleanly", sessionID)
		return errors.New("session could not be restarted cleanly")
	}
	s.completeAgentStart(sessionKey, startedAt)
	return nil
}

// completeAgentStart republishes a freshly started agent: freeze and exit
// state belong to the previous process instance and never carry over, and
// neither does its output, which the front end resets on PhaseStarted.
// completeAgentStart marks the agent running. The previous process's prompts
// are dropped: an exit that arrives during the start is the previous
// process's and is set aside as stale, and its prompts used to stay pending,
// blocking the new process's automatic decisions and hiding its first prompt,
// which carries the same ID. Only prompts detected before startedAt go; the
// new process may already have raised one.
func (s *Supervisor) completeAgentStart(sessionKey string, startedAt time.Time) {
	// The front end drops the previous process's output before the core shows
	// the new one running. The desktop reset the output and marked the agent
	// running under one lock; the core's state and the front end's output are
	// now under two, and a front end reading between the two steps showed the
	// new process running with its predecessor's output. In this order a reader
	// between them sees the agent still starting, or stopping for a Restart,
	// with no output yet, which is true. A session's identity never changes
	// after New, so reading it first is safe. The report is made, not only
	// queued, before the agent is marked running: flush returns once every
	// call queued before it was made.
	s.mu.Lock()
	displaySessionID := ""
	if index, found := s.agentIndex[sessionKey]; found {
		displaySessionID = s.agents[index].SessionID
		s.emitLocked(func(sink Sink) { sink.Lifecycle(displaySessionID, PhaseStarted) })
	}
	s.mu.Unlock()
	s.flush()
	s.mu.Lock()
	delete(s.stoppingSessions, sessionKey)
	delete(s.startingSessions, sessionKey)
	delete(s.frozen, sessionKey)
	// Rounded to the millisecond, the resolution clients compare at, so the
	// interface drops exactly the prompts dropped here.
	bound := startedAt.Truncate(time.Millisecond)
	for key, item := range s.pending {
		if key.sessionID == sessionKey && promptDetectedAt(item).Before(bound) {
			delete(s.pending, key)
		}
	}
	s.rebuildPendingLocked()
	// The previous process's answered prompts and its exit stay resolved: a
	// new process gives every event an ID of its own, so nothing of the
	// replacement's is mistaken for them, and a late copy of an old one is
	// still refused.
	//
	// The status is derived from what was kept: a prompt the new process
	// raised while it started waits on the operator, and showing the agent
	// running hid it.
	status := "running"
	if s.hasPendingForSessionLocked(sessionKey) {
		status = "waiting"
	}
	if index, found := s.agentIndex[sessionKey]; found {
		agent := &s.agents[index]
		agent.Running = true
		agent.Status = status
		agent.ExitCode = nil
		agent.InputFrozen = false
	}
	if displaySessionID != "" {
		s.showStatusLocked(Status{RunID: s.runID, Scope: "session", SessionID: displaySessionID, Status: status, ClearedBefore: bound.Format(time.RFC3339Nano)})
		s.emitLocked(func(sink Sink) { sink.Refresh(displaySessionID) })
	}
	s.mu.Unlock()
	s.flush()
	if displaySessionID == "" {
		return
	}
	// A prompt the replacement raised while it was starting waited: no
	// automatic decision is taken while a session is changing.
	s.scheduleAutomatic(displaySessionID)
}

// promptDetectedAt is when a prompt was detected. An adapter stamps every
// event, but the view falls back to the time it was received, and the
// interface compares that: taking a zero timestamp as older than any start
// dropped such a prompt here while the interface kept showing it.
func promptDetectedAt(item pendingEvent) time.Time {
	if !item.event.Timestamp.IsZero() {
		return item.event.Timestamp
	}
	detected, _ := time.Parse(time.RFC3339Nano, item.view.Timestamp)
	return detected
}

// clearedAll is the ClearedBefore of a status that dropped every prompt of the
// session: a millisecond past now, the resolution clients compare at.
func clearedAll() string {
	return time.Now().UTC().Truncate(time.Millisecond).Add(time.Millisecond).Format(time.RFC3339Nano)
}

// dropSessionPendingLocked removes a session's prompts when a Start or a
// Restart begins: none of them can be answered while it runs, and left
// pending they blocked the replacement's automatic decisions.
func (s *Supervisor) dropSessionPendingLocked(sessionKey string) {
	for key := range s.pending {
		if key.sessionID == sessionKey {
			delete(s.pending, key)
		}
	}
	s.rebuildPendingLocked()
}

// clearSessionPendingLocked drops the prompts of a session whose process
// ended, and remembers them so a late copy is refused. It leaves the
// session's write claim alone: a write still in progress holds the session
// until it returns (finishDecision), whatever the process did meanwhile.
// Released here, the claim let a Start, and its replacement's first automatic
// answer, go while the write had not returned, and lost the Signature the
// repeat guard reads from it.
func (s *Supervisor) clearSessionPendingLocked(sessionID string) {
	normalized := strings.ToLower(strings.TrimSpace(sessionID))
	for key := range s.pending {
		if key.sessionID == normalized {
			delete(s.pending, key)
			s.markResolvedLocked(key)
		}
	}
}
