package supervise_test

import (
	"fmt"
	"math/rand"
	"reflect"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// These tests pin the order in which the sink sees what the core shows: the
// order of the state changes it reports. A front end renders what it is shown
// as it comes; the web reducer, for one, puts a prompt's card back when the
// prompt is shown delivering after it was shown delivered. Each goroutine that
// changed the state used to show its change itself, once it had released the
// core's lock, and two goroutines reached the sink in either order.

// gateAt holds the first sink call match accepts before it is shown, until the
// test lets it go; arrived is signalled when the call is held.
func gateAt(t *testing.T, sink *recordingSink, match func(sinkCall) bool) (arrived <-chan struct{}, letGo func()) {
	t.Helper()
	held := make(chan struct{}, 1)
	open := make(chan struct{})
	var once sync.Once
	letGo = func() { once.Do(func() { close(open) }) }
	t.Cleanup(letGo)
	var taken atomic.Bool
	sink.mu.Lock()
	sink.gate = func(call sinkCall) {
		if match(call) && taken.CompareAndSwap(false, true) {
			held <- struct{}{}
			<-open
		}
	}
	sink.mu.Unlock()
	return held, letGo
}

// shownWithin waits up to within for the sink to show a call match accepts.
func shownWithin(sink *recordingSink, within time.Duration, match func(sinkCall) bool) bool {
	deadline := time.Now().Add(within)
	for {
		for _, call := range sink.snapshot() {
			if match(call) {
				return true
			}
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(time.Millisecond)
	}
}

func promptShown(eventID, delivery string) func(sinkCall) bool {
	return func(call sinkCall) bool {
		return call.kind == "prompt" && call.view.ID == eventID && call.view.DeliveryStatus == delivery
	}
}

func statusShown(status string) func(sinkCall) bool {
	return func(call sinkCall) bool { return call.kind == "status" && call.status.Status == status }
}

func within(t *testing.T, done <-chan struct{}, what string) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for %s", what)
	}
}

// A prompt is never shown delivering after it was shown delivered. The agent
// may withdraw a prompt as the operator's answer to it starts: the answer's
// write keeps the session, and the withdrawal shows the prompt delivered.
// Both goroutines showed their change once they had released the core's
// lock, and the withdrawal's could reach the sink first; the web reducer then
// put the answered prompt's card back, delivering, for good.
func TestAPromptIsNeverShownDeliveringAfterItWasShownDelivered(t *testing.T) {
	engine := newFakeEngine()
	release := make(chan struct{})
	engine.applyRelease = release
	sup, sink := newCoreForTest(t, engine, "agent-a")
	releaseWrites := releaser(t, release)
	prompt := promptEvent("agent-a", "prompt-1")
	sup.Handle(session.AdapterEvent{Event: prompt})
	sink.reset()
	arrived, letGo := gateAt(t, sink, promptShown("prompt-1", "delivering"))

	answered := make(chan error, 1)
	go func() { answered <- sup.SubmitDecision(testRunID, "agent-a", "prompt-1", "y", desktop) }()
	within(t, arrived, "the answer to be shown delivering")
	withdrawn := make(chan struct{})
	go func() {
		defer close(withdrawn)
		sup.Handle(session.AdapterEventWithdrawn{Event: prompt})
	}()
	waitFor(t, 2*time.Second, "the withdrawal", func() bool { return len(pendingIDs(sup)) == 0 })
	// Shown out of order, the withdrawal's delivered comes at once; in order,
	// it waits behind the answer's delivering, which the sink still holds.
	shownWithin(sink, 200*time.Millisecond, promptShown("prompt-1", "delivered"))
	letGo()
	within(t, withdrawn, "the withdrawal to return")
	releaseWrites()
	if err := <-answered; err != nil {
		t.Fatalf("SubmitDecision: %v", err)
	}

	var shown []string
	for _, call := range sink.snapshot() {
		if call.kind == "prompt" {
			shown = append(shown, call.view.DeliveryStatus)
		}
	}
	if want := []string{"delivering", "delivered"}; !reflect.DeepEqual(shown, want) {
		t.Fatalf("the prompt was shown %v, want %v", shown, want)
	}
}

// A session's statuses are shown in the order its changes were made. An
// automatic answer's delivery shows the agent running again, and an exit that
// came as that was being shown could be shown first: the agent was then left
// shown running although it had exited.
func TestASessionsStatusesAreShownInTheOrderOfItsChanges(t *testing.T) {
	engine := newFakeEngine()
	engine.evaluation = automaticAllow()
	sup, sink := newCoreForTest(t, engine, "agent-a")
	arrived, letGo := gateAt(t, sink, statusShown("running"))

	go sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "automatic-1")})
	within(t, arrived, "the delivery to show the agent running")
	exited := make(chan struct{})
	go func() {
		defer close(exited)
		sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)})
	}()
	waitFor(t, 2*time.Second, "the exit", func() bool { return !agentOf(t, sup, "agent-a").Running })
	shownWithin(sink, 200*time.Millisecond, statusShown("exited"))
	letGo()
	within(t, exited, "the exit to return")
	sup.BeginDrain()
	sup.Wait()

	var statuses []string
	for _, call := range sink.snapshot() {
		if call.kind == "status" {
			statuses = append(statuses, call.status.Status)
		}
	}
	if want := []string{"running", "exited"}; !reflect.DeepEqual(statuses, want) {
		t.Fatalf("the session was shown %v, want %v, the order its changes were made in", statuses, want)
	}
}

// An operation returns only once what it changed has been shown, even while
// another goroutine is showing what it queued: it waits for that goroutine
// rather than leave its own calls to it. A front end that reads its screen
// once a call returned, as the desktop's tests do, finds the call's change
// there, and a run that drained has nothing left to show.
func TestAnOperationReturnsOnceWhatItChangedIsShown(t *testing.T) {
	sup, sink := newCoreForTest(t, newFakeEngine(), "agent-a", "agent-b")
	arrived, letGo := gateAt(t, sink, promptShown("prompt-a", "pending"))
	go sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-a")})
	within(t, arrived, "the first prompt to be shown")

	returned := make(chan struct{})
	go func() {
		defer close(returned)
		sup.Handle(session.AdapterEvent{Event: promptEvent("agent-b", "prompt-b")})
	}()
	select {
	case <-returned:
		t.Fatal("the second prompt's Handle returned before its prompt was shown")
	case <-time.After(100 * time.Millisecond):
	}
	letGo()
	within(t, returned, "the second prompt's Handle to return")
	if !shownWithin(sink, 0, promptShown("prompt-b", "pending")) {
		t.Fatalf("Handle returned and its prompt is not shown: %v", trace(sink.snapshot()))
	}
}

// Under concurrency, what the sink sees follows the state it reports. An
// operator's answer, the automatic answer queued behind it, the agent's
// withdrawal of either and its exit race on one session, and the sink takes a
// varying moment over each call. Still no prompt is shown after it was shown
// delivered, nothing of the session is shown after its exit, the sink is
// never called by two goroutines at once, and the status it showed last is
// the one the core holds.
func TestWhatTheSinkSeesFollowsTheStateUnderConcurrency(t *testing.T) {
	const iterations = 300
	jitter := rand.New(rand.NewSource(1))
	var jitterMu sync.Mutex
	spin := func(most int) {
		jitterMu.Lock()
		turns := jitter.Intn(most + 1)
		jitterMu.Unlock()
		for turn := 0; turn < turns; turn++ {
			runtime.Gosched()
		}
	}
	for iteration := 0; iteration < iterations; iteration++ {
		engine := newFakeEngine()
		engine.evaluationByID["automatic-2"] = automaticAllow()
		sup, sink := newCoreForTest(t, engine, "agent-a")
		human := promptEvent("agent-a", "human-1")
		automatic := promptEvent("agent-a", "automatic-2")
		automatic.Sequence = 2
		sup.Handle(session.AdapterEvent{Event: human})
		sup.Handle(session.AdapterEvent{Event: automatic})
		sink.reset()
		sink.mu.Lock()
		sink.gate = func(sinkCall) { spin(40) }
		sink.mu.Unlock()

		var group sync.WaitGroup
		race := func(op func()) {
			group.Add(1)
			go func() {
				defer group.Done()
				spin(60)
				op()
			}()
		}
		race(func() { _ = sup.SubmitDecision(testRunID, "agent-a", "human-1", "y", desktop) })
		race(func() { sup.Handle(session.AdapterEventWithdrawn{Event: human}) })
		race(func() { sup.Handle(session.AdapterEventWithdrawn{Event: automatic}) })
		race(func() { sup.Handle(session.AdapterEvent{Event: exitEvent("agent-a", 0, false)}) })
		group.Wait()
		sup.BeginDrain()
		sup.Wait()

		if problem := orderProblem(sink, sup); problem != "" {
			t.Fatalf("iteration %d: %s\nshown: %v", iteration, problem, trace(sink.snapshot()))
		}
	}
}

// orderProblem is what the sink was shown out of the order of the state, or
// "" when nothing was.
func orderProblem(sink *recordingSink, sup *supervise.Supervisor) string {
	if sink.overlapped.Load() {
		return "the sink was called by two goroutines at once"
	}
	delivered := map[string]bool{}
	exited := false
	last := ""
	for _, call := range sink.snapshot() {
		if exited && (call.kind == "prompt" || call.kind == "status") {
			return fmt.Sprintf("%s %s shown after the session's exit", call.kind, call.view.ID+call.status.Status)
		}
		switch call.kind {
		case "prompt":
			if delivered[call.view.ID] {
				return fmt.Sprintf("prompt %s shown %s after it was shown delivered", call.view.ID, call.view.DeliveryStatus)
			}
			delivered[call.view.ID] = call.view.DeliveryStatus == "delivered"
		case "status":
			last = call.status.Status
			exited = call.status.ClearedBefore != "" && (last == "exited" || last == "failed")
		}
	}
	if agent, _ := sup.Agent("agent-a"); last != agent.Status {
		return fmt.Sprintf("the last status shown is %q, the core's is %q", last, agent.Status)
	}
	return ""
}
