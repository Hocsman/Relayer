package adapters

import (
	"fmt"
	"strings"
	"testing"
)

// What becomes of an answered question when the screen changes under it: a
// full-screen program, a terminal that loses height, a first repaint that comes
// only after the answer. Each of these asked an answered question a second
// time, and under an automatic policy the desktop typed a second answer into
// the agent.

// conptyRepaint is how ConPTY redraws its whole viewport: the cursor hidden,
// home, every row erased to its end and the rows separated by CRLF, then the
// cursor put back and shown. Byte for byte from captures on Windows 11, once
// the alternate screen was left and once the terminal lost height; report is
// what ConPTY sends before the home, the new size after a resize.
func conptyRepaint(report string, height int, rows []string, cursor string) string {
	lines := make([]string, height)
	copy(lines, rows)
	for index := range lines {
		lines[index] += "\x1b[K"
	}
	return "\x1b[?25l" + report + "\x1b[H" + strings.Join(lines, "\r\n") + cursor + "\x1b[?25h"
}

// A full-screen program run after the answer, an editor or a pager, takes the
// alternate screen, and the primary one comes back unchanged when it exits,
// the answered question still on it. The answered row was parked with the
// primary screen, and a row that is on no visible grid was taken for a row that
// is gone: the memory went, and the next write asked the question again. Under
// an automatic policy the desktop typed a second answer into the agent.
func TestAFullScreenProgramAfterTheAnswerDoesNotAskItAgain(t *testing.T) {
	const editor = "\x1b[?1049h\x1b[H\x1b[2Jeditor contents\r\n~\r\n~"
	for _, question := range []struct {
		adapter string
		prompt  string
		answer  string
	}{
		{adapter: GenericID, prompt: overwritePrompt, answer: "yes"},
		{adapter: AiderID, prompt: aiderApplyPrompt, answer: "y"},
	} {
		t.Run(question.adapter+"/in a terminal that passes the alternate screen through", func(t *testing.T) {
			processor := playSession(t, question.adapter, true, []sessionStep{
				agentWrites(question.prompt, 1),
				operatorAnswers(1),
				agentWrites(question.answer+"\r\n", 1),
				agentWrites(editor, 1),
				agentWrites("\x1b[?1049l", 1),
				agentWrites(escapeOnlyWrite, 1),
			})
			if pending := processor.Pending(); pending != nil {
				t.Fatalf("the session is blocked on an answered question: %q", pending.Match)
			}
		})
		// ConPTY passes the switch through and repaints the primary screen
		// after it, byte for byte as captured on Windows 11.
		t.Run(question.adapter+"/on ConPTY", func(t *testing.T) {
			processor := playSession(t, question.adapter, true, []sessionStep{
				agentWrites(question.prompt, 1),
				operatorAnswers(1),
				agentWrites(question.answer+"\r\n", 1),
				agentWrites("\x1b[?1049h\x1b[2J", 1),
				agentWrites(conptyRepaint("", 30, []string{"editor contents", "~", "~"}, "\x1b[3;2H"), 1),
				agentWrites("\x1b[?1049l", 1),
				agentWrites(conptyRepaint("", 30, []string{"agent ready", question.prompt + question.answer}, "\x1b[3;1H"), 1),
			})
			if pending := processor.Pending(); pending != nil {
				t.Fatalf("the session is blocked on an answered question: %q", pending.Match)
			}
		})
		// An agent that only printed until then raised its question on the
		// byte window, with no row. The program is its first repaint, and the
		// question is looked for on the alternate grid, where it is not.
		t.Run(question.adapter+"/before the agent ever repainted", func(t *testing.T) {
			processor := playSession(t, question.adapter, false, []sessionStep{
				agentWrites("agent ready\r\n", 0),
				agentWrites(question.prompt, 1),
				operatorAnswers(1),
				agentWrites(question.answer+"\r\n", 1),
				agentWrites("\x1b[?1049h\x1b[H\x1b[2Jeditor\r\n~", 1),
				agentWrites("\x1b[?1049l", 1),
				agentWrites(escapeOnlyWrite, 1),
				agentWrites("\x1b[K", 1),
			})
			if pending := processor.Pending(); pending != nil {
				t.Fatalf("the session is blocked on an answered question: %q", pending.Match)
			}
		})
	}
}

// A terminal that loses height keeps the cursor's row in view, and pushes the
// rows above it into the scrollback. The screen kept the top rows instead, so
// with the answered question at the bottom of a full screen, its row was
// dropped. ConPTY then repainted the viewport with the question at its new
// place, a row the memory did not know, and it was asked again. In the desktop
// that is a window resized, or the agent grid rearranged when another agent is
// added, while an answer is being worked on.
//
// The bytes after the resize are ConPTY's, captured on Windows 11 when the
// terminal went from 30 rows to 20.
func TestATerminalThatLosesHeightAfterTheAnswerDoesNotAskItAgain(t *testing.T) {
	for _, question := range []struct {
		adapter string
		prompt  string
		answer  string
	}{
		{adapter: AiderID, prompt: aiderApplyPrompt, answer: "y"},
		{adapter: GenericID, prompt: overwritePrompt, answer: "yes"},
	} {
		t.Run(question.adapter, func(t *testing.T) {
			// What was left in view: the last eighteen lines of output, the
			// question with its echo, and the cursor's row below it.
			visible := strings.Split(strings.TrimSuffix(outputLines("agent output line %d", 27, 44), "\r\n"), "\r\n")
			visible = append(visible, question.prompt+question.answer)
			processor := playSession(t, question.adapter, true, []sessionStep{
				agentWrites(outputLines("agent output line %d", 0, 44), 0),
				agentWrites(question.prompt, 1),
				operatorAnswers(1),
				// The question is on the last row, so the echo's line feed
				// scrolls it one row up.
				agentWrites(question.answer+"\r", 1),
				agentWrites("\n", 1),
				terminalResizedTo(120, 20, 1),
				agentWrites(conptyRepaint("\x1b[8;20;120t", 20, visible, ""), 1),
				agentWrites(escapeOnlyWrite, 1),
			})
			if pending := processor.Pending(); pending != nil {
				t.Fatalf("the session is blocked on an answered question: %q", pending.Match)
			}
		})
	}
}

// Off Windows an agent that only prints raises its question on the byte window,
// before it has repainted anything, so the answer is remembered with no row. It
// gets one on the first repaint, from the row that shows it, and it was looked
// for by its match alone. A vendor match is the label of a kind of prompt,
// "Apply changes?" for "Apply edit to notes.py?", which may never be painted:
// found nowhere, the entry went, and the question still on screen was asked
// again. It is looked for by the line it was asked on first.
func TestAQuestionAnsweredBeforeTheFirstRepaintIsFoundByItsLine(t *testing.T) {
	for _, question := range []struct {
		adapter string
		prompt  string
	}{
		{adapter: AiderID, prompt: "Apply edit to notes.py? (Y)es/(N)o [Yes]: "},
		{adapter: GooseID, prompt: "Allow Goose to run: 'ls -la' (y/n) "},
		{adapter: OpenInterpreterID, prompt: "Execute this Python code? (y/n) "},
	} {
		t.Run(question.adapter, func(t *testing.T) {
			processor := playSession(t, question.adapter, false, []sessionStep{
				agentWrites("agent ready\r\n", 0),
				agentWrites(question.prompt, 1),
				operatorAnswers(1),
				agentWrites("y\r\n", 1),
				// The first write a byte stream cannot express.
				agentWrites("\x1b[K", 1),
				agentWrites(escapeOnlyWrite, 1),
			})
			if pending := processor.Pending(); pending != nil {
				t.Fatalf("the session is blocked on an answered question: %q", pending.Match)
			}
		})
	}
}

// A window resized while a full-screen program runs after the answer. ConPTY
// leaves the primary buffer alone while the alternate one is shown, resizes it
// once when the program exits, and repaints it. The screen resized the parked
// primary screen at every step instead: a shrink pushed its top rows into the
// history and the grow that followed did not pull them back, so ConPTY drew the
// answered question on a row the memory did not know, and it was asked again.
// Under an automatic policy that is a second answer typed into the agent.
//
// The bytes are ConPTY's, captured on Windows 11 with the question on the last
// row of a 120x30 terminal. A shrink or a grow alone did not ask again before,
// and is pinned alongside.
func TestAWindowResizedWhileAProgramRunsAfterTheAnswerDoesNotAskItAgain(t *testing.T) {
	editor := func(report string, height int) string {
		return conptyRepaint(report, height, []string{"editor contents", "~", "~"}, "\x1b[3;2H")
	}
	for _, question := range []struct {
		adapter string
		prompt  string
		answer  string
	}{
		{adapter: AiderID, prompt: aiderApplyPrompt, answer: "y"},
		{adapter: GenericID, prompt: overwritePrompt, answer: "nope"},
	} {
		// The primary screen as ConPTY repaints it when the program exits: the
		// output from line first on, then the question with its echo.
		primary := func(first int) []string {
			rows := strings.Split(strings.TrimSuffix(outputLines("agent output line %d", first, 44), "\r\n"), "\r\n")
			return append(rows, question.prompt+question.answer)
		}
		for _, resize := range []struct {
			name    string
			heights []int
			repaint string
		}{
			{name: "shrink then grow back", heights: []int{20, 30}, repaint: conptyRepaint("", 30, primary(17), "")},
			{name: "a window dragged smaller and back", heights: []int{28, 25, 22, 26, 30}, repaint: conptyRepaint("", 30, primary(17), "")},
			{name: "shrink, then grow past the old height", heights: []int{20, 40}, repaint: conptyRepaint("", 40, primary(17), "\x1b[30;1H")},
			{name: "shrink only", heights: []int{20}, repaint: conptyRepaint("", 20, primary(27), "")},
			{name: "grow only", heights: []int{40}, repaint: conptyRepaint("", 40, primary(17), "\x1b[30;1H")},
		} {
			t.Run(question.adapter+"/"+resize.name, func(t *testing.T) {
				steps := []sessionStep{
					agentWrites(outputLines("agent output line %d", 0, 44), 0),
					agentWrites(question.prompt, 1),
					operatorAnswers(1),
					// The question is on the last row, so the echo's line feed
					// scrolls it one row up.
					agentWrites(question.answer+"\r", 1),
					agentWrites("\n", 1),
					agentWrites("\x1b[?1049h\x1b[2J", 1),
					agentWrites(editor("", 30), 1),
				}
				height := 30
				for _, next := range resize.heights {
					// ConPTY reports the new size before a repaint that shrinks.
					report := ""
					if next < height {
						report = fmt.Sprintf("\x1b[8;%d;120t", next)
					}
					height = next
					steps = append(steps, terminalResizedTo(120, next, 1), agentWrites(editor(report, next), 1))
				}
				steps = append(steps,
					agentWrites("\x1b[?1049l", 1),
					agentWrites(resize.repaint, 1),
					agentWrites(escapeOnlyWrite, 1),
				)
				if pending := playSession(t, question.adapter, true, steps).Pending(); pending != nil {
					t.Fatalf("the session is blocked on an answered question: %q", pending.Match)
				}
			})
		}
	}
}

// A pane that more than doubles its height while a full-screen agent has it:
// four agents to one in the desktop grid, a small pane maximised. The parked
// primary screen was resized with it, and gave its new rows names the agent's
// screen had already given out, so a row of the agent's screen was reported
// parked. An answer given on such a row was kept for the rest of the program:
// the same dialog asked again on that row later was never offered, and the
// agent waited on a question nobody was shown. On Codex, which compares the
// line alone, the same command asked anywhere was not offered either.
func TestAPaneThatGrowsUnderAFullScreenAgentStillAsksTheSameDialogAgain(t *testing.T) {
	const dialog = "Permission required: run npm test? [y/n] "
	for _, adapterID := range []string{GenericID, ClaudeID} {
		for _, onConPTY := range []bool{false, true} {
			for _, row := range []int{14, 20, 27} {
				t.Run(fmt.Sprintf("%s/conpty=%t/the dialog on row %d", adapterID, onConPTY, row), func(t *testing.T) {
					processor := playSession(t, adapterID, onConPTY, []sessionStep{
						// Four agents share the window.
						terminalResizedTo(120, 12, 0),
						agentWrites("$ agent\r\n", 0),
						agentWrites("\x1b[?1049h\x1b[H\x1b[2JAgent header\x1b[12;1H> ", 0),
						// The other three are closed, and this pane takes the
						// window.
						terminalResizedTo(120, 40, 0),
						agentWrites("\x1b[H\x1b[2JAgent header\x1b[40;1H> ", 0),
						agentWrites(fmt.Sprintf("\x1b[%d;1H%s", row, dialog), 1),
						operatorAnswers(1),
						agentWrites(fmt.Sprintf("\x1b[%d;1H\x1b[2Kran npm test: ok\x1b[40;3H", row), 1),
						agentWrites(fmt.Sprintf("\x1b[%d;1H\x1b[2K%s", row, dialog), 2),
					})
					if processor.Pending() == nil {
						t.Fatal("the dialog asked again is not pending")
					}
				})
			}
		}
	}
	t.Run("codex/the same command asked again lower down", func(t *testing.T) {
		processor := playSession(t, CodexID, false, []sessionStep{
			terminalResizedTo(120, 12, 0),
			agentWrites("$ codex\r\n", 0),
			agentWrites("\x1b[?1049h\x1b[H\x1b[2JCodex header", 0),
			terminalResizedTo(120, 30, 0),
			agentWrites("\x1b[13;1H"+codexCommandPrompt, 1),
			operatorAnswers(1),
			// The dialog goes, and its rows show the command's output.
			agentWrites("\x1b[13;1H\x1b[2Kran ls\x1b[14;1H\x1b[2Kfile1\x1b[15;1H\x1b[2Kfile2\x1b[16;1H\x1b[2Kfile3\x1b[17;1H\x1b[2K> ", 1),
			agentWrites(escapeOnlyWrite, 1),
			agentWrites("\x1b[H\x1b[2JCodex header\x1b[4;1Hran ls\r\nfile1\r\n\x1b[8;1H"+codexCommandPrompt, 2),
		})
		if processor.Pending() == nil {
			t.Fatal("the command asked again is not pending")
		}
	})
}

// What was answered on the primary screen is kept while a full-screen program
// has the alternate one, so that the question still painted underneath is not
// asked again when the program exits. Kept, it went on answering for questions
// on the program's screen, which is not where it was answered, and each of
// these was put to nobody:
//
//   - an answered row that was blank when the program started kept its blank
//     flag, and the generic adapter took the identical question on the
//     program's screen for the answered one moved off its blank row;
//   - an answer with no row, given before the agent first repainted, matched
//     the identical line anywhere on the program's screen, since without a row
//     the line alone decides;
//   - and after a full reset, which a program can send instead of leaving the
//     alternate screen, that went on for the rest of the session.
func TestAQuestionOnAProgramsScreenIsAskedWhateverWasAnsweredUnderneath(t *testing.T) {
	for _, adapterID := range []string{GenericID, ClaudeID} {
		t.Run(adapterID+"/the answered row was blank when the program started", func(t *testing.T) {
			processor := playSession(t, adapterID, true, []sessionStep{
				agentWrites(overwritePrompt, 1),
				operatorAnswers(1),
				agentWrites("yes\r\n", 1),
				// The agent tidies the answered question away.
				agentWrites("\x1b[1A\x1b[2K", 1),
				agentWrites("\x1b[?1049h\x1b[H\x1b[2Jfull-screen tool\r\n", 1),
				agentWrites("\x1b[5;1H"+overwritePrompt, 2),
			})
			if processor.Pending() == nil {
				t.Fatal("the question on the program's screen is not pending")
			}
		})
	}
	for _, question := range []struct {
		adapter string
		prompt  string
	}{
		{adapter: GenericID, prompt: overwritePrompt},
		{adapter: AiderID, prompt: aiderApplyPrompt},
	} {
		t.Run(question.adapter+"/answered before the agent ever repainted", func(t *testing.T) {
			processor := playSession(t, question.adapter, false, []sessionStep{
				agentWrites("agent ready\r\n", 0),
				agentWrites(question.prompt, 1),
				operatorAnswers(1),
				agentWrites("y\r\n", 1),
				agentWrites("\x1b[?1049h\x1b[H\x1b[2Jfull-screen tool\r\n", 1),
				agentWrites("\x1b[5;1H"+question.prompt, 2),
			})
			if processor.Pending() == nil {
				t.Fatal("the question on the program's screen is not pending")
			}
		})
		t.Run(question.adapter+"/the program ended with a full reset", func(t *testing.T) {
			processor := playSession(t, question.adapter, false, []sessionStep{
				agentWrites("agent ready\r\n", 0),
				agentWrites(question.prompt, 1),
				operatorAnswers(1),
				agentWrites("y\r\n", 1),
				agentWrites("\x1b[?1049h\x1b[H\x1b[2Jfull-screen tool", 1),
				agentWrites("\x1bc", 1),
				agentWrites("agent output\r\n"+question.prompt, 2),
			})
			if processor.Pending() == nil {
				t.Fatal("the question after the reset is not pending")
			}
		})
	}
}

// An answer with no row is kept for the primary screen only when it was there
// before the program took the grid. One given while the program has it is about
// a question on the program's own grid: raised from a tmux snapshot of the pane,
// and answered before any write could find its row. Treated as the primary
// screen's, it answered for nothing on the program's grid, and the next write,
// which still showed the question just answered, asked it again.
func TestAQuestionFromASnapshotOfAProgramsScreenIsNotAskedAgainOnceAnswered(t *testing.T) {
	for _, question := range []struct {
		adapter string
		prompt  string
	}{
		{adapter: GenericID, prompt: overwritePrompt},
		{adapter: AiderID, prompt: aiderApplyPrompt},
	} {
		t.Run(question.adapter, func(t *testing.T) {
			processor := playSession(t, question.adapter, false, []sessionStep{
				agentWrites("\x1b[?1049h\x1b[H\x1b[2Jfull-screen agent\r\n", 0),
				// An operator attached to the pane saw the question and
				// detached; the backend resyncs from a snapshot.
				tmuxResyncsWith("full-screen agent\n"+strings.TrimSpace(question.prompt)+"\n", 0),
				operatorAnswers(0),
				agentWrites("\x1b[2;1H"+question.prompt, 0),
				agentWrites(escapeOnlyWrite, 0),
			})
			if pending := processor.Pending(); pending != nil {
				t.Fatalf("the session is blocked on an answered question: %q", pending.Match)
			}
		})
	}
}

// The same answer, once the dialog went: when the agent later asks that question
// again on its program's grid, it is a new question. Kept without a row for as
// long as the program ran, the answer matched its line anywhere on that grid,
// and the question was put to nobody for the rest of the program.
func TestAQuestionFromASnapshotOfAProgramsScreenIsAskedWhenTheAgentAsksItAgain(t *testing.T) {
	for _, question := range []struct {
		adapter string
		prompt  string
	}{
		{adapter: GenericID, prompt: overwritePrompt},
		{adapter: ClaudeID, prompt: overwritePrompt},
		{adapter: AiderID, prompt: aiderApplyPrompt},
	} {
		for _, again := range []struct {
			name  string
			steps []sessionStep
		}{
			{name: "lower down", steps: []sessionStep{
				agentWrites("\x1b[2;1H\x1b[2Kworking...", 0),
				agentWrites("\x1b[2;1H\x1b[2Kdone\x1b[5;1H\x1b[2K> ", 0),
				agentWrites("\x1b[8;1H\x1b[2K"+question.prompt, 1),
			}},
			{name: "after the frame is cleared", steps: []sessionStep{
				agentWrites("\x1b[H\x1b[2Jfull-screen agent\r\nran it\r\n", 0),
				agentWrites("\x1b[H\x1b[2Jfull-screen agent\r\nran it\r\n"+question.prompt, 1),
			}},
		} {
			t.Run(question.adapter+"/"+again.name, func(t *testing.T) {
				steps := []sessionStep{
					agentWrites("\x1b[?1049h\x1b[H\x1b[2Jfull-screen agent\r\n", 0),
					tmuxResyncsWith("full-screen agent\n"+strings.TrimSpace(question.prompt)+"\n", 0),
					operatorAnswers(0),
					agentWrites("\x1b[2;1H"+question.prompt, 0),
					agentWrites(escapeOnlyWrite, 0),
				}
				processor := playSession(t, question.adapter, false, append(steps, again.steps...))
				if processor.Pending() == nil {
					t.Fatal("the question asked again is not pending")
				}
			})
		}
	}
}

// A question answered on a full-screen program's grid is not the one answered
// on the primary screen underneath, even in the same words. Taken for it, the
// primary screen's answer moved onto the program's row, went with that row
// when the program exited, and the question still painted on the primary
// screen was asked a second time: under an automatic policy, a second answer
// typed into the agent. Any yes/no question will do for the generic adapter,
// whose pattern captures only the "[y/n]".
func TestAnAnswerOnAProgramsScreenLeavesThePrimaryScreensAnswerAlone(t *testing.T) {
	for _, question := range []struct {
		adapter string
		prompt  string
		program string
	}{
		{adapter: GenericID, prompt: overwritePrompt, program: overwritePrompt},
		{adapter: GenericID, prompt: overwritePrompt, program: "Delete the backup too? [y/n] "},
		{adapter: AiderID, prompt: aiderApplyPrompt, program: aiderApplyPrompt},
	} {
		for _, onConPTY := range []bool{true, false} {
			name := fmt.Sprintf("%s/%s/conpty=%t", question.adapter, strings.TrimSpace(question.program), onConPTY)
			t.Run(name, func(t *testing.T) {
				processor := playSession(t, question.adapter, onConPTY, []sessionStep{
					agentWrites(outputLines("agent output line %d", 0, 4), 0),
					agentWrites(question.prompt, 1),
					operatorAnswers(1),
					agentWrites("y\r\n", 1),
					agentWrites("\x1b[?1049h\x1b[H\x1b[2Jfull-screen tool\r\n", 1),
					agentWrites("\x1b[5;1H"+question.program, 2),
					operatorAnswers(2),
					agentWrites("y\r\n", 2),
					agentWrites("\x1b[H\x1b[2Jfull-screen tool\r\ndone", 2),
					agentWrites("\x1b[?1049l", 2),
					agentWrites("agent continues\r\n", 2),
					agentWrites(escapeOnlyWrite, 2),
				})
				if pending := processor.Pending(); pending != nil {
					t.Fatalf("the primary screen's answered question is pending again: %q", pending.Match)
				}
			})
		}
	}
}
