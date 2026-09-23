package main

import (
	"encoding/json"
	"reflect"
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
