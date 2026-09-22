package tui

import "testing"

// TestRestartForgetsTheSessionsResolvedPrompts: a fresh process numbers its
// prompts from 1 again, so the restarted agent's first prompt had the ID of the
// one the previous process left pending at its exit. That ID was remembered as
// resolved, so the new prompt was dropped and the agent could not be answered.
func TestRestartForgetsTheSessionsResolvedPrompts(t *testing.T) {
	application, _, _ := newModelHarness(t)
	application.rememberResolved("agent-a", "evt-1")
	application.rememberResolved("agent-b", "evt-1")

	application.applyLifecycleResult(agentLifecycleMsg{Action: "restart", SessionID: "agent-a", Name: "Agent A"})

	if application.eventResolved("agent-a", "evt-1") {
		t.Fatal("the restarted agent's first prompt would be dropped as already resolved")
	}
	if !application.eventResolved("agent-b", "evt-1") {
		t.Fatal("restarting one agent forgot another agent's resolved prompts")
	}
}
