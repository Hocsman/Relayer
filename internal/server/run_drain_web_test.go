package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// runStatuses is every run-scope status the gateway broadcast so far.
func (g *webGateway) runStatuses() []StatusEvent {
	var statuses []StatusEvent
	for _, frame := range g.broadcast(eventStatus) {
		if status, ok := frame.payload.(StatusEvent); ok && status.Scope == "run" {
			statuses = append(statuses, status)
		}
	}
	return statuses
}

// awaitRunStatus waits until the gateway broadcast the run-scope status of
// runID.
func (g *webGateway) awaitRunStatus(runID, status string, within time.Duration) {
	g.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, shown := range g.runStatuses() {
			if shown.RunID == runID && shown.Status == status {
				return
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	g.t.Fatalf("no run status %q for run %s within %s; run statuses: %+v", status, runID, within, g.runStatuses())
}

// forgetScreens drops the screens kept of the previous run's agents. A
// restarted agent's screen starts empty, and until the new process paints its
// own, the one kept would be taken for it, its "agent ready" included: a test
// would type before the new agent reads, or check a screen it never painted.
func (g *webGateway) forgetScreens() {
	g.mu.Lock()
	g.screens = map[string]string{}
	g.mu.Unlock()
}

// profileInputs is the run's agents as the settings panel sends them back,
// each with the command of a helper agent in the mode modes names for it.
func profileInputs(t *testing.T, g *webGateway, modes map[string]string) (string, []AgentProfileInput) {
	t.Helper()
	profiles, err := g.ctrl.GetAgentProfiles()
	if err != nil {
		t.Fatalf("GetAgentProfiles: %v", err)
	}
	inputs := make([]AgentProfileInput, 0, len(profiles.Profiles))
	for _, profile := range profiles.Profiles {
		inputs = append(inputs, AgentProfileInput{
			ID:      profile.ID,
			Name:    profile.Name,
			Cwd:     profile.Cwd,
			Backend: "pty",
			Adapter: "generic",
			Argv:    webAgentArgv(t, modes[profile.ID]),
		})
	}
	return profiles.Revision, inputs
}

// "Save and restart" acts only on the run it names, and the run it starts
// keeps nothing of the one it replaces: not its prompts, not who held its
// terminals. The gateway ignored the run the request named, so a tab left
// open on a replaced run restarted the run that replaced it. And it gave the
// new run the previous run's hands: the holder's tab still showed a terminal
// of the process that was gone, and the connection went on typing into the
// new process, which the operator had never attached to and the journal had
// no attach of. Every client is told the new run's ID, so a tab on the
// previous run can load the new one.
func TestAWebRunStartedAgainKeepsNothingOfThePreviousRun(t *testing.T) {
	g := startWebRun(t, webRun{
		auditMode: "detailed",
		agents: []webAgent{
			{id: "web-generic", mode: webAgentGeneric, adapter: "generic"},
			{id: "web-held", mode: webAgentListen, adapter: "generic"},
		},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-held", "agent ready", 30*time.Second)
	previousRun := g.runID()
	alice.mustCall("setInteractiveSession", map[string]any{"runID": previousRun, "sessionID": "web-held", "active": true}, nil)
	previousPrompt := g.awaitPending("web-generic", 30*time.Second)
	revision, profiles := profileInputs(t, g, map[string]string{"web-generic": webAgentGeneric, "web-held": webAgentListen})

	for _, runID := range []string{"", "a-run-that-was-replaced"} {
		err := alice.call("saveAgentProfilesAndRestart", SaveAgentProfilesAndRestartRequest{
			ExpectedRunID: runID, ExpectedRevision: revision, Profiles: profiles,
		}, nil)
		if err == nil {
			t.Fatalf("a restart naming run %q was taken", runID)
		}
	}
	if g.runID() != previousRun {
		t.Fatalf("a restart naming another run replaced the run")
	}

	var result LifecycleResult
	if err := alice.call("saveAgentProfilesAndRestart", SaveAgentProfilesAndRestartRequest{
		ExpectedRunID: previousRun, ExpectedRevision: revision, Profiles: profiles,
	}, &result); err != nil {
		t.Fatalf("saveAgentProfilesAndRestart: %v", err)
	}
	g.forgetScreens()
	newRun := g.runID()
	if result.Outcome != "restarted" || newRun == previousRun || result.State.RunID != newRun {
		t.Fatalf("the run did not start again: outcome %q, run %q, previous %q", result.Outcome, newRun, previousRun)
	}
	g.awaitRunStatus(newRun, "running", 5*time.Second)

	for _, prompt := range g.ctrl.GetState().PendingEvents {
		if prompt.ID == previousPrompt.ID || prompt.RunID != newRun {
			t.Fatalf("the new run offers a prompt of the previous one: %+v", prompt)
		}
	}
	if hand := g.ctrl.HandFor("web-held"); hand.State != HandFree {
		t.Fatalf("after the restart the hand is %+v, want the new run's terminal free", hand)
	}
	if agent := g.agent("web-held"); agent.Attached || agent.HolderIdentity != "" {
		t.Fatalf("after the restart the agent is shown held: %+v", agent)
	}
	g.awaitScreen("web-held", "agent ready", 30*time.Second)
	if err := alice.typeKeys(newRun, "web-held", "into-the-new-run\r"); err == nil {
		t.Fatal("the previous run's holder typed into the new run's terminal")
	}
	alice.sendKeys("web-held", "framed-into-the-new-run\r")
	g.assertScreenLacks("web-held", "into-the-new-run", time.Second)

	// The previous run's journal says its terminal was let go.
	released := false
	for _, entry := range g.sessionJournal("web-held") {
		if entry.RunID == previousRun && entry.Kind == audit.KindControlReleased {
			released = entry.Reason == "control_released_run_end" && entry.DecisionBy == audit.DecisionBySystem && entry.Metadata["conn_id"] == alice.connID
		}
	}
	if !released {
		t.Fatalf("the previous run's journal does not say its terminal was let go:\n%s", journalTrace(g.sessionJournal("web-held")))
	}
}

// Ending a run waits for the answer being written to it, and journals that
// answer's outcome before the run's end, while every client can still read the
// state. The gateway cancelled the run and closed its runtime under its own
// lock without waiting: an answer being written was cut off from its journal,
// whose last entry said it was being delivered, and the gateway's clients
// could read nothing for as long as the runtime took to close.
func TestEndingAWebRunWaitsForTheAnswerBeingWritten(t *testing.T) {
	for _, ending := range []struct {
		name string
		end  func(g *webGateway, runID string) error
	}{
		{"StopRun", func(g *webGateway, runID string) error {
			_, err := g.ctrl.StopRun(runID)
			return err
		}},
		{"Close", func(g *webGateway, runID string) error {
			ctx, cancel := context.WithTimeout(context.Background(), session.StopBudget+10*time.Second)
			defer cancel()
			return g.ctrl.Close(ctx)
		}},
	} {
		t.Run(ending.name, func(t *testing.T) {
			fault, wrap := newFaultEngine()
			g := startWebRun(t, webRun{
				engineWrap: wrap,
				agents:     []webAgent{{id: "web-generic", mode: webAgentGeneric, adapter: "generic"}},
			})
			prompt := g.awaitPending("web-generic", 30*time.Second)
			runID := g.runID()
			release := make(chan struct{})
			released := false
			defer func() {
				if !released {
					close(release)
				}
			}()
			fault.set(func(f *faultEngine) { f.holdApplyFirm = release })
			answered := make(chan error, 1)
			go func() {
				answered <- g.ctrl.SubmitDecision(runID, prompt.SessionID, prompt.ID, "the last answer", webOperator)
			}()
			select {
			case <-fault.applyStarted:
			case <-time.After(10 * time.Second):
				t.Fatal("the answer's write never started")
			}

			ended := make(chan error, 1)
			go func() { ended <- ending.end(g, runID) }()
			time.Sleep(time.Second)
			select {
			case err := <-ended:
				t.Fatalf("the run ended (%v) while an answer was still being written to it", err)
			default:
			}
			read := make(chan AppState, 1)
			go func() { read <- g.ctrl.GetState() }()
			select {
			case <-read:
			case <-time.After(2 * time.Second):
				t.Fatal("the state could not be read while the run was ending")
			}
			if err := g.ctrl.ResizeSession(runID, "web-generic", 100, 30, ""); err == nil {
				t.Fatal("a resize was written to a run that is ending")
			}

			close(release)
			released = true
			if err := <-answered; err == nil {
				t.Fatal("an answer cut off by the run's end reported it was delivered")
			}
			select {
			case err := <-ended:
				if err != nil {
					t.Fatalf("ending the run: %v", err)
				}
			case <-time.After(30 * time.Second):
				t.Fatal("the run never ended once the answer's write returned")
			}

			entries := g.journal()
			decided, outcome, finished := -1, -1, -1
			for index, entry := range entries {
				switch {
				case entry.Kind == audit.KindDecision && entry.EventID == prompt.ID:
					decided = index
				case entry.Kind == audit.KindDelivery && entry.EventID == prompt.ID:
					outcome = index
				case entry.Kind == audit.KindRunFinished && entry.RunID == runID:
					finished = index
				}
			}
			if decided < 0 || outcome < decided || finished < outcome {
				t.Fatalf("the journal does not say what became of the answer before the run ended:\n%s", journalTrace(entries))
			}
		})
	}
}

// A run that stopped says so to every client, and takes nothing more: no
// answer, line, keystroke or terminal. And "Save and restart" starts the run
// again from there.
func TestAStoppedWebRunTakesNothingUntilItIsStartedAgain(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()
	revision, profiles := profileInputs(t, g, map[string]string{"web-listen": webAgentListen})

	var stopped AppState
	alice.mustCall("stopRun", map[string]any{"runID": runID}, &stopped)
	if stopped.RunStatus != "stopped" || len(stopped.Agents) != 0 || len(stopped.PendingEvents) != 0 {
		t.Fatalf("the stopped run's state = %+v, want stopped with no agent", stopped)
	}
	g.awaitRunStatus(runID, "stopped", 5*time.Second)
	for _, err := range []error{
		alice.call("submitLine", map[string]any{"runID": runID, "sessionID": "web-listen", "line": "late"}, nil),
		alice.typeKeys(runID, "web-listen", "late\r"),
		alice.call("setInteractiveSession", map[string]any{"runID": runID, "sessionID": "web-listen", "active": true}, nil),
		alice.call("stopSession", map[string]any{"runID": runID, "sessionID": "web-listen"}, nil),
	} {
		if err == nil {
			t.Fatal("a stopped run took a request")
		}
	}

	var result LifecycleResult
	if err := alice.call("saveAgentProfilesAndRestart", SaveAgentProfilesAndRestartRequest{
		ExpectedRunID: runID, ExpectedRevision: revision, Profiles: profiles,
	}, &result); err != nil {
		t.Fatalf("saveAgentProfilesAndRestart after the stop: %v", err)
	}
	g.forgetScreens()
	if result.State.RunID == runID || result.State.RunStatus != "running" {
		t.Fatalf("the run did not start again: %+v", result.State)
	}
	g.awaitRunStatus(result.State.RunID, "running", 5*time.Second)
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	if err := g.ctrl.SubmitLine(result.State.RunID, "web-listen", "after-restart", webOperator); err != nil {
		t.Fatalf("a line to the new run: %v", err)
	}
	g.awaitScreen("web-listen", webAgentLine+`"after-restart`, 10*time.Second)
	if err := g.ctrl.SubmitLine(runID, "web-listen", "to-the-stopped-run", webOperator); !errors.Is(err, supervise.ErrRunStale) {
		t.Fatalf("a line naming the stopped run returned %v, want ErrRunStale", err)
	}
	if strings.Contains(g.screen("web-listen"), "to-the-stopped-run") {
		t.Fatal("a line naming the stopped run reached the new run's agent")
	}
}

// Ending a run waits for what its core admitted for at most its budget. A
// write that never returns, as keystrokes blocked in the terminal of an agent
// that reads nothing do, held StopRun, "Save and restart" and Close for good:
// the drain waited with no bound, under the lock that serialises a run's end,
// with the run shown stopping, Serve unable to shut down and no run started or
// stopped again until the process was killed. Past its budget, the run's end
// goes on and closes the runtime, and the run is shown failed, since it did not
// end cleanly: no other run is started beside it.
func TestEndingAWebRunIsBoundedWhenAWriteNeverReturns(t *testing.T) {
	fault, wrap := newFaultEngine()
	g := startWebRun(t, webRun{
		engineWrap: wrap,
		agents:     []webAgent{{id: "web-generic", mode: webAgentGeneric, adapter: "generic"}},
	})
	prompt := g.awaitPending("web-generic", 30*time.Second)
	runID := g.runID()
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseWrite := func() { releaseOnce.Do(func() { close(release) }) }
	t.Cleanup(releaseWrite)
	// A write that ignores its context, as a PTY write blocked in the kernel
	// does.
	fault.set(func(f *faultEngine) { f.holdApplyFirm = release })
	go func() {
		_ = g.ctrl.SubmitDecision(runID, prompt.SessionID, prompt.ID, "never delivered", webOperator)
	}()
	select {
	case <-fault.applyStarted:
	case <-time.After(10 * time.Second):
		t.Fatal("the answer's write never started")
	}

	g.ctrl.drainBudget = 2 * time.Second
	stopped := make(chan error, 1)
	go func() {
		_, err := g.ctrl.StopRun(runID)
		stopped <- err
	}()
	// The strict stop, the bounded drain, then the runtime's close.
	limit := runEndBudget + g.ctrl.drainBudget + 10*time.Second
	select {
	case err := <-stopped:
		if !errors.Is(err, errLifecycleBlocked) {
			t.Fatalf("StopRun of a run whose write never returned = %v, want it reported not cleanly ended", err)
		}
	case <-time.After(limit):
		releaseWrite()
		<-stopped
		t.Fatalf("StopRun had not returned %s after it began: the drain waited on a write that never returns", limit)
	}
	g.awaitRunStatus(runID, "failed", 5*time.Second)
	if status := g.ctrl.GetState().RunStatus; status != "failed" {
		t.Fatalf("the run is shown %q, want failed", status)
	}
	if _, err := g.ctrl.StopRun(runID); !errors.Is(err, errLifecycleBlocked) {
		t.Fatalf("a run started or stopped beside the one that did not end cleanly: %v", err)
	}
}
