package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// awaitReport waits until the agent reported it was done, or that no answer
// came: either way it no longer listens, and what it received is final.
func (g *webGateway) awaitReport(sessionID string, within time.Duration) string {
	g.t.Helper()
	deadline := time.Now().Add(within)
	screen := ""
	for time.Now().Before(deadline) {
		screen = g.screen(sessionID)
		if strings.Contains(screen, webAgentDone) || strings.Contains(screen, webAgentNoAnswer) {
			return screen
		}
		time.Sleep(50 * time.Millisecond)
	}
	g.t.Fatalf("the agent %s never reported within %s; screen:\n%s", sessionID, within, screen)
	return screen
}

// assertNoAnswerFor fails if the agent reports an answer within the period.
func (g *webGateway) assertNoAnswerFor(sessionID string, period time.Duration) {
	g.t.Helper()
	deadline := time.Now().Add(period)
	for time.Now().Before(deadline) {
		if screen := g.screen(sessionID); strings.Contains(screen, webAgentAnswer) {
			g.t.Fatalf("the agent %s received an answer nobody may have sent; screen:\n%s", sessionID, screen)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// faultEngine is a run's runtime with the failures a test asks for, set in
// place of the runtime where the supervision core meets it. A journal that
// refuses entries, a write that fails without writing or waits, and a stop
// that fails are what no real agent can be made to do on demand.
type faultEngine struct {
	supervise.Engine

	mu sync.Mutex
	// auditFails makes every entry fail once set.
	auditFails bool
	// applyErr is returned by every answer's write, which then writes
	// nothing.
	applyErr error
	// holdApply, when set, holds the next answer's write until it is closed
	// or the write's context ends.
	holdApply chan struct{}
	// holdApplyFirm, when set, holds the next answer's write until it is
	// closed, whatever the write's context: a write stuck below the context,
	// which is what a run's drain must wait for.
	holdApplyFirm chan struct{}
	// applyStarted receives the session of each answer's write as it starts.
	applyStarted chan string
	applying     int
	// overlapped is set when an answer's write started while another was in
	// progress.
	overlapped bool
	stopErr    error
	// holdAuditKind, when set with holdAudit, holds each entry of that kind,
	// before it is journaled, until holdAudit is closed; auditHeld receives
	// the kind as each entry waits.
	holdAuditKind audit.Kind
	holdAudit     chan struct{}
	auditHeld     chan audit.Kind
}

func newFaultEngine() (*faultEngine, func(supervise.Engine) supervise.Engine) {
	fault := &faultEngine{applyStarted: make(chan string, 16)}
	return fault, func(engine supervise.Engine) supervise.Engine {
		fault.Engine = engine
		return fault
	}
}

func (f *faultEngine) RecordAudit(entry audit.Entry) error {
	f.mu.Lock()
	fails := f.auditFails
	hold, held := f.holdAudit, f.auditHeld
	if entry.Kind != f.holdAuditKind {
		hold = nil
	}
	f.mu.Unlock()
	if hold != nil {
		if held != nil {
			held <- entry.Kind
		}
		<-hold
	}
	if fails {
		return errors.New("write audit: the disk is full")
	}
	return f.Engine.RecordAudit(entry)
}

func (f *faultEngine) ApplyDecision(ctx context.Context, sessionID string, event adapters.Event, decision adapters.Decision, manualInput string) error {
	f.mu.Lock()
	f.applying++
	if f.applying > 1 {
		f.overlapped = true
	}
	hold := f.holdApply
	f.holdApply = nil
	firm := f.holdApplyFirm
	f.holdApplyFirm = nil
	err := f.applyErr
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.applying--
		f.mu.Unlock()
	}()
	f.applyStarted <- sessionID
	if firm != nil {
		<-firm
	}
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	if err != nil {
		return err
	}
	return f.Engine.ApplyDecision(ctx, sessionID, event, decision, manualInput)
}

func (f *faultEngine) StopAgent(ctx context.Context, agentID string) error {
	f.mu.Lock()
	err := f.stopErr
	f.mu.Unlock()
	if err != nil {
		return err
	}
	return f.Engine.StopAgent(ctx, agentID)
}

func (f *faultEngine) set(change func(*faultEngine)) {
	f.mu.Lock()
	defer f.mu.Unlock()
	change(f)
}

func (f *faultEngine) overlap() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.overlapped
}

// entriesOf is the entries of one kind among entries.
func entriesOf(entries []audit.Entry, kind audit.Kind) []audit.Entry {
	var found []audit.Entry
	for _, entry := range entries {
		if entry.Kind == kind {
			found = append(found, entry)
		}
	}
	return found
}

// The web gateway answers the question the policy allows, once, and journals
// it as the policy's. It never delivered an automatic decision at all: it
// showed the prompt as automatic and waited for a person to click, and the
// click was journaled as a human decision, so the policy's limits never saw
// it. The agent here asks a real Aider question under a policy that allows
// it, and reports every answer it reads.
func TestTheWebGatewayAnswersAnAutomaticQuestionOnce(t *testing.T) {
	g := startWebRun(t, webRun{
		policy: "allow",
		agents: []webAgent{{id: "web-aider", mode: webAgentAider, adapter: "aider"}},
	})
	screen := g.awaitReport("web-aider", 40*time.Second)
	first, count, extras := answers(screen)
	if count != 1 || extras != 0 {
		t.Fatalf("the agent received %d answer(s) and %d extra one(s), want exactly one; screen:\n%s", count, extras, screen)
	}
	if !strings.HasPrefix(strings.ToLower(first), `"y`) {
		t.Fatalf("the agent received %s, want the Aider adapter's allow", first)
	}
	entries := g.sessionJournal("web-aider")
	var decisions, applied int
	for _, entry := range entries {
		if entry.Reason == supervise.ReasonRepeatAfterDelivery {
			t.Fatalf("the core declined a repeat of the answered question, so the adapter raised one:\n%s", journalTrace(entries))
		}
		if entry.Kind == audit.KindDecision {
			decisions++
			if entry.DecisionBy != audit.DecisionByPolicy || entry.Decision != audit.DecisionAllow || entry.Operator != "" {
				t.Fatalf("decision = %+v, want the policy's allow, naming nobody", entry)
			}
		}
		if entry.Kind == audit.KindDelivery && entry.Outcome == audit.OutcomeApplied {
			applied++
			if entry.DecisionBy != audit.DecisionByPolicy {
				t.Fatalf("delivery = %+v, want the policy's", entry)
			}
		}
	}
	if decisions != 1 || applied != 1 {
		t.Fatalf("the journal has %d decision(s) and %d applied delivery(ies), want one of each:\n%s", decisions, applied, journalTrace(entries))
	}
	if len(entriesOf(entries, audit.KindEventDetected)) == 0 || len(entriesOf(entries, audit.KindPolicyEvaluated)) == 0 {
		t.Fatalf("the prompt's detection or evaluation is not journaled:\n%s", journalTrace(entries))
	}
	if prompts := g.pending("web-aider"); len(prompts) != 0 {
		t.Fatalf("the answered prompt is still offered: %+v", prompts)
	}
}

// A prompt stays offered until its answer is written. The gateway deleted the
// prompt before it tried the answer, so an answer the adapter cannot encode, a
// Generic prompt's allow, took the prompt off every screen while the agent
// still waited on it, and the next answer was refused as "pending event not
// found". The core refuses such an answer before anything changes, and the
// agent then takes the answer the operator types, journaled as asked: typed
// text is never journaled as allowed.
func TestTheWebGatewayKeepsAPromptOfferedUntilItsAnswerIsWritten(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-generic", mode: webAgentGeneric, adapter: "generic"}},
	})
	prompt := g.awaitPending("web-generic", 30*time.Second)
	if err := g.ctrl.SubmitAutomaticDecision(prompt.RunID, prompt.SessionID, prompt.ID, "allow", webOperator); err == nil {
		t.Fatal("an allow the Generic adapter cannot encode was accepted")
	}
	prompts := g.pending("web-generic")
	if len(prompts) != 1 || prompts[0].ID != prompt.ID || prompts[0].DeliveryStatus != "pending" {
		t.Fatalf("after the refused answer the prompts are %+v, want the prompt still offered", prompts)
	}
	if agent := g.agent("web-generic"); agent.Status != "waiting" {
		t.Fatalf("after the refused answer the agent is %q, want waiting", agent.Status)
	}
	g.assertNoAnswerFor("web-generic", 500*time.Millisecond)

	if err := g.ctrl.SubmitDecision(prompt.RunID, prompt.SessionID, prompt.ID, "yes please", webOperator); err != nil {
		t.Fatalf("the typed answer was refused: %v", err)
	}
	screen := g.awaitReport("web-generic", 20*time.Second)
	first, count, extras := answers(screen)
	if count != 1 || extras != 0 || !strings.Contains(first, "yes please") {
		t.Fatalf("the agent received %s (%d answers, %d extra), want the typed answer once; screen:\n%s", first, count, extras, screen)
	}
	decisions := entriesOf(g.sessionJournal("web-generic"), audit.KindDecision)
	if len(decisions) != 1 || decisions[0].Decision != audit.DecisionAsk || decisions[0].DecisionBy != audit.DecisionByHuman {
		t.Fatalf("decisions = %s, want one: the typed answer, journaled as asked", journalTrace(decisions))
	}
}

// The journal says what became of every prompt: that it was detected and how
// the policy evaluated it, that the agent took it back, and that the process
// ended on its own. The gateway journaled none of these, so its journal held
// decisions about prompts it never recorded, and a session that exited never
// ended in it.
func TestTheWebJournalSaysWhatBecameOfEachPrompt(t *testing.T) {
	gate, openGate := webAgentGate(t)
	g := startWebRun(t, webRun{
		agents: []webAgent{
			{id: "web-withdraw", mode: webAgentWithdraw, adapter: "generic", env: gate},
			{id: "web-exit", mode: webAgentExit, adapter: "generic", env: map[string]string{webAgentExitEnv: "3", webAgentDelayEnv: "1s"}},
		},
	})

	prompt := g.awaitPending("web-withdraw", 30*time.Second)
	// The agent takes its question back once the question is offered.
	openGate()
	g.awaitScreen("web-withdraw", webAgentWithdrew, 20*time.Second)
	withdrawn := g.awaitJournal("web-withdraw", 10*time.Second, func(entry audit.Entry) bool {
		return entry.Kind == audit.KindEventWithdrawn && entry.EventID == prompt.ID
	})
	if withdrawn.Reason != "agent_withdrew_occurrence" {
		t.Fatalf("withdrawal = %+v, want the agent's", withdrawn)
	}
	for _, kind := range []audit.Kind{audit.KindEventDetected, audit.KindPolicyEvaluated} {
		found := false
		for _, entry := range g.sessionJournal("web-withdraw") {
			found = found || (entry.Kind == kind && entry.EventID == prompt.ID)
		}
		if !found {
			t.Fatalf("no %s entry for the prompt:\n%s", kind, journalTrace(g.sessionJournal("web-withdraw")))
		}
	}
	// Every client drops the prompt: it is shown answered, which is how the
	// interface lets a prompt go.
	deadline := time.Now().Add(5 * time.Second)
	for len(g.pending("web-withdraw")) != 0 {
		if time.Now().After(deadline) {
			t.Fatalf("the withdrawn prompt is still offered: %+v", g.pending("web-withdraw"))
		}
		time.Sleep(20 * time.Millisecond)
	}
	shownGone := false
	for _, frame := range g.broadcast(eventSemantic) {
		if view, ok := frame.payload.(SupervisionEvent); ok && view.ID == prompt.ID && view.DeliveryStatus == "delivered" {
			shownGone = true
		}
	}
	if !shownGone {
		t.Fatal("no client was told the withdrawn prompt is gone: its card stays until a reload")
	}

	finished := g.awaitJournal("web-exit", 30*time.Second, func(entry audit.Entry) bool {
		return entry.Kind == audit.KindSessionFinished && entry.Reason == "process_exit"
	})
	if finished.DecisionBy != audit.DecisionBySystem || finished.Operator != "" {
		t.Fatalf("session_finished = %+v, want the system's, naming nobody", finished)
	}
	exited := g.awaitAgent("web-exit", 10*time.Second, func(agent AgentState) bool { return !agent.Running })
	if exited.ExitCode == nil || *exited.ExitCode != 3 {
		t.Fatalf("the exited agent shows exit code %v, want 3", exited.ExitCode)
	}
}

// A backend stream error reaches every client, viewers included, by a fixed
// message, and is journaled. The gateway sent the error's own text, which can
// name a path or carry what the backend was handed, and journaled nothing.
func TestTheWebGatewayReportsAStreamErrorByAFixedMessage(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	const leaked = `read C:\Users\alice\.relayer\token=sk-live-0123456789abcdef: broken pipe`
	g.inject(session.Error{SessionID: "web-listen", Err: errors.New(leaked)})

	var failures []SafeErrorEvent
	for _, frame := range g.broadcast(eventError) {
		if failure, ok := frame.payload.(SafeErrorEvent); ok && failure.SessionID == "web-listen" {
			failures = append(failures, failure)
		}
	}
	if len(failures) != 1 || failures[0].Code != "backend_stream_failed" || failures[0].Message != "The backend stream failed." {
		t.Fatalf("errors shown = %+v, want the one fixed stream failure", failures)
	}
	if text := g.broadcastText(); strings.Contains(text, "sk-live") || strings.Contains(text, `alice`) {
		t.Fatalf("the stream error's own text reached the clients:\n%s", text)
	}
	backendError := g.awaitJournal("web-listen", 5*time.Second, func(entry audit.Entry) bool {
		return entry.Kind == audit.KindBackendError
	})
	if backendError.Reason != "backend_stream_failed" {
		t.Fatalf("backend_error = %+v, want the stream failure", backendError)
	}
	if agent := g.agent("web-listen"); agent.Status != "failed" {
		t.Fatalf("the agent shows %q after its stream failed, want failed", agent.Status)
	}
}

// A prompt about an MCP tool call is shown to every client, viewers included,
// with the call's arguments redacted. The call's badge is the gateway's own
// account of what the agent printed, and it carried the arguments as they were
// printed: an agent handing a token to a tool had the token in every prompt
// the gateway sent, a viewer's among them. The agent's screen itself is shown
// as the agent painted it, and is not what this pins.
func TestTheWebGatewayShowsAToolCallOnlyAsItMayBeShown(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-tool", mode: webAgentToolCall, adapter: "generic"}},
	})
	prompt := g.awaitPending("web-tool", 30*time.Second)
	if prompt.ToolCall == nil {
		t.Fatalf("the prompt carries no tool call: %+v", prompt)
	}
	if prompt.ToolCall.Server != "github" || prompt.ToolCall.Tool != "create_issue" {
		t.Fatalf("tool call = %+v, want github/create_issue", *prompt.ToolCall)
	}
	values := map[string]string{}
	for _, param := range prompt.ToolCall.Params {
		values[param.Name] = param.Value
	}
	if values["repo"] != "Hocsman/Relayer" || values["auth"] != "[REDACTED]" {
		t.Fatalf("parameters = %v, want the repository shown and the token redacted", values)
	}
	shown := false
	for _, frame := range g.broadcast(eventSemantic) {
		if view, ok := frame.payload.(SupervisionEvent); ok && view.ID == prompt.ID {
			shown = shown || view.ToolCall != nil
		}
	}
	if !shown {
		t.Fatal("the prompt was never shown with its tool call")
	}
	if text := g.broadcastText(); strings.Contains(text, webAgentToken) {
		t.Fatalf("the tool call's token reached the clients:\n%s", text)
	}
	if strings.Contains(stateText(t, AppState{PendingEvents: g.ctrl.GetState().PendingEvents}), webAgentToken) {
		t.Fatal("the tool call's token is in the prompts every client reads")
	}
}

// Operators are notified by the core's rules: a prompt the policy answers is
// not a prompt waiting on anybody, and a notice names the agent as the
// operators configured it. The gateway notified every prompt as awaiting a
// decision, the policy's own included, under the agent's ID.
func TestTheWebGatewayNotifiesOnlyWhatWaitsOnAPerson(t *testing.T) {
	g := startWebRun(t, webRun{
		policy:        "allow",
		notifications: true,
		rules: "    - name: ask-the-asking-agent\n" +
			"      match:\n" +
			"        agent_ids: [web-ask]\n" +
			"      action: ask\n",
		agents: []webAgent{
			{id: "web-auto", name: "Automatic Agent", mode: webAgentAider, adapter: "aider"},
			{id: "web-ask", name: "Asking Agent", mode: webAgentAider, adapter: "aider"},
		},
	})
	g.awaitReport("web-auto", 40*time.Second)
	asked := g.awaitPending("web-ask", 30*time.Second)
	if asked.Evaluation.Automatic {
		t.Fatalf("the ruled prompt is automatic: %+v", asked.Evaluation)
	}
	// A notice follows the prompt it is about.
	notified := func() []NotificationEvent {
		var notices []NotificationEvent
		for _, frame := range g.broadcast(eventNotification) {
			if notice, ok := frame.payload.(NotificationEvent); ok {
				notices = append(notices, notice)
			}
		}
		return notices
	}
	for deadline := time.Now().Add(5 * time.Second); len(notified()) == 0 && time.Now().Before(deadline); {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(200 * time.Millisecond)
	notices := notified()
	if len(notices) != 1 {
		t.Fatalf("notifications = %+v, want one, for the prompt that waits on a person", notices)
	}
	notice := notices[0]
	if notice.SessionID != "web-ask" || notice.AgentName != "Asking Agent" || notice.Kind != "pending_decision" {
		t.Fatalf("notification = %+v, want the Asking Agent's prompt, by its name", notice)
	}
	if notice.Body != asked.Summary {
		t.Fatalf("notification body = %q, want the summary the prompt is shown with, %q", notice.Body, asked.Summary)
	}
	if err := g.ctrl.SubmitAutomaticDecision(asked.RunID, asked.SessionID, asked.ID, "deny", webOperator); err != nil {
		t.Fatalf("the operator's deny was refused: %v", err)
	}
	g.awaitReport("web-ask", 20*time.Second)
}

// Taking the terminal keeps the policy from answering what its holder may be
// answering by typing, and releasing it gives nothing back to the policy. With
// the policy answering on its own, a holder's keystrokes and the policy's
// answer to the same question would both reach the agent: raw keystrokes
// never resolve the runtime's pending prompt.
func TestTakingAWebTerminalKeepsThePolicyFromAnsweringIt(t *testing.T) {
	gate, openGate := webAgentGate(t)
	g := startWebRun(t, webRun{
		policy: "allow",
		agents: []webAgent{{id: "web-held", mode: webAgentAider, adapter: "aider", env: gate}},
	})
	g.awaitScreen("web-held", "agent ready", 30*time.Second)
	g.ctrl.RegisterPresence("conn-alice", "alice", string(RoleOperator))
	if _, err := g.ctrl.TakeControl("web-held", "conn-alice", "alice"); err != nil {
		t.Fatalf("TakeControl: %v", err)
	}
	// The agent asks once the terminal is held.
	openGate()
	prompt := g.awaitPending("web-held", 30*time.Second)
	if prompt.Evaluation.Automatic || prompt.Evaluation.Reason != supervise.ReasonOperatorAttached {
		t.Fatalf("the prompt of a held terminal is %+v, want it asked for operator_attached", prompt.Evaluation)
	}
	g.assertNoAnswerFor("web-held", time.Second)
	if _, err := g.ctrl.ReleaseControl(g.ctrl.GetState().RunID, "web-held", "conn-alice", "alice"); err != nil {
		t.Fatalf("ReleaseControl: %v", err)
	}
	g.assertNoAnswerFor("web-held", time.Second)
	if prompts := g.pending("web-held"); len(prompts) != 1 || prompts[0].Evaluation.Automatic {
		t.Fatalf("after the release the prompts are %+v, want the prompt still asked", prompts)
	}
	evaluated := false
	for _, entry := range g.sessionJournal("web-held") {
		evaluated = evaluated || (entry.Kind == audit.KindPolicyEvaluated && entry.Reason == supervise.ReasonOperatorAttached)
	}
	if !evaluated {
		t.Fatalf("the journal does not say why the prompt was asked:\n%s", journalTrace(g.sessionJournal("web-held")))
	}
	if err := g.ctrl.SubmitAutomaticDecision(prompt.RunID, prompt.SessionID, prompt.ID, "allow", webOperator); err != nil {
		t.Fatalf("the operator's allow was refused: %v", err)
	}
	screen := g.awaitReport("web-held", 20*time.Second)
	if _, count, extras := answers(screen); count != 1 || extras != 0 {
		t.Fatalf("the agent received %d answer(s) and %d extra one(s), want one; screen:\n%s", count, extras, screen)
	}
}

// The gateway fails closed on its journal: once an entry is refused, nothing
// more is written to any agent, every client is told, and the state every
// client reads says so. The gateway ignored every journal error and went on
// answering, so decisions reached agents with no trace of them anywhere.
func TestAWebJournalThatFailsStopsEveryAnswer(t *testing.T) {
	fault, wrap := newFaultEngine()
	g := startWebRun(t, webRun{
		engineWrap: wrap,
		agents:     []webAgent{{id: "web-generic", mode: webAgentGeneric, adapter: "generic"}},
	})
	prompt := g.awaitPending("web-generic", 30*time.Second)
	fault.set(func(f *faultEngine) { f.auditFails = true })

	err := g.ctrl.SubmitDecision(prompt.RunID, prompt.SessionID, prompt.ID, "nope", webOperator)
	if !errors.Is(err, supervise.ErrAuditUnavailable) {
		t.Fatalf("an answer with a failing journal returned %v, want the journal's refusal", err)
	}
	g.assertNoAnswerFor("web-generic", time.Second)
	state := g.ctrl.GetState()
	if state.Audit.Status != "failed" {
		t.Fatalf("the audit status every client reads is %q, want failed", state.Audit.Status)
	}
	for _, agent := range state.Agents {
		if agent.SessionID == "web-generic" && !agent.InputFrozen {
			t.Fatalf("the agent takes input with a failed journal: %+v", agent)
		}
	}
	told := false
	for _, frame := range g.broadcast(eventStatus) {
		if status, ok := frame.payload.(StatusEvent); ok && status.Scope == "audit" && status.Status == "failed" {
			told = true
		}
	}
	if !told {
		t.Fatal("no client was told the journal failed")
	}
	if err := g.ctrl.SubmitDecision(prompt.RunID, prompt.SessionID, prompt.ID, "nope", webOperator); err == nil {
		t.Fatal("a second answer went through a failed journal")
	}
}

// A write whose outcome is unknown freezes its session: part of the answer may
// have reached the agent, and a second answer would follow it. The gateway
// returned the error and left the session writable, and every client kept the
// card of a prompt it had already dropped.
func TestAnUncertainWebWriteFreezesTheSession(t *testing.T) {
	fault, wrap := newFaultEngine()
	g := startWebRun(t, webRun{
		engineWrap: wrap,
		agents:     []webAgent{{id: "web-generic", mode: webAgentGeneric, adapter: "generic"}},
	})
	prompt := g.awaitPending("web-generic", 30*time.Second)
	fault.set(func(f *faultEngine) { f.applyErr = errors.New("the terminal did not take the write in time") })

	err := g.ctrl.SubmitDecision(prompt.RunID, prompt.SessionID, prompt.ID, "nope", webOperator)
	if !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("an uncertain write returned %v, want ErrDeliveryUncertain", err)
	}
	if agent := g.agent("web-generic"); !agent.InputFrozen {
		t.Fatalf("the session of an uncertain write takes input: %+v", agent)
	}
	prompts := g.pending("web-generic")
	if len(prompts) != 1 || prompts[0].DeliveryStatus != "uncertain" {
		t.Fatalf("prompts = %+v, want the prompt shown uncertain", prompts)
	}
	shown := false
	for _, frame := range g.broadcast(eventError) {
		if failure, ok := frame.payload.(SafeErrorEvent); ok && failure.Code == "delivery_uncertain" {
			shown = true
		}
	}
	if !shown {
		t.Fatal("no client was told the delivery is indeterminate")
	}
	fault.set(func(f *faultEngine) { f.applyErr = nil })
	if err := g.ctrl.SubmitDecision(prompt.RunID, prompt.SessionID, prompt.ID, "again", webOperator); !errors.Is(err, supervise.ErrDeliveryUncertain) {
		t.Fatalf("a second answer to a frozen session returned %v, want ErrDeliveryUncertain", err)
	}
	g.assertNoAnswerFor("web-generic", 500*time.Millisecond)
}

// A session takes one answer at a time. Every RPC runs on its own goroutine,
// and the gateway wrote the answers to two prompts of one session into its
// terminal at once, in either order.
func TestTheWebGatewayWritesOneAnswerAtATimeToASession(t *testing.T) {
	fault, wrap := newFaultEngine()
	g := startWebRun(t, webRun{
		engineWrap: wrap,
		agents:     []webAgent{{id: "web-generic", mode: webAgentGeneric, adapter: "generic"}},
	})
	first := g.awaitPending("web-generic", 30*time.Second)
	release := make(chan struct{})
	fault.set(func(f *faultEngine) { f.holdApply = release })
	firstDone := make(chan error, 1)
	go func() {
		firstDone <- g.ctrl.SubmitDecision(first.RunID, first.SessionID, first.ID, "first answer", webOperator)
	}()
	select {
	case <-fault.applyStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the first answer's write never started")
	}

	// A second question of the same session, while the first answer is
	// being written.
	second := livePrompt(g.ctrl, "web-generic", "second-question", time.Now().UTC())
	if !promptOffered(g.ctrl, second.ID) {
		t.Fatal("the second question was not taken in")
	}
	err := g.ctrl.SubmitDecision(first.RunID, "web-generic", second.ID, "second answer", webOperator)
	if !errors.Is(err, supervise.ErrDecisionInFlight) {
		t.Fatalf("an answer while another is written returned %v, want ErrDecisionInFlight", err)
	}
	close(release)
	if err := <-firstDone; err != nil {
		t.Fatalf("the first answer failed: %v", err)
	}
	if fault.overlap() {
		t.Fatal("two answers were written to one session at once")
	}
	screen := g.awaitReport("web-generic", 20*time.Second)
	if answer, count, extras := answers(screen); count != 1 || extras != 0 || !strings.Contains(answer, "first answer") {
		t.Fatalf("the agent received %s (%d answers, %d extra), want the first answer alone; screen:\n%s", answer, count, extras, screen)
	}
}

// Stopping and starting an agent go through the core, which shows the agent
// stopping and starting while the operation runs. The gateway showed nothing
// until the operation was over, and every client offered Stop and Start on an
// agent already on its way out or in.
func TestAWebAgentIsShownStoppingAndStarting(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()
	if err := g.ctrl.StopSession(runID, "web-listen"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	g.awaitAgent("web-listen", 10*time.Second, func(agent AgentState) bool { return !agent.Running })
	if err := g.ctrl.StartSession(runID, "web-listen"); err != nil {
		t.Fatalf("StartSession: %v", err)
	}
	g.awaitAgent("web-listen", 10*time.Second, func(agent AgentState) bool { return agent.Running && agent.Status == "running" })

	var statuses []string
	for _, frame := range g.broadcast(eventStatus) {
		if status, ok := frame.payload.(StatusEvent); ok && status.SessionID == "web-listen" {
			statuses = append(statuses, status.Status)
		}
	}
	trace := strings.Join(statuses, " ")
	stopping := strings.Index(trace, "stopping")
	starting := strings.Index(trace, "starting")
	if stopping < 0 || starting < stopping || !strings.Contains(trace[starting:], "running") {
		t.Fatalf("statuses shown = %v, want stopping, then starting, then running", statuses)
	}
}

// A stop that failed freezes the session: whether the process still runs is
// unknown, and nothing more is written to it. The gateway returned the
// backend's own error, which may name a path, and left the session writable.
func TestAWebStopThatFailedFreezesTheSession(t *testing.T) {
	fault, wrap := newFaultEngine()
	g := startWebRun(t, webRun{
		engineWrap: wrap,
		agents:     []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	const leaked = `stop C:\agents\secret-project: access denied`
	fault.set(func(f *faultEngine) { f.stopErr = errors.New(leaked) })

	err := g.ctrl.StopSession(g.runID(), "web-listen")
	if err == nil || strings.Contains(err.Error(), "secret-project") {
		t.Fatalf("a failed stop returned %v, want a fixed message", err)
	}
	if agent := g.agent("web-listen"); !agent.InputFrozen {
		t.Fatalf("the session of a failed stop takes input: %+v", agent)
	}
	shown := false
	for _, frame := range g.broadcast(eventError) {
		if failure, ok := frame.payload.(SafeErrorEvent); ok && failure.Code == "stop_failed" && failure.SessionID == "web-listen" {
			shown = true
		}
	}
	if !shown {
		t.Fatal("no client was told the stop failed")
	}
	if strings.Contains(g.broadcastText(), "secret-project") {
		t.Fatal("the backend's own error reached the clients")
	}
}
