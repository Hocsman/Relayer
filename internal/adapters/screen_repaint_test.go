package adapters

import (
	"strings"
	"testing"
)

// repaintProcessor replays raw PTY bytes, escapes included, and reports both the
// events that became actionable and the text detection is working from.
func repaintProcessor(t *testing.T) (*Processor, *[]string) {
	t.Helper()
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	matches := []string{}
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session", "agent", GenericID),
		64*1024,
		Hooks{OnEvent: func(event Event) { matches = append(matches, event.Match) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	return processor, &matches
}

// What the screen model is for.
//
// Relayer strips escape sequences and keeps the remaining bytes in order. That
// is exact for an agent that only appends, and wrong for one that repaints: the
// cursor movements which say WHERE each fragment lands are thrown away, and the
// erases which say what is no longer on screen are thrown away with them.
//
// So a question the agent has already erased is still in the window, and a
// question it painted by addressing rows out of order is not where the byte
// order says it is. Both are silent.
func TestErasedScreenIsStillInTheDetectionWindow(t *testing.T) {
	t.Skip("detection still normalizes a byte stream. internal/screen renders " +
		"these two cases correctly today — see its own conformance suite — but the " +
		"Processor is not wired to it yet, and that wiring needs a terminal size " +
		"the adapters package cannot currently reach. This case is kept, running " +
		"and skipped, so the gap has a name in the suite rather than only in a doc.")

	processor, matches := repaintProcessor(t)

	// The agent asks, then clears the screen and moves on without an answer —
	// a TUI redrawing itself, or a prompt the agent withdrew.
	if err := processor.Consume([]byte("Overwrite file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	if pending := processor.Pending(); pending == nil {
		t.Fatal("the prompt was not detected in the first place")
	}
	if err := processor.Acknowledge(processor.Pending().ID); err != nil {
		t.Fatal(err)
	}

	// ESC[2J erases the display, ESC[H homes the cursor.
	if err := processor.Consume([]byte("\x1b[2J\x1b[HBuilding the project\n")); err != nil {
		t.Fatal(err)
	}

	output := processor.Output()
	if strings.Contains(output, "Overwrite file?") {
		t.Fatalf("the erased question survived the clear-screen:\n%s", output)
	}
	_ = matches
}

// A prompt painted by addressing the cursor lands where the addressing says,
// not where the byte order says. Today the row jumps are dropped, so the
// fragments are concatenated in write order and the question is mangled.
func TestCursorAddressedPaintLandsWhereItWasAddressed(t *testing.T) {
	t.Skip("detection still normalizes a byte stream. internal/screen renders " +
		"these two cases correctly today — see its own conformance suite — but the " +
		"Processor is not wired to it yet, and that wiring needs a terminal size " +
		"the adapters package cannot currently reach. This case is kept, running " +
		"and skipped, so the gap has a name in the suite rather than only in a doc.")

	processor, matches := repaintProcessor(t)

	// A full-frame agent painting a box: it draws the frame first, then jumps
	// back up to fill the question inside it.
	frame := "\x1b[2J\x1b[H" +
		"\x1b[1;1H+--------------------------+" +
		"\x1b[2;1H|                          |" +
		"\x1b[3;1H+--------------------------+" +
		"\x1b[2;3HOverwrite file? [Y/n]"
	if err := processor.Consume([]byte(frame)); err != nil {
		t.Fatal(err)
	}

	output := processor.Output()
	if !strings.Contains(output, "| Overwrite file? [Y/n]") {
		t.Fatalf("the question did not land inside the frame it was addressed into:\n%s", output)
	}
	if len(*matches) == 0 {
		t.Fatal("a prompt painted by cursor addressing was never detected")
	}
}

// The complement: an append-only agent must be unaffected. Every existing
// fixture depends on this, and a screen model that changes plain output would
// be trading one silent failure for another.
func TestAppendOnlyOutputIsUnchangedByTheScreenModel(t *testing.T) {
	processor, matches := repaintProcessor(t)

	const plain = "Building the project\nCompiling 12 files\nOverwrite file? [Y/n]"
	if err := processor.Consume([]byte(plain)); err != nil {
		t.Fatal(err)
	}
	output := processor.Output()
	for _, line := range strings.Split(plain, "\n") {
		if !strings.Contains(output, line) {
			t.Fatalf("plain output lost %q:\n%s", line, output)
		}
	}
	if len(*matches) != 1 {
		t.Fatalf("plain output produced %d event(s), want 1", len(*matches))
	}
}
