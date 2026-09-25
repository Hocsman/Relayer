package supervise

import "strings"

// FreezeSession freezes a running session on its front end's word: the front
// end no longer knows what the agent's terminal holds, for instance because
// the screen could not be read back after the operator typed into it through
// a native attach. Nothing more is written to the session until a new process
// replaces it: no answer, the policy's or a person's, no line and no
// keystrokes (ErrDeliveryUncertain). A Stop is still taken, and the Start
// that follows it, or a Restart, clears the freeze with the rest of the
// previous process's state.
//
// The failure is shown by a fixed code, terminal_state_uncertain, and a fixed
// message; nothing is journaled, since nothing was written. Freezing a
// session already frozen changes nothing and shows nothing more, and an agent
// that is not running has nothing to freeze.
//
// Keeping the terminal's hand held would not do: a person's decision is taken
// whoever holds the hand (SetHolder), and the rule that refused it would live
// in one front end, out of the core's sight.
//
// FreezeSession refuses a run that drains (ErrRuntimeStopped), a run it is not
// addressed to (ErrRunStale) and an agent the run does not have
// (ErrAgentUnknown). It shows the failure on the caller's goroutine, as a
// Stop does: a front end must not call it under a lock its sink takes.
func (s *Supervisor) FreezeSession(runID, sessionID string) error {
	if err := s.activeRun(runID); err != nil {
		return err
	}
	sessionKey := strings.ToLower(strings.TrimSpace(sessionID))
	s.mu.Lock()
	if s.shuttingDown {
		// A drain freezes nothing, as it never did.
		s.mu.Unlock()
		return ErrRuntimeStopped
	}
	index, found := s.agentIndex[sessionKey]
	if !found {
		s.mu.Unlock()
		return ErrAgentUnknown
	}
	if !s.agents[index].Running || s.frozen[sessionKey] {
		s.mu.Unlock()
		return nil
	}
	s.frozen[sessionKey] = true
	s.agents[index].InputFrozen = true
	s.emitSafeErrorLocked("terminal_state_uncertain", "The terminal state is uncertain. The session is frozen until its agent is started again.", sessionKey)
	s.mu.Unlock()
	s.flush()
	return nil
}
