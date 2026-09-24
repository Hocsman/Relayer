package server

import (
	"errors"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// A viewer is refused a decision and a line by the core itself, whatever the
// gateway lets through. The gateway already refuses a viewer twice, by the
// list of calls a viewer may make and by refusing it the terminal; the core,
// given the connection's role, is the third gate, and holds whatever a later
// change to the other two forgets.
func TestTheCoreRefusesAWebViewerWhateverTheGatewayLetsThrough(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-generic", mode: webAgentGeneric, adapter: "generic"}},
	})
	prompt := g.awaitPending("web-generic", 30*time.Second)
	viewer := supervise.Actor{Identity: "dave", Role: string(RoleViewer), ConnID: "conn-dave"}

	if err := g.ctrl.SubmitDecision(prompt.RunID, prompt.SessionID, prompt.ID, "from a viewer", viewer); !errors.Is(err, supervise.ErrReadOnlyActor) {
		t.Fatalf("a viewer's typed answer returned %v, want ErrReadOnlyActor", err)
	}
	if err := g.ctrl.SubmitAutomaticDecision(prompt.RunID, prompt.SessionID, prompt.ID, "deny", viewer); !errors.Is(err, supervise.ErrReadOnlyActor) {
		t.Fatalf("a viewer's chosen answer returned %v, want ErrReadOnlyActor", err)
	}
	if err := g.ctrl.SubmitLine(prompt.RunID, prompt.SessionID, "from a viewer", viewer); !errors.Is(err, supervise.ErrReadOnlyActor) {
		t.Fatalf("a viewer's line returned %v, want ErrReadOnlyActor", err)
	}
	g.assertNoAnswerFor("web-generic", 500*time.Millisecond)
	entries := g.sessionJournal("web-generic")
	if found := append(entriesOf(entries, audit.KindDecision), entriesOf(entries, audit.KindOperatorInput)...); len(found) != 0 {
		t.Fatalf("a viewer's refused request was journaled: %s", journalTrace(found))
	}
}
