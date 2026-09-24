package supervise

import (
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/policy"
)

// guardRepeat is the policy's evaluation of a prompt, unless the policy would
// answer it automatically and it repeats a prompt of its session whose answer
// is being written, or was written less than the run's RepeatWindow ago. The
// policy then does not answer it: the operator is asked, for the reason
// repeat_after_delivery, with the policy's proposal and rule kept. The guard
// applies before the prompt's evaluation is journaled, and again just before
// the policy's decision: a repeat queued behind the prompt it repeats was none
// when it was detected.
//
// A repeat is known by its Signature, which an adapter derives from the
// question itself. An adapter that reads an answered question again, from the
// answer's echo or a repaint, raises it under a new ID but the same Signature,
// and answering it typed a second answer into an agent that had consumed the
// first. The adapters are where that is fixed; this is the backstop. It costs
// a genuine question asked twice within the window a trip to the operator,
// which is the safe way to be wrong. A prompt without a Signature repeats
// nothing.
func (s *Supervisor) guardRepeat(key eventKey, event adapters.Event, evaluation policy.Evaluation) policy.Evaluation {
	if !evaluation.Automatic || event.Signature == "" {
		return evaluation
	}
	now := s.now()
	s.mu.RLock()
	repeat := s.repeatsLocked(key, event.Signature, now)
	s.mu.RUnlock()
	if !repeat {
		return evaluation
	}
	return askEvaluation(evaluation, ReasonRepeatAfterDelivery)
}

// repeatsLocked reports whether the prompt key, with signature, repeats the
// prompt whose answer holds its session, or one answered within the window.
func (s *Supervisor) repeatsLocked(key eventKey, signature string, now time.Time) bool {
	if claim, writing := s.inFlight[key.sessionID]; writing && claim.key != key && claim.signature == signature {
		return true
	}
	answeredAt, found := s.answered[key.sessionID][signature]
	return found && now.Sub(answeredAt) < s.repeatWindow
}

// recordAnswered notes that an answer to a prompt with signature was just
// written to the session, whoever gave it. What the window no longer covers
// is forgotten.
func (s *Supervisor) recordAnswered(sessionKey, signature string) {
	if signature == "" {
		return
	}
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	answered := s.answered[sessionKey]
	if answered == nil {
		answered = make(map[string]time.Time)
		s.answered[sessionKey] = answered
	}
	for other, answeredAt := range answered {
		if now.Sub(answeredAt) >= s.repeatWindow {
			delete(answered, other)
		}
	}
	answered[signature] = now
}
