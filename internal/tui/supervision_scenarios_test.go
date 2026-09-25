package tui

import (
	"fmt"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/policy"
)

// The supervision scenarios pin, for each thing an agent or an operator can
// do, the journal it leaves, what reaches the agent and what the screen
// shows. The rows are those the Model writes today. The TUI is about to
// supervise through the shared core instead, and every row that changes then
// carries a comment naming the rule of the core that changes it
// ("// core: file:line", in internal/supervise unless another package is
// named); a row without one must stay as it is. A write the core does not
// make, and a prompt it leaves waiting, are commented too; a tag whose only
// change is its wording is not.

// row is one journal entry, reduced to what a scenario decides.
type row struct {
	Kind     audit.Kind
	Outcome  audit.Outcome
	Reason   string
	Decision audit.Decision
	By       audit.DecisionBy
	EventID  string
	Rule     string
}

func rowOf(entry audit.Entry) row {
	return row{
		Kind:     entry.Kind,
		Outcome:  entry.Outcome,
		Reason:   entry.Reason,
		Decision: entry.Decision,
		By:       entry.DecisionBy,
		EventID:  entry.EventID,
		Rule:     entry.Rule,
	}
}

func (r row) String() string {
	return fmt.Sprintf("%s %s/%s decision=%q by=%s event=%q rule=%q",
		r.Kind, r.Outcome, r.Reason, r.Decision, r.By, r.EventID, r.Rule)
}

// write is what may have reached an agent: a decision the transport was
// handed once the prompt's own adapter encoded it, or a line. A write refused
// before any byte left, because the prompt was gone or its adapter has no
// bytes for the answer, is none.
type write struct {
	Session  string
	EventID  string
	Decision adapters.Decision
	Value    string
	Line     bool
}

// shown is what the operator sees of the agent once the scenario ends.
type shown struct {
	// Awaiting are the prompts that wait on a person, in the order they are
	// answered, and Target the one the input field answers.
	Awaiting []string
	Target   string
	// Status and Tag are the agent's status and policy tag as its pane's
	// title shows them.
	Status string
	Tag    string
}

type stepKind uint8

const (
	// stepRaise: the agent raises event, and its output processor holds it.
	stepRaise stepKind = iota + 1
	// stepWithdraw: the agent takes event back off its screen.
	stepWithdraw
	// stepAnswer: the operator types text and presses Enter.
	stepAnswer
	// stepAllow and stepDeny: the operator presses F2 or F3.
	stepAllow
	stepDeny
	// stepLine: the operator presses i on the agent's pane, types text and
	// presses Enter.
	stepLine
	// stepStop and stepRestart: the operator presses x twice, or r twice, on
	// the agent's pane.
	stepStop
	stepRestart
	// stepExit: the process that raised event's exit arrives. stale says that
	// a replacement already runs, which only the runtime can tell.
	stepExit
	// stepFailNextWrite: the next write fails after its bytes may have
	// reached the agent.
	stepFailNextWrite
	// stepStaleNextWrite: the agent moves on before the next answer is
	// written, which then finds no prompt to answer.
	stepStaleNextWrite
)

type step struct {
	kind      stepKind
	event     adapters.Event
	text      string
	sessionID string
	stale     bool
}

func raise(event adapters.Event) step    { return step{kind: stepRaise, event: event} }
func withdraw(event adapters.Event) step { return step{kind: stepWithdraw, event: event} }
func answer(text string) step            { return step{kind: stepAnswer, text: text} }
func pressAllow() step                   { return step{kind: stepAllow} }
func pressDeny() step                    { return step{kind: stepDeny} }
func failNextWrite() step                { return step{kind: stepFailNextWrite} }
func staleNextWrite() step               { return step{kind: stepStaleNextWrite} }

func sendLine(text string) step {
	return step{kind: stepLine, text: text, sessionID: scenarioAgent}
}

func confirmStop() step    { return step{kind: stepStop, sessionID: scenarioAgent} }
func confirmRestart() step { return step{kind: stepRestart, sessionID: scenarioAgent} }

func exits(event adapters.Event) step     { return step{kind: stepExit, event: event} }
func exitsLate(event adapters.Event) step { return step{kind: stepExit, event: event, stale: true} }

// scenarioDriver runs a scenario against one implementation of supervision.
type scenarioDriver interface {
	do(step)
	journal() []row
	writes() []write
	shown(sessionID string) shown
}

type scenario struct {
	name        string
	adapter     string
	policy      policy.Config
	steps       []step
	wantJournal []row
	wantWrites  []write
	wantShown   shown
}

const scenarioAgent = "agent-a"

var scenarioSequence atomic.Uint64

// scenarioPrompt is a low-risk confirmation raised by the scenario's agent.
// An Aider prompt carries the interaction its adapter encodes allow and deny
// for; the generic adapter encodes neither.
func scenarioPrompt(id, adapter string) adapters.Event {
	event := adapters.Event{
		ID:        id,
		Signature: "sig-" + id,
		Sequence:  scenarioSequence.Add(1),
		SessionID: scenarioAgent,
		AgentID:   scenarioAgent,
		Adapter:   adapter,
		Type:      adapters.EventConfirmation,
		Summary:   "Apply the edits to main.go?",
		Risk:      adapters.RiskLow,
		Timestamp: time.Now().UTC(),
		Metadata:  map[string]string{"pattern": "confirmation"},
	}
	if adapter == adapters.AiderID {
		event.Metadata = map[string]string{"interaction": "apply_changes"}
	}
	return event
}

func withSignature(event adapters.Event, signature string) adapters.Event {
	event.Signature = signature
	return event
}

func withSummary(event adapters.Event, summary string) adapters.Event {
	event.Summary = summary
	return event
}

func passwordPrompt(id string) adapters.Event {
	event := scenarioPrompt(id, adapters.GenericID)
	event.Type = adapters.EventCredential
	event.Sensitive = true
	event.Risk = adapters.RiskHigh
	event.Summary = "Password:"
	return event
}

func scenarioExit(id, adapter string, code int, failed bool) adapters.Event {
	event := adapters.NewProcessExitEvent(scenarioAgent, scenarioAgent, adapter, scenarioSequence.Add(1), &code, failed)
	event.ID = id
	return event
}

func detectedRow(id string) row {
	return row{Kind: audit.KindEventDetected, Outcome: audit.OutcomeDetected, Reason: "event_detected", By: audit.DecisionBySystem, EventID: id}
}

func evaluatedRow(outcome audit.Outcome, reason string, decision audit.Decision, id, rule string) row {
	return row{Kind: audit.KindPolicyEvaluated, Outcome: outcome, Reason: reason, Decision: decision, By: audit.DecisionByPolicy, EventID: id, Rule: rule}
}

func decisionRow(decision audit.Decision, by audit.DecisionBy, id, rule string) row {
	return row{Kind: audit.KindDecision, Outcome: audit.OutcomeInFlight, Reason: "decision_selected", Decision: decision, By: by, EventID: id, Rule: rule}
}

func deliveryRow(outcome audit.Outcome, reason string, decision audit.Decision, by audit.DecisionBy, id string) row {
	return row{Kind: audit.KindDelivery, Outcome: outcome, Reason: reason, Decision: decision, By: by, EventID: id}
}

func withdrawnRow(reason, id string) row {
	return row{Kind: audit.KindEventWithdrawn, Outcome: audit.OutcomeCancelled, Reason: reason, By: audit.DecisionBySystem, EventID: id}
}

func finishedRow(outcome audit.Outcome, reason, id string) row {
	return row{Kind: audit.KindSessionFinished, Outcome: outcome, Reason: reason, By: audit.DecisionBySystem, EventID: id}
}

func lineRow(outcome audit.Outcome, reason string) row {
	return row{Kind: audit.KindOperatorInput, Outcome: outcome, Reason: reason, By: audit.DecisionByHuman}
}

func allowAiderEdits() policy.Rule {
	return policy.Rule{
		Name:   "allow-edits",
		Match:  policy.Match{EventTypes: []adapters.EventType{adapters.EventConfirmation}},
		Action: policy.ActionAllow,
	}
}

func denyConfirmations() policy.Rule {
	return policy.Rule{
		Name:   "deny-confirmations",
		Match:  policy.Match{EventTypes: []adapters.EventType{adapters.EventConfirmation}},
		Action: policy.ActionDeny,
	}
}

func supervisionScenarios() []scenario {
	var (
		askByDefault = policy.Config{DefaultAction: policy.ActionAsk}
		allowEdits   = policy.Config{DefaultAction: policy.ActionAsk, Rules: []policy.Rule{allowAiderEdits()}}
		denyAll      = policy.Config{DefaultAction: policy.ActionAsk, Rules: []policy.Rule{denyConfirmations()}}
	)
	s01 := scenarioPrompt("s01-p1", adapters.AiderID)
	s02 := scenarioPrompt("s02-p1", adapters.GenericID)
	s03 := scenarioPrompt("s03-p1", adapters.AiderID)
	s04 := scenarioPrompt("s04-p1", adapters.GenericID)
	s05 := scenarioPrompt("s05-p1", adapters.GenericID)
	s06 := scenarioPrompt("s06-p1", adapters.AiderID)
	s07 := scenarioPrompt("s07-p1", adapters.GenericID)
	s08 := scenarioPrompt("s08-p1", adapters.GenericID)
	s09 := scenarioPrompt("s09-p1", adapters.GenericID)
	s10a := withSignature(scenarioPrompt("s10-p1", adapters.AiderID), "sig-s10")
	s10b := withSignature(scenarioPrompt("s10-p2", adapters.AiderID), "sig-s10")
	s11a := scenarioPrompt("s11-p1", adapters.AiderID)
	s11b := scenarioPrompt("s11-p2", adapters.AiderID)
	s11c := scenarioPrompt("s11-p3", adapters.AiderID)
	s11d := scenarioPrompt("s11-p4", adapters.AiderID)
	s12 := withSummary(scenarioPrompt("s12-p1", adapters.AiderID), "Run rm -rf build/ to start clean?")
	s13 := withSummary(scenarioPrompt("s13-p1", adapters.AiderID), "Run rm -rf build/ to start clean?")
	s14 := passwordPrompt("s14-p1")
	s15 := scenarioPrompt("s15-p1", adapters.GenericID)
	s15other := scenarioPrompt("s15-other", adapters.GenericID)
	s16 := scenarioPrompt("s16-p1", adapters.GenericID)
	s17exit := scenarioExit("s17-exit", adapters.GenericID, 0, false)
	s17 := scenarioPrompt("s17-p1", adapters.GenericID)
	s19 := scenarioPrompt("s19-p1", adapters.GenericID)
	s19exit := scenarioExit("s19-exit", adapters.GenericID, 3, true)
	s20 := scenarioPrompt("s20-p1", adapters.GenericID)
	s20exit := scenarioExit("s20-exit", adapters.GenericID, 0, false)
	s21a := scenarioPrompt("s21-p1", adapters.AiderID)
	s21b := scenarioPrompt("s21-p2", adapters.AiderID)
	s22a := scenarioPrompt("s22-p1", adapters.AiderID)
	s22b := scenarioPrompt("s22-p2", adapters.AiderID)
	s22c := scenarioPrompt("s22-p3", adapters.AiderID)

	return []scenario{
		{
			name:    "S01 an Aider prompt a rule allows is answered by the policy",
			adapter: adapters.AiderID,
			policy:  allowEdits,
			steps:   []step{raise(s01)},
			wantJournal: []row{
				detectedRow("s01-p1"),
				evaluatedRow(audit.OutcomeInFlight, "rule_match", audit.DecisionAllow, "s01-p1", "allow-edits"),
				// core: the decision names its rule, decide.go:140 and audit.go:118-122.
				decisionRow(audit.DecisionAllow, audit.DecisionByPolicy, "s01-p1", ""),
				// core: applied/delivery_applied, decide.go:164.
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionAllow, audit.DecisionByPolicy, "s01-p1"),
			},
			wantWrites: []write{{Session: scenarioAgent, EventID: "s01-p1", Decision: adapters.DecisionAllow}},
			wantShown:  shown{Status: "RUNNING", Tag: "AUTO APPLIED"},
		},
		{
			name:    "S02 a typed answer to a generic prompt is written as typed",
			adapter: adapters.GenericID,
			policy:  askByDefault,
			steps:   []step{raise(s02), answer("yes")},
			wantJournal: []row{
				detectedRow("s02-p1"),
				evaluatedRow(audit.OutcomeAsk, "default_action", audit.DecisionAsk, "s02-p1", ""),
				decisionRow(audit.DecisionAsk, audit.DecisionByHuman, "s02-p1", ""),
				// core: applied/delivery_applied, decide.go:633.
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionAsk, audit.DecisionByHuman, "s02-p1"),
			},
			wantWrites: []write{{Session: scenarioAgent, EventID: "s02-p1", Decision: adapters.DecisionManual, Value: "yes"}},
			wantShown:  shown{Status: "RUNNING", Tag: "ASK APPLIED"},
		},
		{
			name:    "S03 F3 on an Aider prompt sends the adapter's deny",
			adapter: adapters.AiderID,
			policy:  askByDefault,
			steps:   []step{raise(s03), pressDeny()},
			wantJournal: []row{
				detectedRow("s03-p1"),
				evaluatedRow(audit.OutcomeAsk, "default_action", audit.DecisionAsk, "s03-p1", ""),
				decisionRow(audit.DecisionDeny, audit.DecisionByHuman, "s03-p1", ""),
				// core: applied/delivery_applied, decide.go:633.
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionDeny, audit.DecisionByHuman, "s03-p1"),
			},
			wantWrites: []write{{Session: scenarioAgent, EventID: "s03-p1", Decision: adapters.DecisionDeny}},
			wantShown:  shown{Status: "RUNNING", Tag: "ASK APPLIED"},
		},
		{
			name:    "S04 F2 on a generic prompt, whose adapter encodes no allow",
			adapter: adapters.GenericID,
			policy:  askByDefault,
			steps:   []step{raise(s04), pressAllow()},
			wantJournal: []row{
				detectedRow("s04-p1"),
				evaluatedRow(audit.OutcomeAsk, "default_action", audit.DecisionAsk, "s04-p1", ""),
				// core: the prompt does not offer allow, so F2 is refused and
				// neither row below is written, decide.go:474-484.
				decisionRow(audit.DecisionAllow, audit.DecisionByHuman, "s04-p1", ""),
				deliveryRow(audit.OutcomeFailed, "delivery_failed", audit.DecisionAllow, audit.DecisionByHuman, "s04-p1"),
			},
			wantShown: shown{Awaiting: []string{"s04-p1"}, Target: "s04-p1", Status: "ACTION REQUIRED", Tag: "ASK REQUIRED"},
		},
		{
			name:    "S05 a typed answer whose write fails in transport",
			adapter: adapters.GenericID,
			policy:  askByDefault,
			steps:   []step{raise(s05), failNextWrite(), answer("yes"), answer("yes")},
			wantJournal: []row{
				detectedRow("s05-p1"),
				evaluatedRow(audit.OutcomeAsk, "default_action", audit.DecisionAsk, "s05-p1", ""),
				decisionRow(audit.DecisionAsk, audit.DecisionByHuman, "s05-p1", ""),
				// core: fallback_delivery_uncertain/delivery_uncertain, and the
				// session freezes, decide.go:626-630.
				deliveryRow(audit.OutcomeFailed, "delivery_failed", audit.DecisionAsk, audit.DecisionByHuman, "s05-p1"),
				// core: the second answer is refused on the frozen session and
				// neither row below is written, decide.go:569-571.
				decisionRow(audit.DecisionAsk, audit.DecisionByHuman, "s05-p1", ""),
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionAsk, audit.DecisionByHuman, "s05-p1"),
			},
			// core: the second write never happens.
			wantWrites: []write{
				{Session: scenarioAgent, EventID: "s05-p1", Decision: adapters.DecisionManual, Value: "yes"},
				{Session: scenarioAgent, EventID: "s05-p1", Decision: adapters.DecisionManual, Value: "yes"},
			},
			// core: the session is frozen and the prompt shown uncertain, so
			// nothing waits on the operator and the agent shows DELIVERY
			// UNCERTAIN, tagged DELIVERY UNCERTAIN, decide.go:353-365.
			wantShown: shown{Status: "RUNNING", Tag: "ASK APPLIED"},
		},
		{
			name:    "S06 an automatic answer that finds the prompt gone",
			adapter: adapters.AiderID,
			policy:  allowEdits,
			steps:   []step{staleNextWrite(), raise(s06)},
			wantJournal: []row{
				detectedRow("s06-p1"),
				evaluatedRow(audit.OutcomeInFlight, "rule_match", audit.DecisionAllow, "s06-p1", "allow-edits"),
				// core: the decision names its rule, decide.go:140.
				decisionRow(audit.DecisionAllow, audit.DecisionByPolicy, "s06-p1", ""),
				// core: reason fallback_stale, decide.go:181-189.
				deliveryRow(audit.OutcomeFallbackStale, "event_stale", audit.DecisionAllow, audit.DecisionByPolicy, "s06-p1"),
			},
			wantShown: shown{Status: "RUNNING", Tag: "AUTO NOT APPLIED"},
		},
		{
			name:    "S07 an allow on a generic prompt, whose adapter encodes none",
			adapter: adapters.GenericID,
			policy:  policy.Config{DefaultAction: policy.ActionAllow},
			steps:   []step{raise(s07)},
			wantJournal: []row{
				detectedRow("s07-p1"),
				evaluatedRow(audit.OutcomeInFlight, "default_action", audit.DecisionAllow, "s07-p1", ""),
				decisionRow(audit.DecisionAllow, audit.DecisionByPolicy, "s07-p1", ""),
				// core: reason fallback_unsupported, decide.go:172-179.
				deliveryRow(audit.OutcomeFallbackUnsupported, "decision_unsupported", audit.DecisionAllow, audit.DecisionByPolicy, "s07-p1"),
			},
			wantShown: shown{Awaiting: []string{"s07-p1"}, Target: "s07-p1", Status: "ACTION REQUIRED", Tag: "AUTO → ASK"},
		},
		{
			name:    "S08 a deny on a generic prompt falls back, then the operator types an answer",
			adapter: adapters.GenericID,
			policy:  denyAll,
			steps:   []step{raise(s08), answer("n")},
			wantJournal: []row{
				detectedRow("s08-p1"),
				evaluatedRow(audit.OutcomeInFlight, "rule_match", audit.DecisionDeny, "s08-p1", "deny-confirmations"),
				// core: the decision names its rule, decide.go:140.
				decisionRow(audit.DecisionDeny, audit.DecisionByPolicy, "s08-p1", ""),
				// core: reason fallback_unsupported, decide.go:172-179.
				deliveryRow(audit.OutcomeFallbackUnsupported, "decision_unsupported", audit.DecisionDeny, audit.DecisionByPolicy, "s08-p1"),
				// core: reason typed_over_policy_deny, decide.go:544-560 and
				// 596-598.
				decisionRow(audit.DecisionAsk, audit.DecisionByHuman, "s08-p1", ""),
				// core: applied/delivery_applied, decide.go:633.
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionAsk, audit.DecisionByHuman, "s08-p1"),
			},
			wantWrites: []write{{Session: scenarioAgent, EventID: "s08-p1", Decision: adapters.DecisionManual, Value: "n"}},
			wantShown:  shown{Status: "RUNNING", Tag: "ASK APPLIED"},
		},
		{
			name:    "S09 a prompt the policy asks about waits on the operator",
			adapter: adapters.GenericID,
			policy:  askByDefault,
			steps:   []step{raise(s09)},
			wantJournal: []row{
				detectedRow("s09-p1"),
				evaluatedRow(audit.OutcomeAsk, "default_action", audit.DecisionAsk, "s09-p1", ""),
			},
			wantShown: shown{Awaiting: []string{"s09-p1"}, Target: "s09-p1", Status: "ACTION REQUIRED", Tag: "ASK"},
		},
		{
			name:    "S10 a prompt with the Signature of one just answered",
			adapter: adapters.AiderID,
			policy:  allowEdits,
			steps:   []step{raise(s10a), raise(s10b)},
			wantJournal: []row{
				detectedRow("s10-p1"),
				evaluatedRow(audit.OutcomeInFlight, "rule_match", audit.DecisionAllow, "s10-p1", "allow-edits"),
				// core: the decision names its rule, decide.go:140.
				decisionRow(audit.DecisionAllow, audit.DecisionByPolicy, "s10-p1", ""),
				// core: applied/delivery_applied, decide.go:164.
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionAllow, audit.DecisionByPolicy, "s10-p1"),
				detectedRow("s10-p2"),
				// core: ask/repeat_after_delivery, decision ask, the rule kept,
				// and neither row after it is written, repeat.go:27-39 and
				// ingest.go:161-164.
				evaluatedRow(audit.OutcomeInFlight, "rule_match", audit.DecisionAllow, "s10-p2", "allow-edits"),
				decisionRow(audit.DecisionAllow, audit.DecisionByPolicy, "s10-p2", ""),
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionAllow, audit.DecisionByPolicy, "s10-p2"),
			},
			// core: only the first prompt is answered.
			wantWrites: []write{
				{Session: scenarioAgent, EventID: "s10-p1", Decision: adapters.DecisionAllow},
				{Session: scenarioAgent, EventID: "s10-p2", Decision: adapters.DecisionAllow},
			},
			// core: the second prompt waits on the operator, and the agent
			// shows ACTION REQUIRED, tagged REPEAT • ASK.
			wantShown: shown{Status: "RUNNING", Tag: "AUTO APPLIED"},
		},
		{
			name:    "S11 a limit of two automatic decisions, then a line",
			adapter: adapters.AiderID,
			policy:  policy.Config{DefaultAction: policy.ActionAsk, MaxConsecutiveAutoDecisions: 2, Rules: []policy.Rule{allowAiderEdits()}},
			steps:   []step{raise(s11a), raise(s11b), raise(s11c), withdraw(s11c), sendLine("run the tests"), raise(s11d)},
			wantJournal: []row{
				detectedRow("s11-p1"),
				evaluatedRow(audit.OutcomeInFlight, "rule_match", audit.DecisionAllow, "s11-p1", "allow-edits"),
				// core: the decision names its rule, decide.go:140.
				decisionRow(audit.DecisionAllow, audit.DecisionByPolicy, "s11-p1", ""),
				// core: applied/delivery_applied, decide.go:164.
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionAllow, audit.DecisionByPolicy, "s11-p1"),
				detectedRow("s11-p2"),
				evaluatedRow(audit.OutcomeInFlight, "rule_match", audit.DecisionAllow, "s11-p2", "allow-edits"),
				// core: the decision names its rule, decide.go:140.
				decisionRow(audit.DecisionAllow, audit.DecisionByPolicy, "s11-p2", ""),
				// core: applied/delivery_applied, decide.go:164.
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionAllow, audit.DecisionByPolicy, "s11-p2"),
				detectedRow("s11-p3"),
				// core: reason consecutive_auto_limit, safety.go:137.
				evaluatedRow(audit.OutcomeAsk, "unknown", audit.DecisionAsk, "s11-p3", "allow-edits"),
				withdrawnRow("agent_withdrew_occurrence", "s11-p3"),
				lineRow(audit.OutcomeInFlight, "operator_input_started"),
				lineRow(audit.OutcomeApplied, "operator_input_applied"),
				detectedRow("s11-p4"),
				// core: a line does not reset the streak, which counts only the
				// journal's decisions (internal/app/desktop_runtime.go:624-637):
				// ask/consecutive_auto_limit, decision ask, and neither row
				// after it is written.
				evaluatedRow(audit.OutcomeInFlight, "rule_match", audit.DecisionAllow, "s11-p4", "allow-edits"),
				decisionRow(audit.DecisionAllow, audit.DecisionByPolicy, "s11-p4", ""),
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionAllow, audit.DecisionByPolicy, "s11-p4"),
			},
			// core: the fourth prompt is not answered.
			wantWrites: []write{
				{Session: scenarioAgent, EventID: "s11-p1", Decision: adapters.DecisionAllow},
				{Session: scenarioAgent, EventID: "s11-p2", Decision: adapters.DecisionAllow},
				{Session: scenarioAgent, Line: true, Value: "run the tests"},
				{Session: scenarioAgent, EventID: "s11-p4", Decision: adapters.DecisionAllow},
			},
			// core: the fourth prompt waits on the operator, and the agent
			// shows ACTION REQUIRED, tagged LIMIT • ASK.
			wantShown: shown{Status: "RUNNING", Tag: "AUTO APPLIED"},
		},
		{
			name:    "S12 a destructive command a rule allows is held by the guardrail",
			adapter: adapters.AiderID,
			policy:  policy.Config{DefaultAction: policy.ActionAsk, Guardrails: policy.GuardrailsConfig{BlockDestructive: true}, Rules: []policy.Rule{allowAiderEdits()}},
			steps:   []step{raise(s12)},
			wantJournal: []row{
				detectedRow("s12-p1"),
				// core: reason destructive_command_blocked, safety.go:136.
				evaluatedRow(audit.OutcomeAsk, "unknown", audit.DecisionAsk, "s12-p1", "allow-edits"),
			},
			wantShown: shown{Awaiting: []string{"s12-p1"}, Target: "s12-p1", Status: "ACTION REQUIRED", Tag: "GUARD • ASK"},
		},
		{
			name:    "S13 a dry run over a prompt the guardrail holds",
			adapter: adapters.AiderID,
			policy:  policy.Config{DefaultAction: policy.ActionAsk, DryRun: true, Guardrails: policy.GuardrailsConfig{BlockDestructive: true}, Rules: []policy.Rule{allowAiderEdits()}},
			steps:   []step{raise(s13)},
			wantJournal: []row{
				detectedRow("s13-p1"),
				// core: the guardrail keeps its reason,
				// destructive_command_blocked, with outcome dry_run, audit.go:79-83.
				evaluatedRow(audit.OutcomeDryRun, "dry_run", audit.DecisionAsk, "s13-p1", "allow-edits"),
			},
			// core: the prompt's reason is the guardrail's, which tags it
			// ahead of the dry run: GUARD • ASK.
			wantShown: shown{Awaiting: []string{"s13-p1"}, Target: "s13-p1", Status: "ACTION REQUIRED", Tag: "DRY RUN • ASK"},
		},
		{
			// A sensitive prompt's ID is left out of the journal: a generic
			// one is derived from the match, which may be the password.
			name:    "S14 a password prompt the policy would allow waits on the operator",
			adapter: adapters.GenericID,
			policy:  policy.Config{DefaultAction: policy.ActionAllow},
			steps:   []step{raise(s14)},
			wantJournal: []row{
				detectedRow(""),
				evaluatedRow(audit.OutcomeAsk, "sensitive_event", audit.DecisionAsk, "", ""),
			},
			wantShown: shown{Awaiting: []string{"s14-p1"}, Target: "s14-p1", Status: "ACTION REQUIRED", Tag: "ASK"},
		},
		{
			name:    "S15 a withdrawal of a prompt that was never shown",
			adapter: adapters.GenericID,
			policy:  askByDefault,
			steps:   []step{raise(s15), withdraw(s15other)},
			wantJournal: []row{
				detectedRow("s15-p1"),
				evaluatedRow(audit.OutcomeAsk, "default_action", audit.DecisionAsk, "s15-p1", ""),
				// core: withdrawnRow("agent_withdrew_occurrence", "s15-other")
				// is written here: every withdrawal is journaled, ingest.go:69.
			},
			wantShown: shown{Awaiting: []string{"s15-p1"}, Target: "s15-p1", Status: "ACTION REQUIRED", Tag: "ASK"},
		},
		{
			name:    "S16 a withdrawal of the prompt on offer",
			adapter: adapters.GenericID,
			policy:  askByDefault,
			steps:   []step{raise(s16), withdraw(s16)},
			wantJournal: []row{
				detectedRow("s16-p1"),
				evaluatedRow(audit.OutcomeAsk, "default_action", audit.DecisionAsk, "s16-p1", ""),
				withdrawnRow("agent_withdrew_occurrence", "s16-p1"),
			},
			wantShown: shown{Status: "RUNNING"},
		},
		{
			name:    "S17 the late exit of a restarted agent's previous process",
			adapter: adapters.GenericID,
			policy:  askByDefault,
			steps:   []step{confirmRestart(), exitsLate(s17exit), raise(s17)},
			wantJournal: []row{
				detectedRow("s17-exit"),
				// core: reason process_exit_stale, ingest.go:363-378.
				finishedRow(audit.OutcomeFinished, "process_exit", "s17-exit"),
				// core: the replacement, which still runs, takes its prompt in
				// and detectedRow("s17-p1") and its evaluation are written here,
				// ingest.go:139-169.
			},
			// core: the prompt waits on the operator, and the agent shows ACTION
			// REQUIRED.
			wantShown: shown{Status: "FINISHED"},
		},
		{
			name:    "S18 a direct line to an agent with no prompt",
			adapter: adapters.GenericID,
			policy:  askByDefault,
			steps:   []step{sendLine("run the tests")},
			wantJournal: []row{
				lineRow(audit.OutcomeInFlight, "operator_input_started"),
				lineRow(audit.OutcomeApplied, "operator_input_applied"),
			},
			wantWrites: []write{{Session: scenarioAgent, Line: true, Value: "run the tests"}},
			wantShown:  shown{Status: "RUNNING"},
		},
		{
			name:    "S19 an agent that fails while a prompt waits",
			adapter: adapters.GenericID,
			policy:  askByDefault,
			steps:   []step{raise(s19), exits(s19exit)},
			wantJournal: []row{
				detectedRow("s19-p1"),
				evaluatedRow(audit.OutcomeAsk, "default_action", audit.DecisionAsk, "s19-p1", ""),
				detectedRow("s19-exit"),
				finishedRow(audit.OutcomeFailed, "process_exit", "s19-exit"),
			},
			wantShown: shown{Status: "ERROR"},
		},
		{
			name:    "S20 an agent stopped while a prompt waits",
			adapter: adapters.GenericID,
			policy:  askByDefault,
			steps:   []step{raise(s20), confirmStop(), exits(s20exit)},
			wantJournal: []row{
				detectedRow("s20-p1"),
				evaluatedRow(audit.OutcomeAsk, "default_action", audit.DecisionAsk, "s20-p1", ""),
				detectedRow("s20-exit"),
				finishedRow(audit.OutcomeFinished, "process_exit", "s20-exit"),
			},
			wantShown: shown{Status: "FINISHED"},
		},
		{
			name:    "S21 an Aider prompt a rule denies, held by a limit, then answered by typing",
			adapter: adapters.AiderID,
			policy:  policy.Config{DefaultAction: policy.ActionAsk, MaxConsecutiveAutoDecisions: 1, Rules: []policy.Rule{denyConfirmations()}},
			steps:   []step{raise(s21a), raise(s21b), answer("y")},
			wantJournal: []row{
				detectedRow("s21-p1"),
				evaluatedRow(audit.OutcomeInFlight, "rule_match", audit.DecisionDeny, "s21-p1", "deny-confirmations"),
				// core: the decision names its rule, decide.go:140.
				decisionRow(audit.DecisionDeny, audit.DecisionByPolicy, "s21-p1", ""),
				// core: applied/delivery_applied, decide.go:164.
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionDeny, audit.DecisionByPolicy, "s21-p1"),
				detectedRow("s21-p2"),
				// core: reason consecutive_auto_limit, safety.go:137.
				evaluatedRow(audit.OutcomeAsk, "unknown", audit.DecisionAsk, "s21-p2", "deny-confirmations"),
				// core: the prompt offers the adapter's deny alone, so the typed
				// answer is refused with ErrDenyOnly and neither row below is
				// written, decide.go:544-553.
				decisionRow(audit.DecisionAsk, audit.DecisionByHuman, "s21-p2", ""),
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionAsk, audit.DecisionByHuman, "s21-p2"),
			},
			// core: the typed answer is never written.
			wantWrites: []write{
				{Session: scenarioAgent, EventID: "s21-p1", Decision: adapters.DecisionDeny},
				{Session: scenarioAgent, EventID: "s21-p2", Decision: adapters.DecisionManual, Value: "y"},
			},
			// core: the prompt waits on the operator, and the agent shows
			// ACTION REQUIRED, tagged LIMIT • ASK • DENY ONLY.
			wantShown: shown{Status: "RUNNING", Tag: "ASK APPLIED"},
		},
		{
			// A restart is the operator's own action: the prompt the limit
			// held goes with the process that raised it, and the replacement
			// begins a new streak, so the policy answers its first prompt.
			name:    "S22 an agent restarted while a prompt a limit held waits",
			adapter: adapters.AiderID,
			policy:  policy.Config{DefaultAction: policy.ActionAsk, MaxConsecutiveAutoDecisions: 1, Rules: []policy.Rule{allowAiderEdits()}},
			steps:   []step{raise(s22a), raise(s22b), confirmRestart(), raise(s22c)},
			wantJournal: []row{
				detectedRow("s22-p1"),
				evaluatedRow(audit.OutcomeInFlight, "rule_match", audit.DecisionAllow, "s22-p1", "allow-edits"),
				// core: the decision names its rule, decide.go:140.
				decisionRow(audit.DecisionAllow, audit.DecisionByPolicy, "s22-p1", ""),
				// core: applied/delivery_applied, decide.go:164.
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionAllow, audit.DecisionByPolicy, "s22-p1"),
				detectedRow("s22-p2"),
				// core: reason consecutive_auto_limit, safety.go:137.
				evaluatedRow(audit.OutcomeAsk, "unknown", audit.DecisionAsk, "s22-p2", "allow-edits"),
				detectedRow("s22-p3"),
				evaluatedRow(audit.OutcomeInFlight, "rule_match", audit.DecisionAllow, "s22-p3", "allow-edits"),
				// core: the decision names its rule, decide.go:140.
				decisionRow(audit.DecisionAllow, audit.DecisionByPolicy, "s22-p3", ""),
				// core: applied/delivery_applied, decide.go:164.
				deliveryRow(audit.OutcomeSucceeded, "delivery_succeeded", audit.DecisionAllow, audit.DecisionByPolicy, "s22-p3"),
			},
			wantWrites: []write{
				{Session: scenarioAgent, EventID: "s22-p1", Decision: adapters.DecisionAllow},
				{Session: scenarioAgent, EventID: "s22-p3", Decision: adapters.DecisionAllow},
			},
			wantShown: shown{Status: "RUNNING", Tag: "AUTO APPLIED"},
		},
	}
}

// newScenarioDriver is the supervision the table runs against.
func newScenarioDriver(t *testing.T, candidate scenario) scenarioDriver {
	return newLegacyDriver(t, []Pane{{
		ID:      scenarioAgent,
		Name:    "Agent A",
		Command: "agent",
		Backend: "pty",
		Adapter: candidate.adapter,
	}}, candidate.policy)
}

// TestEachSupervisionScenarioLeavesItsJournalWritesAndScreen runs every
// scenario and compares the journal it leaves, what reached the agent and what
// the operator sees with the table.
func TestEachSupervisionScenarioLeavesItsJournalWritesAndScreen(t *testing.T) {
	for _, candidate := range supervisionScenarios() {
		t.Run(candidate.name, func(t *testing.T) {
			driver := newScenarioDriver(t, candidate)
			for _, current := range candidate.steps {
				driver.do(current)
			}
			if got := driver.shown(scenarioAgent); !reflect.DeepEqual(got, candidate.wantShown) {
				t.Errorf("shown = %+v, want %+v", got, candidate.wantShown)
			}
			if got := driver.writes(); !reflect.DeepEqual(got, candidate.wantWrites) {
				t.Errorf("writes = %+v, want %+v", got, candidate.wantWrites)
			}
			if got := driver.journal(); !reflect.DeepEqual(got, candidate.wantJournal) {
				t.Errorf("journal:\n%s\nwant:\n%s", formatRows(got), formatRows(candidate.wantJournal))
			}
		})
	}
}

func formatRows(rows []row) string {
	lines := make([]string, len(rows))
	for index, current := range rows {
		lines[index] = fmt.Sprintf("  %2d %s", index+1, current)
	}
	return strings.Join(lines, "\n")
}
