package adapters

import (
	"strings"
	"testing"
)

func TestQuestionScrolledOffVisibleGridIsNotReported(t *testing.T) {
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}

	var live []Event
	// Terminal of 80 columns x 24 rows
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session-scroll", "agent-scroll", GenericID),
		80*24,
		Hooks{
			OnEvent: func(event Event) {
				live = append(live, event)
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	// In a single write, a question and its options are emitted, immediately followed by
	// 30 lines of subsequent output that scroll the question off the visible grid into scrollback.
	var sb strings.Builder
	sb.WriteString("Overwrite file? [Y/n]\r\n1. Yes\r\n2. No\r\n")
	for i := 1; i <= 30; i++ {
		sb.WriteString("work line log entry\r\n")
	}

	if err := processor.Consume([]byte(sb.String())); err != nil {
		t.Fatalf("Consume failed: %v", err)
	}

	// The question was scrolled off the visible screen into scrollback during the write.
	// It must NOT be reported as an active/pending question!
	if len(live) != 0 {
		t.Fatalf("expected 0 events for question scrolled into scrollback, got %d: %#v", len(live), live)
	}
	if processor.state.Pending() != nil {
		t.Fatalf("pending should be nil for question in scrollback, got %#v", processor.state.Pending())
	}
}

func TestRescanDoesNotResurrectQuestionScrolledIntoScrollback(t *testing.T) {
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}

	var live []Event
	// Small screen: 40x6
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session-rescan", "agent-rescan", GenericID),
		40*6,
		Hooks{
			OnEvent: func(event Event) {
				live = append(live, event)
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	// 1. First question is shown on the 40x6 grid.
	if err := processor.Consume([]byte("Overwrite file? [Y/n]\r\n1. Yes\r\n2. No\r\n")); err != nil {
		t.Fatal(err)
	}
	if len(live) != 1 {
		t.Fatalf("expected 1 event initially, got %d", len(live))
	}
	first := live[0]

	// Operator answers the first question.
	if err := processor.Resolve(first.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	// 2. Ten newlines arrive, scrolling the first question off the 6-row grid into scrollback.
	if err := processor.Consume([]byte(strings.Repeat("scrolling work line\r\n", 10))); err != nil {
		t.Fatal(err)
	}

	// 3. A second, different question is raised and answered.
	if err := processor.Consume([]byte("Do you want to proceed? [y/n]\r\n1. Yes\r\n2. No\r\n")); err != nil {
		t.Fatal(err)
	}
	if len(live) != 2 {
		t.Fatalf("expected 2 events after second question, got %d", len(live))
	}
	second := live[1]

	// Operator answers the second question, which triggers rescanRetainedWindow.
	if err := processor.Resolve(second.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	// The rescan must NOT bring back the first question that scrolled into scrollback!
	if len(live) != 2 {
		t.Fatalf("expected rescan to not resurrect scrollback question, got %d events", len(live))
	}
	if processor.state.Pending() != nil {
		t.Fatalf("pending should remain nil after rescan, got %#v", processor.state.Pending())
	}
}
