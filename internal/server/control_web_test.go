package server

import (
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// controlParams is the parameters of a control verb for the run the client
// shows, with the extra ones given.
func controlParams(t *testing.T, client *sharedGatewayClient, sessionID string, extra map[string]any) map[string]any {
	t.Helper()
	var state AppState
	client.mustCall("getState", map[string]any{}, &state)
	params := map[string]any{"runID": state.RunID, "sessionID": sessionID}
	for key, value := range extra {
		params[key] = value
	}
	return params
}

// Asking for a terminal, handing it over and seizing it name the run they are
// for, and one that names another run, or none, is refused with the terminal
// left as it was. The verbs named no run, and a tab left on a run that "Save
// and restart" had replaced took the new run's free terminal with its "Ask for
// the terminal" button: the tab dropped the new run's hand frame and never
// knew it held the terminal, nobody else could take it, and the policy
// answered nothing more on that agent, since its terminal was held. Letting go
// of a terminal and declining a request need no run.
func TestAWebTerminalIsTakenOrHandedOverOnlyForTheRunItNames(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	g.ctrl.allowForceTakeover = true
	base := g.serve()
	alice := dialSharedGateway(t, base, "opAlice")
	carol := dialSharedGateway(t, base, "opCarol")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()

	for _, stale := range []string{"", "a-run-that-was-replaced"} {
		for _, method := range []string{"requestControl", "forceTakeControl"} {
			err := carol.call(method, map[string]any{"runID": stale, "sessionID": "web-listen"}, nil)
			if err == nil || err.Error() != supervise.ErrRunStale.Error() {
				t.Fatalf("%s naming run %q = %v, want the run refused", method, stale, err)
			}
		}
	}
	if hand := g.ctrl.HandFor("web-listen"); hand.State != HandFree {
		t.Fatalf("a request for another run took the terminal: %+v", hand)
	}

	carol.mustCall("requestControl", controlParams(t, carol, "web-listen", nil), nil)
	alice.mustCall("requestControl", controlParams(t, alice, "web-listen", nil), nil)
	for _, stale := range []string{"", "a-run-that-was-replaced"} {
		err := carol.call("grantControl", map[string]any{"runID": stale, "sessionID": "web-listen", "toConnID": alice.connID}, nil)
		if err == nil || err.Error() != supervise.ErrRunStale.Error() {
			t.Fatalf("grantControl naming run %q = %v, want the run refused", stale, err)
		}
	}
	if hand := g.ctrl.HandFor("web-listen"); hand.HolderConnID != carol.connID || hand.RequesterConnID != alice.connID {
		t.Fatalf("a grant for another run moved the terminal: %+v", hand)
	}
	carol.mustCall("declineControl", map[string]any{"sessionID": "web-listen", "toConnID": alice.connID}, nil)
	carol.mustCall("releaseControl", map[string]any{"sessionID": "web-listen"}, nil)
	if hand := g.ctrl.HandFor("web-listen"); hand.State != HandFree {
		t.Fatalf("after the release the terminal is %+v, want it free", hand)
	}
	if runID != g.runID() {
		t.Fatal("the run changed")
	}
}

// A terminal taken by asking for it while it is free, handed over, or seized
// is journaled before the hand moves: its new holder's keystrokes are not
// admitted until the record of who took the terminal is written. The record
// stays best effort: one the journal refuses freezes the run, which then
// admits no keystroke at all, and the hand moves all the same. The records
// were written once the hand had moved, and the new holder's first keystrokes
// could reach the agent before any record said who was at the terminal.
func TestAWebTerminalChangingHandsIsJournaledBeforeItTakesKeystrokes(t *testing.T) {
	for _, test := range []struct {
		name string
		kind audit.Kind
		// take has carol take the terminal; alice may hold it first.
		take func(g *webGateway, runID string, alice, carol *sharedGatewayClient) error
	}{
		{name: "asked for while free", kind: audit.KindControlRequested,
			take: func(g *webGateway, runID string, alice, carol *sharedGatewayClient) error {
				_, err := g.ctrl.RequestControl(runID, "web-listen", carol.connID, "carol")
				return err
			}},
		{name: "handed over", kind: audit.KindControlGranted,
			take: func(g *webGateway, runID string, alice, carol *sharedGatewayClient) error {
				if _, err := g.ctrl.RequestControl(runID, "web-listen", alice.connID, "alice"); err != nil {
					return err
				}
				if _, err := g.ctrl.RequestControl(runID, "web-listen", carol.connID, "carol"); err != nil {
					return err
				}
				_, err := g.ctrl.GrantControl(runID, "web-listen", alice.connID, "alice", carol.connID)
				return err
			}},
		{name: "seized", kind: audit.KindControlForced,
			take: func(g *webGateway, runID string, alice, carol *sharedGatewayClient) error {
				if _, err := g.ctrl.RequestControl(runID, "web-listen", alice.connID, "alice"); err != nil {
					return err
				}
				_, err := g.ctrl.ForceTakeControl(runID, "web-listen", carol.connID, "carol")
				return err
			}},
	} {
		t.Run(test.name, func(t *testing.T) {
			fault, wrap := newFaultEngine()
			g := startWebRun(t, webRun{
				engineWrap: wrap,
				agents:     []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
			})
			g.ctrl.allowForceTakeover = true
			base := g.serve()
			alice := dialSharedGateway(t, base, "opAlice")
			carol := dialSharedGateway(t, base, "opCarol")
			g.awaitScreen("web-listen", "agent ready", 30*time.Second)
			runID := g.runID()

			hold := make(chan struct{})
			held := make(chan audit.Kind, 4)
			released := false
			release := func() {
				if !released {
					released = true
					close(hold)
				}
			}
			defer release()
			fault.set(func(f *faultEngine) {
				f.holdAuditKind = test.kind
				f.holdAudit = hold
				f.auditHeld = held
			})
			taken := make(chan error, 1)
			go func() { taken <- test.take(g, runID, alice, carol) }()
			select {
			case <-held:
			case err := <-taken:
				t.Fatalf("the terminal changed hands (%v) without its record being written", err)
			case <-time.After(10 * time.Second):
				t.Fatal("the record of the terminal changing hands was never written")
			}

			typed := make(chan error, 1)
			go func() {
				typed <- g.ctrl.SendTerminalInput(runID, "web-listen", []byte("before-the-record\r"), "carol", carol.connID)
			}()
			select {
			case err := <-typed:
				if err == nil {
					t.Fatal("the new holder's keystrokes were written before the record of who took the terminal")
				}
			case <-time.After(500 * time.Millisecond):
			}
			release()
			if err := <-taken; err != nil {
				t.Fatalf("taking the terminal: %v", err)
			}
			select {
			case <-typed:
			case <-time.After(10 * time.Second):
				t.Fatal("the keystrokes never returned")
			}
			if hand := g.ctrl.HandFor("web-listen"); hand.HolderConnID != carol.connID {
				t.Fatalf("the terminal is %+v, want it carol's", hand)
			}
			if entries := entriesOf(g.journal(), test.kind); len(entries) == 0 || entries[len(entries)-1].Outcome != audit.OutcomeApplied {
				t.Fatalf("%s entries = %s, want the change journaled", test.kind, journalTrace(entries))
			}
		})
	}
}
