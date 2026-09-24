package supervise_test

import (
	"reflect"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// These tests pin the guard that keeps the policy from answering a repeat: a
// prompt it would answer automatically, but whose Signature is that of a prompt
// of its session whose answer is being written, or was written less than
// RepeatWindow ago. An adapter that reads an answered question again, from the
// answer's echo or a repaint, raises such a repeat under a new ID; answered, it
// types a second answer into an agent that consumed the first, and whatever the
// agent asks next receives an answer nobody gave. The adapters are where that
// is fixed; the guard is the backstop. The core's clock is the test's.

// repeatOf is a new prompt of the same session asking what prompt asked: a new
// ID, the same Signature.
func repeatOf(prompt adapters.Event, eventID string) adapters.Event {
	repeat := prompt
	repeat.ID = eventID
	repeat.Sequence = prompt.Sequence + 1
	return repeat
}

// waitForTheSessionToBeFree waits until the claim an automatic answer holds
// on the session is released, a moment after its delivery was journaled: a
// line is refused until then, and sent once it is. A repeat detected before
// then is one whose answer is still being written.
func waitForTheSessionToBeFree(t *testing.T, sup *supervise.Supervisor, sessionID string) {
	t.Helper()
	waitFor(t, 2*time.Second, "the session to be free", func() bool {
		return sup.SubmitLine(testRunID, sessionID, "next", desktop) == nil
	})
}

// assertAskedAsARepeat checks that the policy's automatic answer to the prompt
// was turned into a question for the operator, journaled and shown with the
// reason repeat_after_delivery, the policy's proposal and rule kept.
// evaluations is how many policy_evaluated entries the prompt has, the last
// of which says so.
func assertAskedAsARepeat(t *testing.T, engine *fakeEngine, sup *supervise.Supervisor, eventID string, evaluations int) {
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
		last.Reason != supervise.ReasonRepeatAfterDelivery || last.Rule != "allow-safe" || !reflect.DeepEqual(last.Metadata, wantMetadata) {
		t.Fatalf("policy_evaluated entry of the repeat = %#v", last)
	}
	if decisions := engine.auditFor(audit.KindDecision, eventID); len(decisions) != 0 {
		t.Fatalf("the repeat was decided: %#v", decisions)
	}
	var shown *supervise.View
	for _, view := range sup.State().Pending {
		if view.ID == eventID {
			shown = &view
		}
	}
	if shown == nil || shown.DeliveryStatus != "pending" || shown.Evaluation.Action != string(policy.ActionAsk) ||
		shown.Evaluation.ProposedAction != string(policy.ActionAllow) || shown.Evaluation.Automatic ||
		shown.Evaluation.Reason != supervise.ReasonRepeatAfterDelivery {
		t.Fatalf("the repeat is shown as %#v, want pending on the operator", shown)
	}
}

// A prompt that asks again what a prompt of its session asked, less than
// RepeatWindow after that prompt's answer was written, is not answered by the
// policy, whoever gave the first answer: it goes to the operator, who is
// notified. Once the window is over, for another question, or for questions
// the adapter gave no Signature, the policy answers as it always did. The
// window is two seconds unless the run's options set another.
func TestAPromptRepeatingAnAnswerIsAskedNotAnswered(t *testing.T) {
	answerByThePolicy := func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine, prompt adapters.Event) {
		t.Helper()
		engine.set(func(f *fakeEngine) { f.evaluationByID[prompt.ID] = automaticAllow() })
		sup.Handle(session.AdapterEvent{Event: prompt})
		waitFor(t, 2*time.Second, "the policy's answer", func() bool {
			return len(engine.auditFor(audit.KindDelivery, prompt.ID)) == 1
		})
		waitForTheSessionToBeFree(t, sup, prompt.SessionID)
	}
	answerByTyping := func(t *testing.T, sup *supervise.Supervisor, _ *fakeEngine, prompt adapters.Event) {
		t.Helper()
		sup.Handle(session.AdapterEvent{Event: prompt})
		if err := sup.SubmitDecision(testRunID, prompt.SessionID, prompt.ID, "y", desktop); err != nil {
			t.Fatalf("SubmitDecision: %v", err)
		}
	}
	answerByChoosing := func(t *testing.T, sup *supervise.Supervisor, _ *fakeEngine, prompt adapters.Event) {
		t.Helper()
		sup.Handle(session.AdapterEvent{Event: prompt})
		if err := sup.SubmitAutomaticDecision(testRunID, prompt.SessionID, prompt.ID, string(adapters.DecisionAllow), desktop); err != nil {
			t.Fatalf("SubmitAutomaticDecision: %v", err)
		}
	}
	for _, test := range []struct {
		name   string
		window time.Duration
		answer func(*testing.T, *supervise.Supervisor, *fakeEngine, adapters.Event)
		// after is how long after the first answer the repeat is detected.
		after time.Duration
		// signature, when set, is the repeat's own Signature; "none" leaves
		// both prompts without one.
		signature  string
		wantAsking bool
	}{
		{name: "the policy's answer, asked again just inside the window", answer: answerByThePolicy, after: 1999 * time.Millisecond, wantAsking: true},
		{name: "the policy's answer, asked again once the window is over", answer: answerByThePolicy, after: 2 * time.Second},
		{name: "a typed answer, asked again just inside the window", answer: answerByTyping, after: 1999 * time.Millisecond, wantAsking: true},
		{name: "a chosen answer, asked again just inside the window", answer: answerByChoosing, after: 1999 * time.Millisecond, wantAsking: true},
		{name: "a typed answer, asked again once the window is over", answer: answerByTyping, after: 2 * time.Second},
		{name: "another question just after the answer", answer: answerByThePolicy, signature: "signature-another-question"},
		{name: "questions without a signature", answer: answerByThePolicy, signature: "none"},
		{name: "a longer window the run chose", window: 5 * time.Second, answer: answerByTyping, after: 4999 * time.Millisecond, wantAsking: true},
		{name: "a window below zero, which is the default one", window: -time.Second, answer: answerByTyping, after: 1999 * time.Millisecond, wantAsking: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
			engine.evaluationByID["repeat-2"] = automaticAllow()
			clock := newTestClock()
			sup, sink := newCoreWithOptions(t, engine, supervise.Options{RepeatWindow: test.window, Now: clock.Now}, "agent-a")
			first := promptEvent("agent-a", "first-1")
			repeat := repeatOf(first, "repeat-2")
			switch test.signature {
			case "":
			case "none":
				first.Signature, repeat.Signature = "", ""
			default:
				repeat.Signature = test.signature
			}

			test.answer(t, sup, engine, first)
			clock.advance(test.after)
			sink.reset()
			sup.Handle(session.AdapterEvent{Event: repeat})

			if !test.wantAsking {
				waitFor(t, 2*time.Second, "the policy's answer to the second prompt", func() bool {
					return len(engine.auditFor(audit.KindDelivery, "repeat-2")) == 1
				})
				if evaluated := engine.auditFor(audit.KindPolicyEvaluated, "repeat-2"); len(evaluated) != 1 || evaluated[0].Metadata["automatic"] != "true" {
					t.Fatalf("policy_evaluated entries of the second prompt = %#v, want it automatic", evaluated)
				}
				return
			}
			assertAskedAsARepeat(t, engine, sup, "repeat-2", 1)
			notified := false
			for _, call := range sink.snapshot() {
				notified = notified || (call.kind == "notify" && call.notice.Kind == supervise.NoticePendingDecision && call.notice.EventID == "repeat-2")
			}
			if !notified {
				t.Fatalf("the operator was not told of the repeat: %v", trace(sink.snapshot()))
			}
			sup.BeginDrain()
			sup.Wait()
			for _, call := range engine.applySnapshot() {
				if call.event.ID == "repeat-2" {
					t.Fatalf("the repeat was answered: %#v", call)
				}
			}
		})
	}
}

// Every answer written within the window is remembered, not only the last: an
// agent may ask two questions in a row, and the first one's echo may come
// after the second one's answer.
func TestAnAnswerIsRememberedAfterAnotherIsWritten(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["repeat-3"] = automaticAllow()
	clock := newTestClock()
	sup, _ := newCoreWithOptions(t, engine, supervise.Options{Now: clock.Now}, "agent-a")
	first := promptEvent("agent-a", "first-1")
	second := promptEvent("agent-a", "second-2")
	second.Sequence = 2
	second.Signature = "signature-another-question"

	sup.Handle(session.AdapterEvent{Event: first})
	if err := sup.SubmitDecision(testRunID, "agent-a", "first-1", "y", desktop); err != nil {
		t.Fatalf("the first answer: %v", err)
	}
	clock.advance(time.Second)
	sup.Handle(session.AdapterEvent{Event: second})
	if err := sup.SubmitDecision(testRunID, "agent-a", "second-2", "y", desktop); err != nil {
		t.Fatalf("the second answer: %v", err)
	}
	clock.advance(500 * time.Millisecond)
	repeat := repeatOf(first, "repeat-3")
	repeat.Sequence = 3
	sup.Handle(session.AdapterEvent{Event: repeat})
	assertAskedAsARepeat(t, engine, sup, "repeat-3", 1)
}

// The guard only ever takes an answer away from the policy. A repeat that the
// policy asks about anyway keeps the policy's own reason, and with it what the
// operator is told: a guardrail's notice, not a repeat's.
func TestARepeatThePolicyAsksAboutAnywayKeepsThePolicysReason(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["repeat-2"] = policy.Evaluation{
		Action: policy.ActionAsk, ProposedAction: policy.ActionAllow, Reason: policy.ReasonDestructive,
	}
	sup, sink := newCoreWithOptions(t, engine, supervise.Options{Now: newTestClock().Now}, "agent-a")
	first := promptEvent("agent-a", "first-1")
	sup.Handle(session.AdapterEvent{Event: first})
	if err := sup.SubmitDecision(testRunID, "agent-a", "first-1", "y", desktop); err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}
	sink.reset()

	sup.Handle(session.AdapterEvent{Event: repeatOf(first, "repeat-2")})
	if evaluated := engine.auditFor(audit.KindPolicyEvaluated, "repeat-2"); len(evaluated) != 1 || evaluated[0].Reason != policy.ReasonDestructive {
		t.Fatalf("policy_evaluated entries of the repeat = %#v, want the guardrail's", evaluated)
	}
	var notices []supervise.NoticeKind
	for _, call := range sink.snapshot() {
		if call.kind == "notify" {
			notices = append(notices, call.notice.Kind)
		}
	}
	if len(notices) != 1 || notices[0] != supervise.NoticeGuardrailBlocked {
		t.Fatalf("notices = %v, want the guardrail's", notices)
	}
}

// A prompt that asks again what a prompt of its session asked, while that
// prompt's answer is still being written, is not answered by the policy
// either, whoever writes the answer, and even when the agent withdrew the
// prompt being answered: that is how an echo reads, the question repainted
// with the answer after it while the write returns. Waiting behind the write,
// the repeat used to be answered once it had returned.
func TestAPromptRepeatingOneBeingAnsweredIsAskedNotAnswered(t *testing.T) {
	for _, test := range []struct {
		name     string
		human    bool
		withdraw bool
	}{
		{name: "while the policy's answer is written"},
		{name: "while a human's answer is written", human: true},
		{name: "withdrawn while its answer is written", withdraw: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			applyStarted := make(chan struct{}, 4)
			release := make(chan struct{})
			engine.applyStarted = applyStarted
			engine.applyRelease = release
			if !test.human {
				engine.evaluationByID["first-1"] = automaticAllow()
			}
			engine.evaluationByID["repeat-2"] = automaticAllow()
			sup, _ := newCoreWithOptions(t, engine, supervise.Options{Now: newTestClock().Now}, "agent-a")
			releaseWrites := releaser(t, release)
			first := promptEvent("agent-a", "first-1")
			repeat := repeatOf(first, "repeat-2")

			sup.Handle(session.AdapterEvent{Event: first})
			answered := make(chan error, 1)
			if test.human {
				go func() { answered <- sup.SubmitDecision(testRunID, "agent-a", "first-1", "y", desktop) }()
			}
			select {
			case <-applyStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("the first answer never reached the runtime")
			}
			if test.withdraw {
				sup.Handle(session.AdapterEventWithdrawn{Event: first})
			}
			sup.Handle(session.AdapterEvent{Event: repeat})
			assertAskedAsARepeat(t, engine, sup, "repeat-2", 1)

			releaseWrites()
			if test.human {
				if err := <-answered; err != nil {
					t.Fatalf("SubmitDecision: %v", err)
				}
			}
			waitFor(t, 2*time.Second, "the first answer's delivery", func() bool {
				return len(engine.auditFor(audit.KindDelivery, "first-1")) == 1
			})
			sup.BeginDrain()
			sup.Wait()
			if calls := engine.applySnapshot(); len(calls) != 1 || calls[0].event.ID != "first-1" {
				t.Fatalf("deliveries = %#v, want the first answer alone", calls)
			}
			assertAskedAsARepeat(t, engine, sup, "repeat-2", 1)
		})
	}
}

// A prompt without a Signature repeats nothing, even while the answer to
// another prompt without one is being written: an adapter that gives none
// cannot tell its questions apart, and the guard would take every one of them
// from the policy.
func TestAPromptWithoutASignatureRepeatsNothingBeingAnswered(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	applyStarted := make(chan struct{}, 4)
	release := make(chan struct{})
	engine.applyStarted = applyStarted
	engine.applyRelease = release
	sup, _ := newCoreWithOptions(t, engine, supervise.Options{Now: newTestClock().Now}, "agent-a")
	releaseWrites := releaser(t, release)
	first := promptEvent("agent-a", "first-1")
	first.Signature = ""
	second := repeatOf(first, "second-2")

	sup.Handle(session.AdapterEvent{Event: first})
	select {
	case <-applyStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the first answer never reached the runtime")
	}
	sup.Handle(session.AdapterEvent{Event: second})
	if evaluated := engine.auditFor(audit.KindPolicyEvaluated, "second-2"); len(evaluated) != 1 || evaluated[0].Metadata["automatic"] != "true" {
		t.Fatalf("policy_evaluated entries of the second prompt = %#v, want it automatic", evaluated)
	}
	releaseWrites()
	waitFor(t, 2*time.Second, "both answers", func() bool {
		return len(engine.auditFor(audit.KindDelivery, "second-2")) == 1
	})
}

// A repeat queued behind the prompt it repeats was not one when it was
// detected: nothing had been answered yet. The guard is checked again just
// before the policy's decision, with the policy's own evaluation, and the
// repeat then goes to the operator with a second evaluation entry.
func TestARepeatQueuedBehindThePromptItRepeatsIsCheckedAgainBeforeItsAnswer(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	sup, _ := newCoreWithOptions(t, engine, supervise.Options{Now: newTestClock().Now}, "agent-a")
	letTheLineGo := holdWithALine(t, engine, sup, "agent-a")
	first := promptEvent("agent-a", "first-1")
	repeat := repeatOf(first, "repeat-2")
	sup.Handle(session.AdapterEvent{Event: first})
	sup.Handle(session.AdapterEvent{Event: repeat})
	if evaluated := engine.auditFor(audit.KindPolicyEvaluated, "repeat-2"); len(evaluated) != 1 || evaluated[0].Metadata["automatic"] != "true" {
		t.Fatalf("the repeat at detection = %#v, want it automatic: nothing was answered yet", evaluated)
	}

	letTheLineGo()
	waitFor(t, 2*time.Second, "the repeat to go to the operator", func() bool {
		for _, view := range sup.State().Pending {
			if view.ID == "repeat-2" && !view.Evaluation.Automatic {
				return true
			}
		}
		return false
	})
	sup.BeginDrain()
	sup.Wait()
	if calls := engine.applySnapshot(); len(calls) != 1 || calls[0].event.ID != "first-1" {
		t.Fatalf("deliveries = %#v, want the first answer alone", calls)
	}
	assertAskedAsARepeat(t, engine, sup, "repeat-2", 2)
}

// Keystrokes the holder typed count as an answer to every prompt of the
// session they may have answered: those pending when the keystrokes are
// written, and those still being taken in. Raw keystrokes never resolve the
// runtime's prompt, and a repeat of one the holder answered by typing, read
// again from the answer's echo once the hand was released, was answered by
// the policy: a second answer, which the agent took for whatever it asked
// next. It is asked for repeat_after_delivery within the window from the
// moment the keystrokes were written, as after any answer; once the window is
// over the policy answers as usual.
func TestKeystrokesCountAsAnAnswerForTheRepeatGuard(t *testing.T) {
	for _, test := range []struct {
		name string
		// raise takes prompt-1 in, while the hand is held, and has the holder
		// type into the session at the moment under test.
		raise func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine, first adapters.Event)
	}{
		{name: "a prompt pending when they are written", raise: func(t *testing.T, sup *supervise.Supervisor, _ *fakeEngine, first adapters.Event) {
			sup.Handle(session.AdapterEvent{Event: first})
			admitted(t, sup, "agent-a", "conn-1")()
		}},
		{name: "a prompt still being taken in when they are written", raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine, first adapters.Event) {
			journalStarted := make(chan struct{}, 1)
			journalRelease := make(chan struct{})
			releaseJournal := releaser(t, journalRelease)
			engine.set(func(f *fakeEngine) {
				f.auditBlockKind = audit.KindEventDetected
				f.auditBlockEventID = first.ID
				f.auditStarted = journalStarted
				f.auditRelease = journalRelease
			})
			handled := make(chan struct{})
			go func() {
				sup.Handle(session.AdapterEvent{Event: first})
				close(handled)
			}()
			select {
			case <-journalStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("the prompt's detection was never journaled")
			}
			admitted(t, sup, "agent-a", "conn-1")()
			releaseJournal()
			select {
			case <-handled:
			case <-time.After(2 * time.Second):
				t.Fatal("the prompt was never taken in")
			}
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluation = automaticAllow()
			clock := newTestClock()
			sup, _ := newCoreWithOptions(t, engine, supervise.Options{Now: clock.Now}, "agent-a")
			first := promptEvent("agent-a", "prompt-1")
			sup.SetHolder("agent-a", "conn-1")
			test.raise(t, sup, engine, first)
			if shown := viewOf(sup, "prompt-1"); shown == nil || shown.Evaluation.Reason != supervise.ReasonOperatorAttached {
				t.Fatalf("the prompt the holder typed into is shown as %#v", shown)
			}
			// The agent consumed the typed answer and took its question back;
			// the holder lets go of the terminal.
			sup.Handle(session.AdapterEventWithdrawn{Event: first})
			sup.SetHolder("agent-a", "")

			clock.advance(supervise.DefaultRepeatWindow - time.Millisecond)
			sup.Handle(session.AdapterEvent{Event: repeatOf(first, "repeat-2")})
			assertAskedAsARepeat(t, engine, sup, "repeat-2", 1)
			if calls := engine.applySnapshot(); len(calls) != 0 {
				t.Fatalf("the policy answered the repeat of a prompt the holder typed into: %#v", calls)
			}

			sup.Handle(session.AdapterEventWithdrawn{Event: repeatOf(first, "repeat-2")})
			clock.advance(time.Millisecond)
			later := repeatOf(first, "later-3")
			later.Sequence = 3
			sup.Handle(session.AdapterEvent{Event: later})
			waitFor(t, 2*time.Second, "the policy's answer once the window is over", func() bool {
				return len(engine.auditFor(audit.KindDelivery, "later-3")) == 1
			})
		})
	}
}

// A prompt raised while the holder's keystrokes are written, once the hand
// was released, waits for them as the policy's answer always does, and is
// then asked rather than answered: the keystrokes count as an answer to it,
// since the holder may have typed ahead of the question reaching the core. A
// prompt without a Signature repeats nothing, and the policy answers it once
// the keystrokes are written (TestAdmittedRawInputHoldsTheSessionsWriteSlot).
func TestAPromptRaisedWhileKeystrokesAreWrittenIsAskedOnceTheyAre(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	sup, _ := newCoreWithOptions(t, engine, supervise.Options{Now: newTestClock().Now}, "agent-a")
	sup.SetHolder("agent-a", "conn-1")
	release := admitted(t, sup, "agent-a", "conn-1")
	sup.SetHolder("agent-a", "")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
	if shown := viewOf(sup, "automatic-1"); shown == nil || !shown.Evaluation.Automatic || shown.DeliveryStatus != "pending" {
		t.Fatalf("the prompt raised while the keystrokes are written = %#v, want it waiting for them", shown)
	}

	release()
	waitFor(t, 2*time.Second, "the prompt to go to the operator", func() bool {
		shown := viewOf(sup, "automatic-1")
		return shown != nil && !shown.Evaluation.Automatic && shown.DeliveryStatus == "pending"
	})
	sup.BeginDrain()
	sup.Wait()
	assertAskedAsARepeat(t, engine, sup, "automatic-1", 2)
	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("the policy answered a prompt the keystrokes may have answered: %#v", calls)
	}
}
