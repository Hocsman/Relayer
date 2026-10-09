package server

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/supervise"
)

const commandOnTheWire = "rm -rf /home/someone/build"

// The gateway's prompt is built from the core's view by one function; a command
// it forgot to copy would reach no operator, and no other test here goes through
// it (they build the gateway's prompt by hand).
func TestTheGatewayCopiesTheCommandOfTheCoreView(t *testing.T) {
	prompt := supervisionEventFromView(supervise.View{ID: "prompt-command", Command: commandOnTheWire})
	if prompt.Command != commandOnTheWire {
		t.Fatalf("the gateway's prompt holds command %q, want the core view's %q", prompt.Command, commandOnTheWire)
	}
}

// The command a prompt asks about is agent text read from the screen. An
// operator is shown it; a viewer is shown that a prompt waits and what it is,
// not what the agent was about to run, in the frames pushed to it and in the
// state it reads.
func TestAViewerIsNeverSentTheCommandAPromptAsksAbout(t *testing.T) {
	g := startWebRun(t, webRun{agents: []webAgent{{id: "idle", mode: webAgentListen, adapter: "generic"}}})
	baseURL := servedWithHiddenTerminals(g)
	viewer := dialSharedGateway(t, baseURL, "viewDave")
	operator := dialSharedGateway(t, baseURL, "opAlice")

	g.ctrl.broadcast(eventSemantic, SupervisionEvent{
		RunID:     g.ctrl.GetState().RunID,
		ID:        "prompt-command",
		SessionID: "idle",
		AgentID:   "idle",
		Adapter:   "claude",
		Type:      "permission",
		Summary:   "Claude Code asks to run a shell command",
		Risk:      "high",
		Decisions: []string{},
		Command:   commandOnTheWire,
	})

	commandIn := func(client *sharedGatewayClient) (string, bool) {
		deadline := time.After(10 * time.Second)
		for {
			select {
			case event, ok := <-client.events:
				if !ok {
					t.Fatal("a socket closed")
				}
				if event.Event != eventSemantic {
					continue
				}
				encoded, _ := json.Marshal(event.Payload)
				var prompt SupervisionEvent
				if err := json.Unmarshal(encoded, &prompt); err != nil {
					t.Fatalf("decode %s: %v", encoded, err)
				}
				if prompt.ID == "prompt-command" {
					return prompt.Command, prompt.Summary == "Claude Code asks to run a shell command"
				}
			case <-deadline:
				t.Fatal("the prompt's frame never came")
			}
		}
	}
	if command, labelled := commandIn(operator); command != commandOnTheWire || !labelled {
		t.Fatalf("the operator's frame has command %q (labelled %v), want the command", command, labelled)
	}
	if command, labelled := commandIn(viewer); command != "" || !labelled {
		t.Fatalf("the viewer's frame has command %q (labelled %v), want the prompt without it", command, labelled)
	}
}

// stateForRole is what getState and /api/state answer: the pending prompts of a
// viewer's state carry no command, and the state it was made from is not changed.
func TestAViewerStateHoldsNoCommandAndTheOriginalKeepsIt(t *testing.T) {
	state := AppState{PendingEvents: []SupervisionEvent{
		{ID: "a", Summary: "Claude Code asks to run a shell command", Command: commandOnTheWire},
		{ID: "b", Summary: "Overwrite file?"},
	}}
	viewer := stateForRole(state, RoleViewer)
	for _, prompt := range viewer.PendingEvents {
		if prompt.Command != "" {
			t.Fatalf("viewer prompt %s holds %q", prompt.ID, prompt.Command)
		}
	}
	if len(viewer.PendingEvents) != 2 || viewer.PendingEvents[0].Summary == "" {
		t.Fatalf("the viewer lost the prompts: %+v", viewer.PendingEvents)
	}
	if state.PendingEvents[0].Command != commandOnTheWire {
		t.Fatal("masking a viewer's state changed the state it was made from")
	}
	if operator := stateForRole(state, RoleOperator); operator.PendingEvents[0].Command != commandOnTheWire {
		t.Fatalf("the operator's state lost the command: %+v", operator.PendingEvents[0])
	}
}
