package supervise_test

import (
	"errors"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// A terminal's own replies are told from keys a person pressed: they cannot
// answer a question, and the ConPTY of every Windows session asks the browser
// terminal for focus reports in its first bytes.
func TestATerminalsOwnRepliesAreToldFromTyping(t *testing.T) {
	for _, report := range []string{
		"\x1b[I",
		"\x1b[O",
		"\x1b[I\x1b[O\x1b[I",
		"\x1b[12;40R",
		"\x1b[0n",
		"\x1b[?1;2c",
		"\x1b[>0;276;0c",
		"\x1b[I\x1b[3;1R",
	} {
		if !supervise.IsTerminalReport([]byte(report)) {
			t.Errorf("%q is taken for typing, want a terminal report", report)
		}
	}
	for _, typing := range []string{
		"",
		"y",
		"y\r",
		"\r",
		"\x1b",
		"\x1b[A",
		"\x1b[I y",
		"\x1b[Iy",
		"\x1b[<0;10;5M",
		"\x1b[M !!",
		"\x1b[3n",
		"\x1b[200~y\x1b[201~",
		"\x1b[" + "1111111111111111111111111111111111111111" + "R",
	} {
		if supervise.IsTerminalReport([]byte(typing)) {
			t.Errorf("%q is taken for a terminal report, want typing", typing)
		}
	}
}

// A write that is only the terminal's replies takes the session's write slot
// as keystrokes do, but leaves its prompts answerable: taking an agent's
// terminal on Windows sent a focus report at once, and every prompt shown on
// it became the terminal's although nobody had typed.
func TestATerminalReportLeavesThePromptAnswerable(t *testing.T) {
	engine := newFakeEngine()
	engine.supportedDecisions = []adapters.Decision{adapters.DecisionAllow, adapters.DecisionDeny}
	sup, _ := newCoreForTest(t, engine, "agent-a")
	sup.SetHolder("agent-a", "conn-1")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})

	release, err := sup.AdmitReport("agent-a", "conn-1")
	if err != nil {
		t.Fatalf("AdmitReport = %v", err)
	}
	// It holds the slot while it is written, as keystrokes do.
	if err := sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", alice); !errors.Is(err, supervise.ErrDecisionInFlight) {
		t.Fatalf("an answer while a report is written = %v, want ErrDecisionInFlight", err)
	}
	release()
	// And it is admitted only for the terminal's holder.
	if _, err := sup.AdmitReport("agent-a", "conn-2"); !errors.Is(err, supervise.ErrNotHolder) {
		t.Fatalf("a report from a connection without the hand = %v, want ErrNotHolder", err)
	}

	time.Sleep(50 * time.Millisecond)
	shown := viewOf(sup, "prompt-1")
	if shown == nil || shown.Evaluation.Reason == supervise.ReasonTypedAtTerminal || len(shown.Decisions) == 0 {
		t.Fatalf("after a terminal report the prompt is shown as %#v, want it still answerable", shown)
	}
	waitFor(t, 2*time.Second, "the answer to be taken", func() bool {
		err := sup.SubmitAutomaticDecision(testRunID, "agent-a", "prompt-1", "allow", alice)
		if err != nil && !errors.Is(err, supervise.ErrDecisionInFlight) {
			t.Fatalf("answering after a terminal report = %v", err)
		}
		return err == nil
	})
}
