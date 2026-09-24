package supervise_test

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
	"github.com/Hocsman/Relayer/internal/terminal"
)

// These tests pin the hand: the connection that holds a session's terminal
// and types into it directly. Raw keystrokes never reach the runtime's
// pending prompt, so a prompt the holder answers by typing stays pending under
// the same ID, and an automatic answer to it types a second one into whatever
// the agent asks next. While somebody holds the hand, the policy therefore
// answers nothing on that session, and a prompt it would have answered stays
// the operator's once the hand is released.

// viewOf is the prompt the core shows with that ID, or nil.
func viewOf(sup *supervise.Supervisor, eventID string) *supervise.View {
	for _, view := range sup.State().Pending {
		if view.ID == eventID {
			shown := view
			return &shown
		}
	}
	return nil
}

// assertAskedForTheHand checks that the prompt the policy would have answered
// is the operator's for the reason operator_attached, shown and journaled so,
// its proposal and rule kept. evaluations is how many policy_evaluated
// entries the prompt has, the last of which says so.
func assertAskedForTheHand(t *testing.T, engine *fakeEngine, sup *supervise.Supervisor, eventID string, evaluations int) {
	t.Helper()
	evaluated := engine.auditFor(audit.KindPolicyEvaluated, eventID)
	if len(evaluated) != evaluations {
		t.Fatalf("policy_evaluated entries of %s = %#v, want %d", eventID, evaluated, evaluations)
	}
	last := evaluated[len(evaluated)-1]
	wantMetadata := map[string]string{
		"automatic": "false", "effective_action": "ask", "mode": "enforce", "proposed_action": string(policy.ActionAllow),
	}
	if last.DecisionBy != audit.DecisionByPolicy || last.Decision != audit.DecisionAsk || last.Outcome != audit.OutcomeAsk ||
		last.Reason != supervise.ReasonOperatorAttached || last.Rule != "allow-safe" || last.Operator != "" ||
		!reflect.DeepEqual(last.Metadata, wantMetadata) {
		t.Fatalf("the evaluation entry of %s = %#v", eventID, last)
	}
	if decisions := engine.auditFor(audit.KindDecision, eventID); len(decisions) != 0 {
		t.Fatalf("%s was decided: %#v", eventID, decisions)
	}
	assertShownAskedForTheHand(t, sup, eventID)
}

func assertShownAskedForTheHand(t *testing.T, sup *supervise.Supervisor, eventID string) {
	t.Helper()
	shown := viewOf(sup, eventID)
	want := supervise.EvaluationView{
		Action: string(policy.ActionAsk), ProposedAction: string(policy.ActionAllow), RuleName: "allow-safe",
		Reason: supervise.ReasonOperatorAttached,
	}
	if shown == nil || shown.DeliveryStatus != "pending" || shown.Evaluation != want {
		t.Fatalf("%s is shown as %#v, want pending on the operator", eventID, shown)
	}
}

// holdTheFirstWrite starts writing the answer to a first automatic prompt of
// the session and holds it, so that the next automatic prompts of the session
// wait behind it, pending. It returns the function that lets the write
// return, which the test calls before it ends: the run drains only once the
// write has returned.
func holdTheFirstWrite(t *testing.T, engine *fakeEngine) (applyStarted chan struct{}, releaseWrites func()) {
	t.Helper()
	applyStarted = make(chan struct{}, 8)
	release := make(chan struct{})
	engine.applyStarted = applyStarted
	engine.applyRelease = release
	return applyStarted, releaser(t, release)
}

func awaitWrite(t *testing.T, applyStarted <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-applyStarted:
	case <-time.After(2 * time.Second):
		t.Fatalf("%s was never written", what)
	}
}

// Taking the hand turns at once every prompt of the session the policy would
// answer, and whose answer is not already being written, into a question for
// the operator: shown and journaled with the reason operator_attached, the
// policy's proposal kept. The answer being written goes on; the session's
// other prompts, and other sessions' prompts, are left as they were.
func TestTakingTheHandAsksEveryAutomaticPromptNotAlreadyBeingWritten(t *testing.T) {
	engine := newFakeEngine()
	for _, id := range []string{"automatic-1", "automatic-2", "automatic-3", "other-1", "other-2"} {
		engine.evaluationByID[id] = automaticAllow()
	}
	applyStarted, releaseWrites := holdTheFirstWrite(t, engine)
	sup, sink := newCoreForTest(t, engine, "agent-a", "agent-b")

	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
	awaitWrite(t, applyStarted, "the first automatic answer")
	for sequence, id := range []string{"automatic-2", "asked-3", "automatic-3"} {
		prompt := promptEvent("agent-a", id)
		prompt.Sequence = uint64(sequence + 2)
		sup.Handle(session.AdapterEvent{Event: prompt})
	}
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-b", "other-1")})
	awaitWrite(t, applyStarted, "the other session's automatic answer")
	otherQueued := promptEvent("agent-b", "other-2")
	otherQueued.Sequence = 2
	sup.Handle(session.AdapterEvent{Event: otherQueued})
	sink.reset()

	sup.SetHolder("agent-a", "conn-1")

	// At once, before SetHolder's entries are journaled.
	for _, id := range []string{"automatic-2", "automatic-3"} {
		assertShownAskedForTheHand(t, sup, id)
	}
	if shown := viewOf(sup, "automatic-1"); shown == nil || shown.DeliveryStatus != "delivering" || !shown.Evaluation.Automatic {
		t.Fatalf("the answer being written became %#v", shown)
	}
	if shown := viewOf(sup, "asked-3"); shown == nil || shown.Evaluation.Reason != policy.ReasonDefault {
		t.Fatalf("the prompt already asked became %#v", shown)
	}
	if shown := viewOf(sup, "other-2"); shown == nil || shown.DeliveryStatus != "pending" || !shown.Evaluation.Automatic {
		t.Fatalf("the other session's prompt became %#v", shown)
	}
	if held, other := agentOf(t, sup, "agent-a"), agentOf(t, sup, "agent-b"); !held.Attached || other.Attached {
		t.Fatalf("attached: held session %t, other session %t", held.Attached, other.Attached)
	}

	waitFor(t, 2*time.Second, "the hand's entries and what it shows", func() bool {
		return len(engine.auditFor(audit.KindPolicyEvaluated, "automatic-3")) == 2 && len(sink.snapshot()) == 2
	})
	assertAskedForTheHand(t, engine, sup, "automatic-2", 2)
	assertAskedForTheHand(t, engine, sup, "automatic-3", 2)
	for _, id := range []string{"automatic-1", "asked-3", "other-2"} {
		if evaluated := engine.auditFor(audit.KindPolicyEvaluated, id); len(evaluated) != 1 {
			t.Fatalf("policy_evaluated entries of %s = %#v, want only its own", id, evaluated)
		}
	}
	var shown []string
	for _, call := range sink.snapshot() {
		if call.kind != "prompt" || call.view.Evaluation.Reason != supervise.ReasonOperatorAttached {
			t.Fatalf("taking the hand showed %v", trace(sink.snapshot()))
		}
		shown = append(shown, call.view.ID)
	}
	if want := []string{"automatic-2", "automatic-3"}; !reflect.DeepEqual(shown, want) {
		t.Fatalf("taking the hand showed %v, want %v in the order the agent asked them", shown, want)
	}

	releaseWrites()
	waitFor(t, 2*time.Second, "the answers already being written, and the other session's next", func() bool {
		return len(engine.auditFor(audit.KindDelivery, "automatic-1")) == 1 &&
			len(engine.auditFor(audit.KindDelivery, "other-1")) == 1 &&
			len(engine.auditFor(audit.KindDelivery, "other-2")) == 1
	})
	sup.BeginDrain()
	sup.Wait()
	if calls := engine.applySnapshot(); len(calls) != 3 {
		t.Fatalf("answers written = %#v, want the two already being written and the other session's next", calls)
	}
	for _, id := range []string{"automatic-2", "automatic-3"} {
		if decisions := engine.auditFor(audit.KindDecision, id); len(decisions) != 0 {
			t.Fatalf("%s was answered while the hand was held: %#v", id, decisions)
		}
	}
}

// A front end changes the hand under its own lock, and its sink may take that
// lock. SetHolder therefore neither waits on the journal nor calls the sink
// on its caller's goroutine: it changes the state at once, and journals and
// shows the change on a goroutine of its own.
func TestTakingTheHandWaitsOnNothingAndShowsNothingOnItsCaller(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["automatic-1"] = automaticAllow()
	engine.evaluationByID["automatic-2"] = automaticAllow()
	applyStarted, releaseWrites := holdTheFirstWrite(t, engine)
	defer releaseWrites()
	sup, sink := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
	awaitWrite(t, applyStarted, "the first automatic answer")
	queued := promptEvent("agent-a", "automatic-2")
	queued.Sequence = 2
	sup.Handle(session.AdapterEvent{Event: queued})

	journalStarted := make(chan struct{}, 1)
	journalRelease := make(chan struct{})
	releaseJournal := releaser(t, journalRelease)
	engine.set(func(f *fakeEngine) {
		f.auditBlockKind = audit.KindPolicyEvaluated
		f.auditStarted = journalStarted
		f.auditRelease = journalRelease
	})
	var host sync.Mutex
	sink.mu.Lock()
	sink.gate = func(sinkCall) {
		host.Lock()
		//lint:ignore SA2001 the sink takes the host's lock, as a front end's does
		host.Unlock()
	}
	sink.mu.Unlock()

	returned := make(chan struct{})
	go func() {
		host.Lock()
		defer host.Unlock()
		sup.SetHolder("agent-a", "conn-1")
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("SetHolder waited on the journal, or called the sink on its caller while the caller held the lock the sink takes")
	}
	assertShownAskedForTheHand(t, sup, "automatic-2")
	select {
	case <-journalStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the hand's entry was never journaled")
	}
	releaseJournal()
	waitFor(t, 2*time.Second, "the prompt shown asked", func() bool {
		for _, call := range sink.snapshot() {
			if call.kind == "prompt" && call.view.ID == "automatic-2" && call.view.Evaluation.Reason == supervise.ReasonOperatorAttached {
				return true
			}
		}
		return false
	})
	assertAskedForTheHand(t, engine, sup, "automatic-2", 2)
}

// A prompt the session raises while somebody holds the hand is asked from the
// start: its only evaluation entry says operator_attached, and the operator is
// notified as for any prompt that waits on a person. A session nobody holds is
// answered by the policy as usual.
func TestAPromptRaisedWhileTheHandIsHeldIsAskedBeforeItsEvaluationIsJournaled(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["held-1"] = automaticAllow()
	engine.evaluationByID["free-1"] = automaticAllow()
	sup, sink := newCoreForTest(t, engine, "agent-a", "agent-b")
	sup.SetHolder("Agent-A", " conn-1 ")

	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "held-1")})
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-b", "free-1")})
	waitFor(t, 2*time.Second, "the free session's automatic answer", func() bool {
		return len(engine.auditFor(audit.KindDelivery, "free-1")) == 1
	})
	sup.BeginDrain()
	sup.Wait()

	assertAskedForTheHand(t, engine, sup, "held-1", 1)
	notified := false
	for _, call := range sink.snapshot() {
		if call.kind == "notify" && call.notice.EventID == "held-1" && call.notice.Kind == supervise.NoticePendingDecision {
			notified = true
		}
	}
	if !notified {
		t.Fatalf("the operator was not told about the prompt: %v", trace(sink.snapshot()))
	}
	if calls := engine.applySnapshot(); len(calls) != 1 || calls[0].event.ID != "free-1" {
		t.Fatalf("answers written = %#v, want only the free session's", calls)
	}
}

// The hand may be taken while a prompt is being taken in: after the policy
// evaluated it automatic and that was journaled, before the prompt joined
// those SetHolder asks. It is asked all the same, and a second entry says
// why.
func TestAPromptTakenInAsTheHandIsTakenIsAskedAllTheSame(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["prompt-1"] = automaticAllow()
	journalStarted := make(chan struct{}, 1)
	journalRelease := make(chan struct{})
	releaseJournal := releaser(t, journalRelease)
	engine.auditBlockKind = audit.KindPolicyEvaluated
	engine.auditStarted = journalStarted
	engine.auditRelease = journalRelease
	sup, _ := newCoreForTest(t, engine, "agent-a")

	handled := make(chan struct{})
	go func() {
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
		close(handled)
	}()
	select {
	case <-journalStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the prompt's evaluation was never journaled")
	}
	sup.SetHolder("agent-a", "conn-1")
	releaseJournal()
	select {
	case <-handled:
	case <-time.After(2 * time.Second):
		t.Fatal("the prompt was never taken in")
	}
	sup.BeginDrain()
	sup.Wait()

	evaluated := engine.auditFor(audit.KindPolicyEvaluated, "prompt-1")
	if len(evaluated) != 2 || evaluated[0].Outcome != audit.OutcomeInFlight || evaluated[0].Reason != policy.ReasonRule {
		t.Fatalf("policy_evaluated entries = %#v, want the automatic one then the hand's", evaluated)
	}
	assertAskedForTheHand(t, engine, sup, "prompt-1", 2)
	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("the prompt was answered while the hand was held: %#v", calls)
	}
}

// Releasing the hand never makes a prompt automatic again: the holder may have
// answered it by typing, which the runtime never learns, and the policy's
// answer would be a second one. The prompt stays the operator's, whether the
// hand asked it when it was taken or when the prompt was raised. A prompt
// raised after the release is the policy's as usual, once the prompts before
// it are answered.
func TestReleasingTheHandNeverMakesAPromptAutomaticAgain(t *testing.T) {
	for _, test := range []struct {
		name string
		// ask makes automatic-1 the operator's while the hand is held.
		ask func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine)
	}{
		{name: "asked when the hand was taken", ask: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
			applyStarted, releaseWrites := holdTheFirstWrite(t, engine)
			sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-0")})
			awaitWrite(t, applyStarted, "the first automatic answer")
			queued := promptEvent("agent-a", "automatic-1")
			queued.Sequence = 2
			sup.Handle(session.AdapterEvent{Event: queued})
			sup.SetHolder("agent-a", "conn-1")
			engine.set(func(f *fakeEngine) { f.applyStarted = nil })
			releaseWrites()
			waitFor(t, 2*time.Second, "the answer already being written", func() bool {
				return len(engine.auditFor(audit.KindDelivery, "automatic-0")) == 1 &&
					len(engine.auditFor(audit.KindPolicyEvaluated, "automatic-1")) == 2
			})
		}},
		{name: "asked when it was raised", ask: func(t *testing.T, sup *supervise.Supervisor, _ *fakeEngine) {
			sup.SetHolder("agent-a", "conn-1")
			queued := promptEvent("agent-a", "automatic-1")
			queued.Sequence = 2
			sup.Handle(session.AdapterEvent{Event: queued})
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			for _, id := range []string{"automatic-0", "automatic-1", "automatic-2"} {
				engine.evaluationByID[id] = automaticAllow()
			}
			sup, _ := newCoreForTest(t, engine, "agent-a")
			test.ask(t, sup, engine)
			written := len(engine.applySnapshot())

			sup.SetHolder("agent-a", "")
			if agentOf(t, sup, "agent-a").Attached {
				t.Fatal("the agent is still shown attached once the hand is released")
			}
			assertShownAskedForTheHand(t, sup, "automatic-1")
			// Whatever the core does next, it may not answer the prompt: a
			// prompt raised after the release asks for the session's next
			// automatic answer, and a line for a reconciliation.
			next := promptEvent("agent-a", "automatic-2")
			next.Sequence = 3
			sup.Handle(session.AdapterEvent{Event: next})
			_ = sup.SubmitLine(testRunID, "agent-a", "hello", alice)
			time.Sleep(50 * time.Millisecond)
			if calls := engine.applySnapshot(); len(calls) != written {
				t.Fatalf("the release answered %#v", calls[written:])
			}
			assertShownAskedForTheHand(t, sup, "automatic-1")
			if shown := viewOf(sup, "automatic-2"); shown == nil || !shown.Evaluation.Automatic {
				t.Fatalf("the prompt raised after the release is %#v, want the policy's", shown)
			}

			// A person answers it; the policy answers the next.
			if err := sup.SubmitDecision(testRunID, "agent-a", "automatic-1", "y", alice); err != nil {
				t.Fatalf("SubmitDecision: %v", err)
			}
			waitFor(t, 2*time.Second, "the policy's answer to the prompt raised after the release", func() bool {
				return len(engine.auditFor(audit.KindDelivery, "automatic-2")) == 1
			})
			if decisions := engine.auditFor(audit.KindDecision, "automatic-1"); len(decisions) != 1 || decisions[0].DecisionBy != audit.DecisionByHuman {
				t.Fatalf("decisions on the prompt the hand asked = %#v, want the person's only", decisions)
			}
			if decisions := engine.auditFor(audit.KindDecision, "automatic-2"); len(decisions) != 1 || decisions[0].DecisionBy != audit.DecisionByPolicy {
				t.Fatalf("decisions on the prompt raised after the release = %#v, want the policy's", decisions)
			}
		})
	}
}

// The hand is the front end's, and the core changes it only when told: an
// exit, a Start or a line the session could not take leave it with its
// holder, and the new process's prompts are asked like the old one's. The
// core and the front end therefore never disagree on who holds a terminal.
func TestTheHandStaysWithItsHolderWhateverTheProcessDoes(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["new-1"] = automaticAllow()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.SetHolder("agent-a", "conn-1")

	sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
	if agent := agentOf(t, sup, "agent-a"); agent.Running || !agent.Attached {
		t.Fatalf("the agent after its exit = %#v, want exited and still held", agent)
	}
	if err := sup.StartSession(testRunID, "agent-a"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if agent := agentOf(t, sup, "agent-a"); !agent.Running || !agent.Attached {
		t.Fatalf("the agent after its start = %#v, want running and still held", agent)
	}
	fresh := promptEvent("agent-a", "new-1")
	fresh.Timestamp = time.Now().UTC().Add(time.Second)
	sup.Handle(session.AdapterEvent{Event: fresh})
	sup.BeginDrain()
	sup.Wait()
	assertAskedForTheHand(t, engine, sup, "new-1", 1)

	// A line written as the hand was taken, which the session could not take,
	// leaves the hand where it is too.
	lined := newFakeEngine()
	lineStarted := make(chan string, 1)
	lineRelease := make(chan struct{})
	releaseLine := releaser(t, lineRelease)
	lined.lineStarted = lineStarted
	lined.lineRelease = lineRelease
	lined.lineErr = terminal.ErrClosed
	other, _ := newCoreForTest(t, lined, "agent-a")
	sent := make(chan error, 1)
	go func() { sent <- other.SubmitLine(testRunID, "agent-a", "hello", alice) }()
	select {
	case <-lineStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the line was never written")
	}
	other.SetHolder("agent-a", "conn-1")
	releaseLine()
	if err := <-sent; !errors.Is(err, supervise.ErrLineUnavailable) {
		t.Fatalf("the line the session could not take = %v, want ErrLineUnavailable", err)
	}
	if agent := agentOf(t, other, "agent-a"); agent.Running || !agent.Attached {
		t.Fatalf("the agent after a line it could not take = %#v, want not running and still held", agent)
	}
}

// A line is refused while anybody holds the hand, the holder included: it
// would interleave with the keystrokes. A person's decision is not: the hand
// governs the terminal, not supervision, and anyone who may act may answer.
func TestALineWaitsForTheHandToBeReleasedAndADecisionDoesNot(t *testing.T) {
	engine := newFakeEngine()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.SetHolder("agent-a", "conn-2")

	for _, actor := range []supervise.Actor{alice, {Identity: "bob", Role: supervise.RoleOperator, ConnID: "conn-2"}, desktop} {
		if err := sup.SubmitLine(testRunID, "agent-a", "hello", actor); !errors.Is(err, supervise.ErrLineUnavailable) {
			t.Fatalf("a line by %#v while the hand is held = %v, want ErrLineUnavailable", actor, err)
		}
	}
	if lines := engine.lineSnapshot(); len(lines) != 0 {
		t.Fatalf("a line was sent while the hand was held: %#v", lines)
	}

	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", alice); err != nil {
		t.Fatalf("an answer by someone who does not hold the hand = %v", err)
	}
	if calls := engine.applySnapshot(); len(calls) != 1 || calls[0].event.ID != "prompt-1" {
		t.Fatalf("answers written = %#v", calls)
	}

	sup.SetHolder("agent-a", "")
	if err := sup.SubmitLine(testRunID, "agent-a", "hello", alice); err != nil {
		t.Fatalf("a line once the hand is released = %v", err)
	}
}

// answerOnceFree answers a prompt of agent-a as alice, again for as long as
// the session is still claimed by the write before it: the claim is released a
// moment after that write's delivery is journaled, and a refusal for it
// changes nothing.
func answerOnceFree(sup *supervise.Supervisor, eventID string) error {
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := sup.SubmitDecision(testRunID, "agent-a", eventID, "y", alice)
		if !errors.Is(err, supervise.ErrDecisionInFlight) || time.Now().After(deadline) {
			return err
		}
		time.Sleep(time.Millisecond)
	}
}

// The hand turns a prompt into an ask at once, but journals why on another
// goroutine. A person may answer that prompt, and the agent withdraw it, the
// moment it is shown asked: the journal still says why it was asked before
// it says who answered it or that it went away.
func TestTheJournalSaysWhyAPromptWasAskedBeforeWhatBecameOfIt(t *testing.T) {
	for _, test := range []struct {
		name string
		// then does what becomes of the prompt; its entry is of kind.
		then func(sup *supervise.Supervisor) error
		kind audit.Kind
	}{
		{name: "answered", kind: audit.KindDecision, then: func(sup *supervise.Supervisor) error {
			return answerOnceFree(sup, "automatic-1")
		}},
		{name: "withdrawn", kind: audit.KindEventWithdrawn, then: func(sup *supervise.Supervisor) error {
			queued := promptEvent("agent-a", "automatic-1")
			queued.Sequence = 2
			sup.Handle(session.AdapterEventWithdrawn{Event: queued})
			return nil
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluationByID["automatic-0"] = automaticAllow()
			engine.evaluationByID["automatic-1"] = automaticAllow()
			applyStarted, releaseWrites := holdTheFirstWrite(t, engine)
			defer releaseWrites()
			sup, _ := newCoreForTest(t, engine, "agent-a")
			sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-0")})
			awaitWrite(t, applyStarted, "the first automatic answer")
			queued := promptEvent("agent-a", "automatic-1")
			queued.Sequence = 2
			sup.Handle(session.AdapterEvent{Event: queued})

			journalStarted := make(chan struct{}, 1)
			journalRelease := make(chan struct{})
			releaseJournal := releaser(t, journalRelease)
			defer releaseJournal()
			engine.set(func(f *fakeEngine) {
				f.auditBlockKind = audit.KindPolicyEvaluated
				f.auditStarted = journalStarted
				f.auditRelease = journalRelease
				f.applyStarted = nil
			})
			sup.SetHolder("agent-a", "conn-1")
			select {
			case <-journalStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("the hand's entry was never journaled")
			}
			// The first answer returns and frees the session.
			releaseWrites()
			waitFor(t, 2*time.Second, "the first answer", func() bool {
				return len(engine.auditFor(audit.KindDelivery, "automatic-0")) == 1
			})

			done := make(chan error, 1)
			go func() { done <- test.then(sup) }()
			time.Sleep(50 * time.Millisecond)
			if entries := engine.auditFor(test.kind, "automatic-1"); len(entries) != 0 {
				t.Fatalf("%s was journaled before the hand's entry: %#v", test.kind, entries)
			}
			if calls := engine.applySnapshot(); len(calls) != 1 {
				t.Fatalf("the prompt was answered before the hand's entry was journaled: %#v", calls)
			}
			releaseJournal()
			select {
			case err := <-done:
				if err != nil {
					t.Fatalf("%s: %v", test.name, err)
				}
			case <-time.After(2 * time.Second):
				t.Fatalf("the prompt was never %s", test.name)
			}

			asked, then := -1, -1
			for index, entry := range engine.auditSnapshot() {
				if entry.EventID != "automatic-1" {
					continue
				}
				if entry.Kind == audit.KindPolicyEvaluated && entry.Reason == supervise.ReasonOperatorAttached {
					asked = index
				}
				if entry.Kind == test.kind && then < 0 {
					then = index
				}
			}
			if asked < 0 || then < asked {
				t.Fatalf("journal = %v, want the hand's entry (%d) before the %s (%d)", kindsOf(engine.auditSnapshot()), asked, test.kind, then)
			}
		})
	}
}

func kindsOf(entries []audit.Entry) []string {
	kinds := make([]string, 0, len(entries))
	for _, entry := range entries {
		kinds = append(kinds, string(entry.Kind)+":"+entry.EventID)
	}
	return kinds
}

// Taking the hand the holder already has, or handing it to another
// connection, asks nothing twice: the prompts are the operator's already.
// Neither does a hand taken during a drain, which answers nothing more, nor
// one for a session the run does not have, nor one with no run at all.
func TestTheHandAsksEachPromptOnce(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["automatic-0"] = automaticAllow()
	engine.evaluationByID["automatic-1"] = automaticAllow()
	applyStarted, releaseWrites := holdTheFirstWrite(t, engine)
	defer releaseWrites()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-0")})
	awaitWrite(t, applyStarted, "the first automatic answer")
	queued := promptEvent("agent-a", "automatic-1")
	queued.Sequence = 2
	sup.Handle(session.AdapterEvent{Event: queued})

	sup.SetHolder("agent-a", "conn-1")
	sup.SetHolder("agent-a", "conn-1")
	sup.SetHolder("agent-a", "conn-2")
	sup.SetHolder("agent-z", "conn-1")
	var none *supervise.Supervisor
	none.SetHolder("agent-a", "conn-1")
	waitFor(t, 2*time.Second, "the hand's entry", func() bool {
		return len(engine.auditFor(audit.KindPolicyEvaluated, "automatic-1")) >= 2
	})
	time.Sleep(20 * time.Millisecond)
	assertAskedForTheHand(t, engine, sup, "automatic-1", 2)
	if _, found := sup.Agent("agent-z"); found {
		t.Fatal("the hand made up an agent")
	}

	drained := newFakeEngine()
	drained.evaluationByID["automatic-0"] = automaticAllow()
	drained.evaluationByID["automatic-1"] = automaticAllow()
	drainedStarted, releaseDrained := holdTheFirstWrite(t, drained)
	defer releaseDrained()
	other, _ := newCoreForTest(t, drained, "agent-a")
	other.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-0")})
	awaitWrite(t, drainedStarted, "the first automatic answer")
	other.Handle(session.AdapterEvent{Event: queued})
	other.BeginDrain()
	other.SetHolder("agent-a", "conn-1")
	if !agentOf(t, other, "agent-a").Attached {
		t.Fatal("the hand taken during a drain is not recorded")
	}
	if shown := viewOf(other, "automatic-1"); shown == nil || !shown.Evaluation.Automatic {
		t.Fatalf("the hand taken during a drain changed the prompt to %#v", shown)
	}
	time.Sleep(20 * time.Millisecond)
	if evaluated := drained.auditFor(audit.KindPolicyEvaluated, "automatic-1"); len(evaluated) != 1 {
		t.Fatalf("the hand taken during a drain journaled %#v", evaluated)
	}
}

// The hand's entries go through the journal like any other: one it refuses
// freezes the run, and nothing more is sent.
func TestAJournalThatRefusesTheHandsEntryFreezesTheRun(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["automatic-0"] = automaticAllow()
	engine.evaluationByID["automatic-1"] = automaticAllow()
	applyStarted, releaseWrites := holdTheFirstWrite(t, engine)
	defer releaseWrites()
	sup, sink := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-0")})
	awaitWrite(t, applyStarted, "the first automatic answer")
	queued := promptEvent("agent-a", "automatic-1")
	queued.Sequence = 2
	sup.Handle(session.AdapterEvent{Event: queued})
	engine.set(func(f *fakeEngine) { f.auditFailAt = f.auditCalls + 1 })

	sup.SetHolder("agent-a", "conn-1")
	waitFor(t, 2*time.Second, "the run to freeze", func() bool {
		for _, call := range sink.snapshot() {
			if call.kind == "status" && call.status.Scope == "audit" && call.status.Status == "failed" {
				return true
			}
		}
		return false
	})
	if state := sup.State(); !state.AuditFailed || !state.Agents[0].InputFrozen {
		t.Fatalf("the run after the journal refused the hand's entry = %#v", state)
	}
	if err := sup.SubmitDecision(testRunID, "agent-a", "automatic-1", "y", alice); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("an answer once the journal failed = %v, want ErrDeliveryUncertain", err)
	}
}
