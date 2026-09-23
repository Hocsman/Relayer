package main

import (
	"context"
	"encoding/json"
	"reflect"
	"sync"
	"testing"

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
