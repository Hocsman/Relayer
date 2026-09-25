package supervise_test

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// These tests pin what a view says beside its JSON, for a front end that shows
// more than the desktop does: how a prompt shown delivered left the run, and
// that the policy denies a prompt that offers deny alone. Neither is in the
// JSON, which stays the desktop's SupervisionEvent.

// shownViews returns, in order, every view of one prompt the sink was shown.
func shownViews(sink *recordingSink, eventID string) []supervise.View {
	var views []supervise.View
	for _, call := range sink.snapshot() {
		if call.kind == "prompt" && call.view.ID == eventID {
			views = append(views, call.view)
		}
	}
	return views
}

// A prompt shown delivered says how it left: its answer, the policy's or a
// person's, was written; the agent no longer showed it when its answer was to
// be written, so nothing was; or it went before an answer was applied to it.
// Shown "delivered" alone, a stale answer read as applied. Every view before
// the last, pending or delivering, says nothing.
func TestAViewSaysHowItsPromptLeft(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(*fakeEngine)
		// leave raises prompt-1 of agent-a and has it leave the run.
		leave func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine)
		want  string
	}{
		{name: "the policy's answer, written", want: supervise.ResolutionAnswered,
			setup: func(f *fakeEngine) { f.evaluation = automaticAllow() },
			leave: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
			}},
		{name: "a person's answer, written", want: supervise.ResolutionAnswered,
			leave: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
				if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", alice); err != nil {
					t.Fatalf("SubmitDecision: %v", err)
				}
			}},
		{name: "the policy's answer to a prompt no longer shown", want: supervise.ResolutionNotApplied,
			setup: func(f *fakeEngine) {
				f.evaluation = automaticAllow()
				f.applyErr = adapters.ErrEventMismatch
			},
			leave: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
			}},
		{name: "a person's answer to a prompt no longer shown", want: supervise.ResolutionNotApplied,
			setup: func(f *fakeEngine) { f.applyErr = adapters.ErrEventMismatch },
			leave: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
				if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", alice); !errors.Is(err, supervise.ErrDecisionStale) {
					t.Fatalf("SubmitDecision = %v, want ErrDecisionStale", err)
				}
			}},
		{name: "a prompt the agent withdrew", want: supervise.ResolutionWithdrawn,
			leave: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				prompt := promptEvent("agent-a", "prompt-1")
				sup.Handle(session.AdapterEvent{Event: prompt})
				sup.Handle(session.AdapterEventWithdrawn{Event: prompt})
			}},
		{name: "a prompt a resync no longer found", want: supervise.ResolutionWithdrawn,
			leave: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				prompt := promptEvent("agent-a", "prompt-1")
				sup.Handle(session.AdapterEvent{Event: prompt})
				sup.Handle(session.AdapterEventWithdrawn{Event: prompt, Reason: session.WithdrawnByResync})
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			if test.setup != nil {
				test.setup(engine)
			}
			sup, sink := newCoreForTest(t, engine, "agent-a")
			test.leave(t, sup, engine)
			waitFor(t, 2*time.Second, "the prompt to leave", func() bool {
				views := shownViews(sink, "prompt-1")
				return len(views) > 0 && views[len(views)-1].DeliveryStatus == "delivered"
			})
			sup.BeginDrain()
			sup.Wait()

			views := shownViews(sink, "prompt-1")
			last := views[len(views)-1]
			if last.DeliveryStatus != "delivered" || last.Resolution() != test.want {
				t.Fatalf("the prompt left shown %s, resolution %q, want delivered, %q", last.DeliveryStatus, last.Resolution(), test.want)
			}
			for _, view := range views[:len(views)-1] {
				if view.DeliveryStatus == "delivered" || view.Resolution() != "" {
					t.Fatalf("a view before the last says %s, resolution %q: %#v", view.DeliveryStatus, view.Resolution(), view)
				}
			}
			assertJSONDoesNotContain(t, last, "resolution")
			assertJSONDoesNotContain(t, last, test.want)
		})
	}
}

// A prompt the policy denies that goes to a person all the same says so,
// whichever path sent it there: taken in while the hand is held or past a
// limit, handed back when the adapter could not write the policy's deny, or
// asked when the hand was taken while it waited. It offers deny alone, or
// nothing where the adapter encodes no deny; a front end reads which from its
// decisions, and why from DenyOnly, as the core decided it. A prompt the
// policy does not deny by itself says nothing of the kind.
func TestADenyOnlyViewSaysSo(t *testing.T) {
	both := []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
	for _, test := range []struct {
		name       string
		evaluation policy.Evaluation
		supported  []adapters.Decision
		// raise brings prompt deny-2 of agent-a to the operator.
		raise         func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine)
		wantDenyOnly  bool
		wantDecisions []string
	}{
		{name: "taken in while the hand is held", evaluation: automaticDeny(), supported: both,
			wantDenyOnly: true, wantDecisions: []string{"deny"},
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				sup.SetHolder("agent-a", "conn-1")
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-2")})
			}},
		{name: "taken in past the policy's limit", evaluation: automaticDeny(), supported: both,
			wantDenyOnly: true, wantDecisions: []string{"deny"},
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				engine.set(func(f *fakeEngine) { f.maxConsecutiveAuto = 1 })
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-1")})
				waitForTheSessionToBeFree(t, sup, "agent-a")
				second := promptEvent("agent-a", "deny-2")
				second.Sequence = 2
				sup.Handle(session.AdapterEvent{Event: second})
			}},
		{name: "handed back when the adapter could not write the deny", evaluation: automaticDeny(), supported: both,
			wantDenyOnly: true, wantDecisions: []string{"deny"},
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				engine.set(func(f *fakeEngine) { f.applyErrs = []error{adapters.ErrDecisionUnsupported} })
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-2")})
			}},
		{name: "asked when the hand is taken while it waits", evaluation: automaticDeny(), supported: both,
			wantDenyOnly: true, wantDecisions: []string{"deny"},
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				letTheLineGo := holdWithALine(t, engine, sup, "agent-a")
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-2")})
				sup.SetHolder("agent-a", "conn-1")
				letTheLineGo()
			}},
		{name: "where the adapter encodes no deny", evaluation: automaticDeny(), supported: nil,
			wantDenyOnly: true, wantDecisions: []string{},
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				sup.SetHolder("agent-a", "conn-1")
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-2")})
			}},
		{name: "an allow the hand holds back", evaluation: automaticAllow(), supported: both,
			wantDenyOnly: false, wantDecisions: []string{"allow", "deny"},
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				sup.SetHolder("agent-a", "conn-1")
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-2")})
			}},
		{name: "a prompt the policy asks about", supported: both,
			evaluation:   policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionAsk, Reason: policy.ReasonDefault},
			wantDenyOnly: false, wantDecisions: []string{"allow", "deny"},
			raise: func(t *testing.T, sup *supervise.Supervisor, engine *fakeEngine) {
				sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-2")})
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluation = test.evaluation
			engine.supportedDecisions = test.supported
			sup, sink := newCoreWithOptions(t, engine, supervise.Options{Now: newTestClock().Now}, "agent-a")
			test.raise(t, sup, engine)
			waitFor(t, 2*time.Second, "the prompt to wait on the operator", func() bool {
				shown := viewOf(sup, "deny-2")
				return shown != nil && !shown.Evaluation.Automatic && shown.DeliveryStatus == "pending"
			})
			// What the hand changes is shown on a goroutine of its own, which a
			// drain waits for.
			sup.BeginDrain()
			sup.Wait()

			shown := viewOf(sup, "deny-2")
			if shown.DenyOnly() != test.wantDenyOnly || !reflect.DeepEqual(shown.Decisions, test.wantDecisions) {
				t.Fatalf("the prompt says deny only %t and offers %v, want %t and %v", shown.DenyOnly(), shown.Decisions, test.wantDenyOnly, test.wantDecisions)
			}
			views := shownViews(sink, "deny-2")
			if last := views[len(views)-1]; last.DenyOnly() != test.wantDenyOnly || !reflect.DeepEqual(last.Decisions, test.wantDecisions) {
				t.Fatalf("the prompt was last shown saying deny only %t and offering %v, want %t and %v", last.DenyOnly(), last.Decisions, test.wantDenyOnly, test.wantDecisions)
			}
			for _, view := range append(views, *shown) {
				assertJSONDoesNotContain(t, view, "denyOnly")
				assertJSONDoesNotContain(t, view, "deny_only")
			}
		})
	}
}

// A deny-only prompt its terminal's holder typed into stays deny-only but
// offers nothing, whatever the adapter encodes, and a typed answer to it is
// refused as the terminal's (ErrTypedAtTerminal): neither refused as deny-only
// nor taken over the policy's deny. A front end that read its empty decisions
// as an adapter that encodes no deny would offer a typed answer the core
// refuses; its reason, typed_at_terminal, is what tells the two apart.
func TestADenyOnlyPromptTypedIntoAtItsTerminalSaysWhyItOffersNothing(t *testing.T) {
	for _, test := range []struct {
		name      string
		supported []adapters.Decision
	}{
		{name: "where the adapter encodes a deny", supported: []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}},
		{name: "where the adapter encodes none"},
	} {
		t.Run(test.name, func(t *testing.T) {
			engine := newFakeEngine()
			engine.evaluation = automaticDeny()
			engine.supportedDecisions = test.supported
			sup, sink := newCoreForTest(t, engine, "agent-a")
			sup.SetHolder("agent-a", "conn-1")
			sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "deny-2")})
			// The holder types at the terminal.
			admitted(t, sup, "agent-a", "conn-1")()
			waitFor(t, 2*time.Second, "the prompt to be the terminal's", func() bool {
				shown := viewOf(sup, "deny-2")
				return shown != nil && shown.Evaluation.Reason == supervise.ReasonTypedAtTerminal
			})
			if err := sup.SubmitDecision(testRunID, "agent-a", "deny-2", "y", alice); !errors.Is(err, supervise.ErrTypedAtTerminal) {
				t.Fatalf("a typed answer = %v, want ErrTypedAtTerminal", err)
			}
			// What the keystrokes change is shown on a goroutine of their
			// own, which a drain waits for.
			sup.BeginDrain()
			sup.Wait()

			views := shownViews(sink, "deny-2")
			last := views[len(views)-1]
			if !last.DenyOnly() || len(last.Decisions) != 0 || last.Evaluation.Reason != supervise.ReasonTypedAtTerminal {
				t.Fatalf("the prompt was last shown saying deny only %t, offering %v, for the reason %q; want deny only, nothing offered, %q",
					last.DenyOnly(), last.Decisions, last.Evaluation.Reason, supervise.ReasonTypedAtTerminal)
			}
			if calls := engine.applySnapshot(); len(calls) != 0 {
				t.Fatalf("a prompt the holder typed into was answered through the core: %#v", calls)
			}
		})
	}
}
