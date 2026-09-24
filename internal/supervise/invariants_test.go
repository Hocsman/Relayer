package supervise_test

import (
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// These tests pin invariants a mutation run of the core found unguarded: the
// state machine was changed one guard at a time, and each change below left
// every test green. Most of the holes predate the move into internal/supervise;
// the desktop's tests never pinned them either. Each test names what it keeps.

// returnsBeforeReaching runs op on a goroutine of its own and returns its
// result. reached is signalled when a call gets past the core to where the
// fixture holds it: an op the core should have refused then fails the test
// with message instead of blocking it.
func returnsBeforeReaching[T any](t *testing.T, reached <-chan T, message string, op func() error) error {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- op() }()
	select {
	case err := <-done:
		return err
	case <-reached:
		t.Fatal(message)
	case <-time.After(2 * time.Second):
		t.Fatalf("neither returned nor got past the core: %s", message)
	}
	return nil
}

// releaser closes release once, from the test or from its cleanup, so a test
// that fails while the fixture holds a call still lets the run drain.
func releaser(t *testing.T, release chan struct{}) func() {
	var once sync.Once
	closeRelease := func() { once.Do(func() { close(release) }) }
	t.Cleanup(closeRelease)
	return closeRelease
}

func indexOf(operations []string, want string) int {
	for index, operation := range operations {
		if operation == want {
			return index
		}
	}
	return -1
}

// An automatic prompt raised after a human one waits for the human's answer:
// the agent reads its answers in order, and an answer written first would
// answer the human's question. Once the human answers, the automatic one goes
// without anything else asking for it. The runtime holds any write until the
// human answers, so a prompt claimed too early is still seen delivering.
func TestAnAutomaticPromptWaitsForAnEarlierHumanOneAndGoesOnceItIsAnswered(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["automatic-2"] = automaticAllow()
	release := make(chan struct{})
	engine.applyRelease = release
	sup, _ := newCoreForTest(t, engine, "agent-a")
	releaseWrites := releaser(t, release)
	automatic := promptEvent("agent-a", "automatic-2")
	automatic.Sequence = 2

	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "human-1")})
	sup.Handle(session.AdapterEvent{Event: automatic})
	for _, view := range sup.State().Pending {
		if view.DeliveryStatus != "pending" {
			t.Fatalf("prompt %s is %s before the human answered", view.ID, view.DeliveryStatus)
		}
	}
	releaseWrites()
	if err := sup.SubmitDecision(testRunID, "agent-a", "human-1", "y", desktop); err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}
	waitFor(t, 2*time.Second, "the queued automatic answer", func() bool {
		return len(engine.auditFor(audit.KindDelivery, "automatic-2")) == 1
	})
	if calls := engine.applySnapshot(); len(calls) != 2 || calls[0].event.ID != "human-1" || calls[1].event.ID != "automatic-2" {
		t.Fatalf("deliveries = %#v, want the human's answer then the automatic one", calls)
	}
}

// An automatic prompt queued behind a human one goes when the agent withdraws
// the human one, with nothing else asking for it: no write was in progress,
// so no write's end will ask; the withdrawal must. Its asking used to be
// tested only where the withdrawn prompt's answer was being written, and
// there the write's end now asks instead, once it has released the session.
func TestAnAutomaticPromptGoesOnceTheHumanOneBeforeItIsWithdrawn(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["automatic-2"] = automaticAllow()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	human := promptEvent("agent-a", "human-1")
	automatic := promptEvent("agent-a", "automatic-2")
	automatic.Sequence = 2
	sup.Handle(session.AdapterEvent{Event: human})
	sup.Handle(session.AdapterEvent{Event: automatic})
	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("deliveries before the withdrawal = %#v, want none", calls)
	}

	sup.Handle(session.AdapterEventWithdrawn{Event: human})
	waitFor(t, 2*time.Second, "the queued automatic answer", func() bool {
		return len(engine.auditFor(audit.KindDelivery, "automatic-2")) == 1
	})
	if calls := engine.applySnapshot(); len(calls) != 1 || calls[0].event.ID != "automatic-2" {
		t.Fatalf("deliveries = %#v, want the automatic answer alone", calls)
	}
}

// Automatic prompts queued behind a human one go in the agent's order, not in
// the order the adapter reported them.
func TestQueuedAutomaticPromptsAreAnsweredInTheAgentsOrder(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["automatic-2"] = automaticAllow()
	engine.evaluationByID["automatic-3"] = automaticAllow()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "human-1")})
	late := promptEvent("agent-a", "automatic-3")
	late.Sequence = 3
	early := promptEvent("agent-a", "automatic-2")
	early.Sequence = 2
	sup.Handle(session.AdapterEvent{Event: late})
	sup.Handle(session.AdapterEvent{Event: early})

	if err := sup.SubmitDecision(testRunID, "agent-a", "human-1", "y", desktop); err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}
	waitFor(t, 2*time.Second, "both automatic answers", func() bool { return len(engine.applySnapshot()) == 3 })
	calls := engine.applySnapshot()
	if calls[0].event.ID != "human-1" || calls[1].event.ID != "automatic-2" || calls[2].event.ID != "automatic-3" {
		t.Fatalf("order = %s, %s, %s, want human-1, automatic-2, automatic-3", calls[0].event.ID, calls[1].event.ID, calls[2].event.ID)
	}
}

// No automatic decision is taken while an agent starts: the prompt the new
// process raised waits, pending, until the start completes. It is not even
// claimed, which would show it delivering on the way.
func TestNoAutomaticDecisionIsTakenWhileTheAgentStarts(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	started := make(chan string, 1)
	release := make(chan struct{})
	engine.agentStartStarted = started
	engine.agentStartRelease = release
	sup, _ := newCoreForTest(t, engine, "agent-a")
	releaseStart := releaser(t, release)
	sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
	done := make(chan error, 1)
	go func() { done <- sup.StartSession(testRunID, "agent-a") }()
	<-started

	prompt := promptEvent("agent-a", "automatic-early")
	prompt.Timestamp = time.Now().UTC()
	sup.Handle(session.AdapterEvent{Event: prompt})
	state := sup.State()
	releaseStart()
	if err := <-done; err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "pending" {
		t.Fatalf("the prompt raised during the start = %#v, want it still pending", state.Pending)
	}
}

// One start at a time: a second Start while the first runs is refused before
// it reaches the runtime, which would otherwise start a second process.
func TestASecondStartWhileTheFirstRunsIsRefused(t *testing.T) {
	engine := newFakeEngine()
	started := make(chan string, 2)
	release := make(chan struct{})
	engine.agentStartStarted = started
	engine.agentStartRelease = release
	sup, _ := newCoreForTest(t, engine, "agent-a")
	releaseStart := releaser(t, release)
	sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
	done := make(chan error, 1)
	go func() { done <- sup.StartSession(testRunID, "agent-a") }()
	<-started

	err := returnsBeforeReaching(t, started, "a second process was started while the first start ran", func() error {
		return sup.StartSession(testRunID, "agent-a")
	})
	if !errors.Is(err, supervise.ErrLineUnavailable) {
		t.Fatalf("second start = %v, want ErrLineUnavailable", err)
	}
	releaseStart()
	if err := <-done; err != nil {
		t.Fatalf("first start: %v", err)
	}
	if _, starts, _ := engine.lifecycleCalls(); starts != 1 {
		t.Fatalf("starts reaching the runtime = %d, want one", starts)
	}
}

// A freeze belongs to the process whose write was uncertain. The replacement
// started after it takes lines again.
func TestAFreezeDoesNotOutliveItsProcess(t *testing.T) {
	engine := newFakeEngine()
	engine.lineErr = errors.New("write failed half way")
	sup, _ := newCoreForTest(t, engine, "agent-a")
	if err := sup.SubmitLine(testRunID, "agent-a", "hello", desktop); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("first line = %v, want ErrDeliveryUncertain", err)
	}
	sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
	if err := sup.StartSession(testRunID, "agent-a"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	engine.set(func(f *fakeEngine) { f.lineErr = nil })

	if agent := agentOf(t, sup, "agent-a"); agent.InputFrozen {
		t.Fatalf("the replacement inherited the freeze: %#v", agent)
	}
	if err := sup.SubmitLine(testRunID, "agent-a", "hello", desktop); err != nil {
		t.Fatalf("a line to the new process = %v, want it sent", err)
	}
}

// A session frozen by an uncertain line takes no automatic decision either:
// the policy's answer would be a second write to a terminal in an unknown
// state. The prompt stays pending, unclaimed.
func TestAFrozenSessionTakesNoAutomaticDecision(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	engine.lineErr = errors.New("write failed half way")
	sup, _ := newCoreForTest(t, engine, "agent-a")
	if err := sup.SubmitLine(testRunID, "agent-a", "hello", desktop); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("line = %v, want ErrDeliveryUncertain", err)
	}

	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
	if state := sup.State(); len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "pending" {
		t.Fatalf("a frozen session claimed an automatic answer: %#v", state.Pending)
	}
	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("a frozen session received %d deliveries", len(calls))
	}
}

// A prompt of an agent that is not running, and not starting, belongs to no
// process: it is dropped, unjournaled, and remembered so a copy is too.
func TestAPromptOfAStoppedAgentIsDropped(t *testing.T) {
	engine := newFakeEngine()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})

	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "after-exit")})
	if ids := pendingIDs(sup); len(ids) != 0 {
		t.Fatalf("pending = %v, want the stopped agent's prompt dropped", ids)
	}
	if detected := engine.auditFor(audit.KindEventDetected, "after-exit"); len(detected) != 0 {
		t.Fatalf("a stopped agent's prompt was journaled: %#v", detected)
	}
}

// A prompt the journal could not record, as detected or as evaluated, offers
// no answer: it is shown failed, the session is frozen, and nothing reaches the
// agent for it. Past a failed detection the policy is not even consulted.
func TestAPromptTheJournalCouldNotRecordOffersNoAnswer(t *testing.T) {
	for _, test := range []struct {
		name   string
		failAt int
	}{
		{name: "detection", failAt: 1},
		{name: "evaluation", failAt: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.auditFailAt = test.failAt
			engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
			sup, _ := newCoreForTest(t, engine, "agent-a")

			sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
			state := sup.State()
			if len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "failed" || len(state.Pending[0].Decisions) != 0 {
				t.Fatalf("prompt the journal could not record = %#v, want it failed with no answer offered", state.Pending)
			}
			// The evaluation's own entry is the one that failed in the second
			// case; in the first, the policy must not have been consulted.
			if evaluated := engine.auditFor(audit.KindPolicyEvaluated, "prompt-1"); len(evaluated) != 0 {
				t.Fatalf("policy entries = %#v, want none", evaluated)
			}
			if agent := agentOf(t, sup, "agent-a"); !agent.InputFrozen {
				t.Fatalf("agent = %#v, want the session frozen", agent)
			}
			if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", desktop); !errors.Is(err, supervise.ErrDeliveryUncertain) {
				t.Fatalf("an answer to it = %v, want ErrDeliveryUncertain", err)
			}
			if calls := engine.applySnapshot(); len(calls) != 0 {
				t.Fatalf("deliveries = %#v, want none", calls)
			}
		})
	}
}

// A withdrawn prompt is remembered: a late copy of it is not taken in again.
func TestAWithdrawnPromptIsNotTakenInAgain(t *testing.T) {
	engine := newFakeEngine()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	prompt := promptEvent("agent-a", "prompt-1")
	sup.Handle(session.AdapterEvent{Event: prompt})
	sup.Handle(session.AdapterEventWithdrawn{Event: prompt})

	sup.Handle(session.AdapterEvent{Event: prompt})
	if ids := pendingIDs(sup); len(ids) != 0 {
		t.Fatalf("pending = %v, want the withdrawn prompt refused", ids)
	}
	if detected := engine.auditFor(audit.KindEventDetected, "prompt-1"); len(detected) != 1 {
		t.Fatalf("the withdrawn prompt was journaled again: %#v", detected)
	}
}

// A withdrawal that arrives while its prompt is still being taken in, by the
// event loop or by a reconciliation on another goroutine, is not lost: the
// prompt is not left pending, and its withdrawal is journaled after the
// entries that took it in. The withdrawal found nothing pending and did
// nothing, and the prompt then waited on the operator for a question the
// agent no longer asked, holding up every automatic answer behind it until
// somebody clicked it, which failed as stale.
func TestAWithdrawalWhileItsPromptIsTakenInIsNotLost(t *testing.T) {
	for _, test := range []struct {
		name    string
		blocked audit.Kind
	}{
		{name: "while its detection is journaled", blocked: audit.KindEventDetected},
		{name: "while its evaluation is journaled", blocked: audit.KindPolicyEvaluated},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluationByID["automatic-2"] = automaticAllow()
			journalStarted := make(chan struct{}, 1)
			journalRelease := make(chan struct{})
			releaseJournal := releaser(t, journalRelease)
			engine.auditBlockKind = test.blocked
			engine.auditBlockEventID = "prompt-1"
			engine.auditStarted = journalStarted
			engine.auditRelease = journalRelease
			sup, sink := newCoreForTest(t, engine, "agent-a")
			prompt := promptEvent("agent-a", "prompt-1")

			handled := make(chan struct{})
			go func() {
				sup.Handle(session.AdapterEvent{Event: prompt})
				close(handled)
			}()
			select {
			case <-journalStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("the prompt's entry was never journaled")
			}
			sup.Handle(session.AdapterEventWithdrawn{Event: prompt})
			releaseJournal()
			select {
			case <-handled:
			case <-time.After(2 * time.Second):
				t.Fatal("the prompt was never taken in")
			}

			if ids := pendingIDs(sup); len(ids) != 0 {
				t.Fatalf("pending = %v, want the withdrawn prompt gone", ids)
			}
			for _, call := range sink.snapshot() {
				if call.kind == "prompt" || call.kind == "notify" {
					t.Fatalf("a prompt withdrawn before it was taken in was shown: %v", trace(sink.snapshot()))
				}
			}
			var journal []string
			for _, entry := range engine.auditSnapshot() {
				if entry.EventID == "prompt-1" {
					journal = append(journal, string(entry.Kind))
				}
			}
			want := []string{string(audit.KindEventDetected), string(audit.KindPolicyEvaluated), string(audit.KindEventWithdrawn)}
			if !reflect.DeepEqual(journal, want) {
				t.Fatalf("the prompt's journal = %v, want %v", journal, want)
			}
			if agent := agentOf(t, sup, "agent-a"); agent.Status != "running" {
				t.Fatalf("the agent after the withdrawal = %#v, want it running", agent)
			}

			// A late copy is refused, and the next prompt is answered as usual.
			sup.Handle(session.AdapterEvent{Event: prompt})
			if ids := pendingIDs(sup); len(ids) != 0 {
				t.Fatalf("pending after a late copy = %v", ids)
			}
			next := promptEvent("agent-a", "automatic-2")
			next.Sequence = 2
			sup.Handle(session.AdapterEvent{Event: next})
			waitFor(t, 2*time.Second, "the next prompt's automatic answer", func() bool {
				return len(engine.auditFor(audit.KindDelivery, "automatic-2")) == 1
			})
		})
	}
}

// A Stop or a Restart the runtime could not confirm leaves the process in an
// unknown state, so the session is frozen: a line is refused as uncertain, not
// merely as unavailable.
func TestAFailedStopOrRestartFreezesTheSession(t *testing.T) {
	for _, test := range []struct {
		name    string
		setup   func(*fakeEngine)
		operate func(*supervise.Supervisor) error
	}{
		{
			name:    "stop",
			setup:   func(f *fakeEngine) { f.stopErr = errors.New("still running") },
			operate: func(sup *supervise.Supervisor) error { return sup.StopSession(testRunID, "agent-a") },
		},
		{
			name:    "restart",
			setup:   func(f *fakeEngine) { f.agentRestartErr = errors.New("still running") },
			operate: func(sup *supervise.Supervisor) error { return sup.RestartSession(testRunID, "agent-a") },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			test.setup(engine)
			sup, _ := newCoreForTest(t, engine, "agent-a")
			if err := test.operate(sup); err == nil {
				t.Fatalf("a failed %s succeeded", test.name)
			}
			if agent := agentOf(t, sup, "agent-a"); !agent.InputFrozen {
				t.Fatalf("agent after a failed %s = %#v, want it frozen", test.name, agent)
			}
			if err := sup.SubmitLine(testRunID, "agent-a", "hello", desktop); !errors.Is(err, supervise.ErrDeliveryUncertain) {
				t.Fatalf("a line after a failed %s = %v, want ErrDeliveryUncertain", test.name, err)
			}
		})
	}
}

// A line being written holds the session: an automatic answer raised meanwhile
// waits, unclaimed, and goes once the line has returned, without anything else
// asking for it.
func TestAnAutomaticAnswerWaitsForALineBeingWrittenAndGoesAfterIt(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	lineStarted := make(chan string, 1)
	release := make(chan struct{})
	engine.lineStarted = lineStarted
	engine.lineRelease = release
	sup, _ := newCoreForTest(t, engine, "agent-a")
	releaseLine := releaser(t, release)
	sent := make(chan error, 1)
	go func() { sent <- sup.SubmitLine(testRunID, "agent-a", "hello", desktop) }()
	select {
	case <-lineStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the line never reached the runtime")
	}

	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
	if state := sup.State(); len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "pending" {
		t.Fatalf("an automatic answer was claimed while a line was written: %#v", state.Pending)
	}
	releaseLine()
	if err := <-sent; err != nil {
		t.Fatalf("SubmitLine: %v", err)
	}
	waitFor(t, 2*time.Second, "the automatic answer after the line", func() bool {
		return len(engine.auditFor(audit.KindDelivery, "automatic-1")) == 1
	})
	operations := engine.operationSnapshot()
	if returned, applied := indexOf(operations, "line:return:agent-a"), indexOf(operations, "apply:start:automatic-1"); returned < 0 || applied < returned {
		t.Fatalf("operations = %v, want the automatic answer written after the line", operations)
	}
}

// A session takes one answer at a time, whoever gives it: two writes to one
// terminal would interleave.
func TestASessionTakesOneAnswerAtATime(t *testing.T) {
	t.Run("an automatic prompt raised while a human answer is written waits for it", func(t *testing.T) {
		engine := newFakeEngine()
		engine.evaluationByID["automatic-1"] = automaticAllow()
		applyStarted := make(chan struct{}, 2)
		release := make(chan struct{})
		engine.applyStarted = applyStarted
		engine.applyRelease = release
		sup, _ := newCoreForTest(t, engine, "agent-a")
		releaseWrites := releaser(t, release)
		human := promptEvent("agent-a", "human-2")
		human.Sequence = 2
		sup.Handle(session.AdapterEvent{Event: human})
		answered := make(chan error, 1)
		go func() { answered <- sup.SubmitDecision(testRunID, "agent-a", "human-2", "y", desktop) }()
		select {
		case <-applyStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("the human answer never reached the runtime")
		}

		// The adapter reports, late, an earlier question the policy decides.
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
		for _, view := range sup.State().Pending {
			if view.ID == "automatic-1" && view.DeliveryStatus != "pending" {
				t.Fatalf("an automatic answer was claimed while a human one was written: %#v", view)
			}
		}
		releaseWrites()
		if err := <-answered; err != nil {
			t.Fatalf("SubmitDecision: %v", err)
		}
		waitFor(t, 2*time.Second, "the automatic answer after the human one", func() bool {
			return len(engine.auditFor(audit.KindDelivery, "automatic-1")) == 1
		})
		operations := engine.operationSnapshot()
		if returned, applied := indexOf(operations, "apply:return:human-2"), indexOf(operations, "apply:start:automatic-1"); returned < 0 || applied < returned {
			t.Fatalf("operations = %v, want the automatic answer written after the human one", operations)
		}
	})
	t.Run("a human answer while an automatic one is written is refused", func(t *testing.T) {
		engine := newFakeEngine()
		engine.evaluationByID["automatic-1"] = automaticAllow()
		applyStarted := make(chan struct{}, 2)
		release := make(chan struct{})
		engine.applyStarted = applyStarted
		engine.applyRelease = release
		sup, _ := newCoreForTest(t, engine, "agent-a")
		releaseWrites := releaser(t, release)
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
		select {
		case <-applyStarted:
		case <-time.After(2 * time.Second):
			t.Fatal("the automatic answer never reached the runtime")
		}
		human := promptEvent("agent-a", "human-2")
		human.Sequence = 2
		sup.Handle(session.AdapterEvent{Event: human})

		err := returnsBeforeReaching(t, applyStarted, "a human answer was written while an automatic one was", func() error {
			return sup.SubmitDecision(testRunID, "agent-a", "human-2", "y", desktop)
		})
		if !errors.Is(err, supervise.ErrDecisionInFlight) {
			t.Fatalf("human answer = %v, want ErrDecisionInFlight", err)
		}
		if decisions := engine.auditFor(audit.KindDecision, "human-2"); len(decisions) != 0 {
			t.Fatalf("the refused answer was journaled: %#v", decisions)
		}
		releaseWrites()
		// The automatic answer holds the session a moment past its journal
		// entry, until its goroutine returns; the human answer goes once it
		// has.
		var answerErr error
		waitFor(t, 2*time.Second, "the session to be free for the human answer", func() bool {
			answerErr = sup.SubmitDecision(testRunID, "agent-a", "human-2", "y", desktop)
			return !errors.Is(answerErr, supervise.ErrDecisionInFlight)
		})
		if answerErr != nil {
			t.Fatalf("the human answer once the session is free = %v", answerErr)
		}
		if deliveries := engine.auditFor(audit.KindDelivery, "automatic-1"); len(deliveries) != 1 {
			t.Fatalf("automatic delivery entries = %#v, want one", deliveries)
		}
	})
}

// A human answer is refused while its agent is being stopped: the process it
// was meant for is going away.
func TestAHumanAnswerIsRefusedWhileTheAgentIsStopped(t *testing.T) {
	engine := newFakeEngine()
	stopStarted := make(chan string, 1)
	release := make(chan struct{})
	engine.stopStarted = stopStarted
	engine.stopRelease = release
	sup, _ := newCoreForTest(t, engine, "agent-a")
	releaseStop := releaser(t, release)
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	stopped := make(chan error, 1)
	go func() { stopped <- sup.StopSession(testRunID, "agent-a") }()
	select {
	case <-stopStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the stop never reached the runtime")
	}

	if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", desktop); !errors.Is(err, supervise.ErrRuntimeStopped) {
		t.Fatalf("an answer during the stop = %v, want ErrRuntimeStopped", err)
	}
	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("an answer reached a stopping agent: %#v", calls)
	}
	if decisions := engine.auditFor(audit.KindDecision, "prompt-1"); len(decisions) != 0 {
		t.Fatalf("the refused answer was journaled: %#v", decisions)
	}
	releaseStop()
	if err := <-stopped; err != nil {
		t.Fatalf("StopSession: %v", err)
	}
}

// A line is refused as unavailable while the agent is being stopped, whatever
// its status shows and whatever it has raised: the process it was meant for is
// going away. The Stop's own claim on the session decides it, before anything
// else. A prompt raised during the Stop used to set the agent waiting over
// "stopping", and its withdrawal running, so the status check alone then let a
// line through to a process on its way out; the status is now kept at
// "stopping" (TestAPromptRaisedWhileTheAgentStopsLeavesItShownStopping), and
// the claim still decides. While such a prompt waits, the line is not sent
// back to it either: no answer can reach it before the Stop ends.
func TestALineIsRefusedWhileTheAgentIsStoppedWhateverItsStatusShows(t *testing.T) {
	for _, withdraw := range []bool{true, false} {
		name := "the prompt it raised is still waiting"
		if withdraw {
			name = "the prompt it raised was withdrawn"
		}
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine()
			stopStarted := make(chan string, 1)
			release := make(chan struct{})
			engine.stopStarted = stopStarted
			engine.stopRelease = release
			sup, sink := newCoreForTest(t, engine, "agent-a")
			releaseStop := releaser(t, release)
			stopped := make(chan error, 1)
			go func() { stopped <- sup.StopSession(testRunID, "agent-a") }()
			select {
			case <-stopStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("the stop never reached the runtime")
			}
			prompt := promptEvent("agent-a", "prompt-1")
			sup.Handle(session.AdapterEvent{Event: prompt})
			if withdraw {
				sup.Handle(session.AdapterEventWithdrawn{Event: prompt})
			}
			sink.reset()

			if err := sup.SubmitLine(testRunID, "agent-a", "hello", desktop); !errors.Is(err, supervise.ErrLineUnavailable) {
				t.Fatalf("a line during the stop = %v, want ErrLineUnavailable", err)
			}
			if lines := engine.lineSnapshot(); len(lines) != 0 {
				t.Fatalf("a line reached a stopping agent: %#v", lines)
			}
			if calls := sink.snapshot(); len(calls) != 0 {
				t.Fatalf("a line refused during the stop showed %v", trace(calls))
			}
			releaseStop()
			if err := <-stopped; err != nil {
				t.Fatalf("StopSession: %v", err)
			}
		})
	}
}

// An agent is neither stopped nor restarted while an answer is written to it:
// the answer's outcome would be lost with the process.
func TestAStopOrRestartIsRefusedWhileAnAnswerIsWritten(t *testing.T) {
	engine := newFakeEngine()
	applyStarted := make(chan struct{}, 1)
	release := make(chan struct{})
	engine.applyStarted = applyStarted
	engine.applyRelease = release
	sup, _ := newCoreForTest(t, engine, "agent-a")
	releaseWrite := releaser(t, release)
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	answered := make(chan error, 1)
	go func() { answered <- sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", desktop) }()
	select {
	case <-applyStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the answer never reached the runtime")
	}

	if err := sup.StopSession(testRunID, "agent-a"); !errors.Is(err, supervise.ErrDecisionInFlight) {
		t.Fatalf("a stop during an answer = %v, want ErrDecisionInFlight", err)
	}
	if err := sup.RestartSession(testRunID, "agent-a"); !errors.Is(err, supervise.ErrDecisionInFlight) {
		t.Fatalf("a restart during an answer = %v, want ErrDecisionInFlight", err)
	}
	if stops, _, restarts := engine.lifecycleCalls(); stops != 0 || restarts != 0 {
		t.Fatalf("stops = %d, restarts = %d reached the runtime during an answer", stops, restarts)
	}
	releaseWrite()
	if err := <-answered; err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}
}

// Nor are they while the holder's keystrokes are being written: the session
// takes one write at a time, and a Stop or a Restart is one. A Restart let the
// old process's keystrokes reach its replacement.
func TestAStopOrRestartIsRefusedWhileTheHoldersKeystrokesAreWritten(t *testing.T) {
	engine := newFakeEngine()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.SetHolder("agent-a", "conn-1")
	release := admitted(t, sup, "agent-a", "conn-1")
	t.Cleanup(release)

	if err := sup.StopSession(testRunID, "agent-a"); !errors.Is(err, supervise.ErrDecisionInFlight) {
		t.Fatalf("a stop during the holder's keystrokes = %v, want ErrDecisionInFlight", err)
	}
	if err := sup.RestartSession(testRunID, "agent-a"); !errors.Is(err, supervise.ErrDecisionInFlight) {
		t.Fatalf("a restart during the holder's keystrokes = %v, want ErrDecisionInFlight", err)
	}
	if stops, _, restarts := engine.lifecycleCalls(); stops != 0 || restarts != 0 {
		t.Fatalf("stops = %d, restarts = %d reached the runtime during the holder's keystrokes", stops, restarts)
	}
	if agent := agentOf(t, sup, "agent-a"); !agent.Running || agent.Status != "running" {
		t.Fatalf("a refused stop changed the agent: %#v", agent)
	}
	release()
	if err := sup.StopSession(testRunID, "agent-a"); err != nil {
		t.Fatalf("a stop once the keystrokes are written = %v", err)
	}
}

// A process that exits while something is being written to its terminal
// leaves the session to that write until it returns, and no new process is
// started meanwhile: whatever the write still sends would reach the
// replacement, and the replacement's first automatic answer would be written
// beside it. The exit used to release a decision's claim on the session at
// once, with the Signature the repeat guard reads from it, and a Start looked
// at no write at all, so a person's answer, the policy's, a line or the
// holder's keystrokes could all still be on their way when the replacement
// came up.
func TestAnExitLeavesTheSessionToTheWriteStillInProgress(t *testing.T) {
	for _, test := range []struct {
		name string
		// write starts a write to agent-a and holds it; it returns what lets
		// the write return.
		write func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func()
	}{
		{name: "a person's answer", write: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func() {
			applyStarted, releaseWrites := holdTheFirstWrite(t, engine)
			sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "asked-1")})
			answered := make(chan error, 1)
			go func() { answered <- sup.SubmitDecision(testRunID, "agent-a", "asked-1", "y", desktop) }()
			awaitWrite(t, applyStarted, "the person's answer")
			return func() {
				releaseWrites()
				if err := <-answered; err != nil {
					t.Fatalf("the person's answer = %v", err)
				}
			}
		}},
		{name: "the policy's answer", write: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func() {
			engine.evaluationByID["automatic-1"] = automaticAllow()
			applyStarted, releaseWrites := holdTheFirstWrite(t, engine)
			sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
			awaitWrite(t, applyStarted, "the policy's answer")
			return func() {
				releaseWrites()
				waitFor(t, 2*time.Second, "the policy's answer to return", func() bool {
					return len(engine.auditFor(audit.KindDelivery, "automatic-1")) == 1
				})
			}
		}},
		{name: "a line", write: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func() {
			return holdWithALine(t, engine, sup, "agent-a")
		}},
		{name: "the holder's keystrokes", write: func(t *testing.T, sup *supervise.Supervisor, _ *fakeEngine) func() {
			sup.SetHolder("agent-a", "conn-1")
			release := admitted(t, sup, "agent-a", "conn-1")
			t.Cleanup(release)
			return release
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			sup, _ := newCoreForTest(t, engine, "agent-a")
			letTheWriteReturn := test.write(t, sup, engine)

			sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
			if agent := agentOf(t, sup, "agent-a"); agent.Running {
				t.Fatalf("the agent after its exit = %#v", agent)
			}
			if err := sup.StartSession(testRunID, "agent-a"); !errors.Is(err, supervise.ErrDecisionInFlight) {
				t.Fatalf("a start with %s in progress = %v, want ErrDecisionInFlight", test.name, err)
			}
			if _, starts, _ := engine.lifecycleCalls(); starts != 0 {
				t.Fatalf("a start reached the runtime with %s in progress", test.name)
			}
			if agent := agentOf(t, sup, "agent-a"); agent.Status != "exited" {
				t.Fatalf("a refused start changed the agent: %#v", agent)
			}

			letTheWriteReturn()
			waitFor(t, 2*time.Second, "the start once the write returned", func() bool {
				err := sup.StartSession(testRunID, "agent-a")
				if err != nil && !errors.Is(err, supervise.ErrDecisionInFlight) {
					t.Fatalf("a start once the write returned = %v", err)
				}
				return err == nil
			})
			if _, starts, _ := engine.lifecycleCalls(); starts != 1 {
				t.Fatalf("starts reaching the runtime = %d, want 1", starts)
			}
		})
	}
}

// While a Stop runs, another Stop, a Restart and a Start of the same agent are
// all refused before they reach the runtime.
func TestWhileAStopRunsTheAgentTakesNoOtherLifecycleOperation(t *testing.T) {
	engine := newFakeEngine()
	stopStarted := make(chan string, 2)
	release := make(chan struct{})
	engine.stopStarted = stopStarted
	engine.stopRelease = release
	sup, _ := newCoreForTest(t, engine, "agent-a")
	releaseStop := releaser(t, release)
	stopped := make(chan error, 1)
	go func() { stopped <- sup.StopSession(testRunID, "agent-a") }()
	select {
	case <-stopStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the stop never reached the runtime")
	}

	err := returnsBeforeReaching(t, stopStarted, "a second stop reached the runtime", func() error {
		return sup.StopSession(testRunID, "agent-a")
	})
	if !errors.Is(err, supervise.ErrLineUnavailable) {
		t.Fatalf("a second stop = %v, want ErrLineUnavailable", err)
	}
	if err := sup.RestartSession(testRunID, "agent-a"); !errors.Is(err, supervise.ErrLineUnavailable) {
		t.Fatalf("a restart during the stop = %v, want ErrLineUnavailable", err)
	}
	if err := sup.StartSession(testRunID, "agent-a"); !errors.Is(err, supervise.ErrLineUnavailable) {
		t.Fatalf("a start during the stop = %v, want ErrLineUnavailable", err)
	}
	if stops, starts, restarts := engine.lifecycleCalls(); stops != 1 || starts != 0 || restarts != 0 {
		t.Fatalf("stops, starts, restarts reaching the runtime = %d, %d, %d, want 1, 0, 0", stops, starts, restarts)
	}
	releaseStop()
	if err := <-stopped; err != nil {
		t.Fatalf("StopSession: %v", err)
	}
}

// The failure paths call the sink without the core's lock too.
// TestTheSinkIsNeverCalledUnderTheCoreLock reaches the paths its scenario
// takes; each case here takes one it does not. The sink reads the core from
// every callback, and a call made under the lock deadlocks on the watchdog.
func TestTheSinkIsNeverCalledUnderTheCoreLockOnTheFailurePaths(t *testing.T) {
	for _, test := range []struct {
		name    string
		setup   func(*fakeEngine)
		operate func(*supervise.Supervisor)
		reached func(supervise.State) bool
	}{
		{
			name: "an automatic answer the adapter cannot encode falls back to ask",
			setup: func(f *fakeEngine) {
				f.evaluation = automaticAllow()
				f.applyErr = adapters.ErrDecisionUnsupported
			},
			operate: func(sup *supervise.Supervisor) {
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
			},
			reached: func(state supervise.State) bool {
				return len(state.Pending) == 1 && state.Pending[0].Evaluation.Reason == "fallback_unsupported"
			},
		},
		{
			name: "an uncertain automatic delivery freezes the session",
			setup: func(f *fakeEngine) {
				f.evaluation = automaticAllow()
				f.applyErr = errors.New("write failed half way")
			},
			operate: func(sup *supervise.Supervisor) {
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
			},
			reached: func(state supervise.State) bool {
				return len(state.Pending) == 1 && state.Pending[0].DeliveryStatus == "uncertain"
			},
		},
		{
			name:    "an uncertain line freezes the session",
			setup:   func(f *fakeEngine) { f.lineErr = errors.New("write failed half way") },
			operate: func(sup *supervise.Supervisor) { _ = sup.SubmitLine(testRunID, "agent-a", "hello", desktop) },
			reached: func(state supervise.State) bool { return state.Agents[0].InputFrozen },
		},
		{
			name:    "a failed stop freezes the session",
			setup:   func(f *fakeEngine) { f.stopErr = errors.New("still running") },
			operate: func(sup *supervise.Supervisor) { _ = sup.StopSession(testRunID, "agent-a") },
			reached: func(state supervise.State) bool { return state.Agents[0].InputFrozen },
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			test.setup(engine)
			sup, sink := newCoreForTest(t, engine, "agent-a")
			sink.mu.Lock()
			sink.probe = func() {
				_ = sup.State()
				_, _ = sup.Agent("agent-a")
				_ = sup.Draining()
			}
			sink.mu.Unlock()

			reached := make(chan bool, 1)
			go func() {
				test.operate(sup)
				// Not waitFor: this goroutine is not the test's. The state is
				// read through the lock, so a deadlocked sink call stops it too.
				deadline := time.Now().Add(2 * time.Second)
				for !test.reached(sup.State()) && time.Now().Before(deadline) {
					time.Sleep(time.Millisecond)
				}
				reached <- test.reached(sup.State())
			}()
			select {
			case ok := <-reached:
				if !ok {
					t.Fatal("the scenario never reached its failure path")
				}
			case <-time.After(10 * time.Second):
				t.Fatal("a sink call deadlocked on the core's lock")
			}
		})
	}
}

// A copy of a prompt that arrives while the first is still being taken in, its
// detection being journaled, is refused: it is neither journaled nor shown a
// second time.
func TestACopyOfAPromptArrivingWhileItIsTakenInIsRefused(t *testing.T) {
	engine := newFakeEngine()
	auditStarted := make(chan struct{}, 2)
	release := make(chan struct{})
	engine.auditBlockKind = audit.KindEventDetected
	engine.auditStarted = auditStarted
	engine.auditRelease = release
	sup, _ := newCoreForTest(t, engine, "agent-a")
	releaseJournal := releaser(t, release)
	prompt := promptEvent("agent-a", "prompt-1")
	first := make(chan struct{})
	go func() {
		defer close(first)
		sup.Handle(session.AdapterEvent{Event: prompt})
	}()
	select {
	case <-auditStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("the first copy never reached the journal")
	}

	_ = returnsBeforeReaching(t, auditStarted, "a second copy of the prompt was journaled", func() error {
		sup.Handle(session.AdapterEvent{Event: prompt})
		return nil
	})
	releaseJournal()
	select {
	case <-first:
	case <-time.After(2 * time.Second):
		t.Fatal("the first copy was never taken in")
	}
	if detected := engine.auditFor(audit.KindEventDetected, "prompt-1"); len(detected) != 1 {
		t.Fatalf("detections = %#v, want one", detected)
	}
	if ids := pendingIDs(sup); len(ids) != 1 {
		t.Fatalf("pending = %v, want the prompt once", ids)
	}
}

// Every answered prompt is remembered, not only the last one: a late copy of
// any of them is refused.
func TestEveryAnsweredPromptStaysAnswered(t *testing.T) {
	engine := newFakeEngine()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	first := promptEvent("agent-a", "prompt-1")
	second := promptEvent("agent-a", "prompt-2")
	second.Sequence = 2
	for _, prompt := range []adapters.Event{first, second} {
		sup.Handle(session.AdapterEvent{Event: prompt})
		if err := sup.SubmitDecision(testRunID, "agent-a", prompt.ID, "y", desktop); err != nil {
			t.Fatalf("answering %s: %v", prompt.ID, err)
		}
	}

	sup.Handle(session.AdapterEvent{Event: first})
	sup.Handle(session.AdapterEvent{Event: second})
	if ids := pendingIDs(sup); len(ids) != 0 {
		t.Fatalf("late copies taken in again: %v", ids)
	}
	if detected := engine.auditFor(audit.KindEventDetected, "prompt-1"); len(detected) != 1 {
		t.Fatalf("the first answered prompt was journaled again: %#v", detected)
	}
}

// A start drops the previous process's prompts: those a Stop left pending,
// from the moment the start begins, and those raised while it ran but
// detected before it, once it completes. None of them can be answered by the
// new process.
func TestAStartDropsThePreviousProcesssPrompts(t *testing.T) {
	t.Run("left pending by a stop", func(t *testing.T) {
		engine := newFakeEngine()
		started := make(chan string, 1)
		release := make(chan struct{})
		engine.agentStartStarted = started
		engine.agentStartRelease = release
		sup, _ := newCoreForTest(t, engine, "agent-a")
		releaseStart := releaser(t, release)
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
		if err := sup.StopSession(testRunID, "agent-a"); err != nil {
			t.Fatalf("StopSession: %v", err)
		}
		// Its prompts stay until the process's exit arrives, which here it
		// does not.
		if ids := pendingIDs(sup); len(ids) != 1 {
			t.Fatalf("pending after the stop = %v, want the prompt kept", ids)
		}
		done := make(chan error, 1)
		go func() { done <- sup.StartSession(testRunID, "agent-a") }()
		<-started

		if ids := pendingIDs(sup); len(ids) != 0 {
			t.Fatalf("pending while the replacement starts = %v, want the stopped process's prompts gone", ids)
		}
		releaseStart()
		if err := <-done; err != nil {
			t.Fatalf("StartSession: %v", err)
		}
	})
	t.Run("detected before the start", func(t *testing.T) {
		engine := newFakeEngine()
		started := make(chan string, 1)
		release := make(chan struct{})
		engine.agentStartStarted = started
		engine.agentStartRelease = release
		sup, _ := newCoreForTest(t, engine, "agent-a")
		releaseStart := releaser(t, release)
		sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
		done := make(chan error, 1)
		go func() { done <- sup.StartSession(testRunID, "agent-a") }()
		<-started
		// promptEvent's time is long past: the previous process raised it.
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-previous")})
		if ids := pendingIDs(sup); len(ids) != 1 {
			t.Fatalf("pending while the agent starts = %v, want the prompt taken in", ids)
		}

		releaseStart()
		if err := <-done; err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		if ids := pendingIDs(sup); len(ids) != 0 {
			t.Fatalf("pending after the start = %v, want the prompt detected before it dropped", ids)
		}
	})
}

// A prompt an exit cleared stays answered: a late copy of it that arrives
// after the agent started again is the previous process's, and is refused.
func TestAPromptAnExitClearedIsRefusedAfterTheNextStart(t *testing.T) {
	engine := newFakeEngine()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	prompt := promptEvent("agent-a", "prompt-1")
	sup.Handle(session.AdapterEvent{Event: prompt})
	sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
	if err := sup.StartSession(testRunID, "agent-a"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	sup.Handle(session.AdapterEvent{Event: prompt})
	if ids := pendingIDs(sup); len(ids) != 0 {
		t.Fatalf("pending = %v, want the cleared prompt refused", ids)
	}
	if detected := engine.auditFor(audit.KindEventDetected, "prompt-1"); len(detected) != 1 {
		t.Fatalf("the cleared prompt was journaled again: %#v", detected)
	}
}

// A line is refused while a prompt waits, and the core then asks the runtime
// which prompt the agent shows now, which it may not have heard of yet: the
// operator sees what to answer before sending a line again.
func TestALineRefusedForAWaitingPromptBringsInThePromptTheAgentShows(t *testing.T) {
	engine := newFakeEngine()
	current := promptEvent("agent-a", "prompt-2")
	current.Sequence = 2
	engine.pending["agent-a"] = &current
	sup, sink := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	sink.reset()

	if err := sup.SubmitLine(testRunID, "agent-a", "hello", desktop); !errors.Is(err, supervise.ErrLinePromptPending) {
		t.Fatalf("SubmitLine = %v, want ErrLinePromptPending", err)
	}
	if ids := pendingIDs(sup); len(ids) != 2 || ids[0] != "prompt-1" || ids[1] != "prompt-2" {
		t.Fatalf("pending = %v, want the prompt the agent shows brought in", ids)
	}
	if lines := engine.lineSnapshot(); len(lines) != 0 {
		t.Fatalf("lines = %#v, want none", lines)
	}
	refused := false
	for _, call := range sink.snapshot() {
		refused = refused || (call.kind == "error" && call.failure.Code == "line_prompt_pending")
	}
	if !refused {
		t.Fatalf("the refusal was not shown: %v", trace(sink.snapshot()))
	}
}

// An automatic decision the journal could not record is not attempted: the
// prompt is failed for the journal, not merely cancelled, and no delivery is
// journaled for it.
func TestAnAutomaticDecisionTheJournalCouldNotRecordIsNotAttempted(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	// The prompt's detection and evaluation are journaled; its decision is not.
	engine.auditFailAt = 3
	sup, _ := newCoreForTest(t, engine, "agent-a")

	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
	waitFor(t, 2*time.Second, "the prompt to fail", func() bool {
		pending := sup.State().Pending
		return len(pending) == 1 && pending[0].DeliveryStatus == "failed"
	})
	sup.BeginDrain()
	sup.Wait()
	if view := sup.State().Pending[0]; view.Evaluation.Reason != "audit_unavailable" {
		t.Fatalf("prompt = %#v, want it failed for the journal", view)
	}
	if deliveries := engine.auditFor(audit.KindDelivery, "automatic-1"); len(deliveries) != 0 {
		t.Fatalf("delivery entries = %#v, want none", deliveries)
	}
	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("deliveries = %#v, want none", calls)
	}
}

// No automatic decision is started once the journal has failed, not even for
// an agent that was starting when it failed: that agent was not running, so
// nothing froze it, and its start clears what froze it anyway.
func TestNoAutomaticDecisionIsStartedAfterTheJournalFails(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["automatic-1"] = automaticAllow()
	started := make(chan string, 1)
	release := make(chan struct{})
	engine.agentStartStarted = started
	engine.agentStartRelease = release
	sup, _ := newCoreForTest(t, engine, "agent-a", "agent-b")
	releaseStart := releaser(t, release)
	sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
	done := make(chan error, 1)
	go func() { done <- sup.StartSession(testRunID, "agent-a") }()
	<-started
	engine.set(func(f *fakeEngine) { f.auditFailAt = f.auditCalls + 1 })
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-b", "journal-fails")})
	if !sup.State().AuditFailed {
		t.Fatal("the journal did not fail")
	}
	// The fixture's journal takes entries again; the core must not rely on it.
	prompt := promptEvent("agent-a", "automatic-1")
	prompt.Timestamp = time.Now().UTC()
	sup.Handle(session.AdapterEvent{Event: prompt})

	releaseStart()
	if err := <-done; err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	sup.BeginDrain()
	sup.Wait()
	if decisions := engine.auditFor(audit.KindDecision, "automatic-1"); len(decisions) != 0 {
		t.Fatalf("an automatic decision was started after the journal failed: %#v", decisions)
	}
	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("deliveries = %#v, want none", calls)
	}
}

// A drain starts no automatic decision, not even for a start it admitted
// before it began and that completes during it: the prompt the new process
// raised stays pending, and nothing is journaled for it.
func TestADrainStartsNoAutomaticDecision(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	started := make(chan string, 1)
	release := make(chan struct{})
	engine.agentStartStarted = started
	engine.agentStartRelease = release
	sup, _ := newCoreForTest(t, engine, "agent-a")
	releaseStart := releaser(t, release)
	sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
	done := make(chan error, 1)
	go func() { done <- sup.StartSession(testRunID, "agent-a") }()
	<-started
	prompt := promptEvent("agent-a", "automatic-1")
	prompt.Timestamp = time.Now().UTC()
	sup.Handle(session.AdapterEvent{Event: prompt})

	sup.BeginDrain()
	releaseStart()
	if err := <-done; err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	sup.Wait()
	if state := sup.State(); len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "pending" {
		t.Fatalf("the prompt after the drain = %#v, want it still pending", state.Pending)
	}
	if decisions := engine.auditFor(audit.KindDecision, "automatic-1"); len(decisions) != 0 {
		t.Fatalf("an automatic decision was started during the drain: %#v", decisions)
	}
}

// Once the journal fails, the gate a drain closes is closed too: no write the
// core does not make itself, a terminal resize for instance, is admitted.
func TestAFailedJournalAdmitsNoOtherWrite(t *testing.T) {
	engine := newFakeEngine()
	engine.auditFailAt = 1
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	if !sup.State().AuditFailed {
		t.Fatal("the journal did not fail")
	}

	if release, admitted := sup.AdmitRun(); admitted {
		release()
		t.Fatal("a write was admitted after the journal failed")
	}
}
