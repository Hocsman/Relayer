package supervise_test

import (
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// These tests pin the admission of raw input: the keystrokes a terminal's
// holder types, which reach the agent without the policy and are never
// journaled. They are bounded instead: only the holder may write, never while
// the run or the session cannot take a write, and through the session's one
// write slot, the one its decisions and lines take.

// admitted fails the test unless the write is admitted, and returns its
// release.
func admitted(t *testing.T, sup *supervise.Supervisor, sessionID, connID string) func() {
	t.Helper()
	release, err := sup.Admit(sessionID, connID)
	if err != nil {
		t.Fatalf("Admit(%q, %q) = %v, want the write admitted", sessionID, connID, err)
	}
	return release
}

// Only the connection that holds a session's hand may type into it: not
// another connection, not the empty one, and not anybody while nobody holds
// it, so every raw byte falls within a hand the front end journaled taking.
// The session is named as anywhere else in the core, whatever its case.
func TestRawInputIsAdmittedOnlyFromTheHolder(t *testing.T) {
	engine := newFakeEngine()
	sup, _ := newCoreForTest(t, engine, "agent-a", "agent-b")
	for _, connID := range []string{"conn-1", ""} {
		if _, err := sup.Admit("agent-a", connID); !errors.Is(err, supervise.ErrNotHolder) {
			t.Fatalf("a write by %q while nobody holds the terminal = %v, want ErrNotHolder", connID, err)
		}
	}

	sup.SetHolder("agent-a", "conn-1")
	for _, test := range []struct{ sessionID, connID string }{
		{"agent-a", "conn-2"}, {"agent-a", ""}, {"agent-b", "conn-1"}, {"agent-z", "conn-1"},
	} {
		if _, err := sup.Admit(test.sessionID, test.connID); !errors.Is(err, supervise.ErrNotHolder) {
			t.Fatalf("a write to %q by %q = %v, want ErrNotHolder", test.sessionID, test.connID, err)
		}
	}
	admitted(t, sup, " Agent-A ", " conn-1 ")()

	sup.SetHolder("agent-a", "conn-2")
	if _, err := sup.Admit("agent-a", "conn-1"); !errors.Is(err, supervise.ErrNotHolder) {
		t.Fatalf("a write by the former holder = %v, want ErrNotHolder", err)
	}
	admitted(t, sup, "agent-a", "conn-2")()
	sup.SetHolder("agent-a", "")
	if _, err := sup.Admit("agent-a", "conn-2"); !errors.Is(err, supervise.ErrNotHolder) {
		t.Fatalf("a write once the terminal is released = %v, want ErrNotHolder", err)
	}

	var none *supervise.Supervisor
	if _, err := none.Admit("agent-a", "conn-1"); !errors.Is(err, supervise.ErrRuntimeStopped) {
		t.Fatalf("a write with no run = %v, want ErrRuntimeStopped", err)
	}
	if entries := engine.auditSnapshot(); len(entries) != 0 {
		t.Fatalf("admitting raw input journaled %#v", entries)
	}
}

// The holder's write is refused when the run or the session cannot take one:
// a drain, a journal that failed, a session frozen by an uncertain write, a
// process stopped, stopping or starting, and a session another write already
// holds, whether a person's answer, the policy's, a line or other keystrokes.
func TestRawInputIsRefusedWhenTheSessionCannotTakeAWrite(t *testing.T) {
	for _, test := range []struct {
		name string
		// set brings agent-a, whose hand conn-1 holds, to the state under
		// test; it returns what lets anything it holds go.
		set  func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func()
		want error
	}{
		{name: "the run drains", want: supervise.ErrRuntimeStopped,
			set: func(_ *testing.T, sup *supervise.Supervisor, _ *fakeEngine) func() {
				sup.BeginDrain()
				return func() {}
			}},
		{name: "the journal failed", want: supervise.ErrAuditUnavailable,
			set: func(_ *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func() {
				engine.set(func(f *fakeEngine) { f.auditFailAt = f.auditCalls + 1 })
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
				return func() {}
			}},
		// The drain comes first, as it does in the order Admit documents:
		// refused as a journal failure, the drain read as one.
		{name: "the run drains after the journal failed", want: supervise.ErrRuntimeStopped,
			set: func(_ *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func() {
				engine.set(func(f *fakeEngine) { f.auditFailAt = f.auditCalls + 1 })
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
				sup.BeginDrain()
				return func() {}
			}},
		{name: "the session is frozen", want: supervise.ErrDeliveryUncertain,
			set: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func() {
				engine.set(func(f *fakeEngine) { f.applyErr = errors.New("write failed half way") })
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
				if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", alice); !errors.Is(err, supervise.ErrDeliveryUncertain) {
					t.Fatalf("the uncertain answer = %v", err)
				}
				return func() {}
			}},
		{name: "the process exited", want: supervise.ErrLineUnavailable,
			set: func(_ *testing.T, sup *supervise.Supervisor, _ *fakeEngine) func() {
				sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
				return func() {}
			}},
		{name: "the process is stopping", want: supervise.ErrLineUnavailable,
			set: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func() {
				started, release := make(chan string, 1), make(chan struct{})
				engine.set(func(f *fakeEngine) { f.stopStarted, f.stopRelease = started, release })
				go func() { _ = sup.StopSession(testRunID, "agent-a") }()
				<-started
				return releaser(t, release)
			}},
		{name: "the process is restarting", want: supervise.ErrLineUnavailable,
			set: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func() {
				started, release := make(chan string, 1), make(chan struct{})
				engine.set(func(f *fakeEngine) { f.agentRestartStarted, f.agentRestartRelease = started, release })
				go func() { _ = sup.RestartSession(testRunID, "agent-a") }()
				<-started
				return releaser(t, release)
			}},
		{name: "the process is starting", want: supervise.ErrLineUnavailable,
			set: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func() {
				sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
				started, release := make(chan string, 1), make(chan struct{})
				engine.set(func(f *fakeEngine) { f.agentStartStarted, f.agentStartRelease = started, release })
				go func() { _ = sup.StartSession(testRunID, "agent-a") }()
				<-started
				return releaser(t, release)
			}},
		{name: "a person's answer is being written", want: supervise.ErrDecisionInFlight,
			set: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func() {
				started, release := make(chan struct{}, 1), make(chan struct{})
				engine.set(func(f *fakeEngine) { f.applyStarted, f.applyRelease = started, release })
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
				go func() { _ = sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", alice) }()
				<-started
				return releaser(t, release)
			}},
		{name: "the policy's answer is being written", want: supervise.ErrDecisionInFlight,
			set: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func() {
				started, release := make(chan struct{}, 1), make(chan struct{})
				engine.set(func(f *fakeEngine) {
					f.applyStarted, f.applyRelease = started, release
					f.evaluationByID["automatic-1"] = automaticAllow()
				})
				sup.SetHolder("agent-a", "")
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
				<-started
				sup.SetHolder("agent-a", "conn-1")
				return releaser(t, release)
			}},
		{name: "a line is being written", want: supervise.ErrDecisionInFlight,
			set: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) func() {
				started, release := make(chan string, 1), make(chan struct{})
				engine.set(func(f *fakeEngine) { f.lineStarted, f.lineRelease = started, release })
				sup.SetHolder("agent-a", "")
				go func() { _ = sup.SubmitLine(testRunID, "agent-a", "hello", alice) }()
				<-started
				sup.SetHolder("agent-a", "conn-1")
				return releaser(t, release)
			}},
		{name: "other keystrokes are being written", want: supervise.ErrDecisionInFlight,
			set: func(t *testing.T, sup *supervise.Supervisor, _ *fakeEngine) func() {
				return admitted(t, sup, "agent-a", "conn-1")
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			sup, _ := newCoreForTest(t, engine, "agent-a", "agent-b")
			sup.SetHolder("agent-a", "conn-1")
			sup.SetHolder("agent-b", "conn-1")
			letGo := test.set(t, sup, engine)
			defer letGo()
			journaled := len(engine.auditSnapshot())

			if _, err := sup.Admit("agent-a", "conn-1"); !errors.Is(err, test.want) {
				t.Fatalf("the holder's write = %v, want %v", err, test.want)
			}
			if entries := engine.auditSnapshot(); len(entries) != journaled {
				t.Fatalf("refusing the write journaled %#v", entries[journaled:])
			}
			// Another session is written to as it always is, unless the
			// whole run refuses.
			release, err := sup.Admit("agent-b", "conn-1")
			if err == nil {
				release()
			}
			wholeRun := errors.Is(test.want, supervise.ErrRuntimeStopped) || errors.Is(test.want, supervise.ErrAuditUnavailable)
			if (err == nil) == wholeRun {
				t.Fatalf("a write to the other session = %v", err)
			}
		})
	}
}

// While the holder's keystrokes are admitted they hold the session's one
// write slot: a person's answer and a line are refused as while an answer is
// being written, and so is more raw input; the policy's answer waits. Other
// sessions are not held up. Once the keystrokes are written, the prompts they
// found pending are the terminal's (TestAPromptItsHolderTypedIntoIsTheTerminals),
// and the policy answers the session's next prompt as usual.
func TestAdmittedRawInputHoldsTheSessionsWriteSlot(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["automatic-2"] = automaticAllow()
	engine.evaluationByID["automatic-3"] = automaticAllow()
	engine.evaluationByID["other-1"] = automaticAllow()
	sup, _ := newCoreForTest(t, engine, "agent-a", "agent-b")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "asked-1")})
	sup.SetHolder("agent-a", "conn-1")
	release := admitted(t, sup, "agent-a", "conn-1")
	journaled := len(engine.auditSnapshot())

	if err := sup.SubmitDecision(testRunID, "agent-a", "asked-1", "y", alice); !errors.Is(err, supervise.ErrDecisionInFlight) {
		t.Fatalf("an answer while keystrokes are written = %v, want ErrDecisionInFlight", err)
	}
	if _, err := sup.Admit("agent-a", "conn-1"); !errors.Is(err, supervise.ErrDecisionInFlight) {
		t.Fatalf("more keystrokes while keystrokes are written = %v, want ErrDecisionInFlight", err)
	}
	if entries := engine.auditSnapshot(); len(entries) != journaled {
		t.Fatalf("a refused write journaled %#v", entries[journaled:])
	}
	if shown := viewOf(sup, "asked-1"); shown == nil || shown.DeliveryStatus != "pending" {
		t.Fatalf("the prompt after a refused answer = %#v", shown)
	}

	// The hand is released while the keystrokes are still written: the
	// session is still theirs.
	sup.SetHolder("agent-a", "")
	if err := sup.SubmitDecision(testRunID, "agent-a", "asked-1", "y", alice); !errors.Is(err, supervise.ErrDecisionInFlight) {
		t.Fatalf("an answer while keystrokes are written = %v, want ErrDecisionInFlight", err)
	}
	release()
	if err := sup.SubmitDecision(testRunID, "agent-a", "asked-1", "y", alice); !errors.Is(err, supervise.ErrTypedAtTerminal) {
		t.Fatalf("an answer once the keystrokes are written = %v, want ErrTypedAtTerminal", err)
	}
	sup.Handle(session.AdapterEventWithdrawn{Event: promptEvent("agent-a", "asked-1")})

	// A line, and the policy's answer.
	sup.SetHolder("agent-a", "conn-1")
	release = admitted(t, sup, "agent-a", "conn-1")
	sup.SetHolder("agent-a", "")
	if err := sup.SubmitLine(testRunID, "agent-a", "hello", alice); !errors.Is(err, supervise.ErrDecisionInFlight) {
		t.Fatalf("a line while keystrokes are written = %v, want ErrDecisionInFlight", err)
	}
	if lines := engine.lineSnapshot(); len(lines) != 0 {
		t.Fatalf("a line was sent while keystrokes were written: %#v", lines)
	}
	automatic := promptEvent("agent-a", "automatic-2")
	automatic.Sequence = 2
	automatic.Signature = ""
	sup.Handle(session.AdapterEvent{Event: automatic})
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-b", "other-1")})
	waitFor(t, 2*time.Second, "the other session's automatic answer", func() bool {
		return len(engine.auditFor(audit.KindDelivery, "other-1")) == 1
	})
	time.Sleep(50 * time.Millisecond)
	if decisions := engine.auditFor(audit.KindDecision, "automatic-2"); len(decisions) != 0 {
		t.Fatalf("the policy answered while keystrokes were written: %#v", decisions)
	}
	release()
	waitFor(t, 2*time.Second, "the waiting prompt to be the terminal's once the keystrokes are written", func() bool {
		shown := viewOf(sup, "automatic-2")
		return shown != nil && shown.Evaluation.Reason == supervise.ReasonTypedAtTerminal
	})
	if decisions := engine.auditFor(audit.KindDecision, "automatic-2"); len(decisions) != 0 {
		t.Fatalf("the policy answered a prompt the keystrokes may have answered: %#v", decisions)
	}
	// The agent takes it back, and the policy answers the next prompt.
	sup.Handle(session.AdapterEventWithdrawn{Event: automatic})
	next := promptEvent("agent-a", "automatic-3")
	next.Sequence = 3
	next.Signature = ""
	sup.Handle(session.AdapterEvent{Event: next})
	waitFor(t, 2*time.Second, "the policy's answer to the next prompt", func() bool {
		return len(engine.auditFor(audit.KindDelivery, "automatic-3")) == 1
	})
}

// A drain waits for the keystrokes it admitted, and admits no more. A release
// called twice releases once: the second call neither frees another write's
// slot nor lets the drain miss one.
func TestADrainWaitsForAdmittedRawInput(t *testing.T) {
	sup, _ := newCoreForTest(t, newFakeEngine(), "agent-a")
	sup.SetHolder("agent-a", "conn-1")
	release := admitted(t, sup, "agent-a", "conn-1")
	release()
	release()
	release = admitted(t, sup, "agent-a", "conn-1")
	if _, err := sup.Admit("agent-a", "conn-1"); !errors.Is(err, supervise.ErrDecisionInFlight) {
		t.Fatalf("a write while another is admitted = %v, want ErrDecisionInFlight", err)
	}

	sup.BeginDrain()
	if _, err := sup.Admit("agent-a", "conn-1"); !errors.Is(err, supervise.ErrRuntimeStopped) {
		t.Fatalf("a write during the drain = %v, want ErrRuntimeStopped", err)
	}
	waited := make(chan struct{})
	go func() {
		sup.Wait()
		close(waited)
	}()
	select {
	case <-waited:
		t.Fatal("Wait returned while keystrokes were admitted")
	case <-time.After(50 * time.Millisecond):
	}
	release()
	select {
	case <-waited:
	case <-time.After(2 * time.Second):
		t.Fatal("Wait did not return once the keystrokes were released")
	}
}

// A front end admits and releases keystrokes as they come, possibly under a
// lock of its own that its sink takes. Neither Admit nor its release calls
// the sink on the caller's goroutine, even when the release has a prompt that
// waited for the keystrokes to show: it is the terminal's now.
func TestReleasingRawInputShowsNothingOnItsCaller(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluationByID["automatic-1"] = automaticAllow()
	sup, sink := newCoreForTest(t, engine, "agent-a")
	sup.SetHolder("agent-a", "conn-1")
	release := admitted(t, sup, "agent-a", "conn-1")
	sup.SetHolder("agent-a", "")
	unsigned := promptEvent("agent-a", "automatic-1")
	unsigned.Signature = ""
	sup.Handle(session.AdapterEvent{Event: unsigned})

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
		release()
		close(returned)
	}()
	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("the release called the sink on its caller while the caller held the lock the sink takes")
	}
	waitFor(t, 2*time.Second, "the prompt to be shown the terminal's once the keystrokes are written", func() bool {
		for _, call := range sink.snapshot() {
			if call.kind == "prompt" && call.view.ID == "automatic-1" && call.view.Evaluation.Reason == supervise.ReasonTypedAtTerminal {
				return true
			}
		}
		return false
	})
}

// A front end journals what the core does not write itself, the gateway's
// attach_started for one, through the core: the entry is journaled as given,
// and one the journal refuses freezes the run as a refusal of the core's own
// entries does. The run then journals nothing more, and admits no write.
func TestAFrontEndsEntryIsJournaledThroughTheCore(t *testing.T) {
	engine := newFakeEngine()
	sup, sink := newCoreForTest(t, engine, "agent-a")
	attach := audit.Entry{
		Kind:       audit.KindAttachStarted,
		SessionID:  "agent-a",
		AgentID:    "agent-a",
		DecisionBy: audit.DecisionByHuman,
		Operator:   "alice",
		Outcome:    audit.OutcomeApplied,
		Reason:     "attach_started",
		Metadata:   map[string]string{"operator": "alice", "role": "operator", "conn_id": "conn-1"},
	}
	if err := sup.RecordAudit(attach); err != nil {
		t.Fatalf("RecordAudit: %v", err)
	}
	if entries := engine.auditSnapshot(); len(entries) != 1 || entries[0].Kind != audit.KindAttachStarted ||
		entries[0].Operator != "alice" || entries[0].Metadata["conn_id"] != "conn-1" {
		t.Fatalf("journal = %#v, want the entry as given", entries)
	}
	if len(sink.snapshot()) != 0 || sup.State().AuditFailed {
		t.Fatalf("a journaled entry changed the run: %v", trace(sink.snapshot()))
	}

	engine.set(func(f *fakeEngine) { f.auditFailAt = f.auditCalls + 1 })
	if err := sup.RecordAudit(attach); !errors.Is(err, supervise.ErrAuditUnavailable) {
		t.Fatalf("an entry the journal refuses = %v, want ErrAuditUnavailable", err)
	}
	if state := sup.State(); !state.AuditFailed || !state.Agents[0].InputFrozen {
		t.Fatalf("the run after the journal refused the entry = %#v", state)
	}
	// Shown on another goroutine (TestAFrontEndsEntryShowsNothingOnItsCaller).
	waitFor(t, 2*time.Second, "the journal's failure to be shown", func() bool { return len(sink.snapshot()) >= 2 })
	if got := trace(sink.snapshot()); len(got) != 2 || got[0] != "status:failed" || got[1] != "error:audit_unavailable" {
		t.Fatalf("the journal's failure was shown as %v", got)
	}
	calls := engine.auditCalls
	if err := sup.RecordAudit(attach); !errors.Is(err, supervise.ErrAuditUnavailable) {
		t.Fatalf("an entry once the journal failed = %v, want ErrAuditUnavailable", err)
	}
	if engine.auditCalls != calls {
		t.Fatal("an entry was written once the journal had failed")
	}
	sup.SetHolder("agent-a", "conn-1")
	if _, err := sup.Admit("agent-a", "conn-1"); !errors.Is(err, supervise.ErrAuditUnavailable) {
		t.Fatalf("a write once the journal failed = %v, want ErrAuditUnavailable", err)
	}

	var none *supervise.Supervisor
	if err := none.RecordAudit(attach); !errors.Is(err, supervise.ErrRuntimeStopped) {
		t.Fatalf("an entry with no run = %v, want ErrRuntimeStopped", err)
	}
}

// A front end journals its own entries under a lock of its own, the one it
// takes the hand under, which its sink may take too: RecordAudit never calls
// the sink on its caller's goroutine. The journal's failure used to be shown
// by the caller, before RecordAudit returned, and a gateway that journaled
// attach_started under its hub lock deadlocked the first time the journal
// refused an entry, never while it worked. The failure is shown on another
// goroutine, once the caller let go of its lock, and a drain waits for it.
func TestAFrontEndsEntryShowsNothingOnItsCaller(t *testing.T) {
	for _, failing := range []bool{false, true} {
		name := "a journal that works"
		if failing {
			name = "a journal that refuses the entry"
		}
		t.Run(name, func(t *testing.T) {
			engine := newFakeEngine()
			if failing {
				engine.auditFailAt = 1
			}
			sup, sink := newCoreForTest(t, engine, "agent-a")
			var host sync.Mutex
			sink.mu.Lock()
			sink.gate = func(sinkCall) {
				host.Lock()
				//lint:ignore SA2001 the sink takes the host's lock, as a front end's does
				host.Unlock()
			}
			sink.mu.Unlock()
			attach := audit.Entry{
				Kind: audit.KindAttachStarted, SessionID: "agent-a", AgentID: "agent-a",
				DecisionBy: audit.DecisionByHuman, Operator: "alice", Outcome: audit.OutcomeApplied, Reason: "attach_started",
			}
			returned := make(chan error, 1)
			go func() {
				host.Lock()
				defer host.Unlock()
				err := sup.RecordAudit(attach)
				if err == nil {
					sup.SetHolder("agent-a", "conn-1")
				}
				returned <- err
			}()
			select {
			case err := <-returned:
				if failing != errors.Is(err, supervise.ErrAuditUnavailable) || (!failing && err != nil) {
					t.Fatalf("RecordAudit = %v", err)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("RecordAudit called the sink on its caller while the caller held the lock the sink takes")
			}
			if !failing {
				return
			}
			waitFor(t, 2*time.Second, "the journal's failure to be shown", func() bool {
				return len(sink.snapshot()) == 2
			})
			if got := trace(sink.snapshot()); got[0] != "status:failed" || got[1] != "error:audit_unavailable" {
				t.Fatalf("the journal's failure was shown as %v", got)
			}
		})
	}
}

// A front end journals through the core only what the core does not write
// itself: who took or let go of a terminal, how its control changed hands,
// and the lifecycle of a recording. Any other kind is refused
// (ErrUnsupportedEntry) before anything else, with nothing journaled and
// nothing frozen: a decision entry written by a front end reset the policy's
// count of consecutive automatic decisions, and a detection or a delivery
// would read as the core's own.
func TestOnlyAFrontEndsOwnEntriesAreJournaledThroughTheCore(t *testing.T) {
	accepted := []audit.Kind{
		audit.KindAttachStarted, audit.KindAttachFinished,
		audit.KindControlRequested, audit.KindControlGranted, audit.KindControlDeclined,
		audit.KindControlReleased, audit.KindControlForced,
		audit.KindRecordingStarted, audit.KindRecordingFinished, audit.KindRecordingExported, audit.KindRecordingDeleted,
	}
	refused := []audit.Kind{
		audit.KindRunStarted, audit.KindRunFinished, audit.KindSessionStarted, audit.KindSupervisionFinished,
		audit.KindSessionFinished, audit.KindEventDetected, audit.KindEventWithdrawn, audit.KindPolicyEvaluated,
		audit.KindDecision, audit.KindDelivery, audit.KindOperatorInput, audit.KindBackendError,
		audit.KindSessionCleanup, audit.Kind(""), audit.Kind("attach"),
	}
	engine := newFakeEngine()
	sup, sink := newCoreForTest(t, engine, "agent-a")
	for _, kind := range refused {
		entry := audit.Entry{Kind: kind, SessionID: "agent-a", AgentID: "agent-a", DecisionBy: audit.DecisionByHuman, Outcome: audit.OutcomeApplied}
		if err := sup.RecordAudit(entry); !errors.Is(err, supervise.ErrUnsupportedEntry) {
			t.Fatalf("a front end's %q entry = %v, want ErrUnsupportedEntry", kind, err)
		}
		var none *supervise.Supervisor
		if err := none.RecordAudit(entry); !errors.Is(err, supervise.ErrUnsupportedEntry) {
			t.Fatalf("a front end's %q entry with no run = %v, want ErrUnsupportedEntry", kind, err)
		}
	}
	if entries := engine.auditSnapshot(); len(entries) != 0 {
		t.Fatalf("refused entries were journaled: %#v", entries)
	}
	if state := sup.State(); state.AuditFailed || state.Agents[0].InputFrozen || len(sink.snapshot()) != 0 {
		t.Fatalf("refused entries changed the run: %#v, %v", state, trace(sink.snapshot()))
	}
	for _, kind := range accepted {
		entry := audit.Entry{Kind: kind, SessionID: "agent-a", AgentID: "agent-a", DecisionBy: audit.DecisionByHuman, Outcome: audit.OutcomeApplied}
		if err := sup.RecordAudit(entry); err != nil {
			t.Fatalf("a front end's %q entry = %v", kind, err)
		}
	}
	if entries := engine.auditSnapshot(); len(entries) != len(accepted) {
		t.Fatalf("journaled %d of the front end's %d entries", len(entries), len(accepted))
	}
}

// A prompt shown while its terminal's holder types is the terminal's: the
// keystrokes may have answered it, and the core cannot tell. A person's answer
// to it, the holder's or anybody else's, chosen or typed, is refused
// (ErrTypedAtTerminal) with nothing written or journaled, and it offers no
// answer; the policy never answers it either, and letting go of the terminal
// changes none of that. It stays shown until the agent takes it back. It used
// to stay open to anybody until then, and a click on its card typed a second
// answer into whatever the agent asked next, journaled as the answer to the
// prompt the holder had already answered.
func TestAPromptItsHolderTypedIntoIsTheTerminals(t *testing.T) {
	engine := newFakeEngine()
	engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.SetHolder("agent-a", "conn-1")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	// The holder types the answer at the terminal.
	admitted(t, sup, "agent-a", "conn-1")()
	waitFor(t, 2*time.Second, "the prompt to be the terminal's", func() bool {
		shown := viewOf(sup, "prompt-1")
		return shown != nil && shown.Evaluation.Reason == supervise.ReasonTypedAtTerminal
	})
	journaled := len(engine.auditSnapshot())

	bob := supervise.Actor{Identity: "bob", Role: supervise.RoleOperator, ConnID: "conn-2"}
	refusals := []struct {
		name string
		err  error
	}{
		{"the holder's click", sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", alice)},
		{"another operator's click", sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "deny", bob)},
		{"another operator's typed answer", sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", bob)},
	}
	sup.SetHolder("agent-a", "")
	refusals = append(refusals, struct {
		name string
		err  error
	}{"a typed answer once the terminal is let go", sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", bob)})
	for _, refusal := range refusals {
		if !errors.Is(refusal.err, supervise.ErrTypedAtTerminal) {
			t.Errorf("%s = %v, want ErrTypedAtTerminal", refusal.name, refusal.err)
		}
	}
	if calls := engine.applySnapshot(); len(calls) != 0 {
		t.Fatalf("a prompt the holder typed into was answered again: %#v", calls)
	}
	if entries := engine.auditSnapshot(); len(entries) != journaled {
		t.Fatalf("a refused answer was journaled: %#v", entries[journaled:])
	}
	if shown := viewOf(sup, "prompt-1"); shown == nil || shown.DeliveryStatus != "pending" || len(shown.Decisions) != 0 {
		t.Fatalf("the prompt the holder typed into is shown as %#v, want pending with no answer offered", shown)
	}

	// The agent consumed the answer and takes the question back: its next
	// question is anybody's.
	sup.Handle(session.AdapterEventWithdrawn{Event: promptEvent("agent-a", "prompt-1")})
	next := promptEvent("agent-a", "prompt-2")
	next.Sequence = 2
	sup.Handle(session.AdapterEvent{Event: next})
	if err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-2", "allow", bob); err != nil {
		t.Fatalf("an answer to the agent's next question: %v", err)
	}
}
