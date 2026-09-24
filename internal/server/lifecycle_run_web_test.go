package server

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/session"
)

// A stop, a start or a restart of an agent acts only on the run it names. The
// gateway ignored the run: a tab left open on a run a profile save had
// replaced, or a request naming none, stopped, started or restarted the agent
// of the same name in the new run, which that tab had never shown.
func TestAWebLifecycleRequestActsOnlyOnTheRunItNames(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()
	request := func(method, runID string) error {
		return alice.call(method, map[string]any{"runID": runID, "sessionID": "web-listen"}, nil)
	}
	otherRuns := []string{"", "a-run-that-was-replaced"}

	for _, other := range otherRuns {
		for _, method := range []string{"stopSession", "restartSession"} {
			if err := request(method, other); err == nil {
				t.Fatalf("%s for run %q was taken", method, other)
			}
		}
	}
	if agent := g.agent("web-listen"); !agent.Running || agent.Status != "running" {
		t.Fatalf("after requests for other runs the agent is %+v, want it running", agent)
	}
	for _, frame := range g.broadcast(eventStatus) {
		if status, ok := frame.payload.(StatusEvent); ok && status.SessionID == "web-listen" {
			t.Fatalf("a request for another run changed the agent: %+v", status)
		}
	}

	if err := request("stopSession", runID); err != nil {
		t.Fatalf("stopSession for the run: %v", err)
	}
	g.awaitAgent("web-listen", 10*time.Second, func(agent AgentState) bool { return !agent.Running })
	for _, other := range otherRuns {
		if err := request("startSession", other); err == nil {
			t.Fatalf("startSession for run %q was taken", other)
		}
	}
	time.Sleep(300 * time.Millisecond)
	if agent := g.agent("web-listen"); agent.Running {
		t.Fatalf("a start for another run started the agent: %+v", agent)
	}
	if err := request("startSession", runID); err != nil {
		t.Fatalf("startSession for the run: %v", err)
	}
	g.awaitAgent("web-listen", 10*time.Second, func(agent AgentState) bool { return agent.Running })
}

// finishedRecordings is the sessions whose finished recording the gateway
// announced so far.
func (g *webGateway) finishedRecordings() []string {
	var sessions []string
	for _, frame := range g.broadcast(eventRecording) {
		if announced, ok := frame.payload.(RecordingEvent); ok && announced.Action == "finished" {
			sessions = append(sessions, announced.Recording.SessionID)
		}
	}
	return sessions
}

// awaitFinishedRecording waits until the session's finished recording is
// announced.
func (g *webGateway) awaitFinishedRecording(sessionID string, within time.Duration) {
	g.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, announced := range g.finishedRecordings() {
			if strings.EqualFold(announced, sessionID) {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	g.t.Fatalf("the finished recording of %s was never announced; announced: %v", sessionID, g.finishedRecordings())
}

// Every client is told a session's recording is finished when its process
// ends, on its own, stopped by an operator, or because Relayer lost its tmux
// session, so the recordings panel offers the replay without polling. The
// gateway announced it only in the last case: a process that exited, or was
// stopped, left its replay unannounced until a client reloaded the list.
func TestAWebRecordingIsAnnouncedWhenItsProcessEnds(t *testing.T) {
	g := startWebRun(t, webRun{
		recording: true,
		agents: []webAgent{
			{id: "web-exit", mode: webAgentExit, adapter: "generic", env: map[string]string{webAgentDelayEnv: "1s"}},
			{id: "web-stopped", mode: webAgentListen, adapter: "generic"},
			{id: "web-lost", mode: webAgentListen, adapter: "generic"},
		},
	})
	g.awaitScreen("web-stopped", "agent ready", 30*time.Second)
	g.awaitScreen("web-lost", "agent ready", 30*time.Second)

	g.awaitAgent("web-exit", 30*time.Second, func(agent AgentState) bool { return !agent.Running })
	g.awaitFinishedRecording("web-exit", 10*time.Second)

	if err := g.ctrl.StopSession(g.runID(), "web-stopped"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	g.awaitFinishedRecording("web-stopped", 10*time.Second)

	g.inject(session.Exited{SessionID: "web-lost", Err: errors.New("tmux supervision interrupted")})
	g.awaitFinishedRecording("web-lost", 10*time.Second)
}
