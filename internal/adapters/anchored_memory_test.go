package adapters

import (
	"strings"
	"testing"
)

func anchoredProcessor(t *testing.T) (*Processor, *[]string) {
	t.Helper()
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	raised := []string{}
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session", "agent", GenericID),
		64*1024,
		Hooks{OnEvent: func(event Event) { raised = append(raised, event.Match) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	return processor, &raised
}

// The memory anchors on the row the question was DETECTED on.
//
// It used to be located afterwards by searching the grid for the matched text,
// which returns the last row carrying it. Detection excludes several contexts
// from being candidates — a `log:` prefix among them — but that search knows
// nothing of the exclusions, so an excluded line carrying the same fragment
// captured the anchor. The entry then watched a line that never changes, so it
// never expired, and the re-asked question was swallowed for good.
func TestTheAnchorIgnoresALineThatWasNeverACandidate(t *testing.T) {
	processor, raised := anchoredProcessor(t)
	processor.Resize(60, 12)

	if err := processor.Consume([]byte(
		"\x1b[2J\x1b[1;1HDeploy to production? [y/n]\x1b[3;1Hlog: answered Continue? [y/n]",
	)); err != nil {
		t.Fatal(err)
	}
	pending := processor.Pending()
	if pending == nil {
		t.Fatalf("the question was not detected; raised=%v", *raised)
	}
	if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	// The agent takes its question down and asks it again on the same row.
	if err := processor.Consume([]byte("\x1b[1;1H\x1b[2Kdeploying...")); err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte("\x1b[1;1H\x1b[2KDeploy to production? [y/n]")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatalf("the re-asked question was swallowed; raised=%v", *raised)
	}
	if len(*raised) != 2 {
		t.Fatalf("raised %d occurrence(s), want 2: %v", len(*raised), *raised)
	}
}

// The answered question stays suppressed while it is still painted.
//
// This is the other direction, and the reason the entry cannot simply be
// dropped: a repainting agent shows the answered question until it redraws
// without it, and asking about it again would send a second keystroke into an
// agent that has already consumed the first.
func TestAnAnsweredQuestionStaysSuppressedWhileItIsPainted(t *testing.T) {
	processor, raised := anchoredProcessor(t)
	processor.Resize(60, 12)

	frame := "\x1b[2J\x1b[1;1HOverwrite file? [Y/n]\x1b[2;1H  [y] yes"
	if err := processor.Consume([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	pending := processor.Pending()
	if pending == nil {
		t.Fatal("the question was not detected in the first place")
	}
	if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	for repaint := 0; repaint < 4; repaint++ {
		if err := processor.Consume([]byte(frame)); err != nil {
			t.Fatal(err)
		}
		if got := processor.Pending(); got != nil {
			t.Fatalf("repaint %d asked the answered question again; raised=%v", repaint+1, *raised)
		}
	}
}

// A scroll region whose top is row 0 moved the old absolute origin while the
// rows below the region had not moved at all, so an answered question below it
// was released one write later and came straight back.
func TestAScrollRegionDoesNotReleaseAQuestionItNeverMoved(t *testing.T) {
	processor, raised := anchoredProcessor(t)
	processor.Resize(40, 12)

	if err := processor.Consume([]byte("\x1b[2J\x1b[6;1HOverwrite file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	pending := processor.Pending()
	if pending == nil {
		t.Fatal("the question was not detected in the first place")
	}
	if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	// Region rows 0..3, one scroll inside it: row 5 does not move.
	if err := processor.Consume([]byte("\x1b[1;4r\x1b[4;1H\nprogress")); err != nil {
		t.Fatal(err)
	}
	if got := processor.Pending(); got != nil {
		t.Fatalf("a scroll region re-asked a question it never moved; raised=%v", *raised)
	}
}

// Abandoning a question is not answering it.
//
// A snapshot that comes back empty, a snapshot with nothing detectable on it, a
// process that exited: nobody decided anything on any of those paths, so
// nothing may be written into a memory whose whole purpose is "the operator has
// already dealt with this". Writing there left an entry describing a screen the
// caller had just declared stale.
func TestAbandoningAQuestionRemembersNothing(t *testing.T) {
	for _, testCase := range []struct {
		name     string
		abandon  func(t *testing.T, processor *Processor)
		snapshot string
	}{
		{
			name: "a snapshot with nothing on it",
			abandon: func(t *testing.T, processor *Processor) {
				if _, _, err := processor.ReconcileSnapshot([]byte("   \n  \n")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "a snapshot with nothing detectable on it",
			abandon: func(t *testing.T, processor *Processor) {
				if _, _, err := processor.ReconcileSnapshot([]byte("building the project\n")); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			name: "the process exited",
			abandon: func(t *testing.T, processor *Processor) {
				processor.MarkProcessExitEvent(nil, false)
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			processor, _ := anchoredProcessor(t)
			processor.Resize(60, 12)
			if err := processor.Consume([]byte("\x1b[2J\x1b[1;1HOverwrite file? [Y/n]")); err != nil {
				t.Fatal(err)
			}
			if processor.Pending() == nil {
				t.Fatal("the question was not detected in the first place")
			}
			testCase.abandon(t, processor)
			if remembered := processor.state.pendingAnswers(); len(remembered) != 0 {
				t.Fatalf("abandoning the question remembered it as answered: %+v", remembered)
			}
		})
	}
}

// An agent that only appends never reaches the grid substrate, so it keeps the
// behaviour it always had: no anchor is invented for it and nothing about its
// detection changes.
func TestAnAppendOnlyAgentIsUntouched(t *testing.T) {
	processor, raised := anchoredProcessor(t)
	processor.Resize(60, 12)

	if err := processor.Consume([]byte("Overwrite file? [Y/n]\n")); err != nil {
		t.Fatal(err)
	}
	pending := processor.Pending()
	if pending == nil {
		t.Fatal("the question was not detected in the first place")
	}
	if pending.anchor != 0 {
		t.Fatalf("an occurrence raised off the byte window carries a screen row: %d", pending.anchor)
	}
	if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte("Overwrite file? [Y/n]\n")); err != nil {
		t.Fatal(err)
	}
	if len(*raised) != 2 {
		t.Fatalf("raised %d occurrence(s), want 2: %v", len(*raised), *raised)
	}
}

// The frame moves while the operator is deciding.
//
// The anchor is taken when the question is detected and the answer comes many
// frames later. A full-screen agent that erases and repaints its frame at a
// different height moves the question onto another row without ever taking it
// down — an erase keeps a row's identity, so the content slides by one. An
// anchor frozen at detection then names the line above, the memory is born
// stale and is dropped on the next write, and the operator is asked a second
// time for something already delivered to the agent.
func TestAFrameThatMovesBeforeTheAnswerDoesNotReAskTheQuestion(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		moved string
	}{
		{
			name:  "the frame is repainted one row lower",
			moved: "\x1b[2J\x1b[1;1Hbuilding\x1b[2;1HOverwrite file? [Y/n]\x1b[3;1H  [y] yes",
		},
		{
			name:  "the frame is repainted one row higher",
			moved: "\x1b[2J\x1b[1;1HOverwrite file? [Y/n]\x1b[2;1H  [y] yes",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			processor, raised := anchoredProcessor(t)
			processor.Resize(60, 12)

			if err := processor.Consume([]byte(
				"\x1b[2J\x1b[1;1Hworking\x1b[2;1HOverwrite file? [Y/n]\x1b[3;1H  [y] yes",
			)); err != nil {
				t.Fatal(err)
			}
			pending := processor.Pending()
			if pending == nil {
				t.Fatal("the question was not detected in the first place")
			}
			// The agent redraws while the operator is deciding. The question is
			// never taken down.
			if err := processor.Consume([]byte(testCase.moved)); err != nil {
				t.Fatal(err)
			}
			if processor.Pending() == nil {
				t.Fatal("scenario broken: the question was withdrawn")
			}
			if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := processor.Consume([]byte(testCase.moved)); err != nil {
				t.Fatal(err)
			}
			if got := processor.Pending(); got != nil {
				t.Fatalf("the answered question was asked again; raised=%v", *raised)
			}
			if len(*raised) != 1 {
				t.Fatalf("raised %d occurrences for one question: %v", len(*raised), *raised)
			}
		})
	}
}

// A match is not always one line.
//
// The vendor rules are (?is) regexes that run across a whole prompt block —
// Claude's folder-trust prompt spans five lines. Comparing such a match against
// a single logical line can only ever answer false, which releases the memory
// of a question plainly still on screen and puts it to the operator a second
// time, the second Enter reaching an agent that has already left the prompt.
func TestAMultiLineMatchStaysSuppressedWhileItIsPainted(t *testing.T) {
	adapter, err := NewClaudeAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	raised := []string{}
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session", "agent", adapter.ID()),
		64*1024,
		Hooks{OnEvent: func(event Event) { raised = append(raised, event.Match) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	processor.Resize(60, 12)

	frame := "\x1b[2J\x1b[1;1HQuick safety check: Is this a project you created or one you trust?" +
		"\x1b[3;1H1. Yes, I trust this folder\x1b[4;1H2. No, exit" +
		"\x1b[5;1HEnter to confirm Esc to cancel"
	if err := processor.Consume([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	pending := processor.Pending()
	if pending == nil {
		t.Fatalf("the vendor prompt was not detected; raised=%v", raised)
	}
	if !strings.Contains(pending.Match, "\n") {
		t.Fatalf("the scenario needs a match spanning rows, got %q", pending.Match)
	}
	if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	// The agent has not redrawn without it, so it is still painted.
	for repaint := 0; repaint < 3; repaint++ {
		if err := processor.Consume([]byte(frame)); err != nil {
			t.Fatal(err)
		}
		if got := processor.Pending(); got != nil {
			t.Fatalf("repaint %d asked the answered prompt again; raised=%d", repaint+1, len(raised))
		}
	}
}

// The anchor of a pending occurrence is kept current, but never by moving it
// onto a line detection would have excluded.
//
// Uniqueness is not candidacy. During a repaint the only row carrying `[y/n]`
// can be a `log:` echo of the answer, and an anchor parked there never changes
// again — so the entry never expires and every later question of the same
// signature is silenced. That is the reasoning taken out of LocateRow, and it
// has to hold at every site that adopts a row by text.
func TestThePendingAnchorDoesNotMoveOntoAnExcludedLine(t *testing.T) {
	processor, raised := anchoredProcessor(t)
	processor.Resize(60, 12)

	if err := processor.Consume([]byte("\x1b[2J\x1b[1;1HDeploy to production? [y/n]")); err != nil {
		t.Fatal(err)
	}
	pending := processor.Pending()
	if pending == nil {
		t.Fatal("the question was not detected in the first place")
	}
	// A repaint caught between two reads: for one frame the only row carrying
	// the fragment is a line detection would never report.
	if err := processor.Consume([]byte("\x1b[2J\x1b[3;1Hlog: answered Deploy to production? [y/n]")); err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte("\x1b[1;1HDeploy to production? [y/n]")); err != nil {
		t.Fatal(err)
	}
	if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	// The agent takes its question down, clears the log line — real content
	// below a match is rejected as overtaken, which is #29 and not this — and
	// asks a different question.
	if err := processor.Consume([]byte("\x1b[1;1H\x1b[2Kdeploying...\x1b[3;1H\x1b[2K")); err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte("\x1b[1;1H\x1b[2KRoll back the release? [y/n]")); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() == nil {
		t.Fatalf("a later question was silenced by an anchor parked on a log line; raised=%v", *raised)
	}
}
