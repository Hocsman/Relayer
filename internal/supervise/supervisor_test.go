package supervise_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"sync"
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
// they pin a behaviour already known to be wrong, a comment beginning with
// DEFECT names the fix that must flip them.

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

	err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y")
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

	if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y"); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("a human answer after the uncertainty = %v, want ErrDeliveryUncertain", err)
	}
	if err := sup.SubmitLine(testRunID, "agent-a", "hello"); !errors.Is(err, supervise.ErrDeliveryUncertain) {
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
	if err := sup.SubmitDecision(testRunID, "Agent-A", "prompt-1", "y"); err != nil {
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

// DEFECT (v0.8.8 fix "stale failed exit journaled failed"): the exit of a
// process a replacement already superseded is journaled finished even when it
// failed; the current exit above is journaled failed. The fix flips the
// expected outcome to failed. Showing nothing is right: the replacement runs.
func TestAStaleFailedExitIsJournaledAsFinished(t *testing.T) {
	engine := newFakeEngine()
	engine.staleExits = true
	sup, sink := newCoreForTest(t, engine, "agent-a")
	exit := exitEvent("agent-a", 3, true)

	sup.Handle(session.AdapterEvent{Event: exit})

	finished := engine.auditFor(audit.KindSessionFinished, exit.ID)
	if len(finished) != 1 || finished[0].Reason != "process_exit" ||
		!reflect.DeepEqual(finished[0].Metadata, map[string]string{"exit_code": "3", "failed": "true"}) {
		t.Fatalf("stale exit journal = %#v", finished)
	}
	if finished[0].Outcome != audit.OutcomeFinished {
		t.Fatalf("stale failed exit outcome = %q: the defect is fixed, flip this test", finished[0].Outcome)
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
}

// DEFECT (v0.8.8 fix "withdrawal keeps the in-flight claim"): withdrawing
// the prompt whose answer is being written releases the session's claim, so
// the next automatic prompt is delivered while the first write is still in
// progress. The fix keeps the claim until the first delivery returns, and
// flips this test to "no second write starts before the first returns".
func TestWithdrawingTheDeliveringPromptLetsTheNextOneOverlapIt(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	started := make(chan struct{}, 2)
	release := make(chan struct{})
	engine.applyStarted = started
	engine.applyRelease = release
	sup, _ := newCoreForTest(t, engine, "agent-a")
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseAll)

	first := promptEvent("agent-a", "prompt-1")
	second := promptEvent("agent-a", "prompt-2")
	second.Sequence = 2
	sup.Handle(session.AdapterEvent{Event: first})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("the first automatic answer was never written")
	}
	sup.Handle(session.AdapterEvent{Event: second})
	select {
	case <-started:
		t.Fatal("the second answer started while the first held the session")
	case <-time.After(50 * time.Millisecond):
	}

	sup.Handle(session.AdapterEventWithdrawn{Event: first})
	overlapped := false
	select {
	case <-started:
		overlapped = true
	case <-time.After(2 * time.Second):
	}
	releaseAll()
	if !overlapped {
		t.Fatal("the second answer waited for the first: the defect is fixed, flip this test")
	}
	waitFor(t, 2*time.Second, "both deliveries to be journaled", func() bool {
		return len(engine.auditFor(audit.KindDelivery, "prompt-1")) == 1 && len(engine.auditFor(audit.KindDelivery, "prompt-2")) == 1
	})
	if calls := engine.applySnapshot(); len(calls) != 2 {
		t.Fatalf("deliveries = %#v, want two", calls)
	}
}

// DEFECT (v0.8.8 fix "'waiting' after a start with a kept prompt"): a prompt
// the new process raised while it started is kept, but the agent is shown
// running rather than waiting on it. The fix flips the expected status.
func TestAStartShowsRunningWhileThePromptItRaisedWaits(t *testing.T) {
	engine := newFakeEngine()
	startStarted := make(chan string, 1)
	startRelease := make(chan struct{})
	engine.agentStartStarted = startStarted
	engine.agentStartRelease = startRelease
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})

	done := make(chan error, 1)
	go func() { done <- sup.StartSession(testRunID, "agent-a") }()
	select {
	case <-startStarted:
	case <-time.After(2 * time.Second):
		close(startRelease)
		t.Fatal("the start never reached the runtime")
	}
	prompt := promptEvent("agent-a", "prompt-early")
	prompt.Timestamp = time.Now().UTC()
	sup.Handle(session.AdapterEvent{Event: prompt})
	close(startRelease)
	if err := <-done; err != nil {
		t.Fatalf("StartSession: %v", err)
	}

	if ids := pendingIDs(sup); len(ids) != 1 || ids[0] != "prompt-early" {
		t.Fatalf("pending after the start = %v, want the prompt raised during it", ids)
	}
	agent := agentOf(t, sup, "agent-a")
	if !agent.Running {
		t.Fatalf("agent after the start = %#v", agent)
	}
	if agent.Status != "running" {
		t.Fatalf("agent status = %q: the defect is fixed, flip this test to waiting", agent.Status)
	}
}

// DEFECT (v0.8.8 fix "human-path ErrDecisionUnsupported falls back to ask
// without freezing"): an answer the adapter cannot encode is refused before
// any byte is written, yet the human path treats it as an uncertain delivery
// and freezes the session; the automatic path falls back to ask
// (TestAnUnsupportedAutomaticDecisionFallsBackToAsk). The fix flips this test.
func TestAHumanAnswerTheAdapterCannotEncodeFreezesTheSession(t *testing.T) {
	engine := newFakeEngine()
	engine.applyErr = adapters.ErrDecisionUnsupported
	sup, sink := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})

	err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y")
	if !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("SubmitDecision = %v: the defect is fixed, flip this test", err)
	}
	deliveries := engine.auditFor(audit.KindDelivery, "prompt-1")
	if len(deliveries) != 1 || deliveries[0].Outcome != audit.OutcomeFallbackDeliveryUncertain || deliveries[0].Reason != "delivery_uncertain" {
		t.Fatalf("delivery entries = %#v", deliveries)
	}
	if agent := agentOf(t, sup, "agent-a"); !agent.InputFrozen {
		t.Fatalf("agent = %#v, want the session frozen", agent)
	}
	if state := sup.State(); len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "uncertain" {
		t.Fatalf("prompt = %#v", state.Pending)
	}
	found := false
	for _, call := range sink.snapshot() {
		found = found || (call.kind == "error" && call.failure.Code == "delivery_uncertain")
	}
	if !found {
		t.Fatal("the freeze was not reported")
	}
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

// The desktop's TestProcessExitDuringAutomaticDeliveryStillRecordsTerminalOutcome,
// through the core: the process exits while the answer is written, and the
// delivery still gets exactly one terminal journal entry.
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
		if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y"); err != nil {
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
// even when the policy decides alone; a pending one only when a human must.
func TestNoticesFollowTheDesktopsRules(t *testing.T) {
	const secret = "otp-493827-super-secret"
	for _, test := range []struct {
		name       string
		evaluation policy.Evaluation
		sensitive  bool
		want       supervise.Notice
	}{
		{
			name:       "pending decision",
			evaluation: policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionAsk, Reason: policy.ReasonRule},
			want: supervise.Notice{
				Kind: supervise.NoticePendingDecision, Severity: supervise.SeverityWarning, Reason: "confirmation required",
			},
		},
		{
			name:       "sensitive input",
			evaluation: policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionAsk, Reason: policy.ReasonSensitive},
			sensitive:  true,
			want: supervise.Notice{
				Kind: supervise.NoticePendingDecision, Severity: supervise.SeverityWarning, Reason: "sensitive input required",
			},
		},
		{
			name:       "guardrail on an automatic decision",
			evaluation: policy.Evaluation{Action: policy.ActionDeny, ProposedAction: policy.ActionDeny, Reason: policy.ReasonDestructive, Automatic: true},
			want: supervise.Notice{
				Kind: supervise.NoticeGuardrailBlocked, Severity: supervise.SeverityCritical,
				Reason: "security guardrail blocked (destructive_command_blocked)",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluation = test.evaluation
			sup, sink := newCoreForTest(t, engine, "agent-a")
			prompt := promptEvent("agent-a", "prompt-1")
			prompt.Summary = "Password: " + secret
			prompt.Sensitive = test.sensitive
			sup.Handle(session.AdapterEvent{Event: prompt})

			var notices []supervise.Notice
			for _, call := range sink.snapshot() {
				if call.kind == "notify" {
					notices = append(notices, call.notice)
				}
			}
			want := test.want
			want.AgentName = "agent-a"
			want.SessionID = "agent-a"
			want.EventID = "prompt-1"
			// DEFECT (v0.8.8 fix "safe notification summary"): Details is the
			// adapter's raw summary, even for a sensitive prompt. The fix sends
			// the display-safe summary and flips this expectation.
			want.Details = prompt.Summary
			if len(notices) != 1 || !reflect.DeepEqual(notices[0], want) {
				t.Fatalf("notices = %#v, want %#v", notices, want)
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
// read the core — State, Agent, Draining — from inside any callback. A call
// made under the lock would deadlock here and fail on the watchdog.
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
		_ = sup.SubmitAutomaticDecision(testRunID, "agent-a", "ask-1", "deny")
		withdrawn := promptEvent("agent-a", "ask-2")
		withdrawn.Sequence = 3
		sup.Handle(session.AdapterEvent{Event: withdrawn})
		sup.Handle(session.AdapterEventWithdrawn{Event: withdrawn})
		_ = sup.SubmitLine(testRunID, "agent-a", "hello")
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
		"empty answer":   {sup.SubmitDecision("", "agent-a", "prompt-1", " "), supervise.ErrEmptyDecision},
		"answer":         {sup.SubmitDecision("", "agent-a", "prompt-1", "y"), supervise.ErrRuntimeStopped},
		"unknown choice": {sup.SubmitAutomaticDecision("", "agent-a", "prompt-1", "maybe"), supervise.ErrUnsupportedDecision},
		"choice":         {sup.SubmitAutomaticDecision("", "agent-a", "prompt-1", "allow"), supervise.ErrRuntimeStopped},
		"line":           {sup.SubmitLine("", "agent-a", "hello"), supervise.ErrRuntimeStopped},
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
	release, admitted := sup.Admit()
	if !admitted {
		t.Fatal("a live run refused a write")
	}
	sup.BeginDrain()
	if !sup.Draining() {
		t.Fatal("the run does not report draining")
	}
	if _, admitted := sup.Admit(); admitted {
		t.Fatal("a draining run admitted a write")
	}
	if err := sup.SubmitLine(testRunID, "agent-a", "hello"); !errors.Is(err, supervise.ErrRuntimeStopped) {
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
