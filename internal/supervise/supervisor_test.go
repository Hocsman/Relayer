package supervise_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// These tests pin the supervision core as it was moved out of the desktop,
// through its exported API only: the one a second front end will use. Where
// they pinned a behaviour already known to be wrong, a comment beginning with
// DEFECT named the fix that had to flip them; each such test was flipped by
// its fix, and its comment now says what it used to do. A new pin of a known
// defect is marked the same way.

// A front end's event loop feeds Handle from the run's session stream. The
// sink sees each event's emissions in the order the desktop always made them,
// and the output invalidation, which stays with the front end, shows nothing.
func TestHandleTakesTheSessionStreamFromAChannel(t *testing.T) {
	engine := newFakeEngine()
	sup, sink := newCoreForTest(t, engine, "agent-a")
	events := make(chan session.Event)
	done := make(chan struct{})
	go func() {
		defer close(done)
		for message := range events {
			sup.Handle(message)
		}
	}()

	first := promptEvent("agent-a", "prompt-1")
	second := promptEvent("agent-a", "prompt-2")
	second.Sequence = 2
	for _, message := range []session.Event{
		session.OutputAvailable{SessionID: "agent-a"},
		session.AdapterEvent{Event: first},
		session.AdapterEventWithdrawn{Event: first},
		session.AdapterEvent{Event: second},
		session.Error{SessionID: "agent-a", Err: errors.New("stream closed")},
		session.Exited{SessionID: "agent-a"},
	} {
		select {
		case events <- message:
		case <-time.After(2 * time.Second):
			t.Fatalf("the loop stopped taking events at %T", message)
		}
	}
	close(events)
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("the loop did not finish")
	}

	var kinds []audit.Kind
	for _, entry := range engine.auditSnapshot() {
		kinds = append(kinds, entry.Kind)
	}
	wantKinds := []audit.Kind{
		audit.KindEventDetected, audit.KindPolicyEvaluated, audit.KindEventWithdrawn,
		audit.KindEventDetected, audit.KindPolicyEvaluated, audit.KindBackendError,
	}
	if !reflect.DeepEqual(kinds, wantKinds) {
		t.Fatalf("journal = %v, want %v", kinds, wantKinds)
	}
	wantTrace := []string{
		"refresh", "prompt:pending", "notify:pending_decision",
		"prompt:delivered", "status:running", "refresh",
		"refresh", "prompt:pending", "notify:pending_decision",
		"status:failed", "error:backend_stream_failed",
		"status:failed",
	}
	if got := trace(sink.snapshot()); !reflect.DeepEqual(got, wantTrace) {
		t.Fatalf("sink = %v, want %v", got, wantTrace)
	}
	if agent := agentOf(t, sup, "agent-a"); agent.Running || agent.Status != "failed" {
		t.Fatalf("agent after the legacy exit = %#v", agent)
	}
	if ids := pendingIDs(sup); len(ids) != 0 {
		t.Fatalf("pending after the legacy exit = %v", ids)
	}
}

// ErrEventMismatch on the automatic path: the agent no longer shows the prompt
// the policy answered. Nothing was written; the delivery is journaled stale,
// the prompt is resolved and the core asks the runtime what is pending now.
func TestAStaleAutomaticDeliveryIsJournaledAndReconciled(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["prompt-1"] = automaticAllow()
	engine.applyErrs = []error{adapters.ErrEventMismatch}
	current := promptEvent("agent-a", "prompt-2")
	current.Sequence = 2
	engine.pending["agent-a"] = &current
	sup, _ := newCoreForTest(t, engine, "agent-a")

	stale := promptEvent("agent-a", "prompt-1")
	sup.Handle(session.AdapterEvent{Event: stale})
	waitFor(t, 2*time.Second, "the reconciled prompt", func() bool {
		ids := pendingIDs(sup)
		return len(ids) == 1 && ids[0] == "prompt-2" && len(engine.auditFor(audit.KindDelivery, "prompt-1")) == 1
	})

	deliveries := engine.auditFor(audit.KindDelivery, "prompt-1")
	if delivery := deliveries[0]; delivery.Outcome != audit.OutcomeFallbackStale || delivery.Reason != "fallback_stale" ||
		delivery.DecisionBy != audit.DecisionByPolicy || delivery.Decision != audit.DecisionAllow {
		t.Fatalf("stale delivery entry = %#v", delivery)
	}
	if decisions := engine.auditFor(audit.KindDecision, "prompt-1"); len(decisions) != 1 || decisions[0].DecisionBy != audit.DecisionByPolicy {
		t.Fatalf("decision entries = %#v", decisions)
	}
	if detected := engine.auditFor(audit.KindEventDetected, "prompt-2"); len(detected) != 1 {
		t.Fatalf("the reconciled prompt was not journaled: %#v", detected)
	}
	if calls := engine.applySnapshot(); len(calls) != 1 || calls[0].event.ID != "prompt-1" || calls[0].decision != adapters.DecisionAllow {
		t.Fatalf("deliveries = %#v, want the one stale attempt", calls)
	}
	view := sup.State().Pending[0]
	if view.DeliveryStatus != "pending" || view.Evaluation.Action != string(policy.ActionAsk) {
		t.Fatalf("reconciled prompt = %#v", view)
	}
	if agent := agentOf(t, sup, "agent-a"); agent.Status != "waiting" || agent.InputFrozen {
		t.Fatalf("agent after reconciliation = %#v", agent)
	}

	// The stale prompt stays answered: a late copy is refused.
	sup.Handle(session.AdapterEvent{Event: stale})
	if ids := pendingIDs(sup); len(ids) != 1 || ids[0] != "prompt-2" {
		t.Fatalf("pending after a late copy = %v", ids)
	}
	if detected := engine.auditFor(audit.KindEventDetected, "prompt-1"); len(detected) != 1 {
		t.Fatalf("a late copy of the stale prompt was taken in again: %#v", detected)
	}
}

// ErrEventMismatch on the human path: the operator's answer is refused as
// stale, journaled so, and the prompt the agent shows now takes its place.
func TestAStaleHumanDecisionIsJournaledAndReconciled(t *testing.T) {
	engine := newFakeEngine()
	engine.applyErr = adapters.ErrEventMismatch
	current := promptEvent("agent-a", "prompt-2")
	current.Sequence = 2
	engine.pending["agent-a"] = &current
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})

	err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", desktop)
	if !errors.Is(err, supervise.ErrDecisionStale) {
		t.Fatalf("SubmitDecision = %v, want ErrDecisionStale", err)
	}
	decisions := engine.auditFor(audit.KindDecision, "prompt-1")
	if len(decisions) != 1 || decisions[0].DecisionBy != audit.DecisionByHuman || decisions[0].Decision != audit.DecisionAsk {
		t.Fatalf("decision entries = %#v", decisions)
	}
	deliveries := engine.auditFor(audit.KindDelivery, "prompt-1")
	if len(deliveries) != 1 || deliveries[0].Outcome != audit.OutcomeFallbackStale ||
		deliveries[0].Reason != "fallback_stale" || deliveries[0].DecisionBy != audit.DecisionByHuman {
		t.Fatalf("delivery entries = %#v", deliveries)
	}
	if ids := pendingIDs(sup); len(ids) != 1 || ids[0] != "prompt-2" {
		t.Fatalf("pending after a stale answer = %v, want the prompt the agent shows", ids)
	}
	if agent := agentOf(t, sup, "agent-a"); agent.InputFrozen {
		t.Fatalf("a stale answer froze the session: %#v", agent)
	}
	if calls := engine.applySnapshot(); len(calls) != 1 || calls[0].decision != adapters.DecisionManual || calls[0].manualInput != "y" {
		t.Fatalf("deliveries = %#v", calls)
	}
}

// An automatic delivery that fails for any other reason may have written part
// of the answer. The session is frozen so nothing is sent a second time, and
// the failure is shown by a fixed message, never the error's own text.
func TestAnUncertainAutomaticDeliveryFreezesTheSession(t *testing.T) {
	const detail = "write /dev/pts/3: secret-transport-detail"
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	engine.applyErr = errors.New(detail)
	sup, sink := newCoreForTest(t, engine, "agent-a")

	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	waitFor(t, 2*time.Second, "the uncertainty to be reported", func() bool {
		for _, call := range sink.snapshot() {
			if call.kind == "error" && call.failure.Code == "delivery_uncertain" {
				return true
			}
		}
		return false
	})

	deliveries := engine.auditFor(audit.KindDelivery, "prompt-1")
	if len(deliveries) != 1 || deliveries[0].Outcome != audit.OutcomeFallbackDeliveryUncertain ||
		deliveries[0].Reason != "delivery_uncertain" || deliveries[0].DecisionBy != audit.DecisionByPolicy {
		t.Fatalf("delivery entries = %#v", deliveries)
	}
	state := sup.State()
	if len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "uncertain" || state.Pending[0].Evaluation.Reason != "delivery_uncertain" {
		t.Fatalf("prompt after an uncertain delivery = %#v", state.Pending)
	}
	if agent := agentOf(t, sup, "agent-a"); !agent.InputFrozen {
		t.Fatalf("an uncertain delivery left the session writable: %#v", agent)
	}
	for _, call := range sink.snapshot() {
		if call.kind == "error" && (call.failure.Message != "Delivery is indeterminate. The session is frozen to prevent a second answer." ||
			call.failure.SessionID != "agent-a" || call.failure.RunID != testRunID) {
			t.Fatalf("uncertainty error = %#v", call.failure)
		}
	}

	if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", desktop); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("a human answer after the uncertainty = %v, want ErrDeliveryUncertain", err)
	}
	if err := sup.SubmitLine(testRunID, "agent-a", "hello", desktop); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("a line after the uncertainty = %v, want ErrDeliveryUncertain", err)
	}
	second := promptEvent("agent-a", "prompt-2")
	second.Sequence = 2
	sup.Handle(session.AdapterEvent{Event: second})
	time.Sleep(20 * time.Millisecond)
	if calls := engine.applySnapshot(); len(calls) != 1 {
		t.Fatalf("a frozen session received %d deliveries, want the one attempt", len(calls))
	}
	if lines := engine.lineSnapshot(); len(lines) != 0 {
		t.Fatalf("a frozen session received lines: %#v", lines)
	}
	assertJSONDoesNotContain(t, engine.auditSnapshot(), "secret-transport-detail")
	if text := sinkText(sink.snapshot()); len(text) == 0 || strings.Contains(text, "secret-transport-detail") {
		t.Fatalf("the transport error reached the sink or nothing was shown: %s", text)
	}
}

// A human answer whose write fails for any reason but an answer the adapter
// cannot encode, or a prompt the agent no longer shows, may have reached the
// agent in part, as an automatic one may: the session is frozen, the caller
// is told the delivery is uncertain and the failure is shown by a fixed
// message. This was pinned only through the answer the adapter cannot
// encode, which the human path wrongly treated the same way; it no longer
// does (TestAHumanAnswerTheAdapterCannotEncodeGoesBackToTheOperator).
func TestAnUncertainHumanDeliveryFreezesTheSession(t *testing.T) {
	const detail = "write /dev/pts/3: secret-transport-detail"
	engine := newFakeEngine()
	engine.applyErr = errors.New(detail)
	sup, sink := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	sink.reset()

	if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", desktop); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("SubmitDecision = %v, want ErrDeliveryUncertain", err)
	}
	deliveries := engine.auditFor(audit.KindDelivery, "prompt-1")
	if len(deliveries) != 1 || deliveries[0].Outcome != audit.OutcomeFallbackDeliveryUncertain ||
		deliveries[0].Reason != "delivery_uncertain" || deliveries[0].DecisionBy != audit.DecisionByHuman {
		t.Fatalf("delivery entries = %#v", deliveries)
	}
	state := sup.State()
	if len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "uncertain" || state.Pending[0].Evaluation.Reason != "delivery_uncertain" {
		t.Fatalf("prompt after an uncertain delivery = %#v", state.Pending)
	}
	if agent := agentOf(t, sup, "agent-a"); !agent.InputFrozen {
		t.Fatalf("an uncertain delivery left the session writable: %#v", agent)
	}
	if got := trace(sink.snapshot()); !reflect.DeepEqual(got, []string{"prompt:delivering", "prompt:uncertain", "error:delivery_uncertain"}) {
		t.Fatalf("sink = %v", got)
	}
	if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "n", desktop); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("a second answer after the uncertainty = %v, want ErrDeliveryUncertain", err)
	}
	if err := sup.SubmitLine(testRunID, "agent-a", "hello", desktop); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("a line after the uncertainty = %v, want ErrDeliveryUncertain", err)
	}
	if calls := engine.applySnapshot(); len(calls) != 1 {
		t.Fatalf("a frozen session received %d deliveries, want the one attempt", len(calls))
	}
	if strings.Contains(sinkText(sink.snapshot()), "secret-transport-detail") {
		t.Fatal("the transport error reached the sink")
	}
}

// An uncertain write freezes its session even when the agent withdrew the
// prompt while its answer was being written, which is when an echo is most
// likely read as a new question. The freeze is there to stop a second answer
// after a write that may have reached the agent in part, and the prompt it
// answered has nothing to do with that: the session is frozen, the failure is
// shown, and a person answering gets ErrDeliveryUncertain as the message they
// are shown says. The freeze used to be set on the prompt's way to
// "uncertain", which a withdrawn prompt no longer takes: the session stayed
// writable, the next prompt's automatic answer was written after the
// uncertain one, and a person was told the session was frozen when it was
// not.
func TestAnUncertainWriteFreezesTheSessionEvenWhenItsPromptWasWithdrawn(t *testing.T) {
	for _, human := range []bool{false, true} {
		name := "an automatic answer"
		if human {
			name = "a human answer"
		}
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluationByID["prompt-2"] = automaticAllow()
			if !human {
				engine.evaluationByID["prompt-1"] = automaticAllow()
			}
			engine.applyErrs = []error{errors.New("write timed out")}
			applyStarted, releaseWrites := holdTheFirstWrite(t, engine)
			sup, sink := newCoreForTest(t, engine, "agent-a")

			first := promptEvent("agent-a", "prompt-1")
			sup.Handle(session.AdapterEvent{Event: first})
			answered := make(chan error, 1)
			if human {
				go func() { answered <- sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", desktop) }()
			}
			awaitWrite(t, applyStarted, "the first answer")
			sup.Handle(session.AdapterEventWithdrawn{Event: first})
			second := promptEvent("agent-a", "prompt-2")
			second.Sequence = 2
			sup.Handle(session.AdapterEvent{Event: second})
			sink.reset()

			releaseWrites()
			if human {
				if err := <-answered; !errors.Is(err, supervise.ErrDeliveryUncertain) {
					t.Fatalf("the uncertain human answer = %v, want ErrDeliveryUncertain", err)
				}
			}
			waitFor(t, 2*time.Second, "the uncertainty to be shown", func() bool {
				for _, call := range sink.snapshot() {
					if call.kind == "error" && call.failure.Code == "delivery_uncertain" && call.failure.SessionID == "agent-a" {
						return true
					}
				}
				return false
			})
			deliveries := engine.auditFor(audit.KindDelivery, "prompt-1")
			if len(deliveries) != 1 || deliveries[0].Outcome != audit.OutcomeFallbackDeliveryUncertain {
				t.Fatalf("the withdrawn prompt's delivery entries = %#v, want one uncertain", deliveries)
			}
			if agent := agentOf(t, sup, "agent-a"); !agent.InputFrozen {
				t.Fatalf("an uncertain write left the session writable: %#v", agent)
			}
			time.Sleep(50 * time.Millisecond)
			if calls := engine.applySnapshot(); len(calls) != 1 {
				t.Fatalf("the session took %d writes, want the uncertain one alone", len(calls))
			}
			if decisions := engine.auditFor(audit.KindDecision, "prompt-2"); len(decisions) != 0 {
				t.Fatalf("the next prompt was decided after an uncertain write: %#v", decisions)
			}
			if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-2", "y", desktop); !errors.Is(err, supervise.ErrDeliveryUncertain) {
				t.Fatalf("a human answer after the uncertain write = %v, want ErrDeliveryUncertain", err)
			}
			if err := sup.SubmitLine(testRunID, "agent-a", "hello", desktop); !errors.Is(err, supervise.ErrDeliveryUncertain) {
				t.Fatalf("a line after the uncertain write = %v, want ErrDeliveryUncertain", err)
			}
			if calls := engine.applySnapshot(); len(calls) != 1 {
				t.Fatalf("the session took %d writes, want the uncertain one alone", len(calls))
			}
		})
	}
}

// A backend stream error on a live session journals backend_error and marks
// the agent failed. It is not an exit: the process may still run, so the
// agent stays running and its prompts stay answerable.
func TestABackendStreamErrorIsJournaledAndLeavesPromptsAnswerable(t *testing.T) {
	const detail = "read /dev/ptmx: secret-stream-detail"
	engine := newFakeEngine()
	sup, sink := newCoreForTest(t, engine, "Agent-A")
	sup.Handle(session.AdapterEvent{Event: promptEvent("Agent-A", "prompt-1")})
	before := len(engine.auditSnapshot())
	sink.reset()

	sup.Handle(session.Error{SessionID: "Agent-A", Err: errors.New(detail)})

	entries := engine.auditSnapshot()[before:]
	want := audit.Entry{
		Kind:       audit.KindBackendError,
		SessionID:  "Agent-A",
		AgentID:    "Agent-A",
		Backend:    "pty",
		DecisionBy: audit.DecisionBySystem,
		Outcome:    audit.OutcomeFailed,
		Reason:     "backend_stream_failed",
	}
	if len(entries) != 1 || !reflect.DeepEqual(entries[0], want) {
		t.Fatalf("stream error journal = %#v, want %#v", entries, want)
	}
	calls := sink.snapshot()
	if got := trace(calls); !reflect.DeepEqual(got, []string{"status:failed", "error:backend_stream_failed"}) {
		t.Fatalf("sink = %v", got)
	}
	if calls[0].status.ClearedBefore != "" || calls[0].status.Scope != "session" || calls[0].status.SessionID != "Agent-A" {
		t.Fatalf("stream error status = %#v, want a session status that clears nothing", calls[0].status)
	}
	if calls[1].failure.Message != "The backend stream failed." || calls[1].failure.SessionID != "Agent-A" {
		t.Fatalf("stream error = %#v", calls[1].failure)
	}
	if agent := agentOf(t, sup, "agent-a"); !agent.Running || agent.Status != "failed" || agent.InputFrozen {
		t.Fatalf("agent after a stream error = %#v", agent)
	}
	if ids := pendingIDs(sup); len(ids) != 1 || ids[0] != "prompt-1" {
		t.Fatalf("pending after a stream error = %v", ids)
	}
	if strings.Contains(sinkText(calls), "secret-stream-detail") {
		t.Fatal("the stream error's text reached the sink")
	}
	if err := sup.SubmitDecision(testRunID, "Agent-A", "prompt-1", "y", desktop); err != nil {
		t.Fatalf("the prompt was not answerable after a stream error: %v", err)
	}
}

// session.Exited is the legacy exit a backend sends when it loses the
// process, after the Error it journaled. The prompts go, and are remembered
// as answered, but nothing more is journaled.
func TestALegacyExitClearsPromptsWithoutJournaling(t *testing.T) {
	engine := newFakeEngine()
	sup, sink := newCoreForTest(t, engine, "agent-a")
	prompt := promptEvent("agent-a", "prompt-1")
	sup.Handle(session.AdapterEvent{Event: prompt})
	before := len(engine.auditSnapshot())
	sink.reset()
	exitedAt := time.Now().UTC().Truncate(time.Millisecond)

	sup.Handle(session.Exited{SessionID: "agent-a", Err: errors.New("the pane is gone")})

	if after := len(engine.auditSnapshot()); after != before {
		t.Fatalf("a legacy exit wrote %d journal entries, want none", after-before)
	}
	calls := sink.snapshot()
	if got := trace(calls); !reflect.DeepEqual(got, []string{"status:failed"}) {
		t.Fatalf("sink = %v", got)
	}
	cleared, err := time.Parse(time.RFC3339Nano, calls[0].status.ClearedBefore)
	if err != nil || cleared.Before(exitedAt) {
		t.Fatalf("ClearedBefore = %q, want a time at or after the exit", calls[0].status.ClearedBefore)
	}
	if agent := agentOf(t, sup, "agent-a"); agent.Running || agent.Status != "failed" || agent.ExitCode != nil {
		t.Fatalf("agent after a legacy exit = %#v", agent)
	}
	if ids := pendingIDs(sup); len(ids) != 0 {
		t.Fatalf("pending after a legacy exit = %v", ids)
	}
	sup.Handle(session.AdapterEvent{Event: prompt})
	if ids := pendingIDs(sup); len(ids) != 0 {
		t.Fatalf("a late copy of a cleared prompt was taken in: %v", ids)
	}
	if detected := engine.auditFor(audit.KindEventDetected, "prompt-1"); len(detected) != 1 {
		t.Fatalf("a late copy of a cleared prompt was journaled: %#v", detected)
	}
}

// A process exit is journaled twice, as detected and as the session's end,
// with only the exit facts the journal may hold.
func TestAProcessExitJournalsItsDetectionAndTheSessionsEnd(t *testing.T) {
	for _, test := range []struct {
		name        string
		code        int
		failed      bool
		wantOutcome audit.Outcome
		wantStatus  string
		wantMeta    map[string]string
		wantSummary string
	}{
		{
			name: "clean", code: 0, wantOutcome: audit.OutcomeFinished, wantStatus: "exited",
			wantMeta: map[string]string{"exit_code": "0"}, wantSummary: "process exited",
		},
		{
			name: "failed", code: 3, failed: true, wantOutcome: audit.OutcomeFailed, wantStatus: "failed",
			wantMeta: map[string]string{"exit_code": "3", "failed": "true"}, wantSummary: "process exited with error",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			sup, sink := newCoreForTest(t, engine, "agent-a")
			sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
			exit := exitEvent("agent-a", test.code, test.failed)
			exit.Metadata["command_line"] = "secret-argv"
			sink.reset()

			sup.Handle(session.AdapterEvent{Event: exit})

			base := audit.Entry{
				SessionID:  "agent-a",
				AgentID:    "agent-a",
				Backend:    "pty",
				Adapter:    "generic",
				EventID:    exit.ID,
				EventType:  adapters.EventProcessExit,
				Risk:       adapters.RiskUnknown,
				Summary:    test.wantSummary,
				DecisionBy: audit.DecisionBySystem,
				Metadata:   test.wantMeta,
			}
			detected := base
			detected.Kind = audit.KindEventDetected
			detected.Outcome = audit.OutcomeDetected
			detected.Reason = "event_detected"
			finished := base
			finished.Kind = audit.KindSessionFinished
			finished.Outcome = test.wantOutcome
			finished.Reason = "process_exit"
			if got := engine.auditFor(audit.KindEventDetected, exit.ID); len(got) != 1 || !reflect.DeepEqual(got[0], detected) {
				t.Fatalf("event_detected = %#v, want %#v", got, detected)
			}
			if got := engine.auditFor(audit.KindSessionFinished, exit.ID); len(got) != 1 || !reflect.DeepEqual(got[0], finished) {
				t.Fatalf("session_finished = %#v, want %#v", got, finished)
			}
			agent := agentOf(t, sup, "agent-a")
			if agent.Running || agent.Attached || agent.Status != test.wantStatus || agent.ExitCode == nil || *agent.ExitCode != test.code {
				t.Fatalf("agent after the exit = %#v", agent)
			}
			if ids := pendingIDs(sup); len(ids) != 0 {
				t.Fatalf("pending after the exit = %v", ids)
			}
			calls := sink.snapshot()
			if got := trace(calls); !reflect.DeepEqual(got, []string{"refresh", "status:" + test.wantStatus}) {
				t.Fatalf("sink = %v", got)
			}
			if calls[1].status.ClearedBefore == "" {
				t.Fatal("the exit status clears nothing")
			}
		})
	}
}

// The exit of a process a replacement already superseded is journaled with its
// real outcome, failed when it failed, as the current exit above is. Its reason
// says it is stale: the session it ends is not the one running now, and what
// reads the journal — the telemetry's count of active sessions and pending
// prompts — must not end the replacement's. The stale exit used to be
// journaled finished whatever its outcome, under the current exit's reason.
// Showing nothing is right: the replacement runs.
func TestAStaleExitIsJournaledWithItsOutcomeAsStale(t *testing.T) {
	for _, test := range []struct {
		name        string
		code        int
		failed      bool
		wantOutcome audit.Outcome
		wantMeta    map[string]string
		wantSummary string
	}{
		{
			name: "clean", code: 0, wantOutcome: audit.OutcomeFinished,
			wantMeta: map[string]string{"exit_code": "0"}, wantSummary: "process exited",
		},
		{
			name: "failed", code: 3, failed: true, wantOutcome: audit.OutcomeFailed,
			wantMeta: map[string]string{"exit_code": "3", "failed": "true"}, wantSummary: "process exited with error",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.staleExits = true
			sup, sink := newCoreForTest(t, engine, "agent-a")
			exit := exitEvent("agent-a", test.code, test.failed)

			sup.Handle(session.AdapterEvent{Event: exit})

			base := audit.Entry{
				SessionID:  "agent-a",
				AgentID:    "agent-a",
				Backend:    "pty",
				Adapter:    "generic",
				EventID:    exit.ID,
				EventType:  adapters.EventProcessExit,
				Risk:       adapters.RiskUnknown,
				Summary:    test.wantSummary,
				DecisionBy: audit.DecisionBySystem,
				Metadata:   test.wantMeta,
			}
			detected := base
			detected.Kind = audit.KindEventDetected
			detected.Outcome = audit.OutcomeDetected
			detected.Reason = "event_detected"
			finished := base
			finished.Kind = audit.KindSessionFinished
			finished.Outcome = test.wantOutcome
			finished.Reason = "process_exit_stale"
			if got := engine.auditFor(audit.KindEventDetected, exit.ID); len(got) != 1 || !reflect.DeepEqual(got[0], detected) {
				t.Fatalf("event_detected = %#v, want %#v", got, detected)
			}
			if got := engine.auditFor(audit.KindSessionFinished, exit.ID); len(got) != 1 || !reflect.DeepEqual(got[0], finished) {
				t.Fatalf("session_finished = %#v, want %#v", got, finished)
			}
			if agent := agentOf(t, sup, "agent-a"); !agent.Running || agent.Status != "running" {
				t.Fatalf("a stale exit stopped the replacement: %#v", agent)
			}
			if calls := sink.snapshot(); len(calls) != 0 {
				t.Fatalf("a stale exit was shown: %v", trace(calls))
			}
			sup.Handle(session.AdapterEvent{Event: exit})
			if again := engine.auditFor(audit.KindSessionFinished, exit.ID); len(again) != 1 {
				t.Fatalf("a repeated stale exit was journaled again: %#v", again)
			}
		})
	}
}

// Withdrawing the prompt whose answer is being written does not free its
// session. The agent took the question back, so the prompt is shown answered
// and leaves the pending list at once, and the withdrawal is journaled as any
// other; but the write to the terminal is still in progress, so the session's
// claim is kept until it returns, and only then is the next automatic prompt
// considered. The withdrawal used to release the claim with the prompt, and
// the next answer was written into the same terminal while the first still
// was, whoever had given the first.
func TestWithdrawingTheDeliveringPromptKeepsTheSessionUntilItsWriteReturns(t *testing.T) {
	for _, human := range []bool{false, true} {
		name := "an automatic answer"
		if human {
			name = "a human answer"
		}
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluationByID["prompt-2"] = automaticAllow()
			if !human {
				engine.evaluationByID["prompt-1"] = automaticAllow()
			}
			applyStarted := make(chan struct{}, 2)
			release := make(chan struct{})
			engine.applyStarted = applyStarted
			engine.applyRelease = release
			sup, sink := newCoreForTest(t, engine, "agent-a")
			releaseWrites := releaser(t, release)

			first := promptEvent("agent-a", "prompt-1")
			sup.Handle(session.AdapterEvent{Event: first})
			answered := make(chan error, 1)
			if human {
				go func() { answered <- sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", desktop) }()
			}
			select {
			case <-applyStarted:
			case <-time.After(2 * time.Second):
				t.Fatal("the first answer was never written")
			}
			second := promptEvent("agent-a", "prompt-2")
			second.Sequence = 2
			sup.Handle(session.AdapterEvent{Event: second})
			sink.reset()

			sup.Handle(session.AdapterEventWithdrawn{Event: first})

			if ids := pendingIDs(sup); len(ids) != 1 || ids[0] != "prompt-2" {
				t.Fatalf("pending after the withdrawal = %v, want only the next prompt", ids)
			}
			shown := false
			for _, call := range sink.snapshot() {
				shown = shown || (call.kind == "prompt" && call.view.ID == "prompt-1" && call.view.DeliveryStatus == "delivered")
			}
			if !shown {
				t.Fatalf("the withdrawn prompt was not shown answered: %v", trace(sink.snapshot()))
			}
			if withdrawn := engine.auditFor(audit.KindEventWithdrawn, "prompt-1"); len(withdrawn) != 1 {
				t.Fatalf("withdrawal entries = %#v, want one", withdrawn)
			}
			select {
			case <-applyStarted:
				t.Fatal("the next answer was written while the withdrawn prompt's still was")
			case <-time.After(100 * time.Millisecond):
			}
			err := returnsBeforeReaching(t, applyStarted, "a human answer was written while the withdrawn prompt's still was", func() error {
				return sup.SubmitDecision(testRunID, "agent-a", "prompt-2", "y", desktop)
			})
			if !errors.Is(err, supervise.ErrDecisionInFlight) {
				t.Fatalf("a human answer while the withdrawn prompt's is written = %v, want ErrDecisionInFlight", err)
			}

			releaseWrites()
			if human {
				if err := <-answered; err != nil {
					t.Fatalf("the withdrawn prompt's human answer = %v", err)
				}
			}
			waitFor(t, 2*time.Second, "the next prompt to be answered once the session is free", func() bool {
				return len(engine.auditFor(audit.KindDelivery, "prompt-2")) == 1 && len(pendingIDs(sup)) == 0
			})
			operations := engine.operationSnapshot()
			if returned, applied := indexOf(operations, "apply:return:prompt-1"), indexOf(operations, "apply:start:prompt-2"); returned < 0 || applied < returned {
				t.Fatalf("operations = %v, want the next answer written after the withdrawn prompt's returned", operations)
			}
			if deliveries := engine.auditFor(audit.KindDelivery, "prompt-1"); len(deliveries) != 1 || deliveries[0].Outcome != audit.OutcomeApplied {
				t.Fatalf("the withdrawn prompt's delivery entries = %#v, want one applied", deliveries)
			}
			if calls := engine.applySnapshot(); len(calls) != 2 {
				t.Fatalf("deliveries = %#v, want two", calls)
			}
			if agent := agentOf(t, sup, "agent-a"); agent.Status != "running" || agent.InputFrozen {
				t.Fatalf("agent once both answers are written = %#v", agent)
			}
		})
	}
}

// A prompt the new process raised while it started is kept, and the agent is
// shown waiting on it once the start completes, in the core's state and in
// the status the start reports. The start used to show it running whatever
// it had kept, so the agent read as busy while it waited on the operator.
func TestAStartShowsTheAgentWaitingOnThePromptItRaised(t *testing.T) {
	for _, restart := range []bool{false, true} {
		name := "start"
		if restart {
			name = "restart"
		}
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine()
			reached := make(chan string, 1)
			release := make(chan struct{})
			if restart {
				engine.agentRestartStarted = reached
				engine.agentRestartRelease = release
			} else {
				engine.agentStartStarted = reached
				engine.agentStartRelease = release
			}
			sup, sink := newCoreForTest(t, engine, "agent-a")
			releaseStart := releaser(t, release)
			done := make(chan error, 1)
			if restart {
				go func() { done <- sup.RestartSession(testRunID, "agent-a") }()
			} else {
				sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
				go func() { done <- sup.StartSession(testRunID, "agent-a") }()
			}
			select {
			case <-reached:
			case <-time.After(2 * time.Second):
				t.Fatalf("the %s never reached the runtime", name)
			}
			prompt := promptEvent("agent-a", "prompt-early")
			prompt.Timestamp = time.Now().UTC()
			sup.Handle(session.AdapterEvent{Event: prompt})
			sink.reset()
			releaseStart()
			if err := <-done; err != nil {
				t.Fatalf("%s: %v", name, err)
			}

			if ids := pendingIDs(sup); len(ids) != 1 || ids[0] != "prompt-early" {
				t.Fatalf("pending after the %s = %v, want the prompt raised during it", name, ids)
			}
			if agent := agentOf(t, sup, "agent-a"); !agent.Running || agent.Status != "waiting" {
				t.Fatalf("agent after the %s = %#v, want running and waiting on the kept prompt", name, agent)
			}
			var reported []supervise.Status
			for _, call := range sink.snapshot() {
				if call.kind == "status" {
					reported = append(reported, call.status)
				}
			}
			if len(reported) != 1 || reported[0].Status != "waiting" || reported[0].ClearedBefore == "" {
				t.Fatalf("statuses the %s reported = %#v, want one waiting status that clears the previous process's prompts", name, reported)
			}
		})
	}
}

// A prompt raised while the agent is stopped, or restarted, leaves it shown
// stopping: its process is on its way out and the core refuses any answer to
// it until the operation ends. The prompt used to set the agent waiting over
// "stopping", and its withdrawal then set it running, so the agent read as
// ready for input in the middle of its Stop.
func TestAPromptRaisedWhileTheAgentStopsLeavesItShownStopping(t *testing.T) {
	for _, test := range []struct {
		name      string
		hold      func(*fakeEngine, chan string, <-chan struct{})
		operate   func(*supervise.Supervisor) error
		wantAfter string
	}{
		{
			name: "stop",
			hold: func(engine *fakeEngine, reached chan string, release <-chan struct{}) {
				engine.stopStarted, engine.stopRelease = reached, release
			},
			operate:   func(sup *supervise.Supervisor) error { return sup.StopSession(testRunID, "agent-a") },
			wantAfter: "exited",
		},
		{
			name: "restart",
			hold: func(engine *fakeEngine, reached chan string, release <-chan struct{}) {
				engine.agentRestartStarted, engine.agentRestartRelease = reached, release
			},
			operate:   func(sup *supervise.Supervisor) error { return sup.RestartSession(testRunID, "agent-a") },
			wantAfter: "running",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			reached := make(chan string, 1)
			release := make(chan struct{})
			test.hold(engine, reached, release)
			sup, sink := newCoreForTest(t, engine, "agent-a")
			releaseOperation := releaser(t, release)
			done := make(chan error, 1)
			go func() { done <- test.operate(sup) }()
			select {
			case <-reached:
			case <-time.After(2 * time.Second):
				t.Fatalf("the %s never reached the runtime", test.name)
			}

			prompt := promptEvent("agent-a", "prompt-1")
			prompt.Timestamp = time.Now().UTC()
			sup.Handle(session.AdapterEvent{Event: prompt})
			if agent := agentOf(t, sup, "agent-a"); agent.Status != "stopping" {
				t.Fatalf("agent once a prompt is raised during the %s = %#v, want still stopping", test.name, agent)
			}
			sink.reset()
			sup.Handle(session.AdapterEventWithdrawn{Event: prompt})
			if agent := agentOf(t, sup, "agent-a"); agent.Status != "stopping" {
				t.Fatalf("agent once the prompt is withdrawn during the %s = %#v, want still stopping", test.name, agent)
			}
			for _, call := range sink.snapshot() {
				if call.kind == "status" && call.status.Status != "stopping" {
					t.Fatalf("the withdrawal reported %#v during the %s, want stopping", call.status, test.name)
				}
			}

			releaseOperation()
			if err := <-done; err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			if agent := agentOf(t, sup, "agent-a"); agent.Status != test.wantAfter {
				t.Fatalf("agent after the %s = %#v, want %s", test.name, agent, test.wantAfter)
			}
		})
	}
}

// An answer the core refused is not accepted again. Once the adapter could
// not encode it, the prompt no longer offers it, and a screen that still
// shows it, or a second operator's, is refused (ErrUnsupportedDecision) with
// nothing claimed, journaled, shown or written. The core used to ask the
// adapter again rather than read what the prompt offers, and the adapter
// still claimed the answer: it was journaled a second time and handed to the
// runtime again. The answers the prompt still offers are taken as ever.
func TestAnAnswerThePromptNoLongerOffersIsRefused(t *testing.T) {
	engine := newFakeEngine()
	engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
	engine.applyErrs = []error{adapters.ErrDecisionUnsupported}
	sup, sink := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	if err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", desktop); !errors.Is(err, supervise.ErrUnsupportedDecision) {
		t.Fatalf("an answer the adapter cannot encode = %v, want ErrUnsupportedDecision", err)
	}
	if shown := viewOf(sup, "prompt-1"); shown == nil || !reflect.DeepEqual(shown.Decisions, []string{"deny"}) {
		t.Fatalf("the prompt after the refusal = %#v, want it offering deny alone", shown)
	}
	journaled, written := len(engine.auditSnapshot()), len(engine.applySnapshot())
	sink.reset()

	if err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", alice); !errors.Is(err, supervise.ErrUnsupportedDecision) {
		t.Fatalf("the refused answer, again = %v, want ErrUnsupportedDecision", err)
	}
	if entries := engine.auditSnapshot(); len(entries) != journaled {
		t.Fatalf("the refused answer was journaled again: %#v", entries[journaled:])
	}
	if calls := engine.applySnapshot(); len(calls) != written {
		t.Fatalf("the refused answer reached the runtime again: %#v", calls[written:])
	}
	if calls := sink.snapshot(); len(calls) != 0 {
		t.Fatalf("the refused answer showed %v", trace(calls))
	}
	if shown := viewOf(sup, "prompt-1"); shown == nil || shown.DeliveryStatus != "pending" {
		t.Fatalf("the prompt after the second refusal = %#v", shown)
	}

	if err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "deny", alice); err != nil {
		t.Fatalf("the answer the prompt still offers = %v", err)
	}
	if calls := engine.applySnapshot(); len(calls) != written+1 || calls[written].decision != adapters.DecisionDeny {
		t.Fatalf("writes = %#v, want the refused allow then deny", calls)
	}
}

// A chosen answer is judged before anything about the session. The adapter is
// asked again, and an answer it stopped offering since the prompt was shown
// is refused although the prompt still offers it; the prompt's own offer is
// read with it, so an answer the prompt no longer offers is refused as
// unsupported whatever state the run is in, as one the adapter does not offer
// is. Neither is journaled or written.
func TestAnAnswerIsJudgedByTheAdapterAndThePromptBeforeTheSession(t *testing.T) {
	engine := newFakeEngine()
	engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
	engine.applyErrs = []error{adapters.ErrDecisionUnsupported}
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-2")})

	// The adapter stops offering deny: the prompt still does.
	engine.set(func(f *fakeEngine) { f.supportedDecisions = []adapters.Decision{adapters.DecisionAllow} })
	if shown := viewOf(sup, "prompt-2"); shown == nil || !reflect.DeepEqual(shown.Decisions, []string{"allow", "deny"}) {
		t.Fatalf("the prompt offers %#v", shown)
	}
	if err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-2", "deny", alice); !errors.Is(err, supervise.ErrUnsupportedDecision) {
		t.Fatalf("an answer the adapter stopped offering = %v, want ErrUnsupportedDecision", err)
	}
	if decisions := engine.auditFor(audit.KindDecision, "prompt-2"); len(decisions) != 0 {
		t.Fatalf("an answer the adapter stopped offering was journaled: %#v", decisions)
	}

	// prompt-1 no longer offers allow, which the adapter could not encode;
	// then the journal fails and the run is frozen.
	if err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", alice); !errors.Is(err, supervise.ErrUnsupportedDecision) {
		t.Fatalf("an answer the adapter cannot encode = %v", err)
	}
	engine.set(func(f *fakeEngine) { f.auditFailAt = f.auditCalls + 1 })
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-3")})
	if !sup.State().AuditFailed {
		t.Fatal("the journal did not fail")
	}
	written := len(engine.applySnapshot())
	if err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", alice); !errors.Is(err, supervise.ErrUnsupportedDecision) {
		t.Fatalf("an answer the prompt no longer offers, once the run is frozen = %v, want ErrUnsupportedDecision", err)
	}
	if calls := engine.applySnapshot(); len(calls) != written {
		t.Fatalf("an answer the prompt no longer offers was written: %#v", calls[written:])
	}
}

// Two people may choose the same answer at once. When the adapter cannot
// encode the first one's, the prompt stops offering it, and the second one's,
// which read the prompt before, is refused all the same when it claims the
// session: it is checked there against what the prompt offers then. It used
// to be journaled and handed to the runtime a second time.
func TestAnAnswerRefusedToOnePersonIsRefusedToTheNextAtOnce(t *testing.T) {
	engine := newFakeEngine()
	engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
	engine.applyErrs = []error{adapters.ErrDecisionUnsupported}
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	bob := supervise.Actor{Identity: "bob", Role: supervise.RoleOperator, ConnID: "conn-2"}
	var asked atomic.Bool
	var first error
	// alice's answer reads the prompt, then asks the adapter: bob's is
	// refused by the adapter's runtime while she does. Bob's asks the adapter
	// too, and goes on.
	engine.set(func(f *fakeEngine) {
		f.onSupportedDecisions = func(adapters.Event) {
			if asked.CompareAndSwap(false, true) {
				first = sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", bob)
			}
		}
	})

	err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", alice)
	if !errors.Is(first, supervise.ErrUnsupportedDecision) {
		t.Fatalf("the first answer = %v, want ErrUnsupportedDecision", first)
	}
	if !errors.Is(err, supervise.ErrUnsupportedDecision) {
		t.Fatalf("the same answer, read before it was refused = %v, want ErrUnsupportedDecision", err)
	}
	if decisions := engine.auditFor(audit.KindDecision, "prompt-1"); len(decisions) != 1 || decisions[0].Operator != "bob" {
		t.Fatalf("decision entries = %#v, want bob's alone", decisions)
	}
	if calls := engine.applySnapshot(); len(calls) != 1 {
		t.Fatalf("writes = %#v, want the first alone", calls)
	}
}

// automaticDeny is the evaluation of a rule that denies without asking.
func automaticDeny() policy.Evaluation {
	return policy.Evaluation{
		Action:         policy.ActionDeny,
		ProposedAction: policy.ActionDeny,
		RuleName:       "deny-risky",
		Reason:         policy.ReasonRule,
		Automatic:      true,
	}
}

// A prompt the policy would deny, but that the hand, the repeat guard or one
// of the policy's limits keeps it from denying on its own, goes to the
// operator with deny as its only answer, when it is raised or when it waits.
// It went with every answer the adapter offers, and any operator could then
// allow what a deny rule refuses. The restriction binds: allow is refused
// (ErrUnsupportedDecision) with nothing journaled, and deny is taken. A typed
// answer is still sent as typed, since only the adapter knows what its bytes
// mean.
func TestADenyTheCoreHoldsBackOffersOnlyDeny(t *testing.T) {
	for _, test := range []struct {
		name   string
		reason string
		// raise brings prompt deny-2 of agent-a to the operator, the policy
		// denying it automatically, or but for its limit.
		raise func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine)
	}{
		{name: "the hand held when it is raised", reason: supervise.ReasonOperatorAttached,
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				sup.SetHolder("agent-a", "conn-1")
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-2")})
			}},
		{name: "the hand taken while it waits", reason: supervise.ReasonOperatorAttached,
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				letTheLineGo := holdWithALine(t, engine, sup, "agent-a")
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-2")})
				sup.SetHolder("agent-a", "conn-1")
				letTheLineGo()
			}},
		{name: "a repeat of an answer just written", reason: supervise.ReasonRepeatAfterDelivery,
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				first := promptEvent("agent-a", "deny-1")
				sup.Handle(session.AdapterEvent{Event: first})
				waitForTheSessionToBeFree(t, sup, "agent-a")
				sup.Handle(session.AdapterEvent{Event: repeatOf(first, "deny-2")})
			}},
		{name: "the policy's limit when it is raised", reason: policy.ReasonConsecutiveLimit,
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				engine.set(func(f *fakeEngine) { f.maxConsecutiveAuto = 1 })
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-1")})
				waitForTheSessionToBeFree(t, sup, "agent-a")
				second := promptEvent("agent-a", "deny-2")
				second.Sequence = 2
				sup.Handle(session.AdapterEvent{Event: second})
			}},
		{name: "an answer the adapter could not encode", reason: "fallback_unsupported",
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				engine.set(func(f *fakeEngine) { f.applyErrs = []error{adapters.ErrDecisionUnsupported} })
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-2")})
				waitFor(t, 2*time.Second, "the prompt to go back to the operator", func() bool {
					shown := viewOf(sup, "deny-2")
					return shown != nil && !shown.Evaluation.Automatic && shown.DeliveryStatus == "pending"
				})
			}},
		{name: "the policy's limit at its last check", reason: policy.ReasonConsecutiveLimit,
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				engine.set(func(f *fakeEngine) { f.maxConsecutiveAuto = 1 })
				letTheLineGo := holdWithALine(t, engine, sup, "agent-a")
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-1")})
				second := promptEvent("agent-a", "deny-2")
				second.Sequence = 2
				sup.Handle(session.AdapterEvent{Event: second})
				letTheLineGo()
				waitFor(t, 2*time.Second, "the second prompt to go to the operator", func() bool {
					shown := viewOf(sup, "deny-2")
					return shown != nil && !shown.Evaluation.Automatic && shown.DeliveryStatus == "pending"
				})
			}},
		// The policy's evaluation at its last check is its deny; the repeat
		// guard is what hands the prompt back.
		{name: "a repeat at the policy's last check", reason: supervise.ReasonRepeatAfterDelivery,
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				letTheLineGo := holdWithALine(t, engine, sup, "agent-a")
				first := promptEvent("agent-a", "deny-1")
				sup.Handle(session.AdapterEvent{Event: first})
				sup.Handle(session.AdapterEvent{Event: repeatOf(first, "deny-2")})
				letTheLineGo()
				waitFor(t, 2*time.Second, "the repeat to go to the operator", func() bool {
					shown := viewOf(sup, "deny-2")
					return shown != nil && !shown.Evaluation.Automatic && shown.DeliveryStatus == "pending"
				})
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluation = automaticDeny()
			engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
			sup, _ := newCoreWithOptions(t, engine, supervise.Options{Now: newTestClock().Now}, "agent-a")
			test.raise(t, sup, engine)

			shown := viewOf(sup, "deny-2")
			if shown == nil || shown.DeliveryStatus != "pending" || shown.Evaluation.Automatic ||
				shown.Evaluation.ProposedAction != string(policy.ActionDeny) || shown.Evaluation.Reason != test.reason {
				t.Fatalf("the prompt is shown as %#v, want it pending on the operator for %s", shown, test.reason)
			}
			if !reflect.DeepEqual(shown.Decisions, []string{"deny"}) {
				t.Fatalf("the prompt offers %v, want deny alone", shown.Decisions)
			}
			journaled := len(engine.auditSnapshot())
			if err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "deny-2", "allow", alice); !errors.Is(err, supervise.ErrUnsupportedDecision) {
				t.Fatalf("allowing what the policy denies = %v, want ErrUnsupportedDecision", err)
			}
			if entries := engine.auditSnapshot(); len(entries) != journaled {
				t.Fatalf("the refused allow was journaled: %#v", entries[journaled:])
			}
			// The policy's last check releases the session a moment after it
			// shows the prompt pending.
			waitFor(t, 2*time.Second, "the deny to be taken", func() bool {
				err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "deny-2", "deny", alice)
				if err != nil && !errors.Is(err, supervise.ErrDecisionInFlight) {
					t.Fatalf("denying = %v", err)
				}
				return err == nil
			})
			for _, call := range engine.applySnapshot() {
				if call.event.ID == "deny-2" && call.decision != adapters.DecisionDeny {
					t.Fatalf("the prompt was answered %s", call.decision)
				}
			}
		})
	}
}

// A prompt the policy was to answer, handed back to the operator after it was
// detected, is notified like any prompt that waits on a person: the policy's
// last check found a limit reached, a repeat, or another answer, or the
// adapter could not encode the policy's answer. Such a prompt waited in
// silence, shown on a screen nobody may be watching, and a limit exists to
// bring a person in; the agent stalled. The one exception is the hand, whose
// holder is at the terminal (TestTheHandTakenBeforeThePolicysLastCheckStopsItsAnswer),
// and a person's own answer handed back is not notified either
// (TestAHumanAnswerTheAdapterCannotEncodeGoesBackToTheOperator): they are
// there.
func TestAPromptHandedBackAfterItsDetectionIsNotified(t *testing.T) {
	for _, test := range []struct {
		name   string
		reason string
		// handBack raises automatic-2 of agent-a, which the policy would
		// answer, and has it handed back to the operator.
		handBack func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine)
	}{
		{name: "the policy's limit at its last check", reason: policy.ReasonConsecutiveLimit,
			handBack: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				engine.set(func(f *fakeEngine) { f.maxConsecutiveAuto = 1 })
				letTheLineGo := holdWithALine(t, engine, sup, "agent-a")
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
				second := promptEvent("agent-a", "automatic-2")
				second.Sequence = 2
				sup.Handle(session.AdapterEvent{Event: second})
				letTheLineGo()
			}},
		{name: "a repeat at its last check", reason: supervise.ReasonRepeatAfterDelivery,
			handBack: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				letTheLineGo := holdWithALine(t, engine, sup, "agent-a")
				first := promptEvent("agent-a", "automatic-1")
				sup.Handle(session.AdapterEvent{Event: first})
				sup.Handle(session.AdapterEvent{Event: repeatOf(first, "automatic-2")})
				letTheLineGo()
			}},
		{name: "an answer the adapter could not encode", reason: "fallback_unsupported",
			handBack: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				engine.set(func(f *fakeEngine) { f.applyErrs = []error{adapters.ErrDecisionUnsupported} })
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-2")})
			}},
		{name: "an action no adapter encodes", reason: "fallback_unsupported",
			handBack: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				engine.set(func(f *fakeEngine) {
					f.evaluationByID["automatic-2"] = policy.Evaluation{
						Action: policy.ActionAsk, ProposedAction: policy.ActionAsk, RuleName: "odd", Reason: policy.ReasonRule, Automatic: true,
					}
				})
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-2")})
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluation = automaticAllow()
			sup, sink := newCoreWithOptions(t, engine, supervise.Options{Now: newTestClock().Now}, "agent-a")
			test.handBack(t, sup, engine)
			waitFor(t, 2*time.Second, "the prompt to go back to the operator", func() bool {
				shown := viewOf(sup, "automatic-2")
				return shown != nil && !shown.Evaluation.Automatic && shown.DeliveryStatus == "pending"
			})
			sup.BeginDrain()
			sup.Wait()

			if shown := viewOf(sup, "automatic-2"); shown.Evaluation.Reason != test.reason {
				t.Fatalf("the prompt went back for %q, want %q", shown.Evaluation.Reason, test.reason)
			}
			var notices []supervise.Notice
			shownPending := -1
			for index, call := range sink.snapshot() {
				if call.kind == "prompt" && call.view.ID == "automatic-2" && call.view.DeliveryStatus == "pending" && !call.view.Evaluation.Automatic {
					shownPending = index
				}
				if call.kind == "notify" && call.notice.EventID == "automatic-2" {
					if shownPending < 0 {
						t.Fatalf("the notice came before the prompt was shown the operator's: %v", trace(sink.snapshot()))
					}
					notices = append(notices, call.notice)
				}
			}
			want := supervise.Notice{
				Kind: supervise.NoticePendingDecision, Severity: supervise.SeverityWarning, AgentName: "agent-a",
				SessionID: "agent-a", EventID: "automatic-2", Reason: "confirmation required", Details: "Overwrite file?",
			}
			if len(notices) != 1 || notices[0] != want {
				t.Fatalf("notices = %#v, want one that the prompt waits on a person", notices)
			}
		})
	}
}

// Only a deny the policy makes by itself is restricted. A prompt whose rule
// proposes deny but that the policy asks about anyway, being sensitive or in
// a dry run, and an allow the hand holds back, offer every answer the adapter
// encodes, as they always did.
func TestAPromptThePolicyDoesNotDenyByItselfOffersEveryAnswer(t *testing.T) {
	for _, test := range []struct {
		name       string
		evaluation policy.Evaluation
		sensitive  bool
		reason     string
	}{
		{name: "a sensitive prompt a deny rule matches", sensitive: true, reason: policy.ReasonSensitive,
			evaluation: policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionDeny, RuleName: "deny-risky", Reason: policy.ReasonSensitive}},
		{name: "a deny rule in a dry run", reason: policy.ReasonDryRun,
			evaluation: policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionDeny, RuleName: "deny-risky", Reason: policy.ReasonDryRun, DryRun: true}},
		{name: "an allow the hand holds back", reason: supervise.ReasonOperatorAttached, evaluation: automaticAllow()},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluation = test.evaluation
			engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
			sup, _ := newCoreForTest(t, engine, "agent-a")
			sup.SetHolder("agent-a", "conn-1")
			prompt := promptEvent("agent-a", "prompt-1")
			prompt.Sensitive = test.sensitive
			sup.Handle(session.AdapterEvent{Event: prompt})
			shown := viewOf(sup, "prompt-1")
			if shown == nil || shown.Evaluation.Reason != test.reason || !reflect.DeepEqual(shown.Decisions, []string{"allow", "deny"}) {
				t.Fatalf("the prompt is shown as %#v, want every answer offered for %s", shown, test.reason)
			}
			if err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", alice); err != nil {
				t.Fatalf("allowing = %v", err)
			}
		})
	}
}

// A human answer the adapter cannot encode goes back to the operator. The
// runtime encodes an answer before it writes a byte of it (the router's
// ApplyDecision), so the delivery is not uncertain: it is journaled
// fallback_unsupported, the prompt is pending again without the answer it
// could not take, the caller is told ErrUnsupportedDecision, and nothing is
// frozen, so the operator can answer otherwise. The human path used to treat
// it as an uncertain delivery and freeze the session, where the automatic
// path (TestAnUnsupportedAutomaticDecisionFallsBackToAsk) fell back to ask.
func TestAHumanAnswerTheAdapterCannotEncodeGoesBackToTheOperator(t *testing.T) {
	for _, test := range []struct {
		name          string
		answer        func(*supervise.Supervisor) error
		wantDecision  audit.Decision
		wantDecisions []string
	}{
		{
			name: "a typed answer",
			answer: func(sup *supervise.Supervisor) error {
				return sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", desktop)
			},
			wantDecision:  audit.DecisionAsk,
			wantDecisions: []string{"allow", "deny"},
		},
		{
			name: "a button",
			answer: func(sup *supervise.Supervisor) error {
				return sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "deny", desktop)
			},
			wantDecision:  audit.DecisionDeny,
			wantDecisions: []string{"allow"},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
			engine.applyErrs = []error{adapters.ErrDecisionUnsupported}
			sup, sink := newCoreForTest(t, engine, "agent-a")
			sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
			sink.reset()

			if err := test.answer(sup); !errors.Is(err, supervise.ErrUnsupportedDecision) {
				t.Fatalf("an answer the adapter cannot encode = %v, want ErrUnsupportedDecision", err)
			}
			decisions := engine.auditFor(audit.KindDecision, "prompt-1")
			if len(decisions) != 1 || decisions[0].DecisionBy != audit.DecisionByHuman || decisions[0].Decision != test.wantDecision {
				t.Fatalf("decision entries = %#v", decisions)
			}
			deliveries := engine.auditFor(audit.KindDelivery, "prompt-1")
			if len(deliveries) != 1 || deliveries[0].Outcome != audit.OutcomeFallbackUnsupported || deliveries[0].Reason != "fallback_unsupported" ||
				deliveries[0].DecisionBy != audit.DecisionByHuman || deliveries[0].Decision != test.wantDecision {
				t.Fatalf("delivery entries = %#v, want one fallback_unsupported", deliveries)
			}
			if agent := agentOf(t, sup, "agent-a"); agent.InputFrozen || agent.Status != "waiting" {
				t.Fatalf("agent = %#v, want it waiting on the prompt and not frozen", agent)
			}
			state := sup.State()
			if len(state.Pending) != 1 {
				t.Fatalf("pending = %#v, want the prompt back", state.Pending)
			}
			view := state.Pending[0]
			if view.DeliveryStatus != "pending" || !reflect.DeepEqual(view.Decisions, test.wantDecisions) ||
				view.Evaluation.Automatic || view.Evaluation.Action != string(policy.ActionAsk) || view.Evaluation.Reason != "fallback_unsupported" {
				t.Fatalf("prompt after the refused answer = %#v, want it pending without the refused answer", view)
			}
			if got := trace(sink.snapshot()); !reflect.DeepEqual(got, []string{"prompt:delivering", "prompt:pending"}) {
				t.Fatalf("sink = %v, want the prompt shown delivering then pending again", got)
			}

			if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "n", desktop); err != nil {
				t.Fatalf("another answer after the refused one = %v", err)
			}
			if ids := pendingIDs(sup); len(ids) != 0 {
				t.Fatalf("pending after the second answer = %v", ids)
			}
		})
	}

	// An automatic prompt the operator answered is theirs from then on: the
	// policy does not answer it once the operator's answer is refused. The
	// prompt waits behind an earlier one only a human answers.
	t.Run("an automatic prompt a human answered", func(t *testing.T) {
		engine := newFakeEngine()
		engine.evaluationByID["automatic-2"] = automaticAllow()
		engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
		engine.applyErrs = []error{adapters.ErrDecisionUnsupported}
		sup, sink := newCoreForTest(t, engine, "agent-a")
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "human-1")})
		automatic := promptEvent("agent-a", "automatic-2")
		automatic.Sequence = 2
		sup.Handle(session.AdapterEvent{Event: automatic})

		if err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "automatic-2", "deny", desktop); !errors.Is(err, supervise.ErrUnsupportedDecision) {
			t.Fatalf("the operator's refused answer = %v, want ErrUnsupportedDecision", err)
		}
		for _, view := range sup.State().Pending {
			if view.ID == "automatic-2" && (view.Evaluation.Automatic || view.Evaluation.Action != string(policy.ActionAsk) ||
				view.Evaluation.ProposedAction != string(policy.ActionAllow)) {
				t.Fatalf("the refused prompt = %#v, want it shown as the operator's, with the policy's proposal kept", view)
			}
		}
		sink.reset()
		if err := sup.SubmitDecision(testRunID, "agent-a", "human-1", "y", desktop); err != nil {
			t.Fatalf("the earlier prompt's answer = %v", err)
		}
		sup.BeginDrain()
		sup.Wait()
		if calls := engine.applySnapshot(); len(calls) != 2 {
			t.Fatalf("deliveries = %#v, want only the two human answers", calls)
		}
		if ids := pendingIDs(sup); len(ids) != 1 || ids[0] != "automatic-2" {
			t.Fatalf("pending = %v, want the refused prompt still with the operator", ids)
		}
		for _, call := range sink.snapshot() {
			if call.kind == "prompt" && call.view.ID == "automatic-2" {
				t.Fatalf("the refused prompt was taken up again once the session was free: %v", trace(sink.snapshot()))
			}
		}
	})

	// The refused answer's delivery entry is journaled like any other: when
	// the journal fails on it, the run stops sending, as it does everywhere.
	t.Run("the journal fails on the refused answer", func(t *testing.T) {
		engine := newFakeEngine()
		engine.applyErrs = []error{adapters.ErrDecisionUnsupported}
		sup, _ := newCoreForTest(t, engine, "agent-a")
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
		// The decision entry, then the delivery entry, which fails.
		engine.set(func(f *fakeEngine) { f.auditFailAt = f.auditCalls + 2 })

		if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", desktop); !errors.Is(err, supervise.ErrAuditUnavailable) {
			t.Fatalf("a refused answer the journal could not record = %v, want ErrAuditUnavailable", err)
		}
		state := sup.State()
		if !state.AuditFailed || len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "failed" ||
			state.Pending[0].Evaluation.Reason != "audit_unavailable" {
			t.Fatalf("state = %#v, want the journal failed and the prompt with it", state)
		}
		if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "n", desktop); err == nil {
			t.Fatal("an answer was taken after the journal failed")
		}
		if calls := engine.applySnapshot(); len(calls) != 1 {
			t.Fatalf("deliveries = %#v, want the one refused attempt", calls)
		}
	})
}

// The desktop's TestUnsupportedAutomaticDecisionFallsBackToAsk, through the
// core: the adapter cannot encode the policy's answer, so the prompt goes to
// a human with the policy's proposal kept, and nothing is frozen.
func TestAnUnsupportedAutomaticDecisionFallsBackToAsk(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	engine.applyErr = adapters.ErrDecisionUnsupported
	sup, _ := newCoreForTest(t, engine, "agent-a")

	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
	waitFor(t, 2*time.Second, "the fallback to ask", func() bool {
		pending := sup.State().Pending
		return len(pending) == 1 && pending[0].Evaluation.Reason == "fallback_unsupported"
	})
	// The counts below are exact only once the automatic decision has
	// finished: drained, a late second delivery or entry would be counted.
	sup.BeginDrain()
	sup.Wait()

	if calls := engine.applySnapshot(); len(calls) != 1 || calls[0].decision != adapters.DecisionAllow {
		t.Fatalf("deliveries = %#v, want the one automatic attempt", calls)
	}
	view := sup.State().Pending[0]
	if view.Evaluation.Action != string(policy.ActionAsk) || view.Evaluation.ProposedAction != string(policy.ActionAllow) ||
		view.Evaluation.Automatic || view.DeliveryStatus != "pending" {
		t.Fatalf("prompt after the fallback = %#v", view)
	}
	deliveries := engine.auditFor(audit.KindDelivery, "automatic-1")
	if len(deliveries) != 1 || deliveries[0].Outcome != audit.OutcomeFallbackUnsupported {
		t.Fatalf("delivery entries = %#v", deliveries)
	}
	if agent := agentOf(t, sup, "agent-a"); agent.InputFrozen || agent.Status != "waiting" {
		t.Fatalf("agent after the fallback = %#v", agent)
	}
}

// holdWithALine writes a line to the session and holds its write, so that the
// prompts raised meanwhile queue behind it. The function it returns lets the
// line go and waits for it.
func holdWithALine(t *testing.T, engine *fakeEngine, sup *supervise.Supervisor, sessionID string) func() {
	t.Helper()
	started := make(chan string, 1)
	release := make(chan struct{})
	engine.set(func(f *fakeEngine) {
		f.lineStarted = started
		f.lineRelease = release
	})
	releaseLine := releaser(t, release)
	sent := make(chan error, 1)
	go func() { sent <- sup.SubmitLine(testRunID, sessionID, "hello", desktop) }()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the line never reached the runtime")
	}
	return func() {
		t.Helper()
		releaseLine()
		select {
		case err := <-sent:
			if err != nil {
				t.Fatalf("SubmitLine: %v", err)
			}
		case <-time.After(2 * time.Second):
			t.Fatal("the line did not return")
		}
	}
}

// The policy decides a prompt when it is detected, and its answer goes when the
// session is free, which may be after other automatic answers: a limit on
// consecutive automatic decisions can be reached in between. The core asks the
// policy again just before it journals its decision. A prompt that is no
// longer the policy's to answer, or that the policy would now answer another
// way, goes to the operator, with a second policy_evaluated entry that says
// why. Decided at detection alone, two prompts queued on one session were both
// answered under a limit of one.
func TestThePolicyIsAskedAgainJustBeforeItsDecision(t *testing.T) {
	for _, test := range []struct {
		name  string
		limit int
		// now, when set, is how the policy evaluates the second prompt once the
		// first was answered.
		now          *policy.Evaluation
		wantReason   string
		wantRule     string
		wantProposed policy.Action
	}{
		{
			name:         "the consecutive limit is reached while the prompt waits",
			limit:        1,
			wantReason:   policy.ReasonConsecutiveLimit,
			wantRule:     "allow-safe",
			wantProposed: policy.ActionAllow,
		},
		{
			name: "the policy now takes another action",
			now: &policy.Evaluation{
				Action: policy.ActionDeny, ProposedAction: policy.ActionDeny, RuleName: "deny-now",
				Reason: policy.ReasonRule, Automatic: true,
			},
			wantReason:   policy.ReasonRule,
			wantRule:     "deny-now",
			wantProposed: policy.ActionDeny,
		},
		{
			// The desktop's engine turns an evaluation it no longer answers
			// into ask, but an engine need not: that it is no longer
			// automatic is what counts.
			name: "the policy no longer answers automatically, whatever its action",
			now: &policy.Evaluation{
				Action: policy.ActionAllow, ProposedAction: policy.ActionAllow, RuleName: "allow-safe",
				Reason: policy.ReasonRateLimit,
			},
			wantReason:   policy.ReasonRateLimit,
			wantRule:     "allow-safe",
			wantProposed: policy.ActionAllow,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluation = automaticAllow()
			engine.maxConsecutiveAuto = test.limit
			sup, _ := newCoreForTest(t, engine, "agent-a")
			letTheLineGo := holdWithALine(t, engine, sup, "agent-a")
			second := promptEvent("agent-a", "automatic-2")
			second.Sequence = 2
			sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
			sup.Handle(session.AdapterEvent{Event: second})
			for _, id := range []string{"automatic-1", "automatic-2"} {
				if evaluated := engine.auditFor(audit.KindPolicyEvaluated, id); len(evaluated) != 1 || evaluated[0].Metadata["automatic"] != "true" {
					t.Fatalf("%s at detection = %#v, want one automatic evaluation", id, evaluated)
				}
			}
			if test.now != nil {
				engine.set(func(f *fakeEngine) { f.evaluationByID["automatic-2"] = *test.now })
			}

			letTheLineGo()
			waitFor(t, 2*time.Second, "the second prompt to go to the operator", func() bool {
				pending := sup.State().Pending
				return len(pending) == 1 && pending[0].ID == "automatic-2" && !pending[0].Evaluation.Automatic
			})
			// The counts below are exact only once the automatic decisions have
			// finished.
			sup.BeginDrain()
			sup.Wait()

			if calls := engine.applySnapshot(); len(calls) != 1 || calls[0].event.ID != "automatic-1" {
				t.Fatalf("deliveries = %#v, want the first prompt's alone", calls)
			}
			evaluated := engine.auditFor(audit.KindPolicyEvaluated, "automatic-2")
			if len(evaluated) != 2 {
				t.Fatalf("policy_evaluated entries = %#v, want the detection's and the one before the decision", evaluated)
			}
			again := evaluated[1]
			wantMetadata := map[string]string{
				"automatic": "false", "effective_action": "ask", "mode": "enforce", "proposed_action": string(test.wantProposed),
			}
			if again.DecisionBy != audit.DecisionByPolicy || again.Decision != audit.DecisionAsk || again.Outcome != audit.OutcomeAsk ||
				again.Reason != test.wantReason || again.Rule != test.wantRule || !reflect.DeepEqual(again.Metadata, wantMetadata) {
				t.Fatalf("second policy_evaluated entry = %#v", again)
			}
			if decisions := engine.auditFor(audit.KindDecision, "automatic-2"); len(decisions) != 0 {
				t.Fatalf("the prompt the policy no longer answers was decided: %#v", decisions)
			}
			if deliveries := engine.auditFor(audit.KindDelivery, "automatic-2"); len(deliveries) != 0 {
				t.Fatalf("the prompt the policy no longer answers was delivered: %#v", deliveries)
			}
			view := sup.State().Pending[0]
			if view.DeliveryStatus != "pending" || view.Evaluation.Action != string(policy.ActionAsk) ||
				view.Evaluation.ProposedAction != string(test.wantProposed) || view.Evaluation.Reason != test.wantReason ||
				view.Evaluation.RuleName != test.wantRule || view.Evaluation.Automatic {
				t.Fatalf("prompt handed to the operator = %#v", view)
			}
			if agent := agentOf(t, sup, "agent-a"); agent.Status != "waiting" || agent.InputFrozen {
				t.Fatalf("agent = %#v, want waiting on the operator", agent)
			}
		})
	}
}

// A prompt the policy no longer answers is handed back only once the journal
// has recorded why. When the journal refuses that entry, the prompt is shown
// failed and its session frozen, as for a decision the journal refused, even
// in a drain: the journal's failure freezes the whole run only while it is
// active, and a drain waits for this answer, so the prompt would otherwise be
// left shown delivering once the run had stopped.
func TestAPromptWhoseSecondEvaluationTheJournalRefusesIsShownFailed(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	letTheLineGo := holdWithALine(t, engine, sup, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
	blocked := make(chan struct{}, 1)
	release := make(chan struct{})
	releaseEntry := releaser(t, release)
	engine.set(func(f *fakeEngine) {
		f.evaluationByID["automatic-1"] = policy.Evaluation{
			Action: policy.ActionAsk, ProposedAction: policy.ActionAllow, Reason: policy.ReasonConsecutiveLimit,
		}
		f.auditBlockKind = audit.KindPolicyEvaluated
		f.auditStarted = blocked
		f.auditRelease = release
	})

	letTheLineGo()
	within(t, blocked, "the second evaluation entry")
	sup.BeginDrain()
	engine.set(func(f *fakeEngine) { f.auditFailAt = f.auditCalls + 1 })
	releaseEntry()
	sup.Wait()

	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("deliveries = %#v, want none", calls)
	}
	if decisions := engine.auditFor(audit.KindDecision, "automatic-1"); len(decisions) != 0 {
		t.Fatalf("decision entries = %#v, want none", decisions)
	}
	state := sup.State()
	if len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "failed" || state.Pending[0].Evaluation.Reason != "audit_unavailable" {
		t.Fatalf("prompt after the refused entry = %#v, want it failed", state.Pending)
	}
	if agent := agentOf(t, sup, "agent-a"); !agent.InputFrozen {
		t.Fatalf("agent after the refused entry = %#v, want it frozen", agent)
	}
}

// An automatic decision is journaled under the rule that made it, named as the
// policy names it, as its evaluation was. The core decided from an evaluation
// it rebuilt from the prompt's display form, whose rule name is bounded and
// redacted, and its decision entries named no rule at all.
func TestAnAutomaticDecisionIsJournaledUnderItsRule(t *testing.T) {
	rule := "allow-" + strings.Repeat("r", 80)
	engine := newFakeEngine()
	evaluation := automaticAllow()
	evaluation.RuleName = rule
	engine.evaluation = evaluation
	sup, sink := newCoreForTest(t, engine, "agent-a")

	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
	waitFor(t, 2*time.Second, "the delivery", func() bool {
		return len(engine.auditFor(audit.KindDelivery, "automatic-1")) == 1
	})
	sup.BeginDrain()
	sup.Wait()

	if shown := sink.snapshot()[1].view.Evaluation.RuleName; shown == rule || shown == "" {
		t.Fatalf("the prompt shows the rule as %q, want it bounded", shown)
	}
	if evaluated := engine.auditFor(audit.KindPolicyEvaluated, "automatic-1"); len(evaluated) != 1 || evaluated[0].Rule != rule {
		t.Fatalf("policy_evaluated entries = %#v, want one under the rule", evaluated)
	}
	decisions := engine.auditFor(audit.KindDecision, "automatic-1")
	if len(decisions) != 1 || decisions[0].Rule != rule || decisions[0].DecisionBy != audit.DecisionByPolicy {
		t.Fatalf("decision entries = %#v, want one by the policy under its rule", decisions)
	}
}

// The desktop's TestProcessExitDuringAutomaticDeliveryStillRecordsTerminalOutcome,
// through the core: the process exits while the answer is written, and the
// delivery still gets exactly one terminal journal entry. The desktop decided
// synchronously, so its count was final when the test read it; the core
// decides on a goroutine of its own, and the count is final only once the run
// has drained. Counted at the first entry, a second one written a moment
// later went unseen.
func TestAProcessExitDuringAnAutomaticDeliveryLeavesOneTerminalEntry(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	sup, _ := newCoreForTest(t, engine, "agent-a")
	exit := exitEvent("agent-a", 0, false)
	engine.set(func(f *fakeEngine) {
		f.beforeApplyReturn = func() { sup.Handle(session.AdapterEvent{Event: exit}) }
	})

	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-exit")})
	waitFor(t, 2*time.Second, "the terminal delivery entry", func() bool {
		return len(engine.auditFor(audit.KindDelivery, "automatic-exit")) > 0
	})
	sup.BeginDrain()
	sup.Wait()

	deliveries := engine.auditFor(audit.KindDelivery, "automatic-exit")
	if len(deliveries) != 1 || deliveries[0].Outcome != audit.OutcomeApplied || deliveries[0].Reason != "delivery_applied" {
		t.Fatalf("terminal delivery entries = %#v, want exactly one applied", deliveries)
	}
	if agent := agentOf(t, sup, "agent-a"); agent.Running || agent.Status != "exited" {
		t.Fatalf("agent = %#v", agent)
	}
	if ids := pendingIDs(sup); len(ids) != 0 {
		t.Fatalf("pending = %v", ids)
	}
}

// The core emits at the points and in the order the desktop did. A front end
// that renders the emissions as they come relies on that order.
func TestTheSinkSeesTheDesktopsEmissionOrder(t *testing.T) {
	t.Run("automatic decision", func(t *testing.T) {
		engine := newFakeEngine()
		engine.evaluation = automaticAllow()
		sup, sink := newCoreForTest(t, engine, "agent-a")
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
		waitFor(t, 2*time.Second, "the delivery", func() bool { return len(trace(sink.snapshot())) == 5 })
		want := []string{"refresh", "prompt:pending", "prompt:delivering", "prompt:delivered", "status:running"}
		if got := trace(sink.snapshot()); !reflect.DeepEqual(got, want) {
			t.Fatalf("sink = %v, want %v", got, want)
		}
	})
	t.Run("human decision", func(t *testing.T) {
		engine := newFakeEngine()
		sup, sink := newCoreForTest(t, engine, "agent-a")
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
		sink.reset()
		if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", desktop); err != nil {
			t.Fatalf("SubmitDecision: %v", err)
		}
		want := []string{"prompt:delivering", "prompt:delivered", "status:running"}
		if got := trace(sink.snapshot()); !reflect.DeepEqual(got, want) {
			t.Fatalf("sink = %v, want %v", got, want)
		}
	})
	t.Run("stop then start", func(t *testing.T) {
		engine := newFakeEngine()
		sup, sink := newCoreForTest(t, engine, "agent-a")
		if err := sup.StopSession(testRunID, "agent-a"); err != nil {
			t.Fatalf("StopSession: %v", err)
		}
		if err := sup.StartSession(testRunID, "agent-a"); err != nil {
			t.Fatalf("StartSession: %v", err)
		}
		want := []string{"status:stopping", "status:exited", "status:starting", "lifecycle:started", "status:running", "refresh"}
		calls := sink.snapshot()
		if got := trace(calls); !reflect.DeepEqual(got, want) {
			t.Fatalf("sink = %v, want %v", got, want)
		}
		if calls[2].status.ClearedBefore == "" || calls[4].status.ClearedBefore == "" {
			t.Fatalf("a start clears nothing: %#v %#v", calls[2].status, calls[4].status)
		}
	})
}

// A front end drops a session's output when the core reports its new process,
// and shows the agent from the core's state. The report comes before the core
// shows the agent running: in the other order a front end reading between the
// two steps showed the new process running with its predecessor's output. The
// sink reads the core at the report, as a front end's GetState may.
func TestANewProcessIsReportedBeforeTheAgentIsShownRunning(t *testing.T) {
	for _, test := range []struct {
		name       string
		wantStatus string
		replace    func(*supervise.Supervisor) error
	}{
		{
			name:       "start",
			wantStatus: "starting",
			replace: func(sup *supervise.Supervisor) error {
				sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
				return sup.StartSession(testRunID, "agent-a")
			},
		},
		{
			name:       "restart",
			wantStatus: "stopping",
			replace: func(sup *supervise.Supervisor) error {
				return sup.RestartSession(testRunID, "agent-a")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			sup, sink := newCoreForTest(t, newFakeEngine(), "agent-a")
			var atReport []supervise.Agent
			sink.mu.Lock()
			sink.probe = func() {
				// No automatic decision runs here, so the last call is the one
				// that ran this probe.
				calls := sink.snapshot()
				if calls[len(calls)-1].kind == "lifecycle" {
					agent, _ := sup.Agent("agent-a")
					atReport = append(atReport, agent)
				}
			}
			sink.mu.Unlock()

			if err := test.replace(sup); err != nil {
				t.Fatalf("%s: %v", test.name, err)
			}
			if len(atReport) != 1 {
				t.Fatalf("the new process was reported %d times, want once", len(atReport))
			}
			if agent := atReport[0]; agent.Status != test.wantStatus {
				t.Fatalf("agent when its new process was reported = %#v, want still %s", agent, test.wantStatus)
			}
			if agent := agentOf(t, sup, "agent-a"); !agent.Running || agent.Status != "running" {
				t.Fatalf("agent after the %s = %#v", test.name, agent)
			}
		})
	}
}

// The notices a front end turns into notifications. A guardrail notice is sent
// whenever a guardrail decided, automatically or not, and a pending one only
// when a human must decide. A notice's details are the prompt's display-safe
// summary, the one the prompt itself is shown with: a notification leaves
// the machine (a webhook posts it as it is), so it carries nothing the
// journal would not. The adapter's raw summary used to go out, even for a
// prompt that asked for a password.
func TestNoticesFollowTheDesktopsRules(t *testing.T) {
	const secret = "otp-493827-super-secret"
	guardrail := func(reason string, automatic bool) policy.Evaluation {
		return policy.Evaluation{Action: policy.ActionDeny, ProposedAction: policy.ActionDeny, Reason: reason, Automatic: automatic}
	}
	for _, test := range []struct {
		name       string
		evaluation policy.Evaluation
		sensitive  bool
		summary    string
		want       supervise.Notice
	}{
		{
			name:       "pending decision",
			evaluation: policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionAsk, Reason: policy.ReasonRule},
			want: supervise.Notice{
				Kind: supervise.NoticePendingDecision, Severity: supervise.SeverityWarning, Reason: "confirmation required",
				Details: "Password: [REDACTED]",
			},
		},
		{
			name:       "sensitive input",
			evaluation: policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionAsk, Reason: policy.ReasonSensitive},
			sensitive:  true,
			want: supervise.Notice{
				Kind: supervise.NoticePendingDecision, Severity: supervise.SeverityWarning, Reason: "sensitive input required",
				Details: "Sensitive input required",
			},
		},
		{
			// The policy's own reason for a sensitive prompt is sensitive_event;
			// the notice compared it with "sensitive", which it never is.
			name:       "sensitive input by the policy's reason alone",
			evaluation: policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionAsk, Reason: policy.ReasonSensitive},
			want: supervise.Notice{
				Kind: supervise.NoticePendingDecision, Severity: supervise.SeverityWarning, Reason: "sensitive input required",
				Details: "Password: [REDACTED]",
			},
		},
		{
			name:       "a summary on several lines",
			evaluation: policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionAsk, Reason: policy.ReasonRule},
			summary:    "Overwrite\r\nconfig.yaml?\t" + strings.Repeat("x", 200),
			want: supervise.Notice{
				Kind: supervise.NoticePendingDecision, Severity: supervise.SeverityWarning, Reason: "confirmation required",
				Details: "Overwrite config.yaml? " + strings.Repeat("x", 96) + "…",
			},
		},
		{
			name:       "guardrail on an automatic decision",
			evaluation: guardrail(policy.ReasonDestructive, true),
			want: supervise.Notice{
				Kind: supervise.NoticeGuardrailBlocked, Severity: supervise.SeverityCritical,
				Reason: "security guardrail blocked (destructive_command_blocked)", Details: "Password: [REDACTED]",
			},
		},
		{
			name:       "guardrail on a prompt a human decides",
			evaluation: guardrail(policy.ReasonDestructive, false),
			want: supervise.Notice{
				Kind: supervise.NoticeGuardrailBlocked, Severity: supervise.SeverityCritical,
				Reason: "security guardrail blocked (destructive_command_blocked)", Details: "Password: [REDACTED]",
			},
		},
		{
			name:       "exfiltration guardrail",
			evaluation: guardrail(policy.ReasonExfiltration, true),
			want: supervise.Notice{
				Kind: supervise.NoticeGuardrailBlocked, Severity: supervise.SeverityCritical,
				Reason: "security guardrail blocked (exfiltration_attempt_blocked)", Details: "Password: [REDACTED]",
			},
		},
		{
			name:       "guardrail pattern",
			evaluation: guardrail(policy.ReasonGuardrailBlocked, true),
			sensitive:  true,
			want: supervise.Notice{
				Kind: supervise.NoticeGuardrailBlocked, Severity: supervise.SeverityCritical,
				Reason: "security guardrail blocked (guardrail_pattern_blocked)", Details: "Sensitive input required",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluation = test.evaluation
			sup, sink := newCoreForTest(t, engine, "agent-a")
			prompt := promptEvent("agent-a", "prompt-1")
			prompt.Summary = "Password: " + secret
			if test.summary != "" {
				prompt.Summary = test.summary
			}
			prompt.Sensitive = test.sensitive
			sup.Handle(session.AdapterEvent{Event: prompt})

			var notices []supervise.Notice
			shownSummary := ""
			for _, call := range sink.snapshot() {
				switch call.kind {
				case "notify":
					notices = append(notices, call.notice)
				case "prompt":
					shownSummary = call.view.Summary
				}
			}
			want := test.want
			want.AgentName = "agent-a"
			want.SessionID = "agent-a"
			want.EventID = "prompt-1"
			if len(notices) != 1 || !reflect.DeepEqual(notices[0], want) {
				t.Fatalf("notices = %#v, want %#v", notices, want)
			}
			if notices[0].Details != shownSummary {
				t.Fatalf("notice details %q, want the summary the prompt is shown with, %q", notices[0].Details, shownSummary)
			}
			if strings.Contains(sinkText(sink.snapshot()), secret) {
				t.Fatal("the prompt's secret reached the sink")
			}
		})
	}

	t.Run("automatic decision", func(t *testing.T) {
		engine := newFakeEngine()
		engine.evaluation = automaticAllow()
		sup, sink := newCoreForTest(t, engine, "agent-a")
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
		for _, call := range sink.snapshot() {
			if call.kind == "notify" {
				t.Fatalf("an automatic decision notified: %#v", call.notice)
			}
		}
	})
}

// The sink is called without the core's lock held, so a front end's sink may
// read the core — State, Agent, Draining — from inside any callback. What the
// core shows is queued under its lock and shown after it is released: a call
// made under the lock, or a queue shown while holding it, would deadlock here
// and fail on the watchdog.
func TestTheSinkIsNeverCalledUnderTheCoreLock(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["auto-1"] = automaticAllow()
	engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
	sup, sink := newCoreForTest(t, engine, "agent-a", "agent-b")
	sink.mu.Lock()
	sink.probe = func() {
		_ = sup.State()
		_, _ = sup.Agent("agent-a")
		_ = sup.Draining()
	}
	sink.mu.Unlock()

	finished := make(chan struct{})
	go func() {
		defer close(finished)
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "auto-1")})
		// Not waitFor: this goroutine is not the test's and must not stop it.
		for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(time.Millisecond) {
			if len(engine.auditFor(audit.KindDelivery, "auto-1")) == 1 && len(pendingIDs(sup)) == 0 {
				break
			}
		}
		ask := promptEvent("agent-a", "ask-1")
		ask.Sequence = 2
		sup.Handle(session.AdapterEvent{Event: ask})
		_ = sup.SubmitAutomaticDecision(testRunID, "agent-a", "ask-1", "deny", desktop)
		withdrawn := promptEvent("agent-a", "ask-2")
		withdrawn.Sequence = 3
		sup.Handle(session.AdapterEvent{Event: withdrawn})
		sup.Handle(session.AdapterEventWithdrawn{Event: withdrawn})
		_ = sup.SubmitLine(testRunID, "agent-a", "hello", desktop)
		_ = sup.StopSession(testRunID, "agent-a")
		_ = sup.StartSession(testRunID, "agent-a")
		_ = sup.RestartSession(testRunID, "agent-a")
		sup.Handle(session.AdapterEvent{Event: adapters.Event{SessionID: "agent-a"}})
		sup.Handle(session.Error{SessionID: "agent-b", Err: errors.New("stream")})
		engine.set(func(f *fakeEngine) { f.auditFailAt = f.auditCalls + 1 })
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-b", "frozen-1")})
		sup.Handle(session.AdapterEvent{Event: exitEvent("agent-b", 1, true)})
		sup.Handle(session.Exited{SessionID: "agent-a"})
	}()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("a sink call deadlocked on the core's lock")
	}

	seen := map[string]bool{}
	for _, call := range sink.snapshot() {
		seen[call.kind] = true
	}
	for _, kind := range []string{"prompt", "status", "error", "refresh", "lifecycle", "notify"} {
		if !seen[kind] {
			t.Errorf("the scenario never reached the sink's %s", kind)
		}
	}
	if !sup.State().AuditFailed {
		t.Error("the scenario never failed the journal")
	}
}

// Between runs a front end has no supervisor. Every operation then checks its
// own arguments first and refuses as a stopped run, as the desktop always did.
func TestAnOperationWithoutARunRefusesAfterCheckingItsArguments(t *testing.T) {
	var sup *supervise.Supervisor
	for name, test := range map[string]struct {
		err  error
		want error
	}{
		"empty answer":   {sup.SubmitDecision("", "agent-a", "prompt-1", " ", desktop), supervise.ErrEmptyDecision},
		"answer":         {sup.SubmitDecision("", "agent-a", "prompt-1", "y", desktop), supervise.ErrRuntimeStopped},
		"unknown choice": {sup.SubmitAutomaticDecision("", "agent-a", "prompt-1", "maybe", desktop), supervise.ErrUnsupportedDecision},
		"choice":         {sup.SubmitAutomaticDecision("", "agent-a", "prompt-1", "allow", desktop), supervise.ErrRuntimeStopped},
		"line":           {sup.SubmitLine("", "agent-a", "hello", desktop), supervise.ErrRuntimeStopped},
		"stop":           {sup.StopSession("", "agent-a"), supervise.ErrRuntimeStopped},
		"start":          {sup.StartSession("", "agent-a"), supervise.ErrRuntimeStopped},
		"restart":        {sup.RestartSession("", "agent-a"), supervise.ErrRuntimeStopped},
	} {
		if !errors.Is(test.err, test.want) {
			t.Errorf("%s = %v, want %v", name, test.err, test.want)
		}
	}
}

func TestNewRefusesAnIncompleteRun(t *testing.T) {
	engine := newFakeEngine()
	//lint:ignore SA1012 a nil context is the case under test
	if _, err := supervise.New(nil, engine, supervise.Options{RunID: testRunID}); err == nil {
		t.Error("New accepted a nil context")
	}
	if _, err := supervise.New(context.Background(), nil, supervise.Options{RunID: testRunID}); err == nil {
		t.Error("New accepted a nil engine")
	}
	if _, err := supervise.New(context.Background(), engine, supervise.Options{RunID: "  "}); err == nil {
		t.Error("New accepted a blank run ID")
	}
	sup, err := supervise.New(context.Background(), engine, supervise.Options{
		RunID:  testRunID,
		Agents: []supervise.AgentSpec{{SessionID: "Agent-A", AgentID: "Agent-A", Name: "A", Backend: "pty", Adapter: "generic"}},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	agent, found := sup.Agent("agent-a")
	if !found || !agent.Running || agent.Status != "running" || agent.Name != "A" {
		t.Fatalf("a new run's agent = %#v, found=%t", agent, found)
	}
	// A nil sink discards: the run works without one.
	sup.Handle(session.AdapterEvent{Event: promptEvent("Agent-A", "prompt-1")})
	if ids := pendingIDs(sup); len(ids) != 1 {
		t.Fatalf("pending without a sink = %v", ids)
	}
}

// A drain admits nothing more and waits for what it admitted.
func TestADrainWaitsForAdmittedWritesAndAdmitsNoMore(t *testing.T) {
	sup, _ := newCoreForTest(t, newFakeEngine(), "agent-a")
	release, admitted := sup.AdmitRun()
	if !admitted {
		t.Fatal("a live run refused a write")
	}
	sup.BeginDrain()
	if !sup.Draining() {
		t.Fatal("the run does not report draining")
	}
	if _, admitted := sup.AdmitRun(); admitted {
		t.Fatal("a draining run admitted a write")
	}
	if err := sup.SubmitLine(testRunID, "agent-a", "hello", desktop); !errors.Is(err, supervise.ErrRuntimeStopped) {
		t.Fatalf("a line during the drain = %v, want ErrRuntimeStopped", err)
	}
	waited := make(chan struct{})
	go func() {
		sup.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("Wait returned while a write was admitted")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return once the write was released")
	}
}
