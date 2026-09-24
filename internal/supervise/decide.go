package supervise

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
)

// scheduleAutomatic serialises every automatic decision for a session and
// never overtakes an earlier human prompt. The reservation and WaitGroup Add
// happen under s.mu so a drain cannot begin waiting between those steps.
func (s *Supervisor) scheduleAutomatic(sessionID string) {
	sessionKey := strings.ToLower(strings.TrimSpace(sessionID))
	if sessionKey == "" {
		return
	}
	s.mu.Lock()
	if s.shuttingDown || s.auditFailed || s.frozen[sessionKey] || s.stoppingSessions[sessionKey] {
		s.mu.Unlock()
		return
	}
	if _, busy := s.inFlight[sessionKey]; busy || s.lineInFlight[sessionKey] {
		s.mu.Unlock()
		return
	}
	if s.rawInFlight[sessionKey] {
		// The holder's keystrokes are being written, and the session takes
		// one write at a time: their release considers the prompt again.
		s.mu.Unlock()
		return
	}
	key, item, found := s.firstPendingForSessionLocked(sessionKey)
	if !found || !item.evaluation.Automatic || item.view.DeliveryStatus != "pending" {
		s.mu.Unlock()
		return
	}
	item.view.DeliveryStatus = "delivering"
	s.pending[key] = item
	s.inFlight[sessionKey] = writeClaim{key: key, signature: item.event.Signature}
	s.rebuildPendingLocked()
	s.showPromptLocked(item.view)
	event := item.event.Clone()
	evaluation := item.evaluation
	s.eventWG.Add(1)
	s.mu.Unlock()
	s.flush()
	go func() {
		defer s.eventWG.Done()
		s.applyAutomatic(key, event, evaluation)
	}()
}

func (s *Supervisor) firstPendingForSessionLocked(sessionKey string) (eventKey, pendingEvent, bool) {
	var selectedKey eventKey
	var selected pendingEvent
	found := false
	for key, item := range s.pending {
		if key.sessionID != sessionKey {
			continue
		}
		if !found || eventBefore(item.event, selected.event) {
			selectedKey, selected, found = key, item, true
		}
	}
	return selectedKey, selected, found
}

func eventBefore(left, right adapters.Event) bool {
	if left.Sequence != right.Sequence {
		return left.Sequence < right.Sequence
	}
	if !left.Timestamp.Equal(right.Timestamp) {
		return left.Timestamp.Before(right.Timestamp)
	}
	return left.ID < right.ID
}

// finishDecision releases the session's claim once a decision's write has
// returned, and only then considers the session's next automatic prompt. It
// always does: whether one may go is scheduleAutomatic's to decide, from the
// drain, the journal, the freezes, the session's other claims and the prompt
// itself. Deciding here instead, from how the write ended, missed the one
// case where nothing else asks: a prompt withdrawn while its answer was
// written leaves its claim to this write, and the scheduling its withdrawal
// asked for found the session still claimed.
func (s *Supervisor) finishDecision(key eventKey) {
	s.mu.Lock()
	if current, exists := s.inFlight[key.sessionID]; exists && current.key == key {
		delete(s.inFlight, key.sessionID)
	}
	s.mu.Unlock()
	s.scheduleAutomatic(key.sessionID)
}

func (s *Supervisor) applyAutomatic(key eventKey, event adapters.Event, evaluation policy.Evaluation) {
	defer s.finishDecision(key)
	decision, supported := adapterDecisionForPolicy(evaluation.Action)
	if !supported {
		s.fallbackToAsk(key, "fallback_unsupported", "")
		return
	}
	backend := s.backendFor(event.SessionID)
	// The policy decided when the prompt was detected, and the answer goes
	// only now that the session is free, possibly after other automatic
	// answers: a limit the policy enforces, such as its consecutive automatic
	// decisions, may have been reached since. Evaluate has no side effects,
	// so the policy is asked again just before the decision is journaled, and
	// the journal's decisions are what its limits count. A prompt it no longer
	// answers, or would now answer another way, is the operator's, and a
	// second evaluation entry says why. Otherwise the decision is journaled
	// under the evaluation the prompt was detected with, which the policy has
	// just confirmed: its rule is the one the first entry named. The repeat
	// guard is checked again with it: an answer written since the prompt was
	// detected may be the one it repeats. So is the hand: SetHolder leaves a
	// prompt whose answer claimed the session alone, and the policy used to
	// journal and write its answer once a hand taken meanwhile held the
	// terminal. Nothing can be typed while the claim holds the session's
	// write slot, so who holds the hand now is all there is to check.
	current := s.guardRepeat(key, event, s.guardHeld(key.sessionID, s.engine.Evaluate(event)))
	if !current.Automatic || current.Action != evaluation.Action {
		if !s.recordAudit(policyAuditEntry(event, backend, askEvaluation(current, current.Reason))) {
			s.markDelivery(key, "failed", "audit_unavailable")
			return
		}
		s.askOperator(key, &current, current.Reason, "")
		return
	}
	auditDecision := auditDecisionForPolicy(evaluation.Action)
	if !s.recordAudit(policyDecisionAuditEntry(event, backend, evaluation)) {
		s.markDelivery(key, "failed", "audit_unavailable")
		return
	}
	if !s.beginDelivery() {
		if !s.recordAudit(deliveryAuditEntry(
			event,
			backend,
			auditDecision,
			audit.DecisionByPolicy,
			audit.OutcomeCancelled,
			"runtime_stopped",
		)) {
			return
		}
		s.markDelivery(key, "failed", "runtime_stopped")
		return
	}
	defer s.endDelivery()
	ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
	err := s.engine.ApplyDecision(ctx, event.SessionID, event, decision, "")
	cancel()
	if err == nil {
		s.recordAnswered(key.sessionID, event.Signature)
		if !s.recordAudit(deliveryAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy, audit.OutcomeApplied, "delivery_applied")) {
			return
		}
		if s.pendingExists(key) {
			s.resolveEvent(key)
		}
		return
	}
	if errors.Is(err, adapters.ErrDecisionUnsupported) {
		if !s.recordAudit(deliveryAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy, audit.OutcomeFallbackUnsupported, "fallback_unsupported")) {
			return
		}
		if s.pendingExists(key) {
			s.fallbackToAsk(key, "fallback_unsupported", "")
		}
		return
	}
	if errors.Is(err, adapters.ErrEventMismatch) {
		if !s.recordAudit(deliveryAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy, audit.OutcomeFallbackStale, "fallback_stale")) {
			return
		}
		if s.pendingExists(key) {
			s.resolveEvent(key)
			s.reconcilePending(event.SessionID)
		}
		return
	}
	if !s.recordAudit(deliveryAuditEntry(event, backend, auditDecision, audit.DecisionByPolicy, audit.OutcomeFallbackDeliveryUncertain, "delivery_uncertain")) {
		return
	}
	s.freezeSession(key, "delivery_uncertain")
}

// fallbackToAsk hands a prompt to the operator: pending again, and no longer
// the policy's to answer. refused, when not empty, is an answer the adapter
// could not encode for it, which the prompt no longer offers. A prompt the
// operator tried to answer stays theirs even when the policy would have
// answered it: were it automatic again, the policy could send the very answer
// the operator had just tried to refuse.
func (s *Supervisor) fallbackToAsk(key eventKey, reason string, refused adapters.Decision) {
	s.askOperator(key, nil, reason, refused)
}

// askEvaluation is an evaluation made the operator's: nothing is answered
// automatically, for reason, and the policy's proposal and rule are kept.
func askEvaluation(evaluation policy.Evaluation, reason string) policy.Evaluation {
	evaluation.Action = policy.ActionAsk
	evaluation.Automatic = false
	evaluation.Reason = reason
	return evaluation
}

// askOperator is fallbackToAsk with, when current is not nil, the policy's
// evaluation of the prompt now in place of the one it was detected with: the
// prompt then shows the proposal and the rule the policy has now.
func (s *Supervisor) askOperator(key eventKey, current *policy.Evaluation, reason string, refused adapters.Decision) {
	s.mu.Lock()
	item, exists := s.pending[key]
	if !exists {
		s.mu.Unlock()
		return
	}
	evaluation := item.evaluation
	if current != nil {
		evaluation = *current
	}
	item.evaluation = askEvaluation(evaluation, reason)
	item.view.DeliveryStatus = "pending"
	item.view.Evaluation = evaluationView(item.evaluation)
	if refused != "" {
		// A new slice: the views already shown share the old one.
		offered := make([]string, 0, len(item.view.Decisions))
		for _, decision := range item.view.Decisions {
			if decision != string(refused) {
				offered = append(offered, decision)
			}
		}
		item.view.Decisions = offered
	}
	s.pending[key] = item
	s.rebuildPendingLocked()
	s.showPromptLocked(item.view)
	s.mu.Unlock()
	s.flush()
}

func (s *Supervisor) addFrozenEvent(event adapters.Event, evaluation policy.Evaluation) {
	key := makeEventKey(event.SessionID, event.ID)
	// A frozen event offers no answer at all: the session is already blocked on
	// an audit or delivery failure and nothing more may be sent to it.
	view := supervisionView(s.runID, event, evaluation, "failed", nil)
	s.mu.Lock()
	s.pending[key] = pendingEvent{event: event.Clone(), view: view, evaluation: evaluation}
	s.frozen[key.sessionID] = true
	if index, found := s.agentIndex[key.sessionID]; found {
		s.agents[index].InputFrozen = true
	}
	s.setAgentWaitingLocked(event.SessionID)
	s.rebuildPendingLocked()
	s.showPromptLocked(view)
	s.mu.Unlock()
	s.flush()
}

func (s *Supervisor) markDelivery(key eventKey, status, reason string) {
	s.mu.Lock()
	item, exists := s.pending[key]
	if !exists {
		s.mu.Unlock()
		return
	}
	item.view.DeliveryStatus = status
	item.view.Evaluation.Reason = reason
	s.pending[key] = item
	if status == "uncertain" || status == "failed" {
		s.frozen[key.sessionID] = true
		if index, found := s.agentIndex[key.sessionID]; found {
			s.agents[index].InputFrozen = true
		}
	}
	s.rebuildPendingLocked()
	s.showPromptLocked(item.view)
	s.mu.Unlock()
	s.flush()
}

// freezeSession freezes the session after a write whose outcome is unknown:
// part of the answer may have reached the agent, and nothing more is written
// to it until a new process replaces it. The session is frozen, and the
// failure shown, whatever became of the prompt the write answered; the prompt
// is shown uncertain only while it is still there. The freeze used to be set
// on the prompt's way to "uncertain", and a prompt the agent withdrew while
// its answer was being written, the moment an echo is most likely read as a
// new question, left the session writable: the next prompt's automatic answer
// followed the uncertain one, and a person was told the session was frozen
// when it was not.
func (s *Supervisor) freezeSession(key eventKey, reason string) {
	s.mu.Lock()
	s.frozen[key.sessionID] = true
	if index, found := s.agentIndex[key.sessionID]; found {
		s.agents[index].InputFrozen = true
	}
	if item, exists := s.pending[key]; exists {
		item.view.DeliveryStatus = "uncertain"
		item.view.Evaluation.Reason = reason
		s.pending[key] = item
		s.rebuildPendingLocked()
		s.showPromptLocked(item.view)
	}
	s.emitSafeErrorLocked("delivery_uncertain", "Delivery is indeterminate. The session is frozen to prevent a second answer.", key.sessionID)
	s.mu.Unlock()
	s.flush()
}

func (s *Supervisor) resolveEvent(key eventKey) {
	s.mu.Lock()
	item, exists := s.pending[key]
	status := "running"
	if exists {
		delete(s.pending, key)
		s.markResolvedLocked(key)
		if s.hasPendingForSessionLocked(key.sessionID) {
			s.setAgentWaitingLocked(key.sessionID)
			status = "waiting"
		} else {
			s.restoreAgentRunningLocked(key.sessionID)
		}
		s.rebuildPendingLocked()
		item.view.DeliveryStatus = "delivered"
		s.showPromptLocked(item.view)
		s.showStatusLocked(Status{RunID: s.runID, Scope: "session", SessionID: key.sessionID, Status: status})
	}
	s.mu.Unlock()
	s.flush()
}

func (s *Supervisor) hasPendingForSessionLocked(sessionKey string) bool {
	for key := range s.pending {
		if key.sessionID == sessionKey {
			return true
		}
	}
	return false
}

func (s *Supervisor) reconcilePending(sessionID string) {
	ctx, cancel := context.WithTimeout(s.ctx, 2*time.Second)
	pending, err := s.engine.PendingEvent(ctx, sessionID)
	cancel()
	if err != nil || pending == nil {
		return
	}
	s.handleAdapterEvent(pending.Clone())
}

// SubmitDecision relays a manual value to the exact canonical occurrence. The
// value is never copied into application state, events, errors or audit data.
// It is always sent as typed text, DecisionManual, and journaled ask: only the
// adapter knows what the bytes mean. actor is who typed it; a read-only one is
// refused before anything else, and an empty value before any claim or entry.
func (s *Supervisor) SubmitDecision(runID, sessionID, eventID, manualInput string, actor Actor) error {
	if actor.readOnly() {
		return ErrReadOnlyActor
	}
	if strings.TrimSpace(manualInput) == "" {
		return ErrEmptyDecision
	}
	return s.applyHumanDecision(runID, sessionID, eventID, adapters.DecisionManual, manualInput, actor)
}

// SubmitAutomaticDecision relays an answer the adapter encodes itself, so the
// operator does not have to know the keystroke a given CLI expects.
//
// The set of answers a given occurrence accepts is reported on the event, and
// the adapter is asked again here: a decision that arrived from a stale
// interface must be refused by the core rather than by the screen that offered
// it. Only allow and deny are answers here, and only when the adapter offers
// them for this occurrence and the prompt still offers them: an answer the
// core took off the prompt, one the adapter could not encode when it was
// tried, is not taken again. Anything else is ErrUnsupportedDecision, with
// nothing changed and nothing journaled. actor is who chose it; a read-only
// one is refused before anything else.
func (s *Supervisor) SubmitAutomaticDecision(runID, sessionID, eventID, decision string, actor Actor) error {
	if actor.readOnly() {
		return ErrReadOnlyActor
	}
	switch adapters.Decision(decision) {
	case adapters.DecisionAllow, adapters.DecisionDeny:
	default:
		return ErrUnsupportedDecision
	}
	if runErr := s.activeRun(runID); runErr != nil {
		return runErr
	}
	s.mu.RLock()
	item, exists := s.pending[makeEventKey(sessionID, eventID)]
	s.mu.RUnlock()
	if !exists {
		return ErrDecisionStale
	}
	offered := false
	for _, supported := range s.engine.SupportedDecisions(item.event) {
		if supported == adapters.Decision(decision) {
			offered = true
		}
	}
	// The adapter is asked what it can encode, and the prompt what is still
	// on offer: asking the adapter alone accepted an answer the core had
	// already refused, which was journaled and handed to the runtime again.
	if !offered || !offers(item.view, adapters.Decision(decision)) {
		return ErrUnsupportedDecision
	}
	return s.applyHumanDecision(runID, sessionID, eventID, adapters.Decision(decision), "", actor)
}

// offers reports whether the prompt offers decision among its answers.
func offers(view View, decision adapters.Decision) bool {
	for _, offered := range view.Decisions {
		if offered == string(decision) {
			return true
		}
	}
	return false
}

func (s *Supervisor) applyHumanDecision(
	runID, sessionID, eventID string,
	decision adapters.Decision,
	manualInput string,
	actor Actor,
) error {
	if runErr := s.activeRun(runID); runErr != nil {
		return runErr
	}
	key := makeEventKey(sessionID, eventID)
	if !s.beginDelivery() {
		s.mu.RLock()
		frozen := s.auditFailed || s.frozen[key.sessionID]
		s.mu.RUnlock()
		if frozen {
			return ErrDeliveryUncertain
		}
		return ErrRuntimeStopped
	}
	defer s.endDelivery()

	s.mu.Lock()
	item, exists := s.pending[key]
	frozen := s.frozen[key.sessionID] || s.auditFailed
	shuttingDown := s.shuttingDown
	if !exists {
		s.mu.Unlock()
		return ErrDecisionStale
	}
	if decision != adapters.DecisionManual && !offers(item.view, decision) {
		// The prompt stopped offering the answer since SubmitAutomaticDecision
		// read it: another person's attempt at it was refused meanwhile.
		s.mu.Unlock()
		return ErrUnsupportedDecision
	}
	if shuttingDown {
		s.mu.Unlock()
		return ErrRuntimeStopped
	}
	if s.stoppingSessions[key.sessionID] {
		s.mu.Unlock()
		return ErrRuntimeStopped
	}
	if frozen || item.view.DeliveryStatus == "uncertain" || item.view.DeliveryStatus == "failed" {
		s.mu.Unlock()
		return ErrDeliveryUncertain
	}
	if _, busy := s.inFlight[key.sessionID]; busy || s.lineInFlight[key.sessionID] || item.view.DeliveryStatus != "pending" {
		s.mu.Unlock()
		return ErrDecisionInFlight
	}
	if s.rawInFlight[key.sessionID] {
		s.mu.Unlock()
		return ErrDecisionInFlight
	}
	s.inFlight[key.sessionID] = writeClaim{key: key, signature: item.event.Signature}
	item.view.DeliveryStatus = "delivering"
	s.pending[key] = item
	s.rebuildPendingLocked()
	s.showPromptLocked(item.view)
	owed := s.heldEntries[key.sessionID]
	s.mu.Unlock()
	s.flush()
	defer s.finishDecision(key)

	backend := s.backendFor(sessionID)
	// The prompt may be one the hand just turned into an ask: the journal
	// says so before it says who answered it.
	awaitHeldEntries(owed)
	if !s.recordAudit(attributed(decisionAuditEntry(item.event, backend, humanAuditDecision(decision), audit.DecisionByHuman), actor)) {
		return ErrAuditUnavailable
	}
	ctx, cancel := context.WithTimeout(s.ctx, 8*time.Second)
	err := s.engine.ApplyDecision(ctx, sessionID, item.event, decision, manualInput)
	cancel()
	if err != nil {
		if errors.Is(err, adapters.ErrDecisionUnsupported) {
			// The adapter has no bytes for this answer. The runtime encodes an
			// answer before it writes any of it, so nothing reached the agent:
			// the delivery is not uncertain and freezing the session, as an
			// uncertain one does, left a prompt nobody could answer any more.
			// It goes back to the operator without the answer it cannot take.
			if !s.recordAudit(attributed(deliveryAuditEntry(item.event, backend, humanAuditDecision(decision), audit.DecisionByHuman, audit.OutcomeFallbackUnsupported, "fallback_unsupported"), actor)) {
				return ErrAuditUnavailable
			}
			s.fallbackToAsk(key, "fallback_unsupported", decision)
			return ErrUnsupportedDecision
		}
		if errors.Is(err, adapters.ErrEventMismatch) {
			if !s.recordAudit(attributed(deliveryAuditEntry(item.event, backend, humanAuditDecision(decision), audit.DecisionByHuman, audit.OutcomeFallbackStale, "fallback_stale"), actor)) {
				return ErrAuditUnavailable
			}
			s.resolveEvent(key)
			s.reconcilePending(sessionID)
			return ErrDecisionStale
		}
		if !s.recordAudit(attributed(deliveryAuditEntry(item.event, backend, humanAuditDecision(decision), audit.DecisionByHuman, audit.OutcomeFallbackDeliveryUncertain, "delivery_uncertain"), actor)) {
			return ErrAuditUnavailable
		}
		s.freezeSession(key, "delivery_uncertain")
		return ErrDeliveryUncertain
	}
	s.recordAnswered(key.sessionID, item.event.Signature)
	if !s.recordAudit(attributed(deliveryAuditEntry(item.event, backend, humanAuditDecision(decision), audit.DecisionByHuman, audit.OutcomeApplied, "delivery_applied"), actor)) {
		return ErrAuditUnavailable
	}
	s.resolveEvent(key)
	return nil
}
