package adapters

import (
	"fmt"
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

// The screen must follow the terminal, or it wraps in the wrong place and a
// question broken by the right margin rejoins as the wrong sentence.
func TestResizeReachesTheRenderedScreen(t *testing.T) {
	processor, _ := repaintProcessor(t)
	processor.Resize(24, 6)

	// Addressing marks the agent as repainting, so Output() reads the screen.
	if err := processor.Consume([]byte("\x1b[1;1HDo you want to continue with this?")); err != nil {
		t.Fatal(err)
	}
	if got := processor.Output(); !strings.Contains(got, "Do you want to continue with this?") {
		t.Fatalf("the wrapped question did not rejoin at width 24: %q", got)
	}

	// A width that cannot hold the question still has to render it whole once
	// the wrapped rows are joined.
	narrow, _ := repaintProcessor(t)
	narrow.Resize(12, 8)
	if err := narrow.Consume([]byte("\x1b[1;1HDo you want to continue with this?")); err != nil {
		t.Fatal(err)
	}
	if got := narrow.Output(); !strings.Contains(got, "Do you want to continue with this?") {
		t.Fatalf("the wrapped question did not rejoin at width 12: %q", got)
	}
}

// Detection on the rendered screen, which is the point of the whole exercise.
func TestDetectionReadsTheRenderedScreen(t *testing.T) {
	t.Run("a question painted by addressing is detected", func(t *testing.T) {
		processor, matches := repaintProcessor(t)
		processor.Resize(40, 10)
		// The frame first, then the question filled into it — the order a
		// full-screen agent actually paints, and one a byte stream mangles.
		frame := "\x1b[2J\x1b[H" +
			"\x1b[1;1H+--------------------------+" +
			"\x1b[3;1H+--------------------------+" +
			"\x1b[2;1H|                          |" +
			"\x1b[2;3HOverwrite file? [Y/n]"
		if err := processor.Consume([]byte(frame)); err != nil {
			t.Fatal(err)
		}
		if len(*matches) == 0 {
			t.Fatalf("a prompt painted into a frame was never detected; screen:\n%s",
				processor.Output())
		}
		if processor.Pending() == nil {
			t.Fatal("the session is not blocked on the prompt it painted")
		}
	})

	// The unsafe direction, and the reason a screen model matters at all: an
	// agent that withdraws its question must not leave an operator answering
	// something that is no longer on screen.
	t.Run("a question the agent erased is not reported again", func(t *testing.T) {
		processor, _ := repaintProcessor(t)
		processor.Resize(40, 10)
		if err := processor.Consume([]byte("\x1b[1;1HOverwrite file? [Y/n]")); err != nil {
			t.Fatal(err)
		}
		pending := processor.Pending()
		if pending == nil {
			t.Fatal("the prompt was not detected in the first place")
		}
		if err := processor.Acknowledge(pending.ID); err != nil {
			t.Fatal(err)
		}
		if err := processor.Consume([]byte("\x1b[2J\x1b[1;1HBuilding the project")); err != nil {
			t.Fatal(err)
		}
		if got := processor.Pending(); got != nil {
			t.Fatalf("an erased question came back as pending: %#v", got)
		}
		if out := processor.Output(); strings.Contains(out, "Overwrite") {
			t.Fatalf("the erased question is still on screen:\n%s", out)
		}
	})

	// Widening the substrate must not make documentation actionable, the same
	// guard the byte-window region carries.
	t.Run("a fenced example stays inert on a repainted screen", func(t *testing.T) {
		processor, matches := repaintProcessor(t)
		processor.Resize(60, 12)
		if err := processor.Consume([]byte(
			"\x1b[1;1HHere is what you will see:\r\n```\r\nOverwrite file? [Y/n]\r\n```\r\nProceeding.",
		)); err != nil {
			t.Fatal(err)
		}
		if len(*matches) != 0 {
			t.Fatalf("a fenced example became actionable: %v", *matches)
		}
	})
}

// A full-screen agent redraws its whole frame on every tick. If each redraw
// reported the question again, the operator would face a queue that refills
// faster than it can be answered — worse than the defect the rendered screen
// fixes.
func TestARepaintingAgentReportsItsPromptOnce(t *testing.T) {
	processor, matches := repaintProcessor(t)
	processor.Resize(40, 10)
	const frame = "\x1b[2J\x1b[1;1H+------------------+" +
		"\x1b[2;1H| Overwrite file? [Y/n]" +
		"\x1b[3;1H+------------------+" +
		"\x1b[4;1HPress enter to confirm"

	for range 10 {
		if err := processor.Consume([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	if len(*matches) != 1 {
		t.Fatalf("ten identical redraws produced %d events, want 1", len(*matches))
	}
	if processor.Pending() == nil {
		t.Fatal("the session stopped being blocked while its question is still on screen")
	}

	// Answering it must not let the next redraw of the same frame raise it
	// again: the question is still painted, but it has been dealt with.
	pending := processor.Pending()
	if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if err := processor.Consume([]byte(frame)); err != nil {
			t.Fatal(err)
		}
	}
	if got := processor.Pending(); got != nil {
		t.Fatalf("an answered question came back on the next redraw: %#v", got)
	}
}

// The complement of remembering an answer: the same question asked a SECOND
// time, after the agent has redrawn without it, is a second question and must
// be reported. Otherwise remembering would become a way to go silent.
func TestTheSameQuestionAskedAgainIsANewQuestion(t *testing.T) {
	processor, matches := repaintProcessor(t)
	processor.Resize(40, 10)

	ask := "\x1b[2J\x1b[1;1HOverwrite file? [Y/n]"
	work := "\x1b[2J\x1b[1;1HWriting the file\x1b[2;1HDone."

	if err := processor.Consume([]byte(ask)); err != nil {
		t.Fatal(err)
	}
	first := processor.Pending()
	if first == nil {
		t.Fatal("the first question was not detected")
	}
	if err := processor.Resolve(first.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	// The agent moves on: the question leaves the screen.
	if err := processor.Consume([]byte(work)); err != nil {
		t.Fatal(err)
	}
	if got := processor.Pending(); got != nil {
		t.Fatalf("a question that left the screen came back: %#v", got)
	}

	// It asks again. This is a new question about a new file operation.
	if err := processor.Consume([]byte(ask)); err != nil {
		t.Fatal(err)
	}
	second := processor.Pending()
	if second == nil {
		t.Fatalf("the same question asked again was swallowed; events so far: %v", *matches)
	}
	if second.ID == first.ID {
		t.Fatalf("the second question reused the answered occurrence: %#v", second)
	}
}

// Two regressions an adversarial review of this change found, both silent and
// both in the unsafe direction. They are kept as tests because neither was
// reachable from the cases already written.
func TestAnsweredMemoryIsBoundToItsRowNotToTheText(t *testing.T) {
	// The memory used to be released by looking for the answered text in the
	// rendered screen — which carries up to 512 rows of scrollback. A question
	// that had scrolled out of sight was still findable there, so it counted as
	// "still on screen" and every re-ask was swallowed for hundreds of lines.
	t.Run("a question that scrolled away can be asked again", func(t *testing.T) {
		processor, matches := repaintProcessor(t)
		processor.Resize(40, 6)
		if err := processor.Consume([]byte("\x1b[1;1HOverwrite file? [Y/n]")); err != nil {
			t.Fatal(err)
		}
		first := processor.Pending()
		if first == nil {
			t.Fatal("the first question was not detected")
		}
		if err := processor.Resolve(first.ID, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		for index := 1; index <= 12; index++ {
			if err := processor.Consume([]byte(fmt.Sprintf("\r\nline %d", index))); err != nil {
				t.Fatal(err)
			}
		}
		if err := processor.Consume([]byte("\r\nOverwrite file? [Y/n]")); err != nil {
			t.Fatal(err)
		}
		if processor.Pending() == nil {
			t.Fatalf("the re-asked question was swallowed; events: %v", *matches)
		}
	})

	// Text alone cannot tell "the question I answered is still sitting there"
	// from "the agent asked the same thing again lower down". Binding the
	// memory to the row it was answered on can.
	t.Run("scrolling away and asking again in one write is still a new question", func(t *testing.T) {
		processor, matches := repaintProcessor(t)
		processor.Resize(40, 6)
		if err := processor.Consume([]byte("\x1b[1;1HOverwrite file? [Y/n]")); err != nil {
			t.Fatal(err)
		}
		first := processor.Pending()
		if first == nil {
			t.Fatal("the first question was not detected")
		}
		if err := processor.Resolve(first.ID, func() error { return nil }); err != nil {
			t.Fatal(err)
		}
		var burst strings.Builder
		for index := 1; index <= 12; index++ {
			fmt.Fprintf(&burst, "\r\nline %d", index)
		}
		burst.WriteString("\r\nOverwrite file? [Y/n]")
		if err := processor.Consume([]byte(burst.String())); err != nil {
			t.Fatal(err)
		}
		if processor.Pending() == nil {
			t.Fatalf("a re-ask in the scrolling write was swallowed; events: %v", *matches)
		}
	})
}

// A grid materialises the gap between a question and a footer that a byte
// stream never wrote, so the blank-run bound — calibrated for output an agent
// actually emitted — rejected the standard full-screen layout as overtaken.
// This is the very agent the rendered screen exists for.
func TestAFooterFarBelowTheQuestionStillCounts(t *testing.T) {
	for _, gap := range []int{2, 6, 20} {
		t.Run(fmt.Sprintf("gap of %d rows", gap), func(t *testing.T) {
			processor, matches := repaintProcessor(t)
			processor.Resize(60, 30)
			frame := fmt.Sprintf(
				"\x1b[2J\x1b[1;1HOverwrite file? [Y/n]\x1b[%d;1Hpress enter to confirm", 2+gap)
			if err := processor.Consume([]byte(frame)); err != nil {
				t.Fatal(err)
			}
			if len(*matches) != 1 {
				t.Fatalf("a question %d rows above its footer produced %d events, want 1",
					gap, len(*matches))
			}
		})
	}
}

// Two questions can sit on the screen at once, and answering the second must
// not resurrect the first.
//
// The memory of what was answered was a single slot, so the second answer
// overwrote the first — and rescanRetainedWindow, which probes with a fresh
// state, then re-reported the first under its original signature. On a byte
// window this could not happen: the answered text was dropped outright.
func TestAnsweringOneQuestionDoesNotResurrectAnother(t *testing.T) {
	processor, matches := repaintProcessor(t)
	processor.Resize(40, 10)

	if err := processor.Consume([]byte("\x1b[1;1HOverwrite file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	first := processor.Pending()
	if first == nil {
		t.Fatal("the first question was not detected")
	}
	if err := processor.Resolve(first.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	// A second question arrives on another row, while the first is still
	// painted above it.
	if err := processor.Consume([]byte("\x1b[2;1HContinue? [y/n]")); err != nil {
		t.Fatal(err)
	}
	second := processor.Pending()
	if second == nil {
		t.Fatalf("the second question was never detected; events: %v", *matches)
	}
	if err := processor.Resolve(second.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	if got := processor.Pending(); got != nil {
		t.Fatalf("answering the second question resurrected an earlier one: %#v", got)
	}
}
