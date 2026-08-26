package adapters

import (
	"reflect"
	"testing"
	"time"
)

func TestEventCloneAndActionableContract(t *testing.T) {
	original := Event{
		ID:        "evt-example",
		Signature: "signature-example",
		Sequence:  7,
		SessionID: "session-a",
		AgentID:   "agent-a",
		Adapter:   GenericID,
		Type:      EventCredential,
		Summary:   "synthetic credential request",
		Match:     "Password:",
		Sensitive: true,
		Risk:      RiskHigh,
		Timestamp: time.Unix(123, 456).UTC(),
		Metadata:  map[string]string{"pattern": "credential"},
	}
	clone := original.Clone()
	if !reflect.DeepEqual(clone, original) {
		t.Fatalf("Clone = %#v, want %#v", clone, original)
	}
	clone.Metadata["pattern"] = "mutated"
	clone.Metadata["new"] = "value"
	if original.Metadata["pattern"] != "credential" || len(original.Metadata) != 1 {
		t.Fatalf("Clone aliases source metadata: %#v", original.Metadata)
	}

	for _, test := range []struct {
		eventType EventType
		want      bool
	}{
		{eventType: EventConfirmation, want: true},
		{eventType: EventCredential, want: true},
		{eventType: EventProcessExit, want: false},
		{eventType: EventType("future-observation"), want: false},
	} {
		if got := (Event{Type: test.eventType}).Actionable(); got != test.want {
			t.Fatalf("Actionable(%q) = %t, want %t", test.eventType, got, test.want)
		}
	}
}

func TestDetectionStateNormalizesIdentityAndReturnsDefensivePendingCopy(t *testing.T) {
	state := NewDetectionState(" session-a ", " agent-a ", " GeNeRiC ")
	if state.SessionID != "session-a" || state.AgentID != "agent-a" || state.AdapterID != GenericID {
		t.Fatalf("normalized state = %#v", state)
	}
	event := state.replacePending(Event{
		Signature: "stable-signature",
		SessionID: state.SessionID,
		AgentID:   state.AgentID,
		Adapter:   GenericID,
		Type:      EventConfirmation,
		Metadata:  map[string]string{"pattern": "confirmation"},
	})
	if event.ID == "" || event.Sequence != 1 || event.Timestamp.IsZero() || !state.IsBlocked() {
		t.Fatalf("first pending event = %#v, blocked=%t", event, state.IsBlocked())
	}
	pending := state.Pending()
	pending.Metadata["pattern"] = "mutated"
	pending.ID = "mutated"
	again := state.Pending()
	if again.ID != event.ID || again.Metadata["pattern"] != "confirmation" {
		t.Fatalf("Pending returned aliased state: %#v", again)
	}
}

func TestStableSignatureCanonicalizesEquivalentMatchesButOccurrenceIDsRemainDistinct(t *testing.T) {
	first := stableSignature("session-a", GenericID, EventConfirmation, "confirm", "Overwrite   File? [Y/N]")
	second := stableSignature("session-a", GenericID, EventConfirmation, "confirm", " overwrite file?  [y/n] ")
	if first != second {
		t.Fatalf("equivalent signatures differ: %q != %q", first, second)
	}
	if occurrenceID(first, 1) == occurrenceID(first, 2) {
		t.Fatal("two occurrences of one signature received the same ID")
	}
	if first == stableSignature("session-b", GenericID, EventConfirmation, "confirm", "Overwrite File? [Y/N]") {
		t.Fatal("signature did not include stable session identity")
	}
}
