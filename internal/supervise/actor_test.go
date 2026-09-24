package supervise_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
	"github.com/Hocsman/Relayer/internal/terminal"
)

// These tests pin who the core says acted. A front end that serves several
// people names each one with an Actor, which the journal keeps on that
// person's decisions, their deliveries and their lines; the desktop names
// nobody. An actor whose role may only watch acts on nothing.

// alice is a signed-in operator of the web gateway, on one of her tabs.
var alice = supervise.Actor{Identity: "alice", Role: supervise.RoleOperator, ConnID: "conn-1"}

// aliceMetadata is what alice's entries say of her.
var aliceMetadata = map[string]string{"operator": "alice", "role": "operator", "conn_id": "conn-1"}

// assertNamedBy checks that a person's entry names the operator and carries
// exactly the metadata given.
func assertNamedBy(t *testing.T, entry audit.Entry, operator string, metadata map[string]string) {
	t.Helper()
	if entry.Operator != operator || !reflect.DeepEqual(entry.Metadata, metadata) {
		t.Fatalf("%s entry of %s (%s) names operator %q with metadata %#v, want %q with %#v",
			entry.Kind, entry.EventID, entry.Outcome, entry.Operator, entry.Metadata, operator, metadata)
	}
}

// assertThePolicyNamesNobody checks that no entry the core wrote on its own,
// or for the policy, names a person.
func assertThePolicyNamesNobody(t *testing.T, engine *fakeEngine) {
	t.Helper()
	for _, entry := range engine.auditSnapshot() {
		if entry.DecisionBy == audit.DecisionByHuman {
			continue
		}
		if entry.Operator != "" || entry.Metadata["operator"] != "" || entry.Metadata["role"] != "" || entry.Metadata["conn_id"] != "" {
			t.Fatalf("an entry nobody made names a person: %#v", entry)
		}
	}
}

// A person's decision, and its delivery however it ends, name who made it:
// the Operator, and the operator, role and connection in the metadata. The
// same answer from the desktop, the zero Actor, names nobody, and its entries'
// metadata stays as empty as it always was. The entries of the detection, of
// the policy's evaluation and of the policy's own answers never name anybody.
func TestAPersonsDecisionAndItsDeliveryNameWhoMadeIt(t *testing.T) {
	for _, test := range []struct {
		name        string
		applyErr    error
		wantOutcome audit.Outcome
	}{
		{name: "applied", wantOutcome: audit.OutcomeApplied},
		{name: "not encodable", applyErr: adapters.ErrDecisionUnsupported, wantOutcome: audit.OutcomeFallbackUnsupported},
		{name: "stale", applyErr: adapters.ErrEventMismatch, wantOutcome: audit.OutcomeFallbackStale},
		{name: "uncertain", applyErr: errors.New("write failed half way"), wantOutcome: audit.OutcomeFallbackDeliveryUncertain},
	} {
		for _, actor := range []supervise.Actor{alice, desktop} {
			wantOperator, wantMetadata := "alice", aliceMetadata
			if actor == desktop {
				wantOperator, wantMetadata = "", nil
			}
			for _, answer := range []struct {
				name   string
				submit func(sup *supervise.Supervisor) error
			}{
				{name: "typed", submit: func(sup *supervise.Supervisor) error {
					return sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", actor)
				}},
				{name: "chosen", submit: func(sup *supervise.Supervisor) error {
					return sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", actor)
				}},
			} {
				t.Run(test.name+"/"+answer.name+"/"+wantOperator, func(t *testing.T) {
					engine := newFakeEngine()
					engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
					engine.applyErr = test.applyErr
					// The policy answers a prompt of its own first, so that
					// its entries sit in the same journal.
					engine.evaluationByID["automatic-0"] = automaticAllow()
					sup, _ := newCoreForTest(t, engine, "agent-a")
					automatic := promptEvent("agent-a", "automatic-0")
					automatic.Signature = "signature-automatic"
					if test.applyErr == nil {
						sup.Handle(session.AdapterEvent{Event: automatic})
						waitFor(t, 2*time.Second, "the policy's answer", func() bool {
							return len(engine.auditFor(audit.KindDelivery, "automatic-0")) == 1
						})
						waitForTheSessionToBeFree(t, sup, "agent-a")
					}

					sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
					_ = answer.submit(sup)

					decisions := engine.auditFor(audit.KindDecision, "prompt-1")
					deliveries := engine.auditFor(audit.KindDelivery, "prompt-1")
					if len(decisions) != 1 || len(deliveries) != 1 || deliveries[0].Outcome != test.wantOutcome {
						t.Fatalf("decision entries %#v, delivery entries %#v, want one of each, the delivery %s",
							decisions, deliveries, test.wantOutcome)
					}
					assertNamedBy(t, decisions[0], wantOperator, wantMetadata)
					assertNamedBy(t, deliveries[0], wantOperator, wantMetadata)
					assertThePolicyNamesNobody(t, engine)
				})
			}
		}
	}
}

// A front end names only what it knows of a person: an entry carries each of
// the three facts it was given, and no key for one it was not.
func TestAnEntryNamesOnlyWhatTheFrontEndGave(t *testing.T) {
	for _, test := range []struct {
		actor        supervise.Actor
		wantOperator string
		wantMetadata map[string]string
	}{
		{actor: supervise.Actor{Identity: " alice "}, wantOperator: "alice", wantMetadata: map[string]string{"operator": "alice"}},
		{actor: supervise.Actor{ConnID: "conn-1"}, wantMetadata: map[string]string{"conn_id": "conn-1"}},
		{actor: supervise.Actor{Role: supervise.RoleOperator}, wantMetadata: map[string]string{"role": "operator"}},
		{actor: supervise.Actor{Identity: "alice", ConnID: " "}, wantOperator: "alice", wantMetadata: map[string]string{"operator": "alice"}},
	} {
		engine := newFakeEngine()
		sup, _ := newCoreForTest(t, engine, "agent-a")
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
		if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", test.actor); err != nil {
			t.Fatalf("SubmitDecision by %#v: %v", test.actor, err)
		}
		for _, kind := range []audit.Kind{audit.KindDecision, audit.KindDelivery} {
			entries := engine.auditFor(kind, "prompt-1")
			if len(entries) != 1 {
				t.Fatalf("%s entries = %#v", kind, entries)
			}
			assertNamedBy(t, entries[0], test.wantOperator, test.wantMetadata)
		}
	}
}

// A line's entries have a closed shape, which no metadata passes: they name
// the person who sent the line by its Operator, and nothing else of them,
// however the line ends. The desktop's name nobody.
func TestAPersonsLineNamesWhoSentItAndNothingMore(t *testing.T) {
	for _, test := range []struct {
		name    string
		lineErr error
	}{
		{name: "sent"},
		{name: "a prompt came first", lineErr: terminal.ErrEventPending},
		{name: "invalid", lineErr: terminal.ErrInvalidLine},
		{name: "unsupported", lineErr: terminal.ErrLineUnsupported},
		{name: "closed", lineErr: terminal.ErrClosed},
		{name: "gone", lineErr: terminal.ErrSessionNotFound},
		{name: "uncertain", lineErr: errors.New("write failed half way")},
	} {
		for _, actor := range []supervise.Actor{alice, desktop} {
			wantOperator := alice.Identity
			if actor == desktop {
				wantOperator = ""
			}
			t.Run(test.name+"/"+wantOperator, func(t *testing.T) {
				engine := newFakeEngine()
				engine.lineErr = test.lineErr
				sup, _ := newCoreForTest(t, engine, "agent-a")
				_ = sup.SubmitLine(testRunID, "agent-a", "hello", actor)

				var entries []audit.Entry
				for _, entry := range engine.auditSnapshot() {
					if entry.Kind == audit.KindOperatorInput {
						entries = append(entries, entry)
					}
				}
				if len(entries) != 2 {
					t.Fatalf("operator_input entries = %#v, want the line's start and its end", entries)
				}
				for _, entry := range entries {
					if entry.Operator != wantOperator || entry.Metadata != nil {
						t.Fatalf("operator_input entry %s names %q with metadata %#v, want %q and none",
							entry.Reason, entry.Operator, entry.Metadata, wantOperator)
					}
				}
			})
		}
	}
}

// A role that may only watch acts on nothing. Its decision or line is refused
// with ErrReadOnlyActor before anything else, however else it is wrong: an
// empty answer, an answer the adapter cannot encode, a run that is over or
// none at all. Nothing is claimed, written, journaled or shown, and the
// prompt stays as it was for someone who may answer it. Any role the core
// does not know is read-only too; an operator, and the desktop, which has no
// roles, may act.
func TestARoleThatMayOnlyWatchIsRefusedBeforeAnythingElse(t *testing.T) {
	for _, role := range []string{supervise.RoleViewer, " Viewer ", "VIEWER", "auditor"} {
		viewer := supervise.Actor{Identity: "victor", Role: role, ConnID: "conn-2"}
		engine := newFakeEngine()
		engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
		sup, sink := newCoreForTest(t, engine, "agent-a")
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
		journaled, shown := len(engine.auditSnapshot()), len(sink.snapshot())

		var none *supervise.Supervisor
		for name, err := range map[string]error{
			"typed":                    sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", viewer),
			"typed nothing":            sup.SubmitDecision(testRunID, "agent-a", "prompt-1", " ", viewer),
			"typed to a stale run":     sup.SubmitDecision("run-old", "agent-a", "prompt-1", "y", viewer),
			"typed with no run":        none.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", viewer),
			"chosen":                   sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", viewer),
			"chosen, not an answer":    sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "maybe", viewer),
			"chosen with no run":       none.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "deny", viewer),
			"line":                     sup.SubmitLine(testRunID, "agent-a", "hello", viewer),
			"line to a stale run":      sup.SubmitLine("run-old", "agent-a", "hello", viewer),
			"line with no run":         none.SubmitLine(testRunID, "agent-a", "hello", viewer),
			"line to an unknown agent": sup.SubmitLine(testRunID, "agent-z", "hello", viewer),
		} {
			if !errors.Is(err, supervise.ErrReadOnlyActor) {
				t.Errorf("role %q, %s = %v, want ErrReadOnlyActor", role, name, err)
			}
		}
		if calls := engine.applySnapshot(); len(calls) != 0 {
			t.Fatalf("role %q answered the agent: %#v", role, calls)
		}
		if lines := engine.lineSnapshot(); len(lines) != 0 {
			t.Fatalf("role %q sent a line: %#v", role, lines)
		}
		if entries := engine.auditSnapshot(); len(entries) != journaled {
			t.Fatalf("role %q journaled %#v", role, entries[journaled:])
		}
		if calls := sink.snapshot(); len(calls) != shown {
			t.Fatalf("role %q showed %v", role, trace(calls[shown:]))
		}
		if state := sup.State(); len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "pending" {
			t.Fatalf("role %q left the prompt %#v", role, state.Pending)
		}
	}

	for _, actor := range []supervise.Actor{
		{Identity: "alice", Role: supervise.RoleOperator},
		{Identity: "alice", Role: " Operator "},
		{Identity: "alice"},
		desktop,
	} {
		engine := newFakeEngine()
		sup, _ := newCoreForTest(t, engine, "agent-a")
		if err := sup.SubmitLine(testRunID, "agent-a", "hello", actor); err != nil {
			t.Fatalf("%#v may not send a line: %v", actor, err)
		}
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
		if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", actor); err != nil {
			t.Fatalf("%#v may not answer: %v", actor, err)
		}
	}
}

// An empty answer is not an answer, and a choice is only allow or deny, and
// only one the adapter offers for the occurrence. Each is refused before the
// core claims the session or journals anything: the prompt stays pending for
// a real answer, nothing is shown, and the agent receives nothing. It holds
// for a person named by the gateway as for the desktop.
func TestAnAnswerThatIsNoneIsRefusedWithNothingChanged(t *testing.T) {
	for _, actor := range []supervise.Actor{alice, desktop} {
		engine := newFakeEngine()
		engine.supportedDecisions = []adapters.Decision{adapters.DecisionDeny}
		sup, sink := newCoreForTest(t, engine, "agent-a")
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
		journaled, shown := len(engine.auditSnapshot()), len(sink.snapshot())

		for name, test := range map[string]struct {
			err  error
			want error
		}{
			"nothing typed":     {sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "", actor), supervise.ErrEmptyDecision},
			"only spaces typed": {sup.SubmitDecision(testRunID, "agent-a", "prompt-1", " \t\r\n", actor), supervise.ErrEmptyDecision},
			"not a choice":      {sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "yes", actor), supervise.ErrUnsupportedDecision},
			"typed as a choice": {sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", string(adapters.DecisionManual), actor), supervise.ErrUnsupportedDecision},
			"not offered":       {sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", actor), supervise.ErrUnsupportedDecision},
		} {
			if !errors.Is(test.err, test.want) {
				t.Errorf("%#v, %s = %v, want %v", actor, name, test.err, test.want)
			}
		}
		if calls := engine.applySnapshot(); len(calls) != 0 {
			t.Fatalf("an answer that is none reached the agent: %#v", calls)
		}
		if entries := engine.auditSnapshot(); len(entries) != journaled {
			t.Fatalf("an answer that is none was journaled: %#v", entries[journaled:])
		}
		if calls := sink.snapshot(); len(calls) != shown {
			t.Fatalf("an answer that is none showed %v", trace(calls[shown:]))
		}
		if state := sup.State(); len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "pending" {
			t.Fatalf("the prompt after answers that are none = %#v", state.Pending)
		}
		// The session was never claimed: an offered answer goes through.
		if err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "deny", actor); err != nil {
			t.Fatalf("the offered answer after those that are none: %v", err)
		}
	}
}

// A typed answer is one line of text, as a line is: no control character, CR,
// LF and escape included, valid UTF-8, and no more than a line's 4096 bytes.
// Anything else is refused before anything is claimed, journaled or shown,
// from the gateway and from the desktop alike. Only a NUL byte was refused, by
// the adapters, so a typed answer wrote several answers, escape sequences and
// control keys of any length into the agent: the one input that needs no hand
// was a raw keyboard, journaled as a single answer asked, while a line was
// held to one line and refused while anybody held the terminal.
func TestATypedAnswerIsOneLineOfTextAsALineIs(t *testing.T) {
	for _, actor := range []supervise.Actor{alice, desktop} {
		engine := newFakeEngine()
		sup, sink := newCoreForTest(t, engine, "agent-a")
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
		journaled, shown := len(engine.auditSnapshot()), len(sink.snapshot())

		for name, typed := range map[string]string{
			"a second answer after a carriage return": "y\rrm -rf ~\r",
			"a second answer after a line feed":       "y\nrm -rf ~",
			"an escape sequence":                      "y\x1b[A\x1b[A\r",
			"a control key":                           "\x03",
			"a tab":                                   "y\tn",
			"a delete":                                "y\x7f",
			"a C1 control":                            "y\u0085n",
			"text that is not UTF-8":                  "y\xff",
			"more than a line's bytes":                strings.Repeat("y", adapters.MaxLineBytes+1),
		} {
			if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", typed, actor); !errors.Is(err, supervise.ErrAnswerInvalid) {
				t.Errorf("%#v, %s = %v, want ErrAnswerInvalid", actor, name, err)
			}
		}
		if calls := engine.applySnapshot(); len(calls) != 0 {
			t.Fatalf("a typed answer that is not one line reached the agent: %#v", calls)
		}
		if entries := engine.auditSnapshot(); len(entries) != journaled {
			t.Fatalf("a typed answer that is not one line was journaled: %#v", entries[journaled:])
		}
		if calls := sink.snapshot(); len(calls) != shown {
			t.Fatalf("a typed answer that is not one line showed %v", trace(calls[shown:]))
		}
		if state := sup.State(); len(state.Pending) != 1 || state.Pending[0].DeliveryStatus != "pending" {
			t.Fatalf("the prompt after the refused answers = %#v", state.Pending)
		}
		// The longest line is an answer, and goes through as typed.
		longest := strings.Repeat("y", adapters.MaxLineBytes)
		if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", longest, actor); err != nil {
			t.Fatalf("a typed answer of one line: %v", err)
		}
		if calls := engine.applySnapshot(); len(calls) != 1 || calls[0].decision != adapters.DecisionManual || calls[0].manualInput != longest {
			t.Fatalf("the answer of one line reached the agent as %#v", calls)
		}
	}
}
