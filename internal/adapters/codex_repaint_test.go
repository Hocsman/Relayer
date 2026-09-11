package adapters

import (
	"testing"
)

func TestCodexRepaintDoesNotReAskApprovedCommand(t *testing.T) {
	fixture := loadCodexCaptureFixture(t, "command_approval.json")
	var live []Event
	processor, err := NewProcessor(
		newCodexTestAdapter(t, DefaultPatterns()),
		NewDetectionState("session-codex", "reviewer", CodexID),
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

	// Feed ANSI chunks of the Codex approval prompt.
	for _, chunk := range fixture.ANSIChunks {
		if err := processor.Consume([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}

	if len(live) != 1 {
		t.Fatalf("expected 1 event initially, got %d", len(live))
	}
	first := live[0]

	// Operator approves the command.
	if err := processor.Resolve(first.ID, func() error { return nil }); err != nil {
		t.Fatalf("Resolve failed: %v", err)
	}
	if processor.state.Pending() != nil {
		t.Fatalf("pending should be nil after Resolve, got %#v", processor.state.Pending())
	}

	// Codex redraws the same frame on its next tick before completing execution.
	for _, chunk := range fixture.ANSIChunks {
		if err := processor.Consume([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}

	if len(live) != 1 {
		t.Fatalf("expected no new event on repaint of approved command, got %d events", len(live))
	}
	if processor.state.Pending() != nil {
		t.Fatalf("pending should remain nil after repaint of approved command, got %#v", processor.state.Pending())
	}
}
