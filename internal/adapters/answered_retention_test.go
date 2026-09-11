package adapters

import (
	"testing"
)

func TestAnsweredQuestionRetainedAcrossSplitRepaintChunk(t *testing.T) {
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}

	var live []Event
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session-split", "agent-split", GenericID),
		4096,
		Hooks{
			OnEvent: func(event Event) {
				live = append(live, event)
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	// First frame drawing the question.
	frame := "\x1b[2J\x1b[2;1HDo you want to proceed? [y/n]\r\n1. Yes\r\n2. No\r\n"
	if err := processor.Consume([]byte(frame)); err != nil {
		t.Fatalf("Consume failed: %v", err)
	}
	if len(live) != 1 {
		t.Fatalf("expected 1 event, got %d", len(live))
	}
	first := live[0]

	// Operator answers the question.
	if err := processor.Resolve(first.ID, func() error { return nil }); err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if processor.state.Pending() != nil {
		t.Fatalf("pending should be nil, got %#v", processor.state.Pending())
	}

	// Full-screen repaint split across reads:
	// Chunk 1: erase display ED2, placing cursor at 1;1, but question hasn't arrived yet.
	chunk1 := "\x1b[2J\x1b[1;1H"
	if err := processor.Consume([]byte(chunk1)); err != nil {
		t.Fatalf("Consume chunk1 failed: %v", err)
	}

	// Verify that the answered memory was NOT prematurely destroyed during chunk 1.
	if len(processor.state.pendingAnswers()) == 0 {
		t.Fatalf("expected answered memory to be retained across blanked mid-frame read, but it was wiped")
	}

	// Chunk 2: the rest of the frame arrives with the still-painted question.
	chunk2 := "\x1b[2;1HDo you want to proceed? [y/n]\r\n1. Yes\r\n2. No\r\n"
	if err := processor.Consume([]byte(chunk2)); err != nil {
		t.Fatalf("Consume chunk2 failed: %v", err)
	}

	// Verify that no second event was raised.
	if len(live) != 1 {
		t.Fatalf("expected no new event on redraw of answered question after split read, got %d events", len(live))
	}
	if processor.state.Pending() != nil {
		t.Fatalf("pending should remain nil, got %#v", processor.state.Pending())
	}
}
