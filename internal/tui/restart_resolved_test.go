package tui

import (
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
)

// TestARestartKeepsThePreviousProcesssAnsweredPrompts: a new process gives its
// prompts IDs of their own, so the TUI no longer forgets a session's answered
// prompts at a restart. Remembering them keeps a late copy of an old prompt
// from being queued on the replacement, while the replacement's own prompt,
// with its new ID, is taken in.
func TestARestartKeepsThePreviousProcesssAnsweredPrompts(t *testing.T) {
	application, _, _ := newModelHarness(t)
	application.rememberResolved("agent-a", "evt-previous")

	application.applyLifecycleResult(agentLifecycleMsg{Action: "restart", SessionID: "agent-a", Name: "Agent A"})

	if !application.eventResolved("agent-a", "evt-previous") {
		t.Fatal("a restart forgot the previous process's answered prompt, so a late copy of it would be queued again")
	}
	if application.eventResolved("agent-a", "evt-replacement") {
		t.Fatal("the replacement's own prompt is treated as answered")
	}
}

// TestAnEndedProcessLeavesNoPromptTimes: the time a prompt was detected is kept
// until it is decided, and a prompt pending when its process exits is never
// decided; each one stayed in the map for good.
func TestAnEndedProcessLeavesNoPromptTimes(t *testing.T) {
	application, _, _ := newModelHarness(t)
	application.promptDetectedAt[eventKey{sessionID: "agent-a", eventID: "evt-pending"}] = time.Now()
	application.promptDetectedAt[eventKey{sessionID: "agent-b", eventID: "evt-live"}] = time.Now()

	code := 0
	application.applyProcessExit(adapters.NewProcessExitEvent("agent-a", "agent-a", adapters.GenericID, 1, &code, false))

	if _, kept := application.promptDetectedAt[eventKey{sessionID: "agent-a", eventID: "evt-pending"}]; kept {
		t.Fatal("the exited process's prompt time is still kept")
	}
	if _, kept := application.promptDetectedAt[eventKey{sessionID: "agent-b", eventID: "evt-live"}]; !kept {
		t.Fatal("another session's prompt time was dropped")
	}
	for key := range application.promptDetectedAt {
		if key.sessionID == "agent-a" {
			t.Fatalf("the exit left a detection time of its own: %v", key)
		}
	}
}
