package supervisetest_test

import (
	"context"
	"errors"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
	"github.com/Hocsman/Relayer/internal/supervise/supervisetest"
	"github.com/Hocsman/Relayer/internal/terminal"
)

// These tests run each knob of the fake engine under a real supervisor, the
// way the front ends' tests will: what they pin is that the core, over this
// engine, does what it does over the runtime the engine stands for.

const runID = "run-supervisetest"

// person is the zero Actor: it names nobody, and may act.
var person supervise.Actor

// newCore starts the supervisor of a run whose agents are the given sessions,
// each on the pty backend with the generic adapter, over the engine. The run
// is drained when the test ends, and a drain that does not finish fails it.
func newCore(t *testing.T, engine *supervisetest.Engine, options supervise.Options, sessionIDs ...string) *supervise.Supervisor {
	t.Helper()
	return newCoreWithContext(t, context.Background(), engine, options, sessionIDs...)
}

// newCoreWithContext is newCore under a run context derived from parent,
// which the test may cancel as the runtime does when the run stops.
func newCoreWithContext(
	t *testing.T,
	parent context.Context,
	engine *supervisetest.Engine,
	options supervise.Options,
	sessionIDs ...string,
) *supervise.Supervisor {
	t.Helper()
	for _, sessionID := range sessionIDs {
		options.Agents = append(options.Agents, supervise.AgentSpec{
			SessionID: sessionID,
			AgentID:   sessionID,
			Name:      sessionID,
			Backend:   "pty",
			Adapter:   "generic",
		})
	}
	options.RunID = runID
	ctx, cancel := context.WithCancel(parent)
	sup, err := supervise.New(ctx, engine, options)
	if err != nil {
		cancel()
		t.Fatalf("New: %v", err)
	}
	t.Cleanup(func() {
		cancel()
		// Drained on another goroutine: a test that failed on a deadlock
		// must fail rather than hang here.
		drained := make(chan struct{})
		go func() {
			sup.BeginDrain()
			sup.Wait()
			close(drained)
		}()
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Error("the run did not drain")
		}
	})
	return sup
}

// prompt is one confirmation prompt, its Signature its own.
func prompt(sessionID, eventID string) adapters.Event {
	return adapters.Event{
		ID:        eventID,
		Signature: "signature-" + sessionID + "-" + eventID,
		Sequence:  1,
		SessionID: sessionID,
		AgentID:   sessionID,
		Adapter:   "generic",
		Type:      adapters.EventConfirmation,
		Summary:   "Overwrite file?",
		Match:     "Overwrite file? [Y/n]",
		Risk:      adapters.RiskLow,
		Timestamp: time.Date(2026, time.August, 27, 10, 0, 0, 0, time.UTC),
	}
}

// automaticAllow is the evaluation of a rule that allows without asking.
func automaticAllow() policy.Evaluation {
	return policy.Evaluation{
		Action:         policy.ActionAllow,
		ProposedAction: policy.ActionAllow,
		RuleName:       "allow-safe",
		Reason:         policy.ReasonRule,
		Automatic:      true,
	}
}

// entriesFor returns the journal entries of one kind about one event.
func entriesFor(engine *supervisetest.Engine, kind audit.Kind, eventID string) []audit.Entry {
	var entries []audit.Entry
	for _, entry := range engine.Entries() {
		if entry.Kind == kind && entry.EventID == eventID {
			entries = append(entries, entry)
		}
	}
	return entries
}

// deliveredWith reports whether the answer to the event was journaled with
// that outcome.
func deliveredWith(engine *supervisetest.Engine, eventID string, outcome audit.Outcome) bool {
	for _, entry := range entriesFor(engine, audit.KindDelivery, eventID) {
		if entry.Outcome == outcome {
			return true
		}
	}
	return false
}

// answeredByThePolicy hands the core the prompt and waits for the policy's
// answer to be written, then for the session to be free again: the write's
// claim is released a moment after its delivery was journaled, and until
// then another answer, a Stop and a line are refused. A line is sent once it
// is, which is how the session is found free; a line changes nothing the
// engine counts.
func answeredByThePolicy(t *testing.T, engine *supervisetest.Engine, sup *supervise.Supervisor, event adapters.Event) {
	t.Helper()
	sup.Handle(session.AdapterEvent{Event: event})
	supervisetest.WaitFor(t, 2*time.Second, "the policy's answer to "+event.ID, func() bool {
		return deliveredWith(engine, event.ID, audit.OutcomeApplied)
	})
	supervisetest.WaitFor(t, 2*time.Second, "the session to be free", func() bool {
		return sup.SubmitLine(runID, event.SessionID, "next", person) == nil
	})
}

// limitedOn reports whether the limit of consecutive decisions holds back the
// policy's next answer on the session, which is whether its count stands at
// the limit. The engine's evaluation of prompt-probe must be automatic.
func limitedOn(engine *supervisetest.Engine, sessionID string) bool {
	return engine.Evaluate(prompt(sessionID, "prompt-probe")).Reason == policy.ReasonConsecutiveLimit
}

// pendingOf returns the views of the session's pending prompts.
func pendingOf(sup *supervise.Supervisor, sessionID string) []supervise.View {
	var views []supervise.View
	for _, view := range sup.State().Pending {
		if view.SessionID == sessionID {
			views = append(views, view)
		}
	}
	return views
}

// heldBy returns the ID of the prompt the session's backend holds, or "" when
// it holds none.
func heldBy(t *testing.T, engine *supervisetest.Engine, sessionID string) string {
	t.Helper()
	pending, err := engine.PendingEvent(context.Background(), sessionID)
	if err != nil {
		t.Fatalf("PendingEvent(%s): %v", sessionID, err)
	}
	if pending == nil {
		return ""
	}
	return pending.ID
}

func agentOf(t *testing.T, sup *supervise.Supervisor, sessionID string) supervise.Agent {
	t.Helper()
	agent, found := sup.Agent(sessionID)
	if !found {
		t.Fatalf("agent %q is unknown to the core", sessionID)
	}
	return agent
}

// receive returns what the channel carries next, and fails the test when
// nothing arrives in time.
func receive(t *testing.T, channel <-chan string, what string) string {
	t.Helper()
	select {
	case value := <-channel:
		return value
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
		return ""
	}
}

// Until it is told otherwise the engine asks about every prompt for the
// default reason, and an evaluation set for one prompt is that prompt's
// alone: it answers the prompt with that ID and no other.
func TestAnEvaluationSetForOnePromptOverridesTheDefault(t *testing.T) {
	engine := supervisetest.NewEngine()
	engine.SetSupported(adapters.DecisionAllow)
	engine.SetEvaluationFor("prompt-auto", automaticAllow())
	sup := newCore(t, engine, supervise.Options{}, "agent-a", "agent-b")

	sup.Handle(session.AdapterEvent{Event: prompt("agent-a", "prompt-asked")})
	answeredByThePolicy(t, engine, sup, prompt("agent-b", "prompt-auto"))

	askedByDefault := supervise.EvaluationView{
		Action:         string(policy.ActionAsk),
		ProposedAction: string(policy.ActionAsk),
		Reason:         policy.ReasonDefault,
	}
	pending := sup.State().Pending
	if len(pending) != 1 || pending[0].ID != "prompt-asked" || pending[0].DeliveryStatus != "pending" ||
		pending[0].Evaluation != askedByDefault {
		t.Fatalf("pending = %#v, want only prompt-asked, asked by default: %#v", pending, askedByDefault)
	}
	want := policy.Evaluation{
		EventID:        "prompt-other",
		Action:         policy.ActionAsk,
		ProposedAction: policy.ActionAsk,
		Reason:         policy.ReasonDefault,
	}
	if evaluation := engine.Evaluate(prompt("agent-a", "prompt-other")); evaluation != want {
		t.Fatalf("the default evaluation = %#v, want %#v", evaluation, want)
	}
	applies := engine.Applies()
	if len(applies) != 1 || applies[0].SessionID != "agent-b" || applies[0].Event.ID != "prompt-auto" ||
		applies[0].Decision != adapters.DecisionAllow || applies[0].ManualInput != "" {
		t.Fatalf("applies = %#v, want the policy's allow of prompt-auto", applies)
	}
	decisions := entriesFor(engine, audit.KindDecision, "prompt-auto")
	if len(decisions) != 1 || decisions[0].DecisionBy != audit.DecisionByPolicy || decisions[0].Rule != "allow-safe" {
		t.Fatalf("decision entries = %#v, want the policy's, under its rule", decisions)
	}

	// The default changes for every prompt that has no evaluation of its own.
	engine.SetEvaluation(automaticAllow())
	answeredByThePolicy(t, engine, sup, prompt("agent-b", "prompt-later"))
	if got := len(engine.Applies()); got != 2 {
		t.Fatalf("applies = %d, want the later prompt answered too", got)
	}
}

// What the engine supports is what the adapters encode: a prompt offers those
// answers, and an answer it does not support is refused as the runtime
// refuses it, before anything is written. The policy's allow on the generic
// adapter goes back to the operator.
func TestAnAnswerTheEngineDoesNotSupportIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	engine := supervisetest.NewEngine()
	engine.SetEvaluationFor("prompt-1", automaticAllow())
	sup := newCore(t, engine, supervise.Options{}, "agent-a", "agent-b")

	sup.Handle(session.AdapterEvent{Event: prompt("agent-a", "prompt-1")})
	supervisetest.WaitFor(t, 2*time.Second, "the refused answer's delivery entry", func() bool {
		return deliveredWith(engine, "prompt-1", audit.OutcomeFallbackUnsupported)
	})
	supervisetest.WaitFor(t, 2*time.Second, "the prompt handed to the operator", func() bool {
		views := pendingOf(sup, "agent-a")
		return len(views) == 1 && views[0].DeliveryStatus == "pending" && views[0].Evaluation.Reason == "fallback_unsupported"
	})
	if views := pendingOf(sup, "agent-a"); len(views[0].Decisions) != 0 {
		t.Fatalf("decisions offered = %#v, want none but a typed answer", views[0].Decisions)
	}
	if applies := engine.Applies(); len(applies) != 0 {
		t.Fatalf("applies = %#v, want nothing written", applies)
	}

	engine.SetSupported(adapters.DecisionAllow, adapters.DecisionDeny)
	sup.Handle(session.AdapterEvent{Event: prompt("agent-b", "prompt-2")})
	views := pendingOf(sup, "agent-b")
	if len(views) != 1 || !reflect.DeepEqual(views[0].Decisions, []string{"allow", "deny"}) {
		t.Fatalf("agent-b pending = %#v, want allow and deny offered", views)
	}
	if err := sup.SubmitAutomaticDecision(runID, "agent-b", "prompt-2", string(adapters.DecisionAllow), person); err != nil {
		t.Fatalf("SubmitAutomaticDecision: %v", err)
	}
	if applies := engine.Applies(); len(applies) != 1 || applies[0].Decision != adapters.DecisionAllow {
		t.Fatalf("applies = %#v, want the allow written", applies)
	}
}

// A session whose pending prompt the test set is checked as the runtime
// checks it: an answer to another prompt is a mismatch, after which the core
// takes in the prompt the backend holds, and a written answer is the end of
// that prompt in the backend.
func TestAnAnswerToAPromptTheBackendDoesNotHoldIsAMismatch(t *testing.T) {
	engine := supervisetest.NewEngine()
	held := prompt("agent-a", "prompt-held")
	engine.SetPending("agent-a", &held)
	sup := newCore(t, engine, supervise.Options{}, "agent-a")

	sup.Handle(session.AdapterEvent{Event: prompt("agent-a", "prompt-gone")})
	if err := sup.SubmitDecision(runID, "agent-a", "prompt-gone", "y", person); !errors.Is(err, supervise.ErrDecisionStale) {
		t.Fatalf("SubmitDecision to a prompt the backend does not hold = %v, want ErrDecisionStale", err)
	}
	if !deliveredWith(engine, "prompt-gone", audit.OutcomeFallbackStale) {
		t.Fatalf("journal = %#v, want the answer journaled stale", engine.Entries())
	}
	if applies := engine.Applies(); len(applies) != 0 {
		t.Fatalf("applies = %#v, want nothing written", applies)
	}
	views := pendingOf(sup, "agent-a")
	if len(views) != 1 || views[0].ID != "prompt-held" {
		t.Fatalf("pending = %#v, want the prompt the backend holds, taken in", views)
	}

	if err := sup.SubmitDecision(runID, "agent-a", "prompt-held", "y", person); err != nil {
		t.Fatalf("SubmitDecision to the prompt the backend holds: %v", err)
	}
	applies := engine.Applies()
	if len(applies) != 1 || applies[0].Event.ID != "prompt-held" || applies[0].Decision != adapters.DecisionManual ||
		applies[0].ManualInput != "y" {
		t.Fatalf("applies = %#v, want the typed answer to prompt-held", applies)
	}
	if pending, err := engine.PendingEvent(context.Background(), "agent-a"); err != nil || pending != nil {
		t.Fatalf("PendingEvent after the answer = %#v, %v; want none", pending, err)
	}
}

// A session whose backend a test set to hold nothing is checked too: an
// answer to any prompt is a mismatch, as the router finds no prompt to match
// it against, and the core, reading back that nothing is held, drops the
// prompt it showed.
func TestAnAnswerToASessionWhoseBackendHoldsNothingIsAMismatch(t *testing.T) {
	engine := supervisetest.NewEngine()
	engine.SetPending("agent-a", nil)
	sup := newCore(t, engine, supervise.Options{}, "agent-a")

	sup.Handle(session.AdapterEvent{Event: prompt("agent-a", "prompt-ghost")})
	if err := sup.SubmitDecision(runID, "agent-a", "prompt-ghost", "y", person); !errors.Is(err, supervise.ErrDecisionStale) {
		t.Fatalf("SubmitDecision to a session whose backend holds nothing = %v, want ErrDecisionStale", err)
	}
	if !deliveredWith(engine, "prompt-ghost", audit.OutcomeFallbackStale) {
		t.Fatalf("journal = %#v, want the answer journaled stale", engine.Entries())
	}
	if applies := engine.Applies(); len(applies) != 0 {
		t.Fatalf("applies = %#v, want nothing written", applies)
	}
	if views := pendingOf(sup, "agent-a"); len(views) != 0 {
		t.Fatalf("pending = %#v, want the prompt dropped", views)
	}
}

// An answer to a prompt with no ID, or to one another session raised, is
// refused as a mismatch before anything else, as the router refuses it:
// nothing is written, whatever the answer's context. The core never hands the
// engine such a prompt, so only a direct call reaches the check. The prompt's
// own session is matched whatever its case, as the router matches it.
func TestAnAnswerToAPromptWithNoIDOrFromAnotherSessionIsAMismatch(t *testing.T) {
	engine := supervisetest.NewEngine()
	engine.SetSupported(adapters.DecisionAllow)
	foreign := prompt("agent-b", "prompt-1")
	blank := prompt("agent-a", "prompt-1")
	blank.ID = " "
	ended, cancel := context.WithCancel(context.Background())
	cancel()

	for _, test := range []struct {
		name  string
		event adapters.Event
	}{
		{name: "a prompt from another session", event: foreign},
		{name: "a prompt with no ID", event: blank},
	} {
		for _, ctx := range []context.Context{context.Background(), ended} {
			if err := engine.ApplyDecision(ctx, "agent-a", test.event, adapters.DecisionAllow, ""); !errors.Is(err, adapters.ErrEventMismatch) {
				t.Fatalf("an answer to %s on agent-a = %v, want ErrEventMismatch", test.name, err)
			}
		}
	}
	if applies := engine.Applies(); len(applies) != 0 {
		t.Fatalf("applies = %#v, want nothing written", applies)
	}
	if err := engine.ApplyDecision(context.Background(), "AGENT-B", foreign, adapters.DecisionAllow, ""); err != nil {
		t.Fatalf("an answer to agent-b's prompt on AGENT-B: %v", err)
	}
}

// A line is refused, before anything is written, while the session's backend
// holds a prompt the core has not seen yet; the core then takes the prompt
// in. A session whose backend holds nothing takes the line, and so does one
// whose backend holds an event no one answers, as the processor lets a line
// past anything but an actionable prompt.
func TestALineIsRefusedWhileTheBackendHoldsAPrompt(t *testing.T) {
	engine := supervisetest.NewEngine()
	held := prompt("agent-a", "prompt-held")
	engine.SetPending("agent-a", &held)
	notice := prompt("agent-c", "notice-held")
	notice.Type = adapters.EventProcessExit
	engine.SetPending("agent-c", &notice)
	sup := newCore(t, engine, supervise.Options{}, "agent-a", "agent-b", "agent-c")

	if err := sup.SubmitLine(runID, "agent-a", "hello", person); !errors.Is(err, supervise.ErrLinePromptPending) {
		t.Fatalf("SubmitLine while the backend holds a prompt = %v, want ErrLinePromptPending", err)
	}
	if views := pendingOf(sup, "agent-a"); len(views) != 1 || views[0].ID != "prompt-held" {
		t.Fatalf("pending = %#v, want the held prompt taken in", views)
	}
	for _, sessionID := range []string{"agent-b", "agent-c"} {
		if err := sup.SubmitLine(runID, sessionID, "hello", person); err != nil {
			t.Fatalf("SubmitLine to %s, which holds no prompt to answer: %v", sessionID, err)
		}
	}
	want := []supervisetest.Line{{SessionID: "agent-b", Text: "hello"}, {SessionID: "agent-c", Text: "hello"}}
	if lines := engine.Lines(); !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %#v, want %#v", lines, want)
	}
}

// operatorInputReasons returns the reasons of the journal's operator input
// entries about the session, in order.
func operatorInputReasons(engine *supervisetest.Engine, sessionID string) []string {
	var reasons []string
	for _, entry := range engine.Entries() {
		if entry.Kind == audit.KindOperatorInput && entry.SessionID == sessionID {
			reasons = append(reasons, entry.Reason)
		}
	}
	return reasons
}

// A line that is not one line of text is refused as the runtime's line
// boundary refuses it, before anything is written: the core leaves that check
// to the runtime, and journals the line invalid. The check comes before the
// pending prompt's, as in the processor, so a session whose backend holds a
// prompt refuses the line as invalid too, and the core takes nothing in.
func TestALineTheLineBoundaryRefusesIsNeverWritten(t *testing.T) {
	for _, test := range []struct {
		name string
		line string
	}{
		{name: "a control sequence", line: "two\x1b[2Jlines"},
		{name: "a line break", line: "first\nsecond"},
		{name: "invalid UTF-8", line: "caf\xe9"},
		{name: "more than MaxLineBytes", line: strings.Repeat("a", adapters.MaxLineBytes+1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := supervisetest.NewEngine()
			held := prompt("agent-b", "prompt-held")
			engine.SetPending("agent-b", &held)
			sup := newCore(t, engine, supervise.Options{}, "agent-a", "agent-b")

			for _, sessionID := range []string{"agent-a", "agent-b"} {
				if err := sup.SubmitLine(runID, sessionID, test.line, person); !errors.Is(err, supervise.ErrLineInvalid) {
					t.Fatalf("SubmitLine to %s = %v, want ErrLineInvalid", sessionID, err)
				}
				want := []string{"operator_input_started", "operator_input_invalid"}
				if reasons := operatorInputReasons(engine, sessionID); !reflect.DeepEqual(reasons, want) {
					t.Fatalf("%s's operator input entries = %v, want %v", sessionID, reasons, want)
				}
			}
			if lines := engine.Lines(); len(lines) != 0 {
				t.Fatalf("lines = %#v, want nothing written", lines)
			}
			if views := pendingOf(sup, "agent-b"); len(views) != 0 {
				t.Fatalf("agent-b pending = %#v, want nothing taken in", views)
			}
		})
	}
}

// The errors set for the answers are returned by the next ones written, one
// each and in order, as the transport's: an uncertain write freezes its
// session, an answer the adapter turned out unable to encode goes back to the
// operator without it, and once the errors are used the next answer is
// written. The backend keeps a prompt whose write failed, as the processor
// clears one only once its answer is delivered, and lets go of the one whose
// answer was written.
func TestApplyErrorsAreReturnedByTheNextWritesInOrder(t *testing.T) {
	engine := supervisetest.NewEngine()
	engine.SetSupported(adapters.DecisionAllow, adapters.DecisionDeny)
	engine.SetApplyErrs(errors.New("transport lost"), adapters.ErrDecisionUnsupported)
	sup := newCore(t, engine, supervise.Options{}, "agent-a", "agent-b", "agent-c")
	for _, sessionID := range []string{"agent-a", "agent-b", "agent-c"} {
		held := prompt(sessionID, "prompt-1")
		engine.SetPending(sessionID, &held)
		sup.Handle(session.AdapterEvent{Event: held})
	}

	if err := sup.SubmitAutomaticDecision(runID, "agent-a", "prompt-1", string(adapters.DecisionDeny), person); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("first write = %v, want ErrDeliveryUncertain", err)
	}
	if !agentOf(t, sup, "agent-a").InputFrozen {
		t.Fatal("the session of an uncertain write is not frozen")
	}
	if err := sup.SubmitAutomaticDecision(runID, "agent-b", "prompt-1", string(adapters.DecisionAllow), person); !errors.Is(err, supervise.ErrUnsupportedDecision) {
		t.Fatalf("second write = %v, want ErrUnsupportedDecision", err)
	}
	if views := pendingOf(sup, "agent-b"); len(views) != 1 || !reflect.DeepEqual(views[0].Decisions, []string{"deny"}) {
		t.Fatalf("agent-b pending = %#v, want its prompt offering deny alone", views)
	}
	if err := sup.SubmitAutomaticDecision(runID, "agent-c", "prompt-1", string(adapters.DecisionAllow), person); err != nil {
		t.Fatalf("third write: %v", err)
	}
	if got := len(engine.Applies()); got != 3 {
		t.Fatalf("applies = %d, want every write that began", got)
	}
	for sessionID, want := range map[string]string{"agent-a": "prompt-1", "agent-b": "prompt-1", "agent-c": ""} {
		if got := heldBy(t, engine, sessionID); got != want {
			t.Errorf("%s's backend holds %q after its write, want %q", sessionID, got, want)
		}
	}
}

// A held answer is being written until its release: the prompt shows
// delivering, and the operation returns only once it is released.
func TestAHeldAnswerWaitsForItsRelease(t *testing.T) {
	engine := supervisetest.NewEngine()
	started := make(chan string, 1)
	release := make(chan struct{})
	engine.HoldApplies(started, release)
	sup := newCore(t, engine, supervise.Options{}, "agent-a")
	sup.Handle(session.AdapterEvent{Event: prompt("agent-a", "prompt-1")})

	done := make(chan error, 1)
	go func() { done <- sup.SubmitDecision(runID, "agent-a", "prompt-1", "y", person) }()
	if sessionID := receive(t, started, "the write to begin"); sessionID != "agent-a" {
		t.Fatalf("started = %q, want agent-a", sessionID)
	}
	if views := pendingOf(sup, "agent-a"); len(views) != 1 || views[0].DeliveryStatus != "delivering" {
		t.Fatalf("pending while held = %#v, want the prompt delivering", views)
	}
	select {
	case err := <-done:
		t.Fatalf("SubmitDecision returned %v while its write was held", err)
	default:
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}
	if views := pendingOf(sup, "agent-a"); len(views) != 0 {
		t.Fatalf("pending after the release = %#v, want none", views)
	}
}

// A held call also ends when its context does, with the context's error, as
// the runtime's writes and stops do when the run stops, whether it waits for
// its release or for someone to take its start: a front end that stops its run
// must not wait on a test's channel. The core takes it for a call that failed:
// an answer's or a line's delivery is uncertain, and a stop did not happen.
func TestAHeldCallEndsWithItsContext(t *testing.T) {
	operations := []struct {
		name string
		hold func(engine *supervisetest.Engine, started chan<- string, release <-chan struct{})
		// call makes the operation on agent-a.
		call func(sup *supervise.Supervisor) error
		// began reports whether the call reached the engine.
		began func(engine *supervisetest.Engine) bool
		// check is what the core made of the call its context ended.
		check func(t *testing.T, engine *supervisetest.Engine, sup *supervise.Supervisor, err error)
	}{
		{
			name: "an answer",
			hold: (*supervisetest.Engine).HoldApplies,
			call: func(sup *supervise.Supervisor) error {
				sup.Handle(session.AdapterEvent{Event: prompt("agent-a", "prompt-1")})
				return sup.SubmitDecision(runID, "agent-a", "prompt-1", "y", person)
			},
			began: func(engine *supervisetest.Engine) bool { return len(engine.Applies()) == 1 },
			check: func(t *testing.T, engine *supervisetest.Engine, _ *supervise.Supervisor, err error) {
				t.Helper()
				if !errors.Is(err, supervise.ErrDeliveryUncertain) {
					t.Fatalf("SubmitDecision = %v, want ErrDeliveryUncertain", err)
				}
				if !deliveredWith(engine, "prompt-1", audit.OutcomeFallbackDeliveryUncertain) {
					t.Fatalf("journal = %#v, want the delivery journaled uncertain", engine.Entries())
				}
			},
		},
		{
			name: "a line",
			hold: (*supervisetest.Engine).HoldLines,
			call: func(sup *supervise.Supervisor) error {
				return sup.SubmitLine(runID, "agent-a", "hello", person)
			},
			began: func(engine *supervisetest.Engine) bool { return len(engine.Lines()) == 1 },
			check: func(t *testing.T, engine *supervisetest.Engine, _ *supervise.Supervisor, err error) {
				t.Helper()
				if !errors.Is(err, supervise.ErrDeliveryUncertain) {
					t.Fatalf("SubmitLine = %v, want ErrDeliveryUncertain", err)
				}
				want := []string{"operator_input_started", "operator_input_delivery_uncertain"}
				if reasons := operatorInputReasons(engine, "agent-a"); !reflect.DeepEqual(reasons, want) {
					t.Fatalf("operator input entries = %v, want %v", reasons, want)
				}
			},
		},
		{
			name: "a stop",
			hold: func(engine *supervisetest.Engine, started chan<- string, release <-chan struct{}) {
				engine.HoldLifecycle(supervisetest.Stop, started, release)
			},
			call: func(sup *supervise.Supervisor) error {
				return sup.StopSession(runID, "agent-a")
			},
			began: func(engine *supervisetest.Engine) bool {
				return len(engine.LifecycleCalls(supervisetest.Stop)) == 1
			},
			check: func(t *testing.T, _ *supervisetest.Engine, sup *supervise.Supervisor, err error) {
				t.Helper()
				if err == nil {
					t.Fatal("StopSession succeeded, want the stop its context ended to fail")
				}
				if agent := agentOf(t, sup, "agent-a"); !agent.Running {
					t.Fatalf("agent-a = %#v, want it still running", agent)
				}
			},
		},
	}
	holds := []struct {
		name string
		// channels returns the call's started and release channels.
		channels func() (chan string, chan struct{})
	}{
		{
			name: "held until its release",
			channels: func() (chan string, chan struct{}) {
				return make(chan string, 1), make(chan struct{})
			},
		},
		{
			name: "its start announced to nobody",
			channels: func() (chan string, chan struct{}) {
				return make(chan string), nil
			},
		},
	}
	for _, operation := range operations {
		for _, held := range holds {
			t.Run(operation.name+" "+held.name, func(t *testing.T) {
				engine := supervisetest.NewEngine()
				started, release := held.channels()
				operation.hold(engine, started, release)
				run, stop := context.WithCancel(context.Background())
				defer stop()
				sup := newCoreWithContext(t, run, engine, supervise.Options{}, "agent-a")

				done := make(chan error, 1)
				go func() { done <- operation.call(sup) }()
				supervisetest.WaitFor(t, 2*time.Second, "the call to reach the engine", func() bool {
					return operation.began(engine)
				})
				stop()
				select {
				case err := <-done:
					operation.check(t, engine, sup, err)
				case <-time.After(2 * time.Second):
					t.Fatal("the held call outlived its context")
				}
			})
		}
	}
}

// A call whose context has already ended is refused before anything is
// written, as the runtime's backends refuse it. The core does not watch its
// run's context: a front end that cancels it before the run drains sees the
// answer and the line that follow fail, their delivery uncertain, where an
// engine that wrote them would show them applied. A lifecycle call is refused
// the same way, and changes nothing the backend holds or the policy counts.
func TestACallWhoseContextHasEndedIsRefusedBeforeAnythingIsWritten(t *testing.T) {
	engine := supervisetest.NewEngine()
	run, stop := context.WithCancel(context.Background())
	defer stop()
	sup := newCoreWithContext(t, run, engine, supervise.Options{}, "agent-a", "agent-b")
	sup.Handle(session.AdapterEvent{Event: prompt("agent-a", "prompt-1")})
	stop()

	if err := sup.SubmitDecision(runID, "agent-a", "prompt-1", "y", person); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("SubmitDecision after the run's context ended = %v, want ErrDeliveryUncertain", err)
	}
	if !deliveredWith(engine, "prompt-1", audit.OutcomeFallbackDeliveryUncertain) {
		t.Fatalf("journal = %#v, want the answer's delivery journaled uncertain", engine.Entries())
	}
	if err := sup.SubmitLine(runID, "agent-b", "hello", person); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("SubmitLine after the run's context ended = %v, want ErrDeliveryUncertain", err)
	}
	if applies, lines := engine.Applies(), engine.Lines(); len(applies) != 0 || len(lines) != 0 {
		t.Fatalf("applies = %#v, lines = %#v; want nothing written", applies, lines)
	}

	engine.SetSupported(adapters.DecisionAllow)
	engine.SetEvaluation(automaticAllow())
	engine.SetMaxConsecutiveAuto(1)
	if err := engine.RecordAudit(audit.Entry{Kind: audit.KindDecision, SessionID: "agent-c", DecisionBy: audit.DecisionByPolicy}); err != nil {
		t.Fatalf("RecordAudit: %v", err)
	}
	held := prompt("agent-c", "prompt-held")
	engine.SetPending("agent-c", &held)
	ended, cancel := context.WithCancel(context.Background())
	cancel()
	if pending, err := engine.PendingEvent(ended, "agent-c"); !errors.Is(err, context.Canceled) || pending != nil {
		t.Fatalf("PendingEvent = %#v, %v; want the context's error", pending, err)
	}
	if err := engine.ApplyDecision(ended, "agent-c", held, adapters.DecisionAllow, ""); !errors.Is(err, context.Canceled) {
		t.Fatalf("ApplyDecision = %v, want the context's error", err)
	}
	for operation, call := range map[supervisetest.Lifecycle]func(context.Context, string) error{
		supervisetest.Stop:    engine.StopAgent,
		supervisetest.Start:   engine.StartAgent,
		supervisetest.Restart: engine.RestartAgent,
	} {
		if err := call(ended, "agent-c"); !errors.Is(err, context.Canceled) {
			t.Fatalf("%s = %v, want the context's error", operation, err)
		}
		if calls := engine.LifecycleCalls(operation); !reflect.DeepEqual(calls, []string{"agent-c"}) {
			t.Fatalf("%s calls = %v, want the refused call among them", operation, calls)
		}
	}
	if id := heldBy(t, engine, "agent-c"); id != "prompt-held" {
		t.Fatalf("the backend holds %q after the refused calls, want prompt-held", id)
	}
	if !limitedOn(engine, "agent-c") {
		t.Fatal("a refused Start or Restart reset the count of the policy's decisions")
	}
	if applies := engine.Applies(); len(applies) != 0 {
		t.Fatalf("applies = %#v, want nothing written", applies)
	}
}

// A line error is returned by every line from then on, and a held line waits
// for its release while the session refuses a second one.
func TestALineFailsOrWaitsAsTheEngineWasTold(t *testing.T) {
	engine := supervisetest.NewEngine()
	engine.SetLineErr(terminal.ErrClosed)
	sup := newCore(t, engine, supervise.Options{}, "agent-a", "agent-b")

	if err := sup.SubmitLine(runID, "agent-a", "first", person); !errors.Is(err, supervise.ErrLineUnavailable) {
		t.Fatalf("SubmitLine on a closed session = %v, want ErrLineUnavailable", err)
	}
	if agent := agentOf(t, sup, "agent-a"); agent.Running || agent.Status != "exited" {
		t.Fatalf("agent-a = %#v, want it shown exited", agent)
	}

	engine.SetLineErr(nil)
	started := make(chan string, 1)
	release := make(chan struct{})
	engine.HoldLines(started, release)
	done := make(chan error, 1)
	go func() { done <- sup.SubmitLine(runID, "agent-b", "second", person) }()
	if sessionID := receive(t, started, "the line to begin"); sessionID != "agent-b" {
		t.Fatalf("started = %q, want agent-b", sessionID)
	}
	if err := sup.SubmitLine(runID, "agent-b", "third", person); !errors.Is(err, supervise.ErrLineInFlight) {
		t.Fatalf("a second line while the first is held = %v, want ErrLineInFlight", err)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("SubmitLine: %v", err)
	}
	want := []supervisetest.Line{{SessionID: "agent-a", Text: "first"}, {SessionID: "agent-b", Text: "second"}}
	if lines := engine.Lines(); !reflect.DeepEqual(lines, want) {
		t.Fatalf("lines = %#v, want %#v", lines, want)
	}
}

// The journal fails at the call it was told to and at every call after it, as
// the runtime's recorder keeps its first error: the core freezes the run.
func TestTheJournalFailsFromTheCallItWasToldTo(t *testing.T) {
	engine := supervisetest.NewEngine()
	engine.SetAuditFailAt(2)
	sup := newCore(t, engine, supervise.Options{}, "agent-a")

	sup.Handle(session.AdapterEvent{Event: prompt("agent-a", "prompt-1")})
	if !sup.State().AuditFailed {
		t.Fatal("the core did not see the journal fail")
	}
	entries := engine.Entries()
	if len(entries) != 1 || entries[0].Kind != audit.KindEventDetected {
		t.Fatalf("journal = %#v, want only the entry before the failure", entries)
	}
	if err := engine.RecordAudit(audit.Entry{Kind: audit.KindAttachStarted, SessionID: "agent-a"}); !errors.Is(err, supervisetest.ErrJournalUnavailable) {
		t.Fatalf("RecordAudit after the failure = %v, want ErrJournalUnavailable", err)
	}
	if got := len(engine.Entries()); got != 1 {
		t.Fatalf("journal = %d entries, want nothing more journaled", got)
	}
}

// The limit of consecutive decisions counts the policy's journaled decisions
// on a session, as the runtime's tracker does: each adds one, a person's
// decision sets the count back to zero, and so does a Start or a Restart; a
// line and a Stop change nothing. It holds back only what the policy would
// answer, and asks about it as the runtime does: the action becomes ask, and
// the rule's proposal stays. A prompt the policy asks about anyway keeps its
// own reason. The limit is two, so that a count that only noted a decision,
// one that counted each twice, or a reset that only took one off, would show.
func TestTheConsecutiveLimitCountsThePolicysDecisionsUntilAPersonOrAStartResetsIt(t *testing.T) {
	engine := supervisetest.NewEngine()
	engine.SetSupported(adapters.DecisionAllow)
	engine.SetEvaluation(automaticAllow())
	engine.SetEvaluationFor("prompt-asked", policy.Evaluation{
		Action:         policy.ActionAsk,
		ProposedAction: policy.ActionAsk,
		RuleName:       "ask-writes",
		Reason:         policy.ReasonRule,
	})
	engine.SetMaxConsecutiveAuto(2)
	sup := newCore(t, engine, supervise.Options{}, "agent-a")

	assertAskedForTheLimit := func(eventID string) {
		t.Helper()
		sup.Handle(session.AdapterEvent{Event: prompt("agent-a", eventID)})
		views := pendingOf(sup, "agent-a")
		if len(views) != 1 || views[0].ID != eventID {
			t.Fatalf("pending = %#v, want %s alone", views, eventID)
		}
		want := supervise.EvaluationView{
			Action:         string(policy.ActionAsk),
			ProposedAction: string(policy.ActionAllow),
			RuleName:       "allow-safe",
			Reason:         policy.ReasonConsecutiveLimit,
		}
		if views[0].Evaluation != want {
			t.Fatalf("%s's evaluation = %#v, want it asked for the limit: %#v", eventID, views[0].Evaluation, want)
		}
	}
	// answered has the policy answer each prompt on agent-a in turn.
	answered := func(eventIDs ...string) {
		t.Helper()
		for _, eventID := range eventIDs {
			answeredByThePolicy(t, engine, sup, prompt("agent-a", eventID))
		}
	}

	answered("prompt-1")
	if err := sup.SubmitLine(runID, "agent-a", "a line", person); err != nil {
		t.Fatalf("SubmitLine: %v", err)
	}
	if limitedOn(engine, "agent-a") {
		t.Fatal("one decision of the policy brought the count to a limit of two")
	}
	answered("prompt-2")
	assertAskedForTheLimit("prompt-3")
	if evaluation := engine.Evaluate(prompt("agent-a", "prompt-asked")); evaluation.Reason != policy.ReasonRule ||
		evaluation.Action != policy.ActionAsk {
		t.Fatalf("a prompt the policy asks about at the limit = %#v, want it asked under its rule", evaluation)
	}
	if err := sup.SubmitDecision(runID, "agent-a", "prompt-3", "y", person); err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}
	// Two more go through: the person's decision set the count back to zero,
	// not one down.
	answered("prompt-4", "prompt-5")
	assertAskedForTheLimit("prompt-6")

	if err := sup.RestartSession(runID, "agent-a"); err != nil {
		t.Fatalf("RestartSession: %v", err)
	}
	answered("prompt-7", "prompt-8")

	if err := sup.StopSession(runID, "agent-a"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if !limitedOn(engine, "agent-a") {
		t.Fatal("a Stop reset the count of the policy's decisions")
	}
	if err := sup.StartSession(runID, "agent-a"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	if limitedOn(engine, "agent-a") {
		t.Fatal("a Start left the count of the policy's decisions at the limit")
	}
	answered("prompt-9", "prompt-10")
	if !limitedOn(engine, "agent-a") {
		t.Fatal("two decisions of the policy after a Start did not bring the count to the limit")
	}

	var byThePolicy []string
	for _, apply := range engine.Applies() {
		if apply.Decision == adapters.DecisionAllow {
			byThePolicy = append(byThePolicy, apply.Event.ID)
		}
	}
	want := []string{"prompt-1", "prompt-2", "prompt-4", "prompt-5", "prompt-7", "prompt-8", "prompt-9", "prompt-10"}
	if !reflect.DeepEqual(byThePolicy, want) {
		t.Fatalf("answered by the policy = %v, want %v", byThePolicy, want)
	}
}

// The count of the policy's decisions is kept under the session ID each
// decision was journaled with, case and all, as the runtime's tracker keeps
// it: a prompt is held back by the count of its own session ID, and a
// person's decision resets that count alone. A Start or a Restart finds the
// agent whatever the case of the ID it is handed, as the lifecycle does, and
// sets the count of the agent back to zero.
func TestTheConsecutiveCountIsKeptUnderTheSessionIDAsJournaled(t *testing.T) {
	engine := supervisetest.NewEngine()
	engine.SetSupported(adapters.DecisionAllow)
	engine.SetEvaluation(automaticAllow())
	engine.SetMaxConsecutiveAuto(1)
	sup := newCore(t, engine, supervise.Options{}, "agent-a")
	decision := func(sessionID string, by audit.DecisionBy) {
		t.Helper()
		if err := engine.RecordAudit(audit.Entry{Kind: audit.KindDecision, SessionID: sessionID, DecisionBy: by}); err != nil {
			t.Fatalf("RecordAudit: %v", err)
		}
	}

	answeredByThePolicy(t, engine, sup, prompt("agent-a", "prompt-1"))
	if !limitedOn(engine, "agent-a") || limitedOn(engine, "Agent-A") {
		t.Fatal("the count of agent-a was not kept under agent-a alone")
	}
	decision("Agent-A", audit.DecisionByPolicy)
	if !limitedOn(engine, "Agent-A") {
		t.Fatal("a decision journaled under Agent-A was not counted under it")
	}
	decision("AGENT-A", audit.DecisionByHuman)
	if !limitedOn(engine, "agent-a") || !limitedOn(engine, "Agent-A") {
		t.Fatal("a person's decision journaled under AGENT-A reset the count of another spelling")
	}

	if err := sup.RestartSession(runID, "AGENT-A"); err != nil {
		t.Fatalf("RestartSession: %v", err)
	}
	if limitedOn(engine, "agent-a") {
		t.Fatal("a Restart of AGENT-A left the count of agent-a at the limit")
	}
}

// A lifecycle operation fails or waits as the engine was told, for every agent,
// and every call reaches the engine whatever its outcome.
func TestALifecycleOperationFailsOrWaitsAsTheEngineWasTold(t *testing.T) {
	engine := supervisetest.NewEngine()
	engine.SetLifecycleErr(supervisetest.Stop, errors.New("the process did not stop"))
	sup := newCore(t, engine, supervise.Options{}, "agent-a", "agent-b", "agent-c")

	if err := sup.StopSession(runID, "agent-a"); err == nil {
		t.Fatal("StopSession succeeded, want the engine's failure")
	}
	if agent := agentOf(t, sup, "agent-a"); !agent.Running || !agent.InputFrozen {
		t.Fatalf("agent-a after a failed stop = %#v, want it running and frozen", agent)
	}

	started := make(chan string, 1)
	release := make(chan struct{})
	engine.HoldLifecycle(supervisetest.Restart, started, release)
	done := make(chan error, 1)
	go func() { done <- sup.RestartSession(runID, "agent-b") }()
	if agentID := receive(t, started, "the restart to begin"); agentID != "agent-b" {
		t.Fatalf("started = %q, want agent-b", agentID)
	}
	if status := agentOf(t, sup, "agent-b").Status; status != "stopping" {
		t.Fatalf("agent-b while its restart is held = %q, want stopping", status)
	}
	close(release)
	if err := <-done; err != nil {
		t.Fatalf("RestartSession: %v", err)
	}
	if agent := agentOf(t, sup, "agent-b"); !agent.Running || agent.Status != "running" {
		t.Fatalf("agent-b after its restart = %#v, want it running", agent)
	}

	engine.SetLifecycleErr(supervisetest.Stop, nil)
	engine.SetLifecycleErr(supervisetest.Start, errors.New("the process did not start"))
	if err := sup.StopSession(runID, "agent-c"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	if err := sup.StartSession(runID, "agent-c"); err == nil {
		t.Fatal("StartSession succeeded, want the engine's failure")
	}
	if status := agentOf(t, sup, "agent-c").Status; status != "failed" {
		t.Fatalf("agent-c after a failed start = %q, want failed", status)
	}

	for operation, want := range map[supervisetest.Lifecycle][]string{
		supervisetest.Stop:    {"agent-a", "agent-c"},
		supervisetest.Restart: {"agent-b"},
		supervisetest.Start:   {"agent-c"},
	} {
		if calls := engine.LifecycleCalls(operation); !reflect.DeepEqual(calls, want) {
			t.Errorf("%s calls = %v, want %v", operation, calls, want)
		}
	}
}

// A stop, a start or a restart that succeeds lets go of the prompt the
// backend held, as the runtime's does: the process that raised it exited, and
// its processor discarded it, or a new process and processor replaced it. The
// session stays checked, so an answer to that prompt is a mismatch, and after
// a start or a restart the backend takes a line again. (After a stop the
// engine takes one too: it does not model a stopped process refusing a line,
// which the core refuses first.) A start or a restart that succeeds also sets
// the count of the policy's consecutive decisions back to zero, and a stop
// leaves it. One that fails changes neither: the engine takes it for a call
// that left the process as it was.
func TestALifecycleOperationLetsGoOfTheHeldPromptOnlyWhenItSucceeds(t *testing.T) {
	for _, test := range []struct {
		operation supervisetest.Lifecycle
		call      func(sup *supervise.Supervisor) error
		// replaces is whether the operation, once it succeeds, leaves a new
		// process: one that takes a line, and whose count starts at zero.
		replaces bool
	}{
		{operation: supervisetest.Stop, call: func(sup *supervise.Supervisor) error { return sup.StopSession(runID, "agent-a") }},
		{operation: supervisetest.Start, call: func(sup *supervise.Supervisor) error { return sup.StartSession(runID, "agent-a") }, replaces: true},
		{operation: supervisetest.Restart, call: func(sup *supervise.Supervisor) error { return sup.RestartSession(runID, "agent-a") }, replaces: true},
	} {
		t.Run(string(test.operation), func(t *testing.T) {
			engine := supervisetest.NewEngine()
			engine.SetSupported(adapters.DecisionAllow)
			engine.SetEvaluation(automaticAllow())
			engine.SetMaxConsecutiveAuto(1)
			sup := newCore(t, engine, supervise.Options{}, "agent-a")
			answeredByThePolicy(t, engine, sup, prompt("agent-a", "prompt-auto"))
			if test.operation == supervisetest.Start {
				// Only a stopped agent is started.
				if err := sup.StopSession(runID, "agent-a"); err != nil {
					t.Fatalf("StopSession: %v", err)
				}
			}
			held := prompt("agent-a", "prompt-held")
			engine.SetPending("agent-a", &held)

			engine.SetLifecycleErr(test.operation, errors.New("the process did not answer"))
			if err := test.call(sup); err == nil {
				t.Fatalf("%s succeeded, want the engine's failure", test.operation)
			}
			if id := heldBy(t, engine, "agent-a"); id != "prompt-held" {
				t.Fatalf("the backend holds %q after a failed %s, want prompt-held", id, test.operation)
			}
			if !limitedOn(engine, "agent-a") {
				t.Fatalf("a failed %s reset the count of the policy's decisions", test.operation)
			}

			engine.SetLifecycleErr(test.operation, nil)
			if err := test.call(sup); err != nil {
				t.Fatalf("%s: %v", test.operation, err)
			}
			if id := heldBy(t, engine, "agent-a"); id != "" {
				t.Fatalf("the backend holds %q after a %s, want nothing", id, test.operation)
			}
			if err := engine.ApplyDecision(context.Background(), "agent-a", held, adapters.DecisionManual, "y"); !errors.Is(err, adapters.ErrEventMismatch) {
				t.Fatalf("an answer to the prompt let go of after a %s = %v, want ErrEventMismatch", test.operation, err)
			}
			if applies := engine.Applies(); len(applies) != 1 {
				t.Fatalf("applies = %#v, want the policy's answer alone", applies)
			}
			if limited := limitedOn(engine, "agent-a"); limited == test.replaces {
				t.Fatalf("after a %s, the count stands at the limit: %v, want %v", test.operation, limited, !test.replaces)
			}
			if test.replaces {
				if err := engine.SendLine(context.Background(), "agent-a", "hello"); err != nil {
					t.Fatalf("SendLine after a %s: %v", test.operation, err)
				}
			}
		})
	}
}

// The engine takes every exit for its process's own until it is told
// otherwise; an exit it says a replacement superseded leaves the agent
// running, journaled under the stale reason.
func TestTheEngineSaysWhetherAnExitIsTheCurrentProcesss(t *testing.T) {
	engine := supervisetest.NewEngine()
	sup := newCore(t, engine, supervise.Options{}, "agent-a", "agent-b")
	exit := func(sessionID string) adapters.Event {
		code := 0
		return adapters.NewProcessExitEvent(sessionID, sessionID, "generic", 2, &code, false)
	}
	finishedReason := func(sessionID string) string {
		for _, entry := range engine.Entries() {
			if entry.Kind == audit.KindSessionFinished && entry.SessionID == sessionID {
				return entry.Reason
			}
		}
		return ""
	}

	sup.Handle(session.AdapterEvent{Event: exit("agent-a")})
	if agent := agentOf(t, sup, "agent-a"); agent.Running || agent.Status != "exited" {
		t.Fatalf("agent-a after its exit = %#v, want it exited", agent)
	}
	if reason := finishedReason("agent-a"); reason != "process_exit" {
		t.Fatalf("agent-a finished for %q, want process_exit", reason)
	}

	var (
		askedMu sync.Mutex
		asked   []string
	)
	engine.SetMarkExited(func(agentID string) bool {
		askedMu.Lock()
		defer askedMu.Unlock()
		asked = append(asked, agentID)
		return false
	})
	sup.Handle(session.AdapterEvent{Event: exit("agent-b")})
	if agent := agentOf(t, sup, "agent-b"); !agent.Running {
		t.Fatalf("agent-b after a stale exit = %#v, want it running", agent)
	}
	if reason := finishedReason("agent-b"); reason != audit.ReasonProcessExitStale {
		t.Fatalf("agent-b finished for %q, want %s", reason, audit.ReasonProcessExitStale)
	}
	askedMu.Lock()
	defer askedMu.Unlock()
	if !reflect.DeepEqual(asked, []string{"agent-b"}) {
		t.Fatalf("MarkProcessExited was asked about %v, want agent-b", asked)
	}
}

// The clock times the core's repeat window: a repeat is asked while the clock
// reads less than the window after the answer, and answered once the test has
// moved the clock past it.
func TestTheClockTimesTheRepeatWindow(t *testing.T) {
	engine := supervisetest.NewEngine()
	engine.SetSupported(adapters.DecisionAllow)
	engine.SetEvaluation(automaticAllow())
	clock := supervisetest.NewClock(time.Date(2026, time.September, 25, 9, 0, 0, 0, time.UTC))
	sup := newCore(t, engine, supervise.Options{Now: clock.Now}, "agent-a")
	first := prompt("agent-a", "prompt-1")
	repeatOf := func(eventID string) adapters.Event {
		repeat := first
		repeat.ID = eventID
		return repeat
	}

	answeredByThePolicy(t, engine, sup, first)
	clock.Advance(supervise.DefaultRepeatWindow - time.Millisecond)
	sup.Handle(session.AdapterEvent{Event: repeatOf("prompt-2")})
	if views := pendingOf(sup, "agent-a"); len(views) != 1 || views[0].Evaluation.Reason != supervise.ReasonRepeatAfterDelivery {
		t.Fatalf("pending = %#v, want the repeat asked", views)
	}
	sup.Handle(session.AdapterEventWithdrawn{Event: repeatOf("prompt-2")})

	clock.Advance(time.Millisecond)
	answeredByThePolicy(t, engine, sup, repeatOf("prompt-3"))
}

// The clock reads the same time for every goroutine and moves by exactly what
// they advanced it by, whatever the order.
func TestTheClockIsSafeForConcurrentUse(t *testing.T) {
	start := time.Date(2026, time.September, 25, 9, 0, 0, 0, time.UTC)
	clock := supervisetest.NewClock(start)
	var wait sync.WaitGroup
	for range 8 {
		wait.Add(1)
		go func() {
			defer wait.Done()
			for range 100 {
				clock.Advance(time.Millisecond)
				if clock.Now().Before(start) {
					t.Error("the clock went back")
					return
				}
			}
		}()
	}
	wait.Wait()
	if got, want := clock.Now(), start.Add(800*time.Millisecond); !got.Equal(want) {
		t.Fatalf("Now = %v, want %v", got, want)
	}
}

// What the engine hands out is the test's own: changing it changes nothing
// the engine holds.
func TestTheEnginesSnapshotsAreCopies(t *testing.T) {
	engine := supervisetest.NewEngine()
	engine.SetSupported(adapters.DecisionAllow)
	held := prompt("agent-b", "prompt-held")
	held.Metadata = map[string]string{"source": "fixture"}
	engine.SetPending("agent-b", &held)
	sup := newCore(t, engine, supervise.Options{}, "agent-a", "agent-b")
	if err := sup.SubmitLine(runID, "agent-a", "a line", person); err != nil {
		t.Fatalf("SubmitLine: %v", err)
	}
	event := prompt("agent-a", "prompt-1")
	event.Metadata = map[string]string{"source": "fixture"}
	sup.Handle(session.AdapterEvent{Event: event})
	if err := sup.SubmitDecision(runID, "agent-a", "prompt-1", "y", person); err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}
	if err := sup.StopSession(runID, "agent-a"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}

	evaluated := entriesFor(engine, audit.KindPolicyEvaluated, "prompt-1")
	if len(evaluated) != 1 || len(evaluated[0].Metadata) == 0 {
		t.Fatalf("policy_evaluated entries = %#v, want one with metadata", evaluated)
	}
	for key := range evaluated[0].Metadata {
		evaluated[0].Metadata[key] = "changed"
	}
	applies := engine.Applies()
	applies[0].Event.Metadata["source"] = "changed"
	applies[0].ManualInput = "changed"
	lines := engine.Lines()
	lines[0].Text = "changed"
	calls := engine.LifecycleCalls(supervisetest.Stop)
	calls[0] = "changed"
	supported := engine.SupportedDecisions(held)
	supported[0] = adapters.DecisionDeny
	pending, err := engine.PendingEvent(context.Background(), "agent-b")
	if err != nil || pending == nil {
		t.Fatalf("PendingEvent = %#v, %v; want prompt-held", pending, err)
	}
	pending.ID = "changed"
	pending.Metadata["source"] = "changed"

	for _, value := range entriesFor(engine, audit.KindPolicyEvaluated, "prompt-1")[0].Metadata {
		if value == "changed" {
			t.Fatal("a journal entry changed with the test's copy")
		}
	}
	if again := engine.Applies()[0]; again.Event.Metadata["source"] != "fixture" || again.ManualInput != "y" {
		t.Fatalf("apply = %#v, want it unchanged", again)
	}
	if again := engine.Lines(); !reflect.DeepEqual(again, []supervisetest.Line{{SessionID: "agent-a", Text: "a line"}}) {
		t.Fatalf("lines = %#v, want them unchanged", again)
	}
	if again := engine.LifecycleCalls(supervisetest.Stop); !reflect.DeepEqual(again, []string{"agent-a"}) {
		t.Fatalf("stop calls = %v, want them unchanged", again)
	}
	if again := engine.SupportedDecisions(held); !reflect.DeepEqual(again, []adapters.Decision{adapters.DecisionAllow}) {
		t.Fatalf("supported = %v, want it unchanged", again)
	}
	if again, _ := engine.PendingEvent(context.Background(), "agent-b"); again == nil || again.ID != "prompt-held" ||
		again.Metadata["source"] != "fixture" {
		t.Fatalf("PendingEvent = %#v, want prompt-held unchanged", again)
	}
}

// What the engine is handed is its own: changing what a test passed it
// changes nothing it holds.
func TestTheEngineKeepsItsOwnCopyOfWhatItIsHanded(t *testing.T) {
	engine := supervisetest.NewEngine()
	supported := []adapters.Decision{adapters.DecisionAllow}
	engine.SetSupported(supported...)
	held := prompt("agent-b", "prompt-held")
	held.Metadata = map[string]string{"source": "fixture"}
	engine.SetPending("agent-b", &held)
	entry := audit.Entry{Kind: audit.KindAttachStarted, SessionID: "agent-a", Metadata: map[string]string{"source": "fixture"}}
	if err := engine.RecordAudit(entry); err != nil {
		t.Fatalf("RecordAudit: %v", err)
	}
	written := prompt("agent-a", "prompt-1")
	written.Metadata = map[string]string{"source": "fixture"}
	if err := engine.ApplyDecision(context.Background(), "agent-a", written, adapters.DecisionAllow, ""); err != nil {
		t.Fatalf("ApplyDecision: %v", err)
	}

	supported[0] = adapters.DecisionDeny
	held.ID = "changed"
	held.Metadata["source"] = "changed"
	entry.Metadata["source"] = "changed"
	written.Metadata["source"] = "changed"

	if got := engine.SupportedDecisions(written); !reflect.DeepEqual(got, []adapters.Decision{adapters.DecisionAllow}) {
		t.Fatalf("supported = %v, want allow as it was set", got)
	}
	if got, _ := engine.PendingEvent(context.Background(), "agent-b"); got == nil || got.ID != "prompt-held" ||
		got.Metadata["source"] != "fixture" {
		t.Fatalf("PendingEvent = %#v, want prompt-held as it was set", got)
	}
	if entries := engine.Entries(); len(entries) != 1 || entries[0].Metadata["source"] != "fixture" {
		t.Fatalf("journal = %#v, want the entry as it was journaled", entries)
	}
	if applies := engine.Applies(); len(applies) != 1 || applies[0].Event.Metadata["source"] != "fixture" {
		t.Fatalf("applies = %#v, want the prompt as it was answered", applies)
	}
}

// A test may set the engine's knobs from its own goroutine while the core
// calls the engine from another. Each knob is set here to what it already
// was, so the core's answers do not change, only when the engine is touched;
// under the race detector, a knob read or written outside the engine's lock
// fails the test.
func TestTheKnobsCanBeSetWhileTheCoreCallsTheEngine(t *testing.T) {
	engine := supervisetest.NewEngine()
	stale := func(string) bool { return false }
	engine.SetMarkExited(stale)
	sup := newCore(t, engine, supervise.Options{}, "agent-a")
	askedByDefault := policy.Evaluation{
		Action:         policy.ActionAsk,
		ProposedAction: policy.ActionAsk,
		Reason:         policy.ReasonDefault,
	}

	stop := make(chan struct{})
	setting := make(chan struct{})
	defer func() {
		close(stop)
		<-setting
	}()
	go func() {
		defer close(setting)
		for {
			select {
			case <-stop:
				return
			default:
			}
			engine.SetEvaluation(askedByDefault)
			engine.SetSupported()
			engine.SetLineErr(nil)
			engine.SetMarkExited(stale)
			engine.SetMaxConsecutiveAuto(0)
			engine.SetAuditFailAt(0)
		}
	}()
	const rounds = 50
	for index := range rounds {
		event := prompt("agent-a", fmt.Sprintf("prompt-%d", index))
		sup.Handle(session.AdapterEvent{Event: event})
		sup.Handle(session.AdapterEventWithdrawn{Event: event})
		if err := sup.SubmitLine(runID, "agent-a", "a line", person); err != nil {
			t.Fatalf("SubmitLine in round %d: %v", index, err)
		}
		code := 0
		sup.Handle(session.AdapterEvent{Event: adapters.NewProcessExitEvent("agent-a", "agent-a", "generic", uint64(index+2), &code, false)})
	}

	if lines := engine.Lines(); len(lines) != rounds {
		t.Fatalf("lines = %d, want one a round", len(lines))
	}
	if evaluated := entriesFor(engine, audit.KindPolicyEvaluated, "prompt-0"); len(evaluated) != 1 {
		t.Fatalf("policy_evaluated entries of prompt-0 = %#v, want one", evaluated)
	}
	if agent := agentOf(t, sup, "agent-a"); !agent.Running {
		t.Fatalf("agent-a = %#v, want it running through every stale exit", agent)
	}
}

// fatalRecorder is a test whose failure is recorded rather than ending it.
type fatalRecorder struct {
	testing.TB
	failures []string
}

func (r *fatalRecorder) Helper() {}

func (r *fatalRecorder) Fatalf(format string, args ...interface{}) {
	r.failures = append(r.failures, fmt.Sprintf(format, args...))
}

// WaitFor returns as soon as the condition holds, which it checks even when
// no time is left, and otherwise fails the test, naming what it waited for.
func TestWaitForFailsNamingWhatItWaitedFor(t *testing.T) {
	recorder := &fatalRecorder{}
	checks := 0
	supervisetest.WaitFor(recorder, 0, "a condition that holds", func() bool {
		checks++
		return true
	})
	if checks != 1 || len(recorder.failures) != 0 {
		t.Fatalf("checks = %d, failures = %v; want one check and no failure", checks, recorder.failures)
	}

	supervisetest.WaitFor(recorder, 20*time.Millisecond, "the answer that never comes", func() bool { return false })
	if len(recorder.failures) != 1 || !strings.Contains(recorder.failures[0], "the answer that never comes") {
		t.Fatalf("failures = %v, want one naming what was awaited", recorder.failures)
	}
}

// supervisetestPath is the import path no production file may import.
const supervisetestPath = "github.com/Hocsman/Relayer/internal/supervise/supervisetest"

// importsSupervisetest reports whether the file imports the fake, under any
// name.
func importsSupervisetest(file *ast.File) bool {
	for _, spec := range file.Imports {
		if path, err := strconv.Unquote(spec.Path.Value); err == nil && path == supervisetestPath {
			return true
		}
	}
	return false
}

// No production file of either module imports the fake: it imports testing,
// and answers as a runtime would without being one. A front end built on it
// would supervise nothing.
func TestNoProductionFileImportsSupervisetest(t *testing.T) {
	root, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("resolve the repository root: %v", err)
	}
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatalf("read the repository's go.mod: %v", err)
	}
	if first, _, _ := strings.Cut(string(module), "\n"); strings.TrimSpace(first) != "module github.com/Hocsman/Relayer" {
		t.Fatalf("%s is not the repository root: its go.mod starts %q", root, first)
	}
	fileSet := token.NewFileSet()
	checked := make(map[string]bool)
	err = filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		name := entry.Name()
		if entry.IsDir() {
			// Hidden directories hold other worktrees and tooling, and
			// testdata and node_modules are never built.
			if path != root && (strings.HasPrefix(name, ".") || name == "testdata" || name == "node_modules") {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			return nil
		}
		file, err := parser.ParseFile(fileSet, path, nil, parser.ImportsOnly)
		if err != nil {
			return fmt.Errorf("parse %s: %w", path, err)
		}
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		relative = filepath.ToSlash(relative)
		checked[relative] = true
		if importsSupervisetest(file) {
			t.Errorf("%s imports %s", relative, supervisetestPath)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk %s: %v", root, err)
	}
	// A walk that missed a module, or the root, would pass for nothing.
	for _, want := range []string{"main.go", "internal/supervise/supervisor.go", "internal/app/run.go", "cmd/relayer-gui/app.go"} {
		if !checked[want] {
			t.Errorf("%s was not checked", want)
		}
	}
}

// The check finds the fake imported under any name, alone or among others,
// and nothing else.
func TestTheImportCheckFindsTheFakeUnderAnyName(t *testing.T) {
	for _, test := range []struct {
		source string
		want   bool
	}{
		{source: `import "` + supervisetestPath + `"`, want: true},
		{source: `import fake "` + supervisetestPath + `"`, want: true},
		{source: `import _ "` + supervisetestPath + `"`, want: true},
		{source: "import (\n\t\"context\"\n\t\"" + supervisetestPath + "\"\n)", want: true},
		{source: `import "github.com/Hocsman/Relayer/internal/supervise"`, want: false},
		{source: `import "` + supervisetestPath + `x"`, want: false},
	} {
		file, err := parser.ParseFile(token.NewFileSet(), "source.go", "package p\n\n"+test.source+"\n", parser.ImportsOnly)
		if err != nil {
			t.Fatalf("parse %q: %v", test.source, err)
		}
		if got := importsSupervisetest(file); got != test.want {
			t.Errorf("importsSupervisetest(%q) = %v, want %v", test.source, got, test.want)
		}
	}
}
