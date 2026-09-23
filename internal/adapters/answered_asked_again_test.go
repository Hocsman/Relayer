package adapters

import (
	"fmt"
	"strings"
	"testing"
)

// A question the agent asks again after the operator answered it is a new
// question, and has to be put to someone. Remembering the answer, so that its
// echo is not asked again, must not swallow it: a question put to nobody leaves
// the agent waiting for an answer that no card is offering.

// sessionStep is one thing that happens to a session: the agent writes, the
// operator answers the question that is pending, or the terminal changes size.
// raised is how many occurrences the session must have raised once it is done.
type sessionStep struct {
	write   string
	answers bool
	resize  [2]int
	raised  int
}

func agentWrites(text string, raised int) sessionStep {
	return sessionStep{write: text, raised: raised}
}

func operatorAnswers(raised int) sessionStep {
	return sessionStep{answers: true, raised: raised}
}

func terminalResizedTo(columns, rows, raised int) sessionStep {
	return sessionStep{resize: [2]int{columns, rows}, raised: raised}
}

func (step sessionStep) String() string {
	switch {
	case step.answers:
		return "the operator answers"
	case step.resize != [2]int{}:
		return fmt.Sprintf("the terminal is resized to %dx%d", step.resize[0], step.resize[1])
	default:
		return fmt.Sprintf("the agent writes %q", step.write)
	}
}

// playSession runs the steps in a 120x30 terminal. On ConPTY, which runs every
// Windows session, its first two writes come first and put detection on the
// rendered screen; elsewhere an agent reaches the rendered screen only once it
// repaints.
func playSession(t *testing.T, adapterID string, onConPTY bool, steps []sessionStep) *Processor {
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
	if onConPTY {
		for _, write := range conptyFirstWrites {
			if err := processor.Consume([]byte(write)); err != nil {
				t.Fatal(err)
			}
		}
	}
	for index, step := range steps {
		switch {
		case step.answers:
			pending := processor.Pending()
			if pending == nil {
				t.Fatalf("step %d: nothing is pending to answer", index+1)
			}
			if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
		case step.resize != [2]int{}:
			processor.Resize(step.resize[0], step.resize[1])
		default:
			if err := processor.Consume([]byte(step.write)); err != nil {
				t.Fatal(err)
			}
		}
		if len(raised) != step.raised {
			t.Fatalf("step %d, %s: %d occurrences, want %d (%s)\nscreen:\n%s",
				index+1, step, len(raised), step.raised, describeRaised(raised), processor.Output())
		}
	}
	return processor
}

// outputLines is what an agent prints between two questions, one numbered line
// each, first to last included.
func outputLines(format string, first, last int) string {
	var builder strings.Builder
	for index := first; index <= last; index++ {
		fmt.Fprintf(&builder, format+"\r\n", index)
	}
	return builder.String()
}

const (
	aiderShellCommandPrompt  = "Run shell command? (Y)es/(N)o/(D)on't ask again [Yes]: "
	aiderCommandOutputPrompt = "Add command output to the chat? (Y)es/(N)o/(D)on't ask again [Yes]: "
	// clearScreenAndHistory is what `clear` prints on a modern terminal.
	clearScreenAndHistory = "\x1b[H\x1b[2J\x1b[3J"
	// ctrlLAtAidersPrompt is Ctrl+L at Aider's prompt: prompt_toolkit erases
	// the display, homes the cursor and draws its input prompt again.
	ctrlLAtAidersPrompt = "\x1b[?25l\x1b[H\x1b[2J\x1b[0m> \x1b[?25h"
)

// A clear leaves the answered question's row blank, and it stays blank until
// something is written on it: on a full screen, twenty lines of output or more.
// Aider asks for every shell command with one and the same line, so the next
// command's question, drawn above that row, is the answered question's line.
//
// Detection took it for the answered question moved off its blank row, and
// nobody was asked: the agent waited for an answer that no card was offering.
// Aider, Goose and Open Interpreter used to ask it; they remember an answered
// question on its own row only, and ask it again.
func TestAVendorQuestionAskedAgainAfterAClearIsAsked(t *testing.T) {
	type scenario struct {
		name  string
		steps []sessionStep
	}
	scenarios := func(prompt string) []scenario {
		return []scenario{
			{
				name: "a command clears the screen, then the question is asked again",
				steps: []sessionStep{
					agentWrites(outputLines("output line %d", 1, 20), 0),
					agentWrites("ls\r\n"+prompt, 1),
					operatorAnswers(1),
					agentWrites("y\r\nrunning ls\r\n", 1),
					agentWrites(clearScreenAndHistory+"file1\r\nfile2\r\n", 1),
					agentWrites("Let me run it again.\r\nls\r\n"+prompt, 2),
				},
			},
			{
				name: "the answered question was low on a full screen when the clear came",
				steps: []sessionStep{
					agentWrites(outputLines("output line %d", 1, 26), 0),
					agentWrites("npm test\r\n"+prompt, 1),
					operatorAnswers(1),
					agentWrites("y\r\n"+clearScreenAndHistory+" PASS  src/app.test.js\r\n", 1),
					agentWrites("Tests: 1 passed\r\n", 1),
					agentWrites("All green. Running it once more.\r\nnpm test\r\n"+prompt, 2),
				},
			},
		}
	}
	for _, question := range []struct {
		adapter string
		prompt  string
	}{
		{adapter: AiderID, prompt: aiderShellCommandPrompt},
		{adapter: GooseID, prompt: gooseToolPrompt},
		{adapter: OpenInterpreterID, prompt: interpreterCodePrompt},
	} {
		for _, scenario := range scenarios(question.prompt) {
			t.Run(question.adapter+"/"+scenario.name, func(t *testing.T) {
				if playSession(t, question.adapter, true, scenario.steps).Pending() == nil {
					t.Fatal("the question asked again is not pending")
				}
			})
		}
	}
	t.Run("aider/the operator pressed Ctrl+L at its prompt", func(t *testing.T) {
		processor := playSession(t, AiderID, true, []sessionStep{
			agentWrites(outputLines("output line %d", 1, 40), 0),
			agentWrites("pytest -q\r\n"+aiderShellCommandPrompt, 1),
			operatorAnswers(1),
			agentWrites("y\r\nRunning pytest -q\r\n3 passed\r\n", 1),
			agentWrites(aiderCommandOutputPrompt, 2),
			operatorAnswers(2),
			agentWrites("n\r\n> ", 2),
			agentWrites(ctrlLAtAidersPrompt, 2),
			agentWrites("run the linter too\r\n", 2),
			agentWrites("I'll run ruff.\r\n\r\nruff check .\r\n"+aiderShellCommandPrompt, 3),
		})
		if processor.Pending() == nil {
			t.Fatal("the question asked again is not pending")
		}
	})
}

// The tmux backend reconciles a snapshot of the pane on a resync. The snapshot
// is another text than the one the live screen rendered, with another length
// and another layout, and the live screen's anchors were used to read it: an
// offset in the snapshot named whatever live row sat at that offset in the
// other text.
//
// Here the agent asked the same question twice, the second is pending, and the
// operator, attached, typed an answer without pressing Enter before detaching.
// The snapshot's last line, the pending question with that key after it, fell
// on the answered question's row, so it was taken for the answered question's
// echo. Detection found nothing, and a snapshot with nothing detectable on it
// discards what is pending: the agent waited on a question nobody was shown.
func TestASnapshotDoesNotTakeThePendingQuestionForTheAnsweredOne(t *testing.T) {
	for _, question := range []struct {
		adapter string
		prompt  string
	}{
		{adapter: AiderID, prompt: aiderShellCommandPrompt},
		{adapter: GooseID, prompt: gooseToolPrompt},
		{adapter: OpenInterpreterID, prompt: interpreterCodePrompt},
		{adapter: GenericID, prompt: overwritePrompt},
	} {
		for _, keyOnTheStream := range []bool{false, true} {
			name := question.adapter + "/the typed key is only in the snapshot"
			if keyOnTheStream {
				name = question.adapter + "/the typed key reached the stream too"
			}
			t.Run(name, func(t *testing.T) {
				steps := []sessionStep{
					agentWrites("hello\r\nls\r\n"+question.prompt, 1),
					operatorAnswers(1),
					agentWrites("y\r\nfile1\r\n", 1),
					agentWrites("ls\r\n"+question.prompt, 2),
				}
				if keyOnTheStream {
					steps = append(steps, agentWrites("y", 2))
				}
				processor := playSession(t, question.adapter, true, steps)
				asked := strings.TrimSpace(question.prompt)
				// tmux capture-pane -p -J: plain text, trailing spaces trimmed.
				snapshot := "hello\nls\n" + asked + " y\nfile1\nls\n" + asked + " y\n"
				pending, _, err := processor.ReconcileSnapshot([]byte(snapshot))
				if err != nil {
					t.Fatal(err)
				}
				if pending == nil || processor.Pending() == nil {
					t.Fatalf("the resync discarded the pending question (returned %t, pending %t)",
						pending != nil, processor.Pending() != nil)
				}
			})
		}
	}
}
