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
		// The gate is closed by a drain and by a failed journal alike: the
		// drain is named first, in the order above, as rawRefusalLocked
		// names it once the gate is passed.
		s.mu.RLock()
		failed := s.auditFailed && !s.shuttingDown
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
//
// The keystrokes count as an answer, for the repeat guard, to every prompt of
// the session they may have answered: those pending, and those still being
// taken in. Raw keystrokes never resolve the runtime's prompt, and nothing
// else tells the core that the holder answered one by typing; the repeat the
// agent's echo raises once the hand is released was the policy's to answer,
// a second answer. The core cannot tell which prompt, if any, they answered,
// and takes them for an answer to each: the cost is a question asked again
// within the window going to the operator, the guard's usual one.
func (s *Supervisor) finishRaw(sessionKey string) {
	now := s.now()
	s.mu.Lock()
	delete(s.rawInFlight, sessionKey)
	for key, item := range s.pending {
		if key.sessionID == sessionKey {
			s.recordAnsweredLocked(sessionKey, item.event.Signature, now)
		}
	}
	for key, taking := range s.ingesting {
		if key.sessionID == sessionKey {
			s.recordAnsweredLocked(sessionKey, taking.signature, now)
		}
	}
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
// Only a front end's own kinds are taken: who took or let go of a terminal
// (attach_started, attach_finished), how its control changed hands (the
// control_ kinds) and the lifecycle of a recording (the recording_ kinds).
// Any other is refused with ErrUnsupportedEntry before anything else, with
// nothing journaled: the core's own entries say what the core did, and a
// decision entry a front end wrote reset the policy's count of consecutive
// automatic decisions.
//
// RecordAudit never calls the sink on its caller's goroutine: the journal's
// failure is shown on another, which a drain waits for. A front end may call
// it under a lock of its own, one its sink takes included, and take the hand
// under the same lock once the entry is journaled. Shown on the caller, the
// failure deadlocked such a front end the first time the journal refused an
// entry, and never while it worked.
func (s *Supervisor) RecordAudit(entry audit.Entry) error {
	if !frontEndEntry(entry.Kind) {
		return ErrUnsupportedEntry
	}
	if s == nil {
		return ErrRuntimeStopped
	}
	s.mu.RLock()
	failed := s.auditFailed
	s.mu.RUnlock()
	if failed {
		return ErrAuditUnavailable
	}
	if err := s.engine.RecordAudit(entry); err != nil {
		s.freezeAudit(true)
		return ErrAuditUnavailable
	}
	return nil
}

// frontEndEntry reports whether a front end may journal an entry of kind
// through the core (RecordAudit).
func frontEndEntry(kind audit.Kind) bool {
	switch kind {
	case audit.KindAttachStarted, audit.KindAttachFinished,
		audit.KindControlRequested, audit.KindControlGranted, audit.KindControlDeclined,
		audit.KindControlReleased, audit.KindControlForced,
		audit.KindRecordingStarted, audit.KindRecordingFinished,
		audit.KindRecordingExported, audit.KindRecordingDeleted:
		return true
	}
	return false
}
