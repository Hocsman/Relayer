package supervise_test

import (
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/supervise"
)

// toolCallPrompt is a prompt about an MCP tool call whose arguments carry a
// token, a password and a value longer than any badge.
func toolCallPrompt(sessionID, eventID string) adapters.Event {
	event := promptEvent(sessionID, eventID)
	event.ToolCall = &adapters.ToolCall{
		Server: "github",
		Tool:   "create_issue",
		Risk:   adapters.RiskHigh,
		Params: []adapters.ToolCallParam{
			{Name: "repo", Value: "Hocsman/Relayer"},
			{Name: "auth", Value: "ghp_abcdefghijklmnop1234"},
			{Name: "password", Value: "hunter2"},
			{Name: "body", Value: strings.Repeat("x", 300)},
		},
		ParamsTruncated: true,
	}
	return event
}

// A prompt about an MCP tool call shows the call as it may be shown to anyone
// who may see the prompt: the web gateway sends every prompt to every client,
// viewers included, and the call's parameter values are text the agent
// printed. The adapters' parser bounds them and redacts nothing, so a token an
// agent handed to a tool reached every screen as it was printed. The view now
// carries the call with each value redacted as the assignment it is, which
// catches a password that reads as no secret on its own, and bounded; a prompt
// whose text must not be shown carries no call at all; and what the view
// hands out is a copy. The desktop's JSON of a view is unchanged: its
// interface has no badge for a tool call.
func TestAPromptShowsItsToolCallOnlyAsItMayBeShown(t *testing.T) {
	engine := newFakeEngine()
	sup, sink := newCoreForTest(t, engine, "agent-a")
	sup.Handle(session.AdapterEvent{Event: toolCallPrompt("agent-a", "prompt-1")})

	pending := sup.State().Pending
	if len(pending) != 1 {
		t.Fatalf("pending = %v, want the prompt", pendingIDs(sup))
	}
	call := pending[0].ToolCall()
	if call == nil {
		t.Fatal("the prompt's view carries no tool call")
	}
	if call.Server != "github" || call.Tool != "create_issue" || call.Risk != adapters.RiskHigh || !call.ParamsTruncated {
		t.Fatalf("tool call = %+v, want github/create_issue, high risk, parameters truncated", *call)
	}
	values := map[string]adapters.ToolCallParam{}
	for _, param := range call.Params {
		values[param.Name] = param
	}
	if values["repo"].Value != "Hocsman/Relayer" {
		t.Errorf("repo = %q, want it shown as it is", values["repo"].Value)
	}
	for _, name := range []string{"auth", "password"} {
		if value := values[name].Value; value != "[REDACTED]" {
			t.Errorf("%s = %q, want it redacted", name, value)
		}
	}
	body := values["body"]
	if utf8.RuneCountInString(body.Value) > 256 || !body.Truncated {
		t.Errorf("body = %d runes, truncated %v; want at most 256 and marked truncated", utf8.RuneCountInString(body.Value), body.Truncated)
	}

	shown := false
	for _, recorded := range sink.snapshot() {
		if recorded.kind != "prompt" {
			continue
		}
		shown = true
		if recorded.view.ToolCall() == nil {
			t.Fatal("the prompt was shown without its tool call")
		}
		encoded, err := json.Marshal(recorded.view)
		if err != nil {
			t.Fatalf("marshal the view: %v", err)
		}
		if strings.Contains(string(encoded), "toolCall") || strings.Contains(string(encoded), "ghp_") {
			t.Fatalf("the view's JSON, the desktop's, gained the tool call: %s", encoded)
		}
	}
	if !shown {
		t.Fatal("the prompt was never shown")
	}

	call.Params[0].Value = "edited"
	call.Tool = "delete_repo"
	again := sup.State().Pending[0].ToolCall()
	if again.Tool != "create_issue" || again.Params[0].Value != "Hocsman/Relayer" {
		t.Fatalf("an edit of the call a view handed out reached the view: %+v", *again)
	}

	for name, mark := range map[string]func(*adapters.Event){
		"a sensitive prompt":  func(event *adapters.Event) { event.Sensitive = true },
		"a credential prompt": func(event *adapters.Event) { event.Type = adapters.EventCredential },
		"a high-risk prompt":  func(event *adapters.Event) { event.Risk = adapters.RiskHigh },
	} {
		t.Run(name, func(t *testing.T) {
			sup, _ := newCoreForTest(t, newFakeEngine(), "agent-a")
			event := toolCallPrompt("agent-a", "prompt-secret")
			mark(&event)
			sup.Handle(session.AdapterEvent{Event: event})
			pending := sup.State().Pending
			if len(pending) != 1 {
				t.Fatalf("pending = %v, want the prompt", pendingIDs(sup))
			}
			if call := pending[0].ToolCall(); call != nil {
				t.Fatalf("the view of a prompt whose text must not be shown carries its tool call: %+v", *call)
			}
		})
	}
}

// A prompt that asks about no tool call shows none.
func TestAPromptWithoutAToolCallShowsNone(t *testing.T) {
	sup, _ := newCoreForTest(t, newFakeEngine(), "agent-a")
	sup.Handle(session.AdapterEvent{Event: promptEvent("agent-a", "prompt-1")})
	pending := sup.State().Pending
	if len(pending) != 1 {
		t.Fatalf("pending = %v, want the prompt", pendingIDs(sup))
	}
	if call := pending[0].ToolCall(); call != nil {
		t.Fatalf("a prompt about no tool call shows one: %+v", *call)
	}
	var zero supervise.View
	if zero.ToolCall() != nil {
		t.Fatal("the zero view shows a tool call")
	}
}
