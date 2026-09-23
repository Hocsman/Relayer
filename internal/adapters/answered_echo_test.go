package adapters

import (
	"strings"
	"testing"
)

// conptyFirstWrites are the first two writes of every agent ConPTY runs, byte
// for byte from a capture on Windows 11, with the banner and the window title
// replaced. The erase and the home in the second one are what switch detection
// onto the rendered screen, so on Windows every session is on it before its
// first question.
var conptyFirstWrites = []string{
	"\x1b[?9001h\x1b[?1004h",
	"\x1b[?25l\x1b[2J\x1b[m\x1b[Hagent ready\r\n\x1b]0;agent\a\x1b[?25h",
}

const (
	aiderApplyPrompt      = "Apply changes? (Y)es/(N)o/(D)escribe [Yes]: "
	gooseToolPrompt       = "Approve tool execution? (y/n) "
	interpreterCodePrompt = "Would you like to run this code? (y/n) "
	overwritePrompt       = "Overwrite file notes.txt? [y/n] "
	codexCommandPrompt    = "Would you like to run the following command?\r\n  $ ls\r\n  Yes, proceed (y)\r\n  No, and tell Codex what to do differently (esc)\r\nPress enter to confirm or esc to cancel"
	agentNextOutput       = "\r\nApplying the change...\r\n"
	escapeOnlyWrite       = "\x1b[?25h"
	echoedAnswerNewline   = "\r\n"
)

func newAdapterForTest(t *testing.T, id string) Adapter {
	t.Helper()
	for _, candidate := range everyAdapter {
		if candidate.name == id {
			adapter, err := candidate.new(DefaultPatterns())
			if err != nil {
				t.Fatal(err)
			}
			return adapter
		}
	}
	t.Fatalf("no adapter %q", id)
	return nil
}

// answeredOnConPTY starts an agent the way ConPTY does, lets it ask one
// question, and delivers an answer to it. It returns the processor and every
// occurrence raised so far, which is the one question.
func answeredOnConPTY(t *testing.T, adapterID, prompt string) (*Processor, *[]Event) {
	t.Helper()
	raised := []Event{}
	processor, err := NewProcessor(
		newAdapterForTest(t, adapterID),
		NewDetectionState("session", "agent", adapterID),
		64*1024,
		Hooks{OnEvent: func(event Event) { raised = append(raised, event) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	processor.Resize(120, 30)
	for _, write := range append(append([]string(nil), conptyFirstWrites...), prompt) {
		if err := processor.Consume([]byte(write)); err != nil {
			t.Fatal(err)
		}
	}
	if len(raised) != 1 {
		t.Fatalf("the question raised %d occurrence(s), want 1", len(raised))
	}
	if err := processor.Resolve(raised[0].ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	return processor, &raised
}

// describeRaised names what was raised, for a failure message. Matches only:
// the lines are terminal text.
func describeRaised(raised []Event) string {
	matches := make([]string, 0, len(raised))
	for _, event := range raised {
		matches = append(matches, event.Match)
	}
	return strings.Join(matches, " | ")
}

// An answer is typed at the question, and on a rendered screen the question
// stays painted after it. So the next write re-read the answered question as a
// new one, under a new ID: a terminal in cooked mode echoes the answer onto the
// question's line ("...[Yes]: y"), ConPTY flushes that echo as a write of its
// own whenever the agent is slow to print, and even a write that carries no
// text at all makes detection read the unchanged screen again.
//
// Aider, Goose and Open Interpreter remembered nothing about an answered
// question, and the generic adapter, which Claude delegates to, compared whole
// lines, so the echo defeated it. The desktop then typed a second automatic
// "y" into the agent, a human was shown a card for a question already
// answered, and while that card waited the agent's next real question was
// never looked at. Codex was never affected: its footer must end the line.
func TestTheEchoOfAnAnswerIsNotAskedAgain(t *testing.T) {
	for _, question := range []struct {
		adapter string
		prompt  string
		answer  string
	}{
		{adapter: AiderID, prompt: aiderApplyPrompt, answer: "y"},
		{adapter: GooseID, prompt: gooseToolPrompt, answer: "y"},
		{adapter: OpenInterpreterID, prompt: interpreterCodePrompt, answer: "y"},
		{adapter: GenericID, prompt: overwritePrompt, answer: "yes"},
		{adapter: ClaudeID, prompt: overwritePrompt, answer: "yes"},
		// Unaffected before and after; kept so that it stays so.
		{adapter: CodexID, prompt: codexCommandPrompt, answer: "y"},
	} {
		for _, after := range []struct {
			name   string
			writes func(answer string) []string
		}{
			{
				name:   "the echoed answer",
				writes: func(answer string) []string { return []string{answer + echoedAnswerNewline} },
			},
			{
				name:   "a write with no text",
				writes: func(string) []string { return []string{escapeOnlyWrite} },
			},
			{
				name: "the echo, then the agent's next output",
				writes: func(answer string) []string {
					return []string{answer + echoedAnswerNewline, agentNextOutput}
				},
			},
		} {
			t.Run(question.adapter+"/"+after.name, func(t *testing.T) {
				processor, raised := answeredOnConPTY(t, question.adapter, question.prompt)
				for index, write := range after.writes(question.answer) {
					if err := processor.Consume([]byte(write)); err != nil {
						t.Fatal(err)
					}
					if len(*raised) != 1 {
						t.Fatalf("write %d after the answer asked the answered question again: %d occurrences (%s)",
							index+1, len(*raised), describeRaised(*raised))
					}
				}
				if pending := processor.Pending(); pending != nil {
					t.Fatalf("the session is blocked on an answered question: %q", pending.Match)
				}
			})
		}
	}
}

// guardStep is one write and how many occurrences the session must have
// raised once it has been consumed.
type guardStep struct {
	write      string
	wantRaised int
}

func replayAfterTheAnswer(t *testing.T, adapterID, prompt string, steps []guardStep) *Processor {
	t.Helper()
	processor, raised := answeredOnConPTY(t, adapterID, prompt)
	for index, step := range steps {
		if err := processor.Consume([]byte(step.write)); err != nil {
			t.Fatal(err)
		}
		if len(*raised) != step.wantRaised {
			t.Fatalf("after write %d (%q): %d occurrences, want %d (%s)\nscreen:\n%s",
				index+1, step.write, len(*raised), step.wantRaised, describeRaised(*raised), processor.Output())
		}
	}
	return processor
}

// What remembering an answer must never do is swallow the next question.
//
// The answered question is the one on its own row. An agent that asks the same
// thing again asks it on a new row, and the operator has to be asked, whether
// the old question is still painted above it or not. Before, the echo raised a
// repeat of the answered question; while that repeat was pending, detection
// stopped, so the real re-ask was hidden behind it. And the generic adapter
// swallowed an identical question on a new row even with no echo, because it
// compared the line and not the row.
func TestAQuestionAskedAgainOnANewRowIsAsked(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		adapter string
		prompt  string
		steps   []guardStep
	}{
		{
			name:    "aider asks the same question after its output",
			adapter: AiderID,
			prompt:  aiderApplyPrompt,
			steps:   []guardStep{{"y\r\n", 1}, {agentNextOutput, 1}, {aiderApplyPrompt, 2}},
		},
		{
			name:    "aider rejects the answer and asks again on the next row",
			adapter: AiderID,
			prompt:  aiderApplyPrompt,
			steps:   []guardStep{{"q\r\n", 1}, {aiderApplyPrompt, 2}},
		},
		{
			name:    "goose asks the same question after its output",
			adapter: GooseID,
			prompt:  gooseToolPrompt,
			steps:   []guardStep{{"y\r\n", 1}, {agentNextOutput, 1}, {gooseToolPrompt, 2}},
		},
		{
			name:    "open interpreter asks the same question after its output",
			adapter: OpenInterpreterID,
			prompt:  interpreterCodePrompt,
			steps:   []guardStep{{"y\r\n", 1}, {agentNextOutput, 1}, {interpreterCodePrompt, 2}},
		},
		{
			name:    "the generic adapter is asked the identical question after the echo",
			adapter: GenericID,
			prompt:  overwritePrompt,
			steps:   []guardStep{{"yes\r\n", 1}, {agentNextOutput, 1}, {overwritePrompt, 2}},
		},
		{
			name:    "claude is asked the identical question after the echo",
			adapter: ClaudeID,
			prompt:  overwritePrompt,
			steps:   []guardStep{{"yes\r\n", 1}, {agentNextOutput, 1}, {overwritePrompt, 2}},
		},
		{
			name:    "the generic adapter is asked the identical question with no echo",
			adapter: GenericID,
			prompt:  overwritePrompt,
			steps:   []guardStep{{"\r\n", 1}, {agentNextOutput, 1}, {overwritePrompt, 2}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			processor := replayAfterTheAnswer(t, testCase.adapter, testCase.prompt, testCase.steps)
			if processor.Pending() == nil {
				t.Fatal("the question asked again is not pending")
			}
		})
	}
}

// The row that carried the answered question is remembered as answered only
// while it still begins with that question. An agent that erases the line and
// asks something else on it is asking something else.
func TestAnotherQuestionOnTheAnsweredRowIsAsked(t *testing.T) {
	for _, testCase := range []struct {
		name    string
		adapter string
		prompt  string
		steps   []guardStep
	}{
		{
			name:    "generic",
			adapter: GenericID,
			prompt:  overwritePrompt,
			steps:   []guardStep{{"yes", 1}, {"\r\x1b[KDelete file notes.txt? [y/n] ", 2}},
		},
		{
			name:    "aider",
			adapter: AiderID,
			prompt:  aiderApplyPrompt,
			steps:   []guardStep{{"y", 1}, {"\r\x1b[KRun shell command? (Y)es/(N)o [Yes]: ", 2}},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			replayAfterTheAnswer(t, testCase.adapter, testCase.prompt, testCase.steps)
		})
	}
}

// A vendor occurrence's Match is the label of a kind of prompt, not the text
// on the screen: Aider's "Apply edit to notes.py?" is reported as "Apply
// changes?". The answered memory is released when its row stops showing the
// question, and it looked for the label, which was never painted, so the entry
// went on the next write and the question came straight back. The row is
// checked for the question line as well.
func TestAVendorQuestionWhoseLabelIsNotPaintedStaysAnswered(t *testing.T) {
	for _, testCase := range []struct {
		adapter string
		prompt  string
	}{
		{adapter: AiderID, prompt: "Apply edit to notes.py? (Y)es/(N)o [Yes]: "},
		{adapter: GooseID, prompt: "Allow Goose to run: 'ls -la' (y/n) "},
		{adapter: OpenInterpreterID, prompt: "Execute this Python code? (y/n) "},
	} {
		for _, after := range []struct {
			name   string
			writes []string
		}{
			{name: "the echoed answer", writes: []string{"y\r\n"}},
			{name: "writes with no text", writes: []string{escapeOnlyWrite, "\x1b[?25l", escapeOnlyWrite}},
		} {
			t.Run(testCase.adapter+"/"+after.name, func(t *testing.T) {
				processor, raised := answeredOnConPTY(t, testCase.adapter, testCase.prompt)
				if strings.Contains(testCase.prompt, (*raised)[0].Match) {
					t.Fatalf("scenario broken: the label %q is painted", (*raised)[0].Match)
				}
				for index, write := range after.writes {
					if err := processor.Consume([]byte(write)); err != nil {
						t.Fatal(err)
					}
					if len(*raised) != 1 {
						t.Fatalf("write %d after the answer asked the answered question again: %d occurrences",
							index+1, len(*raised))
					}
				}
				if processor.Pending() != nil {
					t.Fatal("the session is blocked on an answered question")
				}
			})
		}
	}
}

// What the row-less comparison was protecting, and still is for the generic
// adapter.
//
// A full-screen agent erases its frame and repaints it; the erase keeps each
// row's identity and the repaint may put the question a row higher or lower.
// Caught between the two, the answered question's own row is blank and the
// same words are on another row, and they are the answered question moved, not
// a new one. Asking there would type a second answer.
//
// Aider, Goose and Open Interpreter ask it again, and are meant to. From the
// screen alone this frame cannot be told from the one after a clear, where the
// answered row is blank and the agent asks the identical question on another
// row: Aider asks for every shell command with the same line. The generic
// adapter already lost that question before it knew rows, so for it the blank
// row costs nothing new. The vendor adapters never compared anything, so for
// them it would silence a question that used to be asked, and a question put to
// nobody blocks the agent without a sign. The cost here is what those adapters
// always did in this frame: the moved question is asked a second time.
func TestARepaintThatMovesTheAnsweredQuestionOffItsBlankRowDoesNotAskItAgain(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		adapter    string
		prompt     string
		wantRaised int
	}{
		{name: "generic does not ask it again", adapter: GenericID, prompt: overwritePrompt, wantRaised: 1},
		{name: "aider asks it again", adapter: AiderID, prompt: aiderApplyPrompt, wantRaised: 2},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			processor := replayAfterTheAnswer(t, testCase.adapter, testCase.prompt, []guardStep{
				// The question was on row 2. The frame is erased and redrawn
				// with it on row 4, and rows 1 to 3 are left for later writes.
				{"\x1b[2J\x1b[4;1H" + testCase.prompt, testCase.wantRaised},
				{"\x1b[1;1Hagent ready", testCase.wantRaised},
			})
			if asked := processor.Pending() != nil; asked != (testCase.wantRaised > 1) {
				t.Fatalf("the moved question is pending: %t, want %t", asked, testCase.wantRaised > 1)
			}
		})
	}
}

// The same label can be painted on another row: an earlier question of the same
// kind, answered, still above the one being asked. A pending question was kept
// on its row by looking for its label, which moved it onto that earlier row
// the moment anything was written; the answer was remembered there, and the
// echo on the question's real row raised it again. It is looked for by its
// line.
func TestAQuestionIsRememberedOnItsOwnRowWhenItsLabelIsPaintedAbove(t *testing.T) {
	for _, testCase := range []struct {
		adapter   string
		first     string
		unlabeled string
	}{
		{adapter: AiderID, first: aiderApplyPrompt, unlabeled: "Apply edit to bar.py? (Y)es/(N)o [Yes]: "},
		{adapter: GooseID, first: gooseToolPrompt, unlabeled: "Allow Goose to run: 'ls -la' (y/n) "},
	} {
		t.Run(testCase.adapter, func(t *testing.T) {
			processor := replayAfterTheAnswer(t, testCase.adapter, testCase.first, []guardStep{
				{"y\r\n", 1}, {agentNextOutput, 1}, {testCase.unlabeled, 2},
			})
			second := processor.Pending()
			if second == nil {
				t.Fatal("the second question is not pending")
			}
			if strings.Contains(testCase.unlabeled, second.Match) {
				t.Fatalf("scenario broken: the label %q is painted on the second question", second.Match)
			}
			// Anything written while the operator decides; the screen is unchanged.
			if err := processor.Consume([]byte(escapeOnlyWrite)); err != nil {
				t.Fatal(err)
			}
			if err := processor.Resolve(second.ID, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := processor.Consume([]byte("y\r\n")); err != nil {
				t.Fatal(err)
			}
			if pending := processor.Pending(); pending != nil {
				t.Fatalf("the echo of the second answer asked it again: %q", pending.Match)
			}
		})
	}
}
