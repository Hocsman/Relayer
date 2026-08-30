package adapters

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// withdrawProcessor is repaintProcessor plus a record of what was withdrawn, so
// a test can tell "the operator stopped being asked" from "nobody was told".
func withdrawProcessor(t *testing.T) (*Processor, *[]Event, *[]Event) {
	t.Helper()
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	raised := []Event{}
	withdrawn := []Event{}
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session", "agent", GenericID),
		64*1024,
		Hooks{
			OnEvent:          func(event Event) { raised = append(raised, event) },
			OnEventWithdrawn: func(event Event) { withdrawn = append(withdrawn, event) },
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return processor, &raised, &withdrawn
}

// A question the agent takes back stops being offered.
//
// The agent withdraws its own request — a timeout, a cancellation, an ESC typed
// in the attached view. Before this, the occurrence stayed pending forever and
// Resolve happily delivered the answer into a shell that was back at its
// prompt.
func TestAQuestionTheAgentErasedStopsBeingPending(t *testing.T) {
	processor, raised, withdrawn := withdrawProcessor(t)
	processor.Resize(40, 10)

	if err := processor.Consume([]byte(
		"\x1b[2J\x1b[1;1HDo you want to continue? [y/n]\x1b[2;1H  [y] yes\x1b[3;1H  [n] no",
	)); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatal("the question was not detected in the first place")
	}
	offered := (*raised)[0]

	if err := processor.Consume([]byte("\x1b[2J\x1b[1;1Hrequest cancelled\x1b[2;1H$ ")); err != nil {
		t.Fatal(err)
	}
	if output := processor.Output(); strings.Contains(output, "continue?") {
		t.Fatalf("scenario broken: the question is still on the rendered screen:\n%s", output)
	}
	if got := processor.Pending(); got != nil {
		t.Fatalf("a question the agent erased is still offered to the operator: %#v", got)
	}
	if len(*withdrawn) != 1 || (*withdrawn)[0].ID != offered.ID {
		t.Fatalf("the withdrawal was not reported to the operator: %#v", *withdrawn)
	}

	// The decision an operator would deliver a moment too late is refused
	// rather than written into whatever the terminal is doing now.
	err := processor.Resolve(offered.ID, func() error {
		t.Fatal("a withdrawn decision reached the terminal")
		return nil
	})
	if !errors.Is(err, ErrEventMismatch) {
		t.Fatalf("Resolve after a withdrawal returned %v, want ErrEventMismatch", err)
	}
}

// The dangerous direction: a question still painted is still asked.
//
// A repainting agent redraws the same frame several times a second. Withdrawing
// on any of those redraws would stop the operator being asked about a question
// that is plainly on screen, and the agent would wait for an answer nobody is
// prompted to give.
func TestAQuestionStillPaintedIsNotWithdrawn(t *testing.T) {
	processor, _, withdrawn := withdrawProcessor(t)
	processor.Resize(40, 10)

	frame := "\x1b[2J\x1b[1;1HWorking on it\x1b[2;1HOverwrite file? [Y/n]\x1b[3;1H  [y] yes\x1b[4;1H  [n] no"
	if err := processor.Consume([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	pending := processor.Pending()
	if pending == nil {
		t.Fatal("the question was not detected in the first place")
	}
	for repaint := 0; repaint < 5; repaint++ {
		if err := processor.Consume([]byte(frame)); err != nil {
			t.Fatal(err)
		}
		if processor.Pending() == nil {
			t.Fatalf("repaint %d withdrew a question that is still on screen", repaint+1)
		}
	}
	if len(*withdrawn) != 0 {
		t.Fatalf("a question still on screen was withdrawn: %#v", *withdrawn)
	}
	if got := processor.Pending(); got.ID != pending.ID {
		t.Fatalf("the pending occurrence changed identity across repaints: %q then %q", pending.ID, got.ID)
	}
}

// An agent that only appends keeps the behaviour it always had.
//
// The byte window is exact for it, the grid substrate is never activated, and
// nothing may be withdrawn on the strength of a screen the agent never painted.
func TestAnAppendOnlyAgentNeverHasItsQuestionWithdrawn(t *testing.T) {
	processor, _, withdrawn := withdrawProcessor(t)
	processor.Resize(40, 10)

	if err := processor.Consume([]byte("Overwrite file? [Y/n]\n")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatal("the question was not detected in the first place")
	}
	for line := 0; line < 20; line++ {
		if err := processor.Consume([]byte("still working\n")); err != nil {
			t.Fatal(err)
		}
	}
	if processor.Pending() == nil {
		t.Fatal("an append-only agent had its question withdrawn")
	}
	if len(*withdrawn) != 0 {
		t.Fatalf("an append-only agent had its question withdrawn: %#v", *withdrawn)
	}
}

// A withdrawn question is not an answered one.
//
// Nobody decided anything, so if the agent paints it again the operator has to
// be asked again. Routing the withdrawal through acknowledge would have
// remembered it as answered and swallowed the second ask.
func TestAWithdrawnQuestionAskedAgainIsAskedAgain(t *testing.T) {
	processor, raised, _ := withdrawProcessor(t)
	processor.Resize(40, 10)

	ask := "\x1b[2J\x1b[1;1HDeploy to production? [y/n]\x1b[2;1H  [y] yes"
	if err := processor.Consume([]byte(ask)); err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte("\x1b[2J\x1b[1;1Hnever mind\x1b[2;1H$ ")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() != nil {
		t.Fatal("the withdrawal did not happen, so the re-ask proves nothing")
	}
	if err := processor.Consume([]byte(ask)); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatal("a question asked again after being withdrawn was swallowed")
	}
	if len(*raised) != 2 {
		t.Fatalf("the re-ask produced %d event(s), want 2", len(*raised))
	}
	if (*raised)[0].ID == (*raised)[1].ID {
		t.Fatal("the re-ask reused the withdrawn occurrence ID")
	}
}

// A question that only ever lived in the byte window is never withdrawn.
//
// The occurrence carries text this grid never rendered, so its absence from the
// grid says nothing about whether the agent is still waiting. Only a question
// that has been seen on screen can be judged to have left it.
func TestAQuestionNeverSeenOnTheGridIsNeverWithdrawn(t *testing.T) {
	processor, _, withdrawn := withdrawProcessor(t)
	processor.Resize(40, 10)

	if err := processor.Consume([]byte("Overwrite file? [Y/n]\n")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatal("the question was not detected in the first place")
	}
	// The agent starts repainting, and never paints that question.
	if err := processor.Consume([]byte("\x1b[2J\x1b[1;1Hsomething else entirely")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatal("an occurrence that was never seen on the grid was withdrawn on its absence")
	}
	if len(*withdrawn) != 0 {
		t.Fatalf("an unsighted occurrence was withdrawn: %#v", *withdrawn)
	}
}

// The other dangerous direction: a question that merely SCROLLED away.
//
// Absence from the grid has two opposite causes. The agent erasing its question
// means it is not being asked any more; its own output pushing the question off
// the top does not — it may still be waiting. Withdrawing on the second is the
// same failure as never reporting it: the operator is not asked and the agent
// waits forever.
func TestAQuestionScrolledOffTheGridIsNotWithdrawn(t *testing.T) {
	processor, _, withdrawn := withdrawProcessor(t)
	processor.Resize(40, 6)

	if err := processor.Consume([]byte("\x1b[2J\x1b[1;1HOverwrite file? [Y/n]\x1b[2;1H  [y] yes")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatal("the question was not detected in the first place")
	}
	// The agent keeps printing underneath, and its own output pushes the
	// question off the top of the grid.
	for line := 0; line < 12; line++ {
		if err := processor.Consume([]byte("still working\r\n")); err != nil {
			t.Fatal(err)
		}
	}
	if strings.Contains(visibleGrid(t, processor), "Overwrite file?") {
		t.Fatal("scenario broken: the question never left the visible grid")
	}
	if processor.Pending() == nil {
		t.Fatalf("a question that only scrolled away was withdrawn: %#v", *withdrawn)
	}
}

// A pager, a diff or an editor takes the whole grid away and gives it back.
// Nothing was erased, so nothing may be withdrawn.
func TestAPagerRoundTripDoesNotWithdrawTheQuestion(t *testing.T) {
	processor, _, withdrawn := withdrawProcessor(t)
	processor.Resize(40, 10)

	if err := processor.Consume([]byte("\x1b[2J\x1b[1;1HOverwrite file? [Y/n]\x1b[2;1H  [y] yes")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatal("the question was not detected in the first place")
	}
	if err := processor.Consume([]byte("\x1b[?1049h\x1b[2J\x1b[1;1Hsome diff output")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatalf("entering a pager withdrew the question: %#v", *withdrawn)
	}
	if err := processor.Consume([]byte("\x1b[?1049l")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatalf("a pager round trip withdrew the question: %#v", *withdrawn)
	}
}

// A resize truncates rows and columns without the agent writing anything, so it
// is not evidence that the question stopped being asked.
func TestAResizeDoesNotWithdrawTheQuestion(t *testing.T) {
	processor, _, withdrawn := withdrawProcessor(t)
	processor.Resize(60, 10)

	if err := processor.Consume([]byte("\x1b[2J\x1b[9;1HOverwrite file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatal("the question was not detected in the first place")
	}
	processor.Resize(30, 4)
	if err := processor.Consume([]byte("\x1b[4;1Hworking")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatalf("a resize withdrew the question: %#v", *withdrawn)
	}
}

func visibleGrid(t *testing.T, processor *Processor) string {
	t.Helper()
	processor.mu.Lock()
	defer processor.mu.Unlock()
	return processor.screen.VisibleText()
}

// A frame is bigger than a PTY read, so the grid is routinely observed halfway
// through a repaint: erased, not yet redrawn. Concluding "withdrawn" there
// destroys and recreates the occurrence on every frame, changes its ID under
// the operator, and makes the decision they are about to take fail.
func TestAFrameSplitAcrossReadsDoesNotWithdrawTheQuestion(t *testing.T) {
	frame := "\x1b[2J\x1b[1;1HWorking on it\x1b[2;1HOverwrite file? [Y/n]\x1b[3;1H  [y] yes\x1b[4;1H  [n] no"
	for _, chunk := range []int{16, 64, 128, len(frame)} {
		t.Run(fmt.Sprintf("%d byte reads", chunk), func(t *testing.T) {
			processor, raised, withdrawn := withdrawProcessor(t)
			processor.Resize(40, 10)
			for repaint := 0; repaint < 6; repaint++ {
				for offset := 0; offset < len(frame); offset += chunk {
					end := min(offset+chunk, len(frame))
					if err := processor.Consume([]byte(frame[offset:end])); err != nil {
						t.Fatal(err)
					}
				}
			}
			if len(*withdrawn) != 0 {
				t.Fatalf("%d withdrawal(s) while the question was painted throughout", len(*withdrawn))
			}
			// The occurrence must also keep its identity, or the decision the
			// operator is looking at stops matching the one the core holds.
			if len(*raised) != 1 {
				t.Fatalf("%d occurrences for one question", len(*raised))
			}
		})
	}
}

// The same characters, split differently across rows, are the same question.
//
// A grid only joins two rows when the first is marked wrapped, and that mark is
// lost when the agent repaints those rows by addressing them. The question then
// stops being one logical line without a single character changing on screen.
func TestARewrappedQuestionIsNotWithdrawn(t *testing.T) {
	processor, raised, withdrawn := withdrawProcessor(t)
	processor.Resize(40, 8)

	if err := processor.Consume([]byte("\x1b[1;1Hworking\r\n")); err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte("[tool] running deploy.sh in project Do you want to continue?")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatalf("the wrapped question was not detected in the first place; raised=%d", len(*raised))
	}
	if err := processor.Consume([]byte("\x1b[1;1Hworking.")); err != nil {
		t.Fatal(err)
	}
	// The agent repaints its two rows itself, exactly the same characters.
	if err := processor.Consume([]byte(
		"\x1b[2;1H\x1b[2K[tool] running deploy.sh in project Do y\x1b[3;1H\x1b[2Kou want to continue?",
	)); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatalf("a question whose characters never changed was withdrawn: %#v", *withdrawn)
	}
}
