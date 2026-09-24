package server

import (
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
)

// decision is the parameters of a submitDecision call.
func decision(prompt SupervisionEvent, runID, value string) map[string]any {
	return map[string]any{"runID": runID, "sessionID": prompt.SessionID, "eventID": prompt.ID, "value": value}
}

// choice is the parameters of a submitAutomaticDecision call.
func choice(prompt SupervisionEvent, value string) map[string]any {
	return map[string]any{"runID": prompt.RunID, "sessionID": prompt.SessionID, "eventID": prompt.ID, "decision": value}
}

// A typed answer reaches the agent as it was typed, and is journaled as asked.
// The gateway read "y", "yes" and "allow" as the adapter's allow and "n", "no"
// and "deny" as its deny: typing "y" into a Generic prompt, whose adapter
// encodes no allow, was refused, and typing "Y" into an Aider prompt sent the
// adapter's "y" and journaled that a person allowed it. Only the adapter
// knows what typed bytes mean.
func TestAWebTypedAnswerReachesTheAgentAsTyped(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{
			{id: "web-generic", mode: webAgentGeneric, adapter: "generic"},
			{id: "web-aider", mode: webAgentAider, adapter: "aider"},
		},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")

	generic := g.awaitPending("web-generic", 30*time.Second)
	if err := alice.call("submitDecision", decision(generic, generic.RunID, "y"), nil); err != nil {
		t.Fatalf("a typed y was refused: %v", err)
	}
	aider := g.awaitPending("web-aider", 30*time.Second)
	if err := alice.call("submitDecision", decision(aider, aider.RunID, "Y"), nil); err != nil {
		t.Fatalf("a typed Y was refused: %v", err)
	}

	for sessionID, typed := range map[string]string{"web-generic": `"y`, "web-aider": `"Y`} {
		screen := g.awaitReport(sessionID, 20*time.Second)
		first, count, extras := answers(screen)
		if count != 1 || extras != 0 || !strings.HasPrefix(first, typed) {
			t.Fatalf("%s received %s (%d answers, %d extra), want %s as typed, once; screen:\n%s", sessionID, first, count, extras, typed, screen)
		}
		decisions := entriesOf(g.sessionJournal(sessionID), audit.KindDecision)
		if len(decisions) != 1 || decisions[0].Decision != audit.DecisionAsk || decisions[0].Operator != "alice" {
			t.Fatalf("%s decisions = %s, want alice's typed answer journaled as asked", sessionID, journalTrace(decisions))
		}
	}
}

// The call meant for the buttons takes only allow or deny, and only one the
// prompt offers. The gateway sent whatever it was given as typed text, so a
// client could put any bytes into an agent through it, journaled as a human
// allowing them.
func TestAWebChosenAnswerIsOnlyOneThePromptOffers(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{
			{id: "web-generic", mode: webAgentGeneric, adapter: "generic"},
			{id: "web-aider", mode: webAgentAider, adapter: "aider"},
		},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")

	generic := g.awaitPending("web-generic", 30*time.Second)
	for _, value := range []string{"nope", "y", "allow"} {
		if err := alice.call("submitAutomaticDecision", choice(generic, value), nil); err == nil {
			t.Fatalf("the Generic prompt, which offers no choice, took %q as one", value)
		}
	}
	g.assertNoAnswerFor("web-generic", 500*time.Millisecond)
	if decisions := entriesOf(g.sessionJournal("web-generic"), audit.KindDecision); len(decisions) != 0 {
		t.Fatalf("a refused choice was journaled: %s", journalTrace(decisions))
	}
	if prompts := g.pending("web-generic"); len(prompts) != 1 || prompts[0].DeliveryStatus != "pending" {
		t.Fatalf("after the refused choices the prompts are %+v, want the prompt still offered", prompts)
	}

	aider := g.awaitPending("web-aider", 30*time.Second)
	if err := alice.call("submitAutomaticDecision", choice(aider, "allow"), nil); err != nil {
		t.Fatalf("an allow the Aider prompt offers was refused: %v", err)
	}
	screen := g.awaitReport("web-aider", 20*time.Second)
	if first, count, extras := answers(screen); count != 1 || extras != 0 || !strings.HasPrefix(first, `"y`) {
		t.Fatalf("the Aider agent received %s (%d answers, %d extra), want the adapter's allow once; screen:\n%s", first, count, extras, screen)
	}
}

// The journal names who answered, their role and the connection they answered
// from, on the decision and on its delivery. The gateway wrote the role
// "operator" whoever answered and never the connection, and the connection is
// what ties an answer to the attach and control records around it: one person
// may be signed in from several tabs.
func TestAWebDecisionNamesWhoMadeItAndFromWhere(t *testing.T) {
	g := startWebRun(t, webRun{
		auditMode: "detailed",
		agents:    []webAgent{{id: "web-generic", mode: webAgentGeneric, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	prompt := g.awaitPending("web-generic", 30*time.Second)
	if err := alice.call("submitDecision", decision(prompt, prompt.RunID, "fine"), nil); err != nil {
		t.Fatalf("submitDecision: %v", err)
	}
	g.awaitReport("web-generic", 20*time.Second)

	entries := g.sessionJournal("web-generic")
	named := append(entriesOf(entries, audit.KindDecision), entriesOf(entries, audit.KindDelivery)...)
	if len(named) != 2 {
		t.Fatalf("decision and delivery entries = %s, want one of each", journalTrace(named))
	}
	for _, entry := range named {
		if entry.Operator != "alice" || entry.Metadata["operator"] != "alice" || entry.Metadata["role"] != "operator" || entry.Metadata["conn_id"] != alice.connID {
			t.Fatalf("%s = operator %q, metadata %v; want alice, her role and her connection %s", entry.Kind, entry.Operator, entry.Metadata, alice.connID)
		}
	}
}

// A decision, a line and a stop of the run name the run they are for, and a
// request that names another run, or none, is refused with nothing changed.
// The gateway took an empty run ID for the current run on decisions and
// ignored the run altogether on lines and on stopping the run, so a tab left
// open on a replaced run, or a client that named none, answered, typed into
// and stopped the agents of the run that replaced it.
func TestAWebRequestActsOnlyOnTheRunItNames(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{
			{id: "web-generic", mode: webAgentGeneric, adapter: "generic"},
			{id: "web-listen", mode: webAgentListen, adapter: "generic"},
		},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	prompt := g.awaitPending("web-generic", 30*time.Second)
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)

	for _, runID := range []string{"", "a-run-that-was-replaced"} {
		if err := alice.call("submitDecision", decision(prompt, runID, "not this run"), nil); err == nil {
			t.Fatalf("a decision for run %q was taken", runID)
		}
		if err := alice.call("submitLine", map[string]any{"runID": runID, "sessionID": "web-listen", "line": "not this run"}, nil); err == nil {
			t.Fatalf("a line for run %q was taken", runID)
		}
		if err := alice.call("stopRun", map[string]any{"runID": runID}, nil); err == nil {
			t.Fatalf("a stop of run %q was taken", runID)
		}
	}
	g.assertNoAnswerFor("web-generic", 500*time.Millisecond)
	if screen := g.screen("web-listen"); strings.Contains(screen, webAgentLine) {
		t.Fatalf("a line for another run reached the agent; screen:\n%s", screen)
	}
	if prompts := g.pending("web-generic"); len(prompts) != 1 || prompts[0].DeliveryStatus != "pending" {
		t.Fatalf("the prompts are %+v, want the prompt still offered", prompts)
	}
	state := g.ctrl.GetState()
	if state.RunStatus != "running" {
		t.Fatalf("the run is %q after stops that named another run, want running", state.RunStatus)
	}
	for _, agent := range state.Agents {
		if !agent.Running {
			t.Fatalf("agent %s is not running after stops that named another run", agent.SessionID)
		}
	}
}

// A line goes through the core, which journals it before it writes it, under
// the operator who sent it, and refuses it while a prompt waits on the
// operator: typed into the agent then, it would answer the question. The
// gateway wrote the line beside anything else being written, with no check,
// and journaled it only afterwards.
func TestAWebLineIsJournaledFirstAndWaitsForTheQuestionToBeAnswered(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()

	if err := alice.call("submitLine", map[string]any{"runID": runID, "sessionID": "web-listen", "line": "first line"}, nil); err != nil {
		t.Fatalf("submitLine: %v", err)
	}
	g.awaitScreen("web-listen", "first line", 20*time.Second)
	lines := entriesOf(g.sessionJournal("web-listen"), audit.KindOperatorInput)
	if len(lines) != 2 || lines[0].Outcome != audit.OutcomeInFlight || lines[1].Outcome != audit.OutcomeApplied {
		t.Fatalf("the line's entries = %s, want it journaled before it was written and once it was", journalTrace(lines))
	}
	for _, entry := range lines {
		if entry.Operator != "alice" {
			t.Fatalf("the line's entry names %q, want alice", entry.Operator)
		}
	}

	livePrompt(g.ctrl, "web-listen", "a-question", time.Now().UTC())
	if err := alice.call("submitLine", map[string]any{"runID": runID, "sessionID": "web-listen", "line": "second line"}, nil); err == nil {
		t.Fatal("a line was taken while a prompt waits on the operator")
	}
	time.Sleep(500 * time.Millisecond)
	if screen := g.screen("web-listen"); strings.Contains(screen, "second line") {
		t.Fatalf("a line reached the agent while a prompt waited on the operator; screen:\n%s", screen)
	}
}

// A line is not written once the journal has failed, and the gateway's own
// journal failure is the core's: the line's entry cannot be written, so
// neither is the line. The gateway wrote the line and then failed to journal
// it, silently.
func TestAWebLineIsNotWrittenWithAFailedJournal(t *testing.T) {
	fault, wrap := newFaultEngine()
	g := startWebRun(t, webRun{
		engineWrap: wrap,
		agents:     []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	fault.set(func(f *faultEngine) { f.auditFails = true })

	if err := alice.call("submitLine", map[string]any{"runID": g.runID(), "sessionID": "web-listen", "line": "unrecorded"}, nil); err == nil {
		t.Fatal("a line was taken with a failing journal")
	}
	time.Sleep(500 * time.Millisecond)
	if screen := g.screen("web-listen"); strings.Contains(screen, "unrecorded") {
		t.Fatalf("a line reached the agent with no record of it; screen:\n%s", screen)
	}
	if state := g.ctrl.GetState(); state.Audit.Status != "failed" {
		t.Fatalf("the audit status every client reads is %q, want failed", state.Audit.Status)
	}
}
