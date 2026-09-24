package supervise

import (
	"sort"
	"strings"

	"github.com/Hocsman/Relayer/internal/policy"
)

// ReasonOperatorAttached is the reason of a prompt the policy would have
// answered automatically on a session whose terminal someone holds: it is
// asked, never answered, and it stays asked once the terminal is released.
//
// The holder types into the agent directly, and raw keystrokes never reach
// the runtime's pending prompt: an answer typed by hand leaves the prompt
// pending under the same ID, and an automatic answer to it would type a
// second one. Only the core's own writes resolve a prompt, so a prompt the
// holder may have answered is never the policy's again.
const ReasonOperatorAttached = "operator_attached"

// SetHolder records which connection holds the session's terminal, the hand:
// connID, or nobody when connID is empty. The hand is the front end's; the
// core changes it only when told, whatever the session's process does, so
// the two always agree on who holds it. Agent.Attached reports that somebody
// does.
//
// While somebody holds the hand:
//   - every prompt of the session the policy would answer automatically, and
//     whose answer is not already being written, is asked instead, for the
//     reason operator_attached, the policy's proposal and rule kept. Each is
//     journaled as a second policy_evaluated entry, ask, and shown again;
//   - a prompt the session raises is asked the same way before its evaluation
//     is journaled, and is notified like any prompt that waits on a person;
//   - a prompt that was being taken in when the hand was taken is asked the
//     same way, even when the hand was released again before the prompt was
//     pending: its holder may have typed the answer meanwhile;
//   - a prompt whose answer the policy already claimed is checked for the
//     hand again just before the policy's decision is journaled, and asked if
//     somebody holds it then; an answer already journaled goes on;
//   - a line is refused (ErrLineUnavailable): it would interleave with the
//     holder's keystrokes;
//   - a person's decision is still accepted, from anyone who may act: the
//     hand governs the terminal, not supervision.
//
// Releasing the hand never makes a prompt automatic again: the holder may
// have answered it by typing, which the runtime never learns. A prompt the
// session raises after the release is the policy's as usual.
//
// SetHolder does not block: it changes the state at once, so that no
// automatic answer starts once it returns, and journals and shows what it
// changed on another goroutine, never on the caller's. A front end may call
// it while it holds a lock of its own, including one its sink takes.
func (s *Supervisor) SetHolder(sessionID, connID string) {
	if s == nil {
		return
	}
	sessionKey := strings.ToLower(strings.TrimSpace(sessionID))
	connID = strings.TrimSpace(connID)
	s.mu.Lock()
	defer s.mu.Unlock()
	index, found := s.agentIndex[sessionKey]
	if !found {
		return
	}
	if connID == "" {
		delete(s.holders, sessionKey)
		s.agents[index].Attached = false
		return
	}
	s.holders[sessionKey] = connID
	s.handGenerations[sessionKey]++
	s.agents[index].Attached = true
	if s.shuttingDown {
		// A drain answers nothing more, and a goroutine started now could
		// outlive Wait.
		return
	}
	var asked []pendingEvent
	for key, item := range s.pending {
		if key.sessionID != sessionKey || !item.evaluation.Automatic || item.view.DeliveryStatus != "pending" {
			continue
		}
		if policyDenies(item.evaluation) {
			// What the policy denies, the operator may only deny.
			item.view.Decisions = onlyDeny(item.view.Decisions)
		}
		item.evaluation = askEvaluation(item.evaluation, ReasonOperatorAttached)
		item.view.Evaluation = evaluationView(item.evaluation)
		s.pending[key] = item
		asked = append(asked, item)
	}
	if len(asked) == 0 {
		return
	}
	sort.Slice(asked, func(left, right int) bool { return eventBefore(asked[left].event, asked[right].event) })
	s.rebuildPendingLocked()
	for _, item := range asked {
		s.showPromptLocked(item.view)
	}
	previous, done := s.oweHeldEntriesLocked(sessionKey)
	s.eventWG.Add(1)
	go func() {
		defer s.eventWG.Done()
		s.journalHeldEntries(sessionKey, asked, previous, done)
		s.flush()
	}()
}

// guardHeld is the policy's evaluation of a prompt of a session, unless the
// policy would answer it automatically and somebody holds the session's
// terminal: it is then the operator's, for the reason operator_attached.
func (s *Supervisor) guardHeld(sessionKey string, evaluation policy.Evaluation) policy.Evaluation {
	if !evaluation.Automatic {
		return evaluation
	}
	s.mu.RLock()
	held := s.holders[sessionKey] != ""
	s.mu.RUnlock()
	if !held {
		return evaluation
	}
	return askEvaluation(evaluation, ReasonOperatorAttached)
}

// guardHeldSince is guardHeld for a prompt being taken in, which started when
// the session's hand generation was hand: a hand taken since is taken into
// account even when it was released again, since its holder may have typed
// the prompt's answer. Only who held the hand at the end used to count, and a
// hand taken, typed into and released while the prompt's entries were
// journaled left the prompt the policy's.
func (s *Supervisor) guardHeldSince(sessionKey string, hand uint64, evaluation policy.Evaluation) policy.Evaluation {
	if !evaluation.Automatic {
		return evaluation
	}
	s.mu.RLock()
	taken := s.handTakenSinceLocked(sessionKey, hand)
	s.mu.RUnlock()
	if !taken {
		return evaluation
	}
	return askEvaluation(evaluation, ReasonOperatorAttached)
}

// handTakenSinceLocked reports whether somebody holds the session's hand, or
// took it since the session's hand generation was hand.
func (s *Supervisor) handTakenSinceLocked(sessionKey string, hand uint64) bool {
	return s.holders[sessionKey] != "" || s.handGenerations[sessionKey] != hand
}

// oweHeldEntriesLocked records that the evaluation entries of prompts the hand
// turned into asks are about to be journaled for the session, off the lock.
// done is closed once they are; previous, when not nil, is the channel of the
// entries owed before them, which are journaled first.
//
// A person may answer, and the agent withdraw, such a prompt the moment it is
// shown asked, and those entries are journaled on other goroutines: each
// waits for done (awaitHeldEntries), so the journal says why the prompt was
// asked before it says how it was answered or that it went away.
func (s *Supervisor) oweHeldEntriesLocked(sessionKey string) (previous, done chan struct{}) {
	previous = s.heldEntries[sessionKey]
	done = make(chan struct{})
	s.heldEntries[sessionKey] = done
	return previous, done
}

// journalHeldEntries journals the second evaluation entry of each prompt the
// hand turned into an ask, after those owed before them, then settles the
// debt. A journal that fails freezes the run, as any entry's failure does.
func (s *Supervisor) journalHeldEntries(sessionKey string, asked []pendingEvent, previous, done chan struct{}) {
	if previous != nil {
		<-previous
	}
	for _, item := range asked {
		if !s.recordAudit(policyAuditEntry(item.event, s.backendFor(item.event.SessionID), item.evaluation)) {
			break
		}
	}
	s.mu.Lock()
	if s.heldEntries[sessionKey] == done {
		delete(s.heldEntries, sessionKey)
	}
	s.mu.Unlock()
	close(done)
}

// awaitHeldEntries waits, when owed is not nil, until the evaluation entries
// it stands for are journaled. owed is the session's heldEntries channel, read
// under the core's lock by the goroutine about to journal an entry about one
// of the session's prompts.
func awaitHeldEntries(owed chan struct{}) {
	if owed != nil {
		<-owed
	}
}
