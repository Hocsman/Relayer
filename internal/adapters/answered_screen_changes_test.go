package adapters

import (
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
