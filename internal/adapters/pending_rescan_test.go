package adapters

import (
	"testing"
)

func rescanTestProcessor(t *testing.T) (*Processor, *[]Event) {
	t.Helper()
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	emitted := []Event{}
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session", "agent", GenericID),
		64*1024,
		Hooks{OnEvent: func(event Event) { emitted = append(emitted, event) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	return processor, &emitted
}

// A prompt that arrives while another one is pending must not be lost.
//
// Detect returns early whenever an occurrence is pending, so output produced
// during that window is never examined, and acknowledging the first prompt used
// to wipe the detection window with it. The second prompt was then gone for
// good: never evaluated by a policy, never audited, and still on the agent's
// screen — where the operator's next line, or their refusal of the first
// prompt, would answer it.
func TestSecondPromptSurvivesResolutionOfTheFirst(t *testing.T) {
	processor, emitted := rescanTestProcessor(t)

	if err := processor.Consume([]byte("Overwrite file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	first := processor.Pending()
	if first == nil {
		t.Fatal("the first prompt was not detected")
	}

	// The agent answers itself and immediately asks something else. This is
	// consumed while the first occurrence is still pending.
	if err := processor.Consume([]byte("\nProceeding.\nDelete branch? [y/n]")); err != nil {
		t.Fatal(err)
	}
	if pending := processor.Pending(); pending == nil || pending.ID != first.ID {
		t.Fatalf("pending changed while the first prompt was unresolved: %#v", pending)
	}

	delivered := false
	if err := processor.Resolve(first.ID, func() error { delivered = true; return nil }); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if !delivered {
		t.Fatal("the decision was not delivered")
	}

	second := processor.Pending()
	if second == nil {
		t.Fatal("the second prompt was lost: nothing is pending after the first was resolved")
	}
	if second.ID == first.ID {
		t.Fatalf("the resolved occurrence came back: %#v", second)
	}
	if second.Signature == first.Signature {
		t.Fatalf("the second occurrence reuses the first signature: %#v", second)
	}
	if !processor.IsBlocked() {
		t.Fatal("the session is not blocked on the surviving prompt")
	}

	// It must reach the supervisor, not merely sit in the state.
	found := false
	for _, event := range *emitted {
		if event.ID == second.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("the surviving prompt was never emitted: %#v", *emitted)
	}
}

// The occurrence that was just answered must not come back. Before this
// change that was guaranteed by wiping the window; it now has to be enforced
// by comparing signatures, and the guarantee is documented.
func TestResolvedPromptDoesNotReblockOnItsOwnHistory(t *testing.T) {
	processor, _ := rescanTestProcessor(t)

	if err := processor.Consume([]byte("Overwrite file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	first := processor.Pending()
	if first == nil {
		t.Fatal("no prompt detected")
	}
	if err := processor.Resolve(first.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if pending := processor.Pending(); pending != nil {
		t.Fatalf("the answered prompt reblocked from its own history: %#v", pending)
	}

	// The same text appearing again is a new occurrence and must block.
	if err := processor.Consume([]byte("\nOverwrite file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	repeated := processor.Pending()
	if repeated == nil {
		t.Fatal("a genuinely repeated prompt did not block")
	}
	if repeated.ID == first.ID {
		t.Fatalf("the repeated prompt reused the resolved occurrence ID: %#v", repeated)
	}
}

// Acknowledge is the non-delivery path and must behave the same way.
func TestAcknowledgeAlsoRecoversASuppressedPrompt(t *testing.T) {
	processor, _ := rescanTestProcessor(t)

	if err := processor.Consume([]byte("Overwrite file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	first := processor.Pending()
	if first == nil {
		t.Fatal("no prompt detected")
	}
	if err := processor.Consume([]byte("\nDelete branch? [y/n]")); err != nil {
		t.Fatal(err)
	}
	if err := processor.Acknowledge(first.ID); err != nil {
		t.Fatalf("Acknowledge: %v", err)
	}
	if pending := processor.Pending(); pending == nil {
		t.Fatal("the suppressed prompt was lost on the acknowledge path")
	}
}
