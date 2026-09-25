package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"

	"github.com/Hocsman/Relayer/internal/supervise"
)

// sendKeys types text into a session's terminal as the web terminal does: one
// binary frame, the session's ID prefixed by its length, then the bytes.
func (c *sharedGatewayClient) sendKeys(sessionID, text string) {
	c.t.Helper()
	frame := append([]byte{byte(len(sessionID))}, sessionID...)
	frame = append(frame, text...)
	if err := c.ws.WriteMessage(websocket.BinaryMessage, frame); err != nil {
		c.t.Fatalf("send keystrokes: %v", err)
	}
}

// typeKeys types text into a session's terminal through the RPC, the web
// terminal's fallback when a frame cannot carry the session's ID.
func (c *sharedGatewayClient) typeKeys(runID, sessionID, text string) error {
	c.t.Helper()
	return c.call("sendTerminalInput", map[string]any{"runID": runID, "sessionID": sessionID, "data": text}, nil)
}

// assertScreenLacks fails if the session's screen shows text within the
// period. The terminal echoes what it is given, so keystrokes that reached the
// agent show on its screen whether or not it read them yet.
func (g *webGateway) assertScreenLacks(sessionID, text string, period time.Duration) {
	g.t.Helper()
	deadline := time.Now().Add(period)
	for time.Now().Before(deadline) {
		if screen := g.screen(sessionID); strings.Contains(screen, text) {
			g.t.Fatalf("%q reached the agent %s; screen:\n%s", text, sessionID, screen)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// A terminal nobody holds takes no keystrokes, from anybody, by a frame or by
// the RPC, and neither does a call that names no connection. A terminal
// nobody held was open to every operator connection: with the policy
// answering on its own, a client typing without the hand typed alongside the
// policy's answer to the same question, and nothing in the journal said
// anybody had been at the terminal. Its holder's keystrokes still reach it.
func TestAWebTerminalNobodyHoldsTakesNoKeystrokes(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()

	alice.sendKeys("web-listen", "free-frame\r")
	if err := alice.typeKeys(runID, "web-listen", "free-call\r"); err == nil {
		t.Fatal("keystrokes into a terminal nobody holds were taken")
	}
	if err := g.ctrl.SendTerminalInput(runID, "web-listen", []byte("no-connection\r"), "alice", ""); err == nil {
		t.Fatal("keystrokes from no connection were taken")
	}
	for _, typed := range []string{"free-frame", "free-call", "no-connection"} {
		g.assertScreenLacks("web-listen", typed, 300*time.Millisecond)
	}

	alice.mustCall("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-listen", "active": true}, nil)
	alice.sendKeys("web-listen", "held-frame\r")
	if err := alice.typeKeys(runID, "web-listen", "held-call\r"); err != nil {
		t.Fatalf("the holder's keystrokes were refused: %v", err)
	}
	g.awaitScreen("web-listen", webAgentLine+`"held-frame`, 10*time.Second)
	g.awaitScreen("web-listen", webAgentLine+`"held-call`, 10*time.Second)
	if screen := g.screen("web-listen"); strings.Contains(screen, "free-") || strings.Contains(screen, "no-connection") {
		t.Fatalf("keystrokes typed without the hand reached the agent; screen:\n%s", screen)
	}
}

// The holder's keystrokes take the session's one write slot, the one an
// answer takes: while an answer is being written they are refused, and never
// land in the middle of it. The gateway wrote them to the terminal whatever
// else was being written, so an operator typing while a colleague's answer was
// written gave the agent a second answer, or half of one.
func TestAWebKeystrokeIsRefusedWhileAnAnswerIsWritten(t *testing.T) {
	fault, wrap := newFaultEngine()
	g := startWebRun(t, webRun{
		engineWrap: wrap,
		agents:     []webAgent{{id: "web-generic", mode: webAgentGeneric, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-generic", "agent ready", 30*time.Second)
	runID := g.runID()
	alice.mustCall("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-generic", "active": true}, nil)
	prompt := g.awaitPending("web-generic", 30*time.Second)

	release := make(chan struct{})
	fault.set(func(f *faultEngine) { f.holdApply = release })
	answered := make(chan error, 1)
	go func() {
		answered <- g.ctrl.SubmitDecision(prompt.RunID, prompt.SessionID, prompt.ID, "the answer", webOperator)
	}()
	select {
	case <-fault.applyStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the answer's write never started")
	}

	err := alice.typeKeys(runID, "web-generic", "typed-over\r")
	if err == nil || !strings.Contains(err.Error(), supervise.ErrDecisionInFlight.Error()) {
		t.Fatalf("keystrokes while an answer is written returned %v, want the answer's write refusing them", err)
	}
	alice.sendKeys("web-generic", "framed-over\r")
	time.Sleep(300 * time.Millisecond)
	close(release)
	if err := <-answered; err != nil {
		t.Fatalf("the answer failed: %v", err)
	}
	screen := g.awaitReport("web-generic", 20*time.Second)
	if first, count, extras := answers(screen); count != 1 || extras != 0 || !strings.Contains(first, "the answer") {
		t.Fatalf("the agent received %s (%d answers, %d extra), want the answer alone; screen:\n%s", first, count, extras, screen)
	}
	if strings.Contains(screen, "-over") {
		t.Fatalf("keystrokes typed while an answer was written reached the agent; screen:\n%s", screen)
	}
}

// The holder's keystrokes are refused once the session cannot take a write:
// once the journal has failed, and on a session frozen by a write whose
// outcome is unknown. Keystrokes are never journaled; what bounds them is that
// they reach an agent only while everything else could. The gateway wrote
// them whatever the core had decided about the session.
func TestAWebKeystrokeIsRefusedWhenTheSessionCannotTakeAWrite(t *testing.T) {
	cases := []struct {
		name   string
		freeze func(t *testing.T, g *webGateway, fault *faultEngine)
		want   error
	}{
		{
			name: "the journal failed",
			freeze: func(t *testing.T, g *webGateway, fault *faultEngine) {
				fault.set(func(f *faultEngine) { f.auditFails = true })
				// Taking in a prompt is the next entry the core writes.
				livePrompt(g.ctrl, "web-held", "journal-fails", time.Now().UTC())
				deadline := time.Now().Add(5 * time.Second)
				for g.ctrl.GetState().Audit.Status != "failed" {
					if time.Now().After(deadline) {
						t.Fatalf("the audit status is %q, want failed", g.ctrl.GetState().Audit.Status)
					}
					time.Sleep(20 * time.Millisecond)
				}
			},
			want: supervise.ErrAuditUnavailable,
		},
		{
			name: "a write's outcome is unknown",
			freeze: func(t *testing.T, g *webGateway, fault *faultEngine) {
				prompt := livePrompt(g.ctrl, "web-held", "uncertain", time.Now().UTC())
				fault.set(func(f *faultEngine) { f.applyErr = errors.New("the terminal did not take the write in time") })
				if err := g.ctrl.SubmitDecision(g.runID(), "web-held", prompt.ID, "lost", webOperator); !errors.Is(err, supervise.ErrDeliveryUncertain) {
					t.Fatalf("the uncertain write returned %v, want ErrDeliveryUncertain", err)
				}
			},
			want: supervise.ErrDeliveryUncertain,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fault, wrap := newFaultEngine()
			g := startWebRun(t, webRun{
				engineWrap: wrap,
				agents:     []webAgent{{id: "web-held", mode: webAgentListen, adapter: "generic"}},
			})
			alice := dialSharedGateway(t, g.serve(), "opAlice")
			g.awaitScreen("web-held", "agent ready", 30*time.Second)
			runID := g.runID()
			alice.mustCall("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-held", "active": true}, nil)
			tc.freeze(t, g, fault)

			err := alice.typeKeys(runID, "web-held", "after-freeze\r")
			if err == nil || !strings.Contains(err.Error(), tc.want.Error()) {
				t.Fatalf("the holder's keystrokes returned %v, want %v", err, tc.want)
			}
			alice.sendKeys("web-held", "framed-after-freeze\r")
			g.assertScreenLacks("web-held", "after-freeze", time.Second)
		})
	}
}

// Taking a terminal on Windows sends a focus report: the ConPTY of every
// session asks the browser terminal for focus reports in its first bytes, and
// the terminal reports its focus as soon as it takes it. The gateway took that
// report for typing, and every prompt shown on the terminal became the
// terminal's, answerable only there, although nobody had typed. A focus report
// still reaches the agent, but leaves the prompt answerable; what the holder
// types does not.
func TestAWebTerminalsFocusReportLeavesThePromptAnswerable(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-generic", mode: webAgentGeneric, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-generic", "agent ready", 30*time.Second)
	runID := g.runID()
	prompt := g.awaitPending("web-generic", 30*time.Second)
	alice.mustCall("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-generic", "active": true}, nil)

	alice.sendKeys("web-generic", "\x1b[I")
	if err := alice.typeKeys(runID, "web-generic", "\x1b[O\x1b[I"); err != nil {
		t.Fatalf("the holder's focus reports were refused: %v", err)
	}
	time.Sleep(300 * time.Millisecond)
	for _, shown := range g.pending("web-generic") {
		if shown.ID == prompt.ID && shown.Evaluation.Reason == supervise.ReasonTypedAtTerminal {
			t.Fatalf("a focus report made the prompt the terminal's: %+v", shown.Evaluation)
		}
	}

	if err := alice.typeKeys(runID, "web-generic", "n"); err != nil {
		t.Fatalf("the holder's keystroke was refused: %v", err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		typedOver := false
		for _, shown := range g.pending("web-generic") {
			typedOver = typedOver || (shown.ID == prompt.ID && shown.Evaluation.Reason == supervise.ReasonTypedAtTerminal)
		}
		if typedOver {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a keystroke left the prompt answerable: %+v", g.pending("web-generic"))
		}
		time.Sleep(20 * time.Millisecond)
	}
}
