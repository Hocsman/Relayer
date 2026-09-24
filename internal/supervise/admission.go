package supervise

import (
	"strings"
	"sync"

	"github.com/Hocsman/Relayer/internal/audit"
)

// AdmitRun admits one backend write the core does not make itself and no
// session's hand governs, such as a terminal resize, through the gate a drain
// closes and then waits on: no write of a run is still in progress when its
// runtime closes. It admits nothing once the run drains or its journal has
// failed. release must be called once, when the write has returned.
func (s *Supervisor) AdmitRun() (release func(), admitted bool) {
	if !s.beginDelivery() {
		return nil, false
	}
	return s.endDelivery, true
}

// Admit admits one raw write to a session's terminal: keystrokes its holder
// types, which reach the agent without the policy and are never journaled.
// They are bounded instead. Only the connection that holds the session's hand
// (SetHolder) may write, so every raw byte falls within a hand the front end
// journaled taking; a terminal nobody holds takes none. And the write takes
// the session's one write slot, the one a decision or a line takes: it is
// refused while one of those is being written, and while it is admitted they
// are refused (ErrDecisionInFlight) or, for the policy's answers, wait for it.
// Whatever the policy would answer is asked anyway while the hand is held,
// but the hand may be released while a write is still admitted.
//
// Admit refuses, in this order: a run that drains (ErrRuntimeStopped), a
// journal that failed (ErrAuditUnavailable), a connection that does not hold
// the hand (ErrNotHolder), a session frozen by an uncertain write
// (ErrDeliveryUncertain), a session whose process is stopped, stopping or
// starting (ErrLineUnavailable), and a session already being written to
// (ErrDecisionInFlight). It journals nothing and shows nothing.
//
// release must be called once the write has returned; calling it again does
// nothing. It frees the session's slot and, on another goroutine, considers
// the automatic answer that waited for it: neither Admit nor release calls
// the sink on the caller's goroutine. A drain waits for every admitted write.
func (s *Supervisor) Admit(sessionID, connID string) (release func(), err error) {
	if s == nil {
		return nil, ErrRuntimeStopped
	}
	sessionKey := strings.ToLower(strings.TrimSpace(sessionID))
	connID = strings.TrimSpace(connID)
	if !s.beginDelivery() {
		s.mu.RLock()
		failed := s.auditFailed
		s.mu.RUnlock()
		if failed {
			return nil, ErrAuditUnavailable
		}
		return nil, ErrRuntimeStopped
	}
	s.mu.Lock()
	refusal := s.rawRefusalLocked(sessionKey, connID)
	if refusal != nil {
		s.mu.Unlock()
		s.endDelivery()
		return nil, refusal
	}
	s.rawInFlight[sessionKey] = true
	s.mu.Unlock()
	var once sync.Once
	return func() { once.Do(func() { s.finishRaw(sessionKey) }) }, nil
}

// rawRefusalLocked is why a raw write by connID may not be admitted on the
// session now, or nil.
func (s *Supervisor) rawRefusalLocked(sessionKey, connID string) error {
	if s.shuttingDown {
		return ErrRuntimeStopped
	}
	if s.auditFailed {
		return ErrAuditUnavailable
	}
	if holder := s.holders[sessionKey]; holder == "" || holder != connID {
		return ErrNotHolder
	}
	if s.frozen[sessionKey] {
		return ErrDeliveryUncertain
	}
	// stoppingSessions marks a Stop, a Start or a Restart for as long as it
	// runs; a Start begins on a process that is not running.
	index, found := s.agentIndex[sessionKey]
	if !found || !s.agents[index].Running || s.stoppingSessions[sessionKey] {
		return ErrLineUnavailable
	}
	if _, busy := s.inFlight[sessionKey]; busy || s.lineInFlight[sessionKey] || s.rawInFlight[sessionKey] {
		return ErrDecisionInFlight
	}
	return nil
}

// finishRaw frees the session's write slot once a raw write has returned,
// and considers on another goroutine the automatic answer that waited for it.
// The goroutine is counted before the write's admission is released, so a
// drain waits for it too.
func (s *Supervisor) finishRaw(sessionKey string) {
	s.mu.Lock()
	delete(s.rawInFlight, sessionKey)
	s.eventWG.Add(1)
	s.mu.Unlock()
	go func() {
		defer s.eventWG.Done()
		s.scheduleAutomatic(sessionKey)
	}()
	s.endDelivery()
}

// RecordAudit journals an entry the core does not write itself, such as the
// gateway's attach_started, through the path the core's own entries take: an
// entry the journal refuses freezes the run, as the refusal of any of the
// core's does, and the caller gets ErrAuditUnavailable. Once the journal has
// failed, nothing more is written and every entry gets ErrAuditUnavailable.
// A front end that must not act unless its record is journaled, such as
// handing over a terminal, acts only once RecordAudit returned nil.
//
// Freezing the run shows the journal's failure: like any operation that
// changes the core, RecordAudit may call the sink before it returns.
func (s *Supervisor) RecordAudit(entry audit.Entry) error {
	if s == nil {
		return ErrRuntimeStopped
	}
	s.mu.RLock()
	failed := s.auditFailed
	s.mu.RUnlock()
	if failed || !s.recordAudit(entry) {
		return ErrAuditUnavailable
	}
	return nil
}
