package server

import (
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// failingStartEngine holds each StartAgent until gate is closed, then fails
// it, as a process that could not be launched.
type failingStartEngine struct {
	supervise.Engine
	gate chan struct{}
}

func (e *failingStartEngine) StartAgent(ctx context.Context, agentID string) error {
	<-e.gate
	return errors.New("injected: the process could not be started")
}

// shownStatus is the status a client shows for the session once it has taken
// in frames, in order, as the web interface's reducer does: a snapshot of the
// run's session at a revision no older than the last one taken, or a
// session-scope status of the run.
func shownStatus(runID, sessionID, initial string, frames []webFrame) string {
	status := initial
	var revision uint64
	for _, frame := range frames {
		switch payload := frame.payload.(type) {
		case SnapshotEvent:
			if payload.RunID == runID && payload.SessionID == sessionID && payload.Revision >= revision {
				revision = payload.Revision
				status = payload.Status
			}
		case StatusEvent:
			if payload.RunID == runID && payload.Scope == "session" && payload.SessionID == sessionID {
				status = payload.Status
			}
		}
	}
	return status
}

// An output snapshot carries the agent's supervision state as it was read, and
// goes out after the core's own statuses when it was read before one of them
// and broadcast after it: the core's statuses go out in its ordered flush, the
// snapshot outside it. A start that failed showed the agent failed, then the
// snapshot showed it starting again, and nothing corrected it: every client
// hid the agent's Start button until it reloaded. A snapshot that no longer
// says what the core holds is now followed by one that does. The test holds
// the snapshot's broadcast where a descheduled event loop would.
func TestAWebSnapshotReadBeforeAFailedStartDoesNotHaveTheLastWord(t *testing.T) {
	gate := make(chan struct{})
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
		engineWrap: func(engine supervise.Engine) supervise.Engine {
			return &failingStartEngine{Engine: engine, gate: gate}
		},
	})
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()
	if err := g.ctrl.StopSession(runID, "web-listen"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	g.awaitAgent("web-listen", 10*time.Second, func(agent AgentState) bool { return !agent.Running && agent.Status == "exited" })
	time.Sleep(300 * time.Millisecond)

	var (
		mu      sync.Mutex
		frames  []webFrame
		armed   = make(chan struct{})
		holding = make(chan struct{})
		release = make(chan struct{})
		once    sync.Once
	)
	// One subscriber is one client's queue: frames enter it in the order its
	// calls are made.
	g.ctrl.Subscribe(func(event string, payload any) {
		if snapshot, ok := payload.(SnapshotEvent); ok && snapshot.SessionID == "web-listen" {
			select {
			case <-armed:
				held := false
				once.Do(func() { held = true })
				if held {
					close(holding)
					<-release
				}
			default:
			}
		}
		mu.Lock()
		frames = append(frames, webFrame{at: time.Now(), event: event, payload: payload})
		mu.Unlock()
	})
	initial := g.agent("web-listen").Status

	started := make(chan error, 1)
	go func() { started <- g.ctrl.StartSession(runID, "web-listen") }()
	g.awaitAgent("web-listen", 10*time.Second, func(agent AgentState) bool { return agent.Status == "starting" })
	close(armed)
	refreshed := make(chan struct{})
	go func() {
		defer close(refreshed)
		injectEvent(g.ctrl, session.OutputAvailable{SessionID: "web-listen"})
	}()
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		t.Fatal("no snapshot was read while the agent was starting")
	}
	close(gate)
	if err := <-started; err == nil {
		t.Fatal("the start was meant to fail")
	}
	close(release)
	<-refreshed

	agent := g.agent("web-listen")
	mu.Lock()
	shown := shownStatus(runID, "web-listen", initial, frames)
	var trace []string
	for _, frame := range frames {
		switch payload := frame.payload.(type) {
		case SnapshotEvent:
			if payload.SessionID == "web-listen" {
				trace = append(trace, "snapshot:"+payload.Status)
			}
		case StatusEvent:
			if payload.SessionID == "web-listen" {
				trace = append(trace, "status:"+payload.Status)
			}
		}
	}
	mu.Unlock()
	if shown != agent.Status {
		t.Fatalf("a client shows the agent %s; the gateway has it %s. Frames: %s", shown, agent.Status, strings.Join(trace, " "))
	}
}

// The status the attach verb shows is read before it is broadcast, outside
// the core's ordered flush, as a snapshot is: a status the core shows between
// the two, such as a stop's, was followed by the attach's older one, and every
// client showed a stopped agent running. It is followed by the core's status
// when that changed meanwhile. The test holds the attach's status where a
// descheduled goroutine would.
func TestAWebAttachStatusReadBeforeAStopDoesNotHaveTheLastWord(t *testing.T) {
	g := startWebRun(t, webRun{
		agents: []webAgent{{id: "web-listen", mode: webAgentListen, adapter: "generic"}},
	})
	alice := dialSharedGateway(t, g.serve(), "opAlice")
	g.awaitScreen("web-listen", "agent ready", 30*time.Second)
	runID := g.runID()

	var (
		mu      sync.Mutex
		frames  []webFrame
		armed   = make(chan struct{})
		holding = make(chan struct{})
		release = make(chan struct{})
		once    sync.Once
	)
	g.ctrl.Subscribe(func(event string, payload any) {
		if status, ok := payload.(StatusEvent); ok && status.SessionID == "web-listen" && status.Status == "running" {
			select {
			case <-armed:
				held := false
				once.Do(func() { held = true })
				if held {
					close(holding)
					<-release
				}
			default:
			}
		}
		mu.Lock()
		frames = append(frames, webFrame{at: time.Now(), event: event, payload: payload})
		mu.Unlock()
	})
	initial := g.agent("web-listen").Status

	close(armed)
	attached := make(chan error, 1)
	go func() { attached <- g.ctrl.SetInteractiveSession(runID, "web-listen", true, "alice", alice.connID) }()
	select {
	case <-holding:
	case <-time.After(10 * time.Second):
		t.Fatal("the attach showed no running status")
	}
	if err := g.ctrl.StopSession(runID, "web-listen"); err != nil {
		t.Fatalf("StopSession: %v", err)
	}
	close(release)
	if err := <-attached; err != nil {
		t.Fatalf("SetInteractiveSession: %v", err)
	}

	agent := g.agent("web-listen")
	mu.Lock()
	shown := shownStatus(runID, "web-listen", initial, frames)
	mu.Unlock()
	if shown != agent.Status {
		t.Fatalf("a client shows the agent %s; the gateway has it %s", shown, agent.Status)
	}
}
