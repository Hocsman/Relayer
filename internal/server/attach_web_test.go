package server

import (
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// awaitAuditFailed waits until the state every client reads says the journal
// failed.
func (g *webGateway) awaitAuditFailed(within time.Duration) {
	g.t.Helper()
	deadline := time.Now().Add(within)
	for g.ctrl.GetState().Audit.Status != "failed" {
		if time.Now().After(deadline) {
			g.t.Fatalf("the audit status every client reads is %q, want failed", g.ctrl.GetState().Audit.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// The journal says a terminal was taken before anybody can see it held, and so
// before its holder can type: keystrokes are never journaled, and the attach
// record is what says who was at the terminal. The gateway handed the terminal
// over first and wrote the record afterwards, off the lock, so every client
// was told the terminal was held, and its holder could type into it, while the
// journal had no record of it yet.
func TestAWebAttachIsJournaledBeforeTheTerminalIsHeld(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)

	var mu sync.Mutex
	seen, journaled := false, false
	unsubscribe := g.ctrl.Subscribe(func(event string, payload any) {
		hand, ok := payload.(HandView)
		if event != eventHand || !ok || hand.State != HandHeld || hand.SessionID != "web-listen" {
			return
		}
		raw, _ := os.ReadFile(g.auditPath)
		mu.Lock()
		defer mu.Unlock()
		if !seen {
			seen = true
			journaled = strings.Contains(string(raw), `"kind":"`+string(audit.KindAttachStarted)+`"`)
		}
	})
	defer unsubscribe()

	alice.mustCall("setInteractiveSession", map[string]any{"runID": g.runID(), "sessionID": "web-listen", "active": true}, nil)
	mu.Lock()
	defer mu.Unlock()
	if !seen {
		t.Fatal("no client was told the terminal is held")
	}
	if !journaled {
		t.Fatalf("the terminal was shown held before its attach was journaled:\n%s", journalTrace(g.sessionJournal("web-listen")))
	}
}

// A terminal whose attach cannot be journaled is not handed over, and once the
// journal has failed no terminal is. The gateway ignored the attach record's
// error: the terminal was held, and typed into, with no record anybody had
// taken it, and the run went on as if its journal worked.
func TestAWebAttachIsRefusedWhenItsRecordCannotBeWritten(t *testing.T) {
	fault, wrap := newFaultEngine()
	g := startWebRun(t, webRun{
		engineWrap: wrap,
		agents:     []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()
	attach := func() error {
		return alice.call("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-listen", "active": true}, nil)
	}

	fault.set(func(f *faultEngine) { f.auditFails = true })
	if err := attach(); err == nil {
		t.Fatal("a terminal was handed over with no record of it")
	}
	assertTerminalFree := func(when string) {
		t.Helper()
		if hand := g.ctrl.HandFor("web-listen"); hand.State != HandFree {
			t.Fatalf("%s the hand is %+v, want the terminal free", when, hand)
		}
		if agent := g.agent("web-listen"); agent.Attached {
			t.Fatalf("%s the agent is shown attached: %+v", when, agent)
		}
		for _, frame := range g.broadcast(eventHand) {
			if hand, ok := frame.payload.(HandView); ok && hand.State != HandFree {
				t.Fatalf("%s a client was told the terminal is held: %+v", when, hand)
			}
		}
	}
	assertTerminalFree("after the refused attach")
	g.awaitAuditFailed(5 * time.Second)
	if err := alice.typeKeys(runID, "web-listen", "unrecorded\r"); err == nil {
		t.Fatal("keystrokes were taken after a refused attach")
	}

	// The journal takes entries again, but the run's stays failed: nothing
	// written from here would say what came before.
	fault.set(func(f *faultEngine) { f.auditFails = false })
	if err := attach(); err == nil {
		t.Fatal("a terminal was handed over once the journal had failed")
	}
	assertTerminalFree("once the journal had failed")
	g.assertScreenLacks("web-listen", "unrecorded", 500*time.Millisecond)
	if entries := entriesOf(g.sessionJournal("web-listen"), audit.KindAttachStarted); len(entries) != 0 {
		t.Fatalf("attach entries = %s, want none", journalTrace(entries))
	}
}

// A hand-over whose record cannot be written stops every keystroke that would
// follow it. The control records stay best effort, since the hand has moved by
// then and refusing the verb would leave the clients disagreeing with the
// gateway about who holds the terminal, but the gateway wrote them around the
// supervision core: the core never learned its journal had failed, and the
// new holder typed into the agent with no record of holding it.
func TestAWebHandOverThatCannotBeJournaledStopsTheKeystrokes(t *testing.T) {
	fault, wrap := newFaultEngine()
	g := startWebRun(t, webRun{
		engineWrap: wrap,
		agents:     []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	base := g.serve()
	alice := dialSharedGateway(t, base, "opAlice")
	carol := dialSharedGateway(t, base, "opCarol")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()
	alice.mustCall("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-listen", "active": true}, nil)
	carol.mustCall("requestControl", map[string]any{"runID": runID, "sessionID": "web-listen"}, nil)

	fault.set(func(f *faultEngine) { f.auditFails = true })
	var granted HandView
	alice.mustCall("grantControl", map[string]any{"runID": runID, "sessionID": "web-listen", "toConnID": carol.connID}, &granted)
	if granted.HolderConnID != carol.connID {
		t.Fatalf("the hand = %+v, want it with carol: a hand-over's record is best effort", granted)
	}
	g.awaitAuditFailed(5 * time.Second)
	err := carol.typeKeys(runID, "web-listen", "unrecorded-holder\r")
	if err == nil || !strings.Contains(err.Error(), supervise.ErrAuditUnavailable.Error()) {
		t.Fatalf("the new holder's keystrokes returned %v, want the failed journal refusing them", err)
	}
	carol.sendKeys("web-listen", "framed-unrecorded-holder\r")
	g.assertScreenLacks("web-listen", "unrecorded-holder", time.Second)
}

// A line is refused while anybody holds the terminal, its holder included: it
// would interleave with the holder's keystrokes, which the bundled interface
// already keeps it from doing by disabling the holder's line box. It is taken
// again once the terminal is released.
func TestAWebLineIsRefusedWhileAnybodyHoldsTheTerminal(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	base := g.serve()
	alice := dialSharedGateway(t, base, "opAlice")
	carol := dialSharedGateway(t, base, "opCarol")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()
	line := func(client *sharedGatewayClient, text string) error {
		return client.call("submitLine", map[string]any{"runID": runID, "sessionID": "web-listen", "line": text}, nil)
	}

	alice.mustCall("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-listen", "active": true}, nil)
	for _, client := range []*sharedGatewayClient{alice, carol} {
		if err := line(client, "held-line"); err == nil || !strings.Contains(err.Error(), supervise.ErrLineUnavailable.Error()) {
			t.Fatalf("a line while the terminal is held returned %v, want ErrLineUnavailable", err)
		}
	}
	g.assertScreenLacks("web-listen", "held-line", 500*time.Millisecond)

	alice.mustCall("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-listen", "active": false}, nil)
	if err := line(carol, "free-line"); err != nil {
		t.Fatalf("a line once the terminal is free: %v", err)
	}
	g.awaitScreen("web-listen", webAgentLine+`"free-line`, 10*time.Second)
}

// A terminal is attached only for the run the caller names. The attach verb
// ignored the run: a tab left open on a run that "Save and restart" had
// replaced attached to the new run's terminal, which it had never shown, and
// its keystrokes, which name no run, then went to the new process. A run's
// hands go with it so that such a tab holds nothing in the next one; an attach
// naming the previous run, or none, must not hand it one back. Letting go of a
// terminal needs no run: it is always safe.
func TestAWebTerminalIsAttachedOnlyForTheRunItNames(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()

	for _, other := range []string{"", "a-run-that-was-replaced"} {
		err := alice.call("setInteractiveSession", map[string]any{"runID": other, "sessionID": "web-listen", "active": true}, nil)
		if err == nil || !strings.Contains(err.Error(), supervise.ErrRunStale.Error()) {
			t.Fatalf("an attach naming run %q returned %v, want ErrRunStale", other, err)
		}
	}
	if hand := g.ctrl.HandFor("web-listen"); hand.State != HandFree {
		t.Fatalf("after attaches naming other runs the hand is %+v, want the terminal free", hand)
	}
	alice.sendKeys("web-listen", "stale-tab\r")
	g.assertScreenLacks("web-listen", "stale-tab", 500*time.Millisecond)
	if entries := entriesOf(g.sessionJournal("web-listen"), audit.KindAttachStarted); len(entries) != 0 {
		t.Fatalf("attach entries = %s, want none", journalTrace(entries))
	}

	alice.mustCall("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-listen", "active": true}, nil)
	alice.mustCall("setInteractiveSession", map[string]any{"runID": "", "sessionID": "web-listen", "active": false}, nil)
	if hand := g.ctrl.HandFor("web-listen"); hand.State != HandFree {
		t.Fatalf("after the release the hand is %+v, want the terminal free", hand)
	}
}

// Detaching from a terminal this connection does not hold changes nothing and
// journals nothing: the bundled interface detaches as it closes the terminal
// view, whether or not it had taken the terminal. The gateway journaled an
// attach_finished for every such call, an end of an attach that never began.
func TestAWebDetachFromATerminalNobodyHeldJournalsNothing(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()

	alice.mustCall("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-listen", "active": false}, nil)
	if entries := entriesOf(g.sessionJournal("web-listen"), audit.KindAttachFinished); len(entries) != 0 {
		t.Fatalf("a detach from a terminal nobody held was journaled: %s", journalTrace(entries))
	}
	alice.mustCall("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-listen", "active": true}, nil)
	alice.mustCall("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-listen", "active": false}, nil)
	if entries := entriesOf(g.sessionJournal("web-listen"), audit.KindAttachFinished); len(entries) != 1 {
		t.Fatalf("attach_finished entries = %s, want the one detach from a terminal held", journalTrace(entries))
	}
}
