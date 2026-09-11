package adapters

import (
	"testing"
)

func TestSnapshotReconcileDoesNotReAskAnsweredQuestionWhenHighlightMoves(t *testing.T) {
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}

	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session-tmux", "worker", GenericID),
		4096,
		Hooks{},
	)
	if err != nil {
		t.Fatal(err)
	}

	// First snapshot with question and cursor on option 1.
	snap1 := "Overwrite file? [Y/n]\n> [1] yes\n  [2] no\n"
	event1, changed, err := processor.ReconcileSnapshot([]byte(snap1))
	if err != nil {
		t.Fatalf("first ReconcileSnapshot error: %v", err)
	}
	if event1 == nil || !changed {
		t.Fatalf("expected event on first snapshot, got event=%v, changed=%t", event1, changed)
	}

	// Operator resolves the question.
	if err := processor.Resolve(event1.ID, func() error { return nil }); err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if processor.state.Pending() != nil {
		t.Fatalf("pending should be nil after Resolve, got %#v", processor.state.Pending())
	}

	// Pane updates while question remains on screen: highlight moves to option 2 (or spinner/counter changes).
	snap2 := "Overwrite file? [Y/n]\n  [1] yes\n> [2] no\n"
	event2, changed, err := processor.ReconcileSnapshot([]byte(snap2))
	if err != nil {
		t.Fatalf("second ReconcileSnapshot error: %v", err)
	}
	if event2 != nil || changed {
		t.Fatalf("expected no event and changed=false on highlight move of answered question, got event=%#v changed=%t", event2, changed)
	}
	if processor.state.Pending() != nil {
		t.Fatalf("pending should remain nil after highlight move, got %#v", processor.state.Pending())
	}
}
