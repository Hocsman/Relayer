package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// The bridge's SupervisionEvent is what the frontend decodes, and the core's
// View is what the core builds: a field the conversion forgot would reach the
// screen empty without failing any other test. Every field is set here, so the
// two encodings must match exactly.
func TestSupervisionEventCarriesEveryFieldOfTheCoreView(t *testing.T) {
	view := supervise.View{
		RunID:     "run-1",
		ID:        "event-1",
		SessionID: "agent-a",
		AgentID:   "agent-a",
		Adapter:   "generic",
		Type:      "confirmation",
		Summary:   "Overwrite file?",
		Sensitive: true,
		Risk:      "low",
		Timestamp: "2026-08-27T10:00:00.000000000Z",
		Evaluation: supervise.EvaluationView{
			Action:         "allow",
			ProposedAction: "allow",
			RuleName:       "allow-safe",
			Reason:         "rule_match",
			Automatic:      true,
			DryRun:         true,
		},
		DeliveryStatus: "delivering",
		Decisions:      []string{"allow", "deny"},
	}
	for index := 0; index < reflect.TypeOf(view).NumField(); index++ {
		// An unexported field, the tool call the web gateway shows, is not
		// part of the JSON, and the desktop has no place for it.
		if !reflect.TypeOf(view).Field(index).IsExported() {
			continue
		}
		if reflect.ValueOf(view).Field(index).IsZero() {
			t.Fatalf("fixture leaves View.%s unset", reflect.TypeOf(view).Field(index).Name)
		}
	}

	fromCore, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("marshal view: %v", err)
	}
	fromBridge, err := json.Marshal(supervisionEventFromView(view))
	if err != nil {
		t.Fatalf("marshal event: %v", err)
	}
	if string(fromBridge) != string(fromCore) {
		t.Fatalf("bridge event differs from the core view:\nbridge=%s\ncore=  %s", fromBridge, fromCore)
	}
}

// Every entry point of the bridge used to check that its run was the active
// one before it emitted; the core checks only that its run is not draining.
// The sink checks the active run again, so a run the bridge no longer holds
// shows nothing: no prompt, status, error, journal state or notification.
func TestTheSinkOfARunThatIsNoLongerActiveShowsNothing(t *testing.T) {
	application := newBridgeForTest(newFakeDesktopEngine("agent-a"))
	application.ctx = context.Background()
	var (
		mu      sync.Mutex
		emitted []string
	)
	application.emitFn = func(_ context.Context, name string, _ ...interface{}) {
		mu.Lock()
		emitted = append(emitted, name)
		mu.Unlock()
	}
	emittedNames := func() []string {
		mu.Lock()
		defer mu.Unlock()
		return append([]string(nil), emitted...)
	}
	notifier := &fakeAppNotifier{}
	application.setNotifier(notifier)
	show := func(sink desktopSink) {
		sink.Prompt(supervise.View{RunID: sink.run.id, ID: "prompt-1", SessionID: "agent-a"})
		sink.Status(supervise.Status{RunID: sink.run.id, Scope: "session", SessionID: "agent-a", Status: "running"})
		sink.Status(supervise.Status{RunID: sink.run.id, Scope: "audit", Status: "failed"})
		sink.Error(supervise.SafeError{RunID: sink.run.id, Code: "fixture", Message: "Fixture."})
		sink.Notify(supervise.Notice{Kind: supervise.NoticePendingDecision, SessionID: "agent-a", EventID: "prompt-1"})
	}

	show(desktopSink{app: application, run: &runGeneration{id: "run-stale"}})
	if names := emittedNames(); len(names) != 0 {
		t.Fatalf("a run that is not active emitted %v", names)
	}
	if notices := notifier.snapshot(); len(notices) != 0 {
		t.Fatalf("a run that is not active notified %#v", notices)
	}
	if state, _ := application.GetState(); state.Audit.Status != "ready" {
		t.Fatalf("a run that is not active set the journal's state to %q", state.Audit.Status)
	}

	show(desktopSink{app: application, run: activeRunForTest(application)})
	if names, want := emittedNames(), []string{eventSemantic, eventStatus, eventStatus, eventError}; !reflect.DeepEqual(names, want) {
		t.Fatalf("the active run emitted %v, want %v", names, want)
	}
	if notices := notifier.snapshot(); len(notices) != 1 {
		t.Fatalf("the active run notified %#v, want one notice", notices)
	}
	if state, _ := application.GetState(); state.Audit.Status != "failed" {
		t.Fatalf("the active run's journal state = %q, want failed", state.Audit.Status)
	}
}

// The core names who made a decision or sent a line when its front end says
// so, and the desktop never does: it has one operator, at the machine, and
// passes the core the zero Actor. What the desktop journals for a person's
// answers and lines is therefore exactly what it journaled before the core
// knew of actors, entry for entry and byte for byte: no operator, no role, no
// connection, and the human entries' metadata as empty as ever. The expected
// journal was captured from the desktop before actors existed.
func TestTheDesktopJournalsWhatAPersonDidAsItAlwaysHas(t *testing.T) {
	engine := newFakeDesktopEngine("agent-a")
	engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
	application := newBridgeForTest(engine)
	runID := activeRunIDForTest(application)

	// A button, a typed answer, a button the adapter cannot encode then a
	// typed answer to the same prompt, and a line.
	application.handleAdapterEvent(bridgeEvent("agent-a", "prompt-1"))
	if err := application.SubmitAutomaticDecision(runID, "agent-a", "prompt-1", "allow"); err != nil {
		t.Fatalf("SubmitAutomaticDecision: %v", err)
	}
	application.handleAdapterEvent(bridgeEvent("agent-a", "prompt-2"))
	if err := application.SubmitDecision(runID, "agent-a", "prompt-2", "typed answer"); err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}
	application.handleAdapterEvent(bridgeEvent("agent-a", "prompt-3"))
	engine.mu.Lock()
	engine.applyErr = adapters.ErrDecisionUnsupported
	engine.mu.Unlock()
	if err := application.SubmitAutomaticDecision(runID, "agent-a", "prompt-3", "deny"); !errors.Is(err, errUnsupportedDecision) {
		t.Fatalf("an answer the adapter cannot encode = %v", err)
	}
	engine.mu.Lock()
	engine.applyErr = nil
	engine.mu.Unlock()
	if err := application.SubmitDecision(runID, "agent-a", "prompt-3", "n"); err != nil {
		t.Fatalf("SubmitDecision after the fallback: %v", err)
	}
	if err := application.SubmitLine(runID, "agent-a", "hello"); err != nil {
		t.Fatalf("SubmitLine: %v", err)
	}

	var journal strings.Builder
	for _, entry := range engine.auditSnapshot() {
		encoded, err := json.Marshal(entry)
		if err != nil {
			t.Fatalf("marshal entry: %v", err)
		}
		journal.Write(encoded)
		journal.WriteByte('\n')
	}
	if got := journal.String(); got != desktopHumanJournal {
		t.Fatalf("the desktop's journal changed:\n got:\n%s\nwant:\n%s", got, desktopHumanJournal)
	}
}

const desktopHumanJournal = `{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"event_detected","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-1","event_type":"confirmation","risk":"low","decision_by":"system","outcome":"detected","reason":"event_detected","summary":"Overwrite file?","sensitive":false}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"policy_evaluated","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-1","event_type":"confirmation","risk":"low","decision":"ask","decision_by":"policy","outcome":"ask","reason":"default_action","summary":"Overwrite file?","sensitive":false,"metadata":{"automatic":"false","effective_action":"ask","mode":"enforce","proposed_action":"ask"}}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"decision","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-1","event_type":"confirmation","risk":"low","decision":"allow","decision_by":"human","outcome":"in_flight","reason":"decision_selected","sensitive":false}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"delivery","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-1","event_type":"confirmation","risk":"low","decision":"allow","decision_by":"human","outcome":"applied","reason":"delivery_applied","sensitive":false}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"event_detected","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-2","event_type":"confirmation","risk":"low","decision_by":"system","outcome":"detected","reason":"event_detected","summary":"Overwrite file?","sensitive":false}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"policy_evaluated","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-2","event_type":"confirmation","risk":"low","decision":"ask","decision_by":"policy","outcome":"ask","reason":"default_action","summary":"Overwrite file?","sensitive":false,"metadata":{"automatic":"false","effective_action":"ask","mode":"enforce","proposed_action":"ask"}}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"decision","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-2","event_type":"confirmation","risk":"low","decision":"ask","decision_by":"human","outcome":"in_flight","reason":"decision_selected","sensitive":false}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"delivery","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-2","event_type":"confirmation","risk":"low","decision":"ask","decision_by":"human","outcome":"applied","reason":"delivery_applied","sensitive":false}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"event_detected","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-3","event_type":"confirmation","risk":"low","decision_by":"system","outcome":"detected","reason":"event_detected","summary":"Overwrite file?","sensitive":false}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"policy_evaluated","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-3","event_type":"confirmation","risk":"low","decision":"ask","decision_by":"policy","outcome":"ask","reason":"default_action","summary":"Overwrite file?","sensitive":false,"metadata":{"automatic":"false","effective_action":"ask","mode":"enforce","proposed_action":"ask"}}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"decision","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-3","event_type":"confirmation","risk":"low","decision":"deny","decision_by":"human","outcome":"in_flight","reason":"decision_selected","sensitive":false}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"delivery","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-3","event_type":"confirmation","risk":"low","decision":"deny","decision_by":"human","outcome":"fallback_unsupported","reason":"fallback_unsupported","sensitive":false}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"decision","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-3","event_type":"confirmation","risk":"low","decision":"ask","decision_by":"human","outcome":"in_flight","reason":"decision_selected","sensitive":false}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"delivery","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","event_id":"prompt-3","event_type":"confirmation","risk":"low","decision":"ask","decision_by":"human","outcome":"applied","reason":"delivery_applied","sensitive":false}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"operator_input","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","decision_by":"human","outcome":"in_flight","reason":"operator_input_started","sensitive":false}
{"schema_version":0,"sequence":0,"timestamp":"0001-01-01T00:00:00Z","entry_id":"","run_id":"","kind":"operator_input","session_id":"agent-a","agent_id":"agent-a","backend":"pty","adapter":"generic","decision_by":"human","outcome":"applied","reason":"operator_input_applied","sensitive":false}
`
