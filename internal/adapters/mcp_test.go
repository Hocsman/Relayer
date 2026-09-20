package adapters

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestDetectToolCallName(t *testing.T) {
	tests := []struct {
		name   string
		block  string
		server string
		tool   string
		found  bool
	}{
		{
			name:   "bare name",
			block:  "mcp__github__create_issue",
			server: "github",
			tool:   "create_issue",
			found:  true,
		},
		{
			name:   "name inside agent furniture",
			block:  "⏺ calling mcp__ccd_session__spawn_task now\n",
			server: "ccd_session",
			tool:   "spawn_task",
			found:  true,
		},
		{
			name:   "dots and dashes are part of the grammar",
			block:  "mcp__api.example-01__list.items",
			server: "api.example-01",
			tool:   "list.items",
			found:  true,
		},
		{
			name:  "no prefix",
			block: "the agent wants to run tests",
			found: false,
		},
		{
			name:  "prefix glued to an identifier",
			block: "xmcp__github__create_issue",
			found: false,
		},
		{
			name:  "missing tool separator",
			block: "mcp__github_create_issue",
			found: false,
		},
		{
			name:  "empty tool",
			block: "mcp__github__",
			found: false,
		},
		{
			name:  "empty server",
			block: "mcp____create_issue",
			found: false,
		},
		{
			name:  "server does not start with an alphanumeric",
			block: "mcp___github__create_issue",
			found: false,
		},
		{
			name:  "server longer than the grammar allows",
			block: "mcp__" + strings.Repeat("a", 65) + "__read",
			found: false,
		},
		{
			name:  "tool longer than the grammar allows",
			block: "mcp__github__" + strings.Repeat("a", 65),
			found: false,
		},
		{
			name:  "name cut by a line break loses its separator",
			block: "mcp__git\nhub__create_issue",
			found: false,
		},
		{
			name:   "server at the grammar limit",
			block:  "mcp__" + strings.Repeat("a", 64) + "__read",
			server: strings.Repeat("a", 64),
			tool:   "read",
			found:  true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			call, found := DetectToolCall(test.block)
			if found != test.found {
				t.Fatalf("DetectToolCall(%q) found = %v, want %v", test.block, found, test.found)
			}
			if !found {
				if !reflect.DeepEqual(call, ToolCall{}) {
					t.Fatalf("rejected block returned %+v, want the zero call", call)
				}
				return
			}
			if call.Server != test.server || call.Tool != test.tool {
				t.Fatalf("DetectToolCall(%q) = %q/%q, want %q/%q",
					test.block, call.Server, call.Tool, test.server, test.tool)
			}
		})
	}
}

func TestDetectToolCallTakesTheLastName(t *testing.T) {
	block := "earlier mcp__github__list_issues finished\nnow mcp__shell__run_command\n"
	call, found := DetectToolCall(block)
	if !found {
		t.Fatalf("DetectToolCall(%q) found nothing", block)
	}
	if call.Server != "shell" || call.Tool != "run_command" {
		t.Fatalf("DetectToolCall = %q/%q, want shell/run_command", call.Server, call.Tool)
	}
}

// TestDetectToolCallDecoyNameCannotDowngrade pins the failure the last-name rule
// used to have: a second name printed as an unquoted argument sits after the
// name of the call being made, so it won a plain last-name contest and badged a
// dangerous call as a read.
func TestDetectToolCallDecoyNameCannotDowngrade(t *testing.T) {
	blocks := []string{
		"mcp__fs__delete_file path=mcp__docs__get_page",
		"mcp__fs__delete_file\nnote: see mcp__docs__get_page for the safe variant\n",
		"mcp__fs__delete_file {tool: mcp__docs__get_page}",
		`mcp__fs__delete_file {"tool": "mcp__docs__get_page"}`,
		"mcp__fs__delete_file\n  fallback: mcp__ccd_session__spawn_task\n",
	}
	for _, block := range blocks {
		call, found := DetectToolCall(block)
		if !found {
			t.Fatalf("DetectToolCall(%q) found nothing", block)
		}
		if call.Tool != "delete_file" || call.Risk != RiskHigh {
			t.Fatalf("DetectToolCall(%q) = %q/%q risk %q, want fs/delete_file risk %q",
				block, call.Server, call.Tool, call.Risk, RiskHigh)
		}
	}
}

// TestDetectToolCallSecondNameOnlyRaisesRisk states the rule in the other
// direction: a block carrying several names reports the most dangerous one, so
// the reading can be wrong but never reassuring.
func TestDetectToolCallSecondNameOnlyRaisesRisk(t *testing.T) {
	block := "mcp__docs__get_page\nfallback: mcp__shell__exec\n"
	call, found := DetectToolCall(block)
	if !found {
		t.Fatalf("DetectToolCall(%q) found nothing", block)
	}
	if call.Tool != "exec" || call.Risk != RiskHigh {
		t.Fatalf("DetectToolCall(%q) = %q risk %q, want exec risk %q", block, call.Tool, call.Risk, RiskHigh)
	}
}

// TestDetectToolCallBoundsCandidates keeps the backwards scan linear. Each
// candidate costs a scan of its line, so an unbounded count is quadratic in the
// block and a single long line of names was enough to stall the parser.
func TestDetectToolCallBoundsCandidates(t *testing.T) {
	for _, block := range []string{
		"`" + strings.Repeat("mcp__aaa__bbb ", 40000) + "`",
		strings.Repeat("> mcp__aaa__bbb\n", 40000),
	} {
		done := make(chan struct{})
		go func() {
			DetectToolCall(block)
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("DetectToolCall(%.24q, %d bytes) did not return in 5s", block, len(block))
		}
	}
}

func TestDetectToolCallSkipsDocumentedNames(t *testing.T) {
	tests := []struct {
		name  string
		block string
		tool  string
		found bool
	}{
		{
			name:  "inside a code fence",
			block: "```\nmcp__github__create_issue\n```\n",
			found: false,
		},
		{
			name:  "quoted prose",
			block: "run `mcp__github__create_issue` when ready\n",
			found: false,
		},
		{
			name:  "quoted block comment",
			block: "> mcp__github__create_issue\n",
			found: false,
		},
		{
			name:  "documented name above a live one",
			block: "```\nmcp__docs__read_page\n```\nmcp__shell__run_command\n",
			tool:  "run_command",
			found: true,
		},
		{
			name:  "live name above a documented one",
			block: "mcp__shell__run_command\n```\nmcp__docs__read_page\n```\n",
			tool:  "run_command",
			found: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			call, found := DetectToolCall(test.block)
			if found != test.found {
				t.Fatalf("DetectToolCall(%q) found = %v, want %v", test.block, found, test.found)
			}
			if found && call.Tool != test.tool {
				t.Fatalf("DetectToolCall(%q) tool = %q, want %q", test.block, call.Tool, test.tool)
			}
		})
	}
}

func TestDetectToolCallJSONParams(t *testing.T) {
	tests := []struct {
		name   string
		block  string
		params []ToolCallParam
	}{
		{
			name:  "object on the same line",
			block: `mcp__github__create_issue {"title": "fix parser", "draft": true}`,
			params: []ToolCallParam{
				{Name: "title", Value: "fix parser"},
				{Name: "draft", Value: "true"},
			},
		},
		{
			name:  "object on the following line",
			block: "mcp__github__create_issue\n{\"title\": \"fix parser\"}\n",
			params: []ToolCallParam{
				{Name: "title", Value: "fix parser"},
			},
		},
		{
			name:  "nested object keeps its source text",
			block: `mcp__github__create_issue {"labels": {"kind": "bug"}, "count": 2}`,
			params: []ToolCallParam{
				{Name: "labels", Value: `{"kind": "bug"}`},
				{Name: "count", Value: "2"},
			},
		},
		{
			name:  "brace inside a string does not close the object",
			block: `mcp__shell__run_command {"cmd": "echo ${HOME} }", "cwd": "/tmp"}`,
			params: []ToolCallParam{
				{Name: "cmd", Value: "echo ${HOME} }"},
				{Name: "cwd", Value: "/tmp"},
			},
		},
		{
			name:   "object too far below the name is not its arguments",
			block:  "mcp__github__create_issue\n\n\n\n{\"title\": \"fix parser\"}\n",
			params: nil,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			call, found := DetectToolCall(test.block)
			if !found {
				t.Fatalf("DetectToolCall(%q) found nothing", test.block)
			}
			if call.ParamsTruncated {
				t.Fatalf("DetectToolCall(%q) reported truncated parameters", test.block)
			}
			if !reflect.DeepEqual(call.Params, test.params) {
				t.Fatalf("DetectToolCall(%q) params = %+v, want %+v", test.block, call.Params, test.params)
			}
		})
	}
}

func TestDetectToolCallLineParams(t *testing.T) {
	tests := []struct {
		name   string
		block  string
		params []ToolCallParam
	}{
		{
			name:  "colon lines below the name",
			block: "mcp__shell__run_command\n  command: ls -la\n  cwd: /tmp\n",
			params: []ToolCallParam{
				{Name: "command", Value: "ls -la"},
				{Name: "cwd", Value: "/tmp"},
			},
		},
		{
			name:  "equals pair on the name line",
			block: "mcp__fs__read_file: path=/tmp/a.txt\n",
			params: []ToolCallParam{
				{Name: "path", Value: "/tmp/a.txt"},
			},
		},
		{
			name:  "frame borders are skipped rather than ending the block",
			block: "│ mcp__shell__run_command\n│ ──────────────\n│ command: ls\n",
			params: []ToolCallParam{
				{Name: "command", Value: "ls"},
			},
		},
		{
			name:  "prose ends the parameter block",
			block: "mcp__shell__run_command\ncommand: ls\nthe agent will now wait\nextra: dropped\n",
			params: []ToolCallParam{
				{Name: "command", Value: "ls"},
			},
		},
		{
			name:   "unparsable json falls back to nothing when no pair follows",
			block:  "mcp__github__create_issue {\"title\": \"fix\n",
			params: nil,
		},
		{
			name:  "value keeps a colon after the first separator",
			block: "mcp__http__fetch_url\nurl=https://example.test/a\n",
			params: []ToolCallParam{
				{Name: "url", Value: "https://example.test/a"},
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			call, found := DetectToolCall(test.block)
			if !found {
				t.Fatalf("DetectToolCall(%q) found nothing", test.block)
			}
			if !reflect.DeepEqual(call.Params, test.params) {
				t.Fatalf("DetectToolCall(%q) params = %+v, want %+v", test.block, call.Params, test.params)
			}
		})
	}
}

func TestDetectToolCallSanitisesValues(t *testing.T) {
	block := "mcp__shell__run_command\ncommand: \x1b[31mls\x1b[0m   -la\t\x07/tmp\n"
	call, found := DetectToolCall(block)
	if !found {
		t.Fatalf("DetectToolCall(%q) found nothing", block)
	}
	want := []ToolCallParam{{Name: "command", Value: "ls -la /tmp"}}
	if !reflect.DeepEqual(call.Params, want) {
		t.Fatalf("DetectToolCall params = %+v, want %+v", call.Params, want)
	}
}

func TestDetectToolCallCapsValueLength(t *testing.T) {
	long := strings.Repeat("a", maxToolCallValueRunes+64)
	block := "mcp__fs__write_file {\"body\": \"" + long + "\"}"
	call, found := DetectToolCall(block)
	if !found {
		t.Fatalf("DetectToolCall(%q) found nothing", block)
	}
	if len(call.Params) != 1 {
		t.Fatalf("DetectToolCall params = %+v, want exactly one", call.Params)
	}
	param := call.Params[0]
	if !param.Truncated {
		t.Fatalf("param %+v was not reported as truncated", param)
	}
	if count := len([]rune(param.Value)); count != maxToolCallValueRunes {
		t.Fatalf("param value has %d runes, want %d", count, maxToolCallValueRunes)
	}
}

func TestDetectToolCallCapsParamCount(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("mcp__shell__run_command {")
	for index := 0; index < maxToolCallParams+3; index++ {
		if index > 0 {
			builder.WriteString(", ")
		}
		builder.WriteString(`"k`)
		builder.WriteString(string(rune('a' + index)))
		builder.WriteString(`": "v"`)
	}
	builder.WriteString("}")
	block := builder.String()

	call, found := DetectToolCall(block)
	if !found {
		t.Fatalf("DetectToolCall(%q) found nothing", block)
	}
	if !call.ParamsTruncated {
		t.Fatalf("DetectToolCall(%q) did not report truncated parameters", block)
	}
	if len(call.Params) != maxToolCallParams {
		t.Fatalf("DetectToolCall returned %d params, want %d", len(call.Params), maxToolCallParams)
	}
	// Parameters keep document order, so the badge is the same on every run.
	for index, param := range call.Params {
		want := "k" + string(rune('a'+index))
		if param.Name != want {
			t.Fatalf("param %d = %q, want %q", index, param.Name, want)
		}
	}
}

func TestDetectToolCallCapsLineParamCount(t *testing.T) {
	var builder strings.Builder
	builder.WriteString("mcp__shell__run_command\n")
	for index := 0; index < maxToolCallParams+3; index++ {
		builder.WriteString("k")
		builder.WriteString(string(rune('a' + index)))
		builder.WriteString(": v\n")
	}
	call, found := DetectToolCall(builder.String())
	if !found {
		t.Fatalf("DetectToolCall found nothing")
	}
	if !call.ParamsTruncated || len(call.Params) != maxToolCallParams {
		t.Fatalf("DetectToolCall returned %d params (truncated=%v), want %d truncated",
			len(call.Params), call.ParamsTruncated, maxToolCallParams)
	}
}

func TestDetectToolCallIsDeterministic(t *testing.T) {
	block := `mcp__github__create_issue {"title": "a", "body": "b", "draft": false}`
	first, found := DetectToolCall(block)
	if !found {
		t.Fatalf("DetectToolCall(%q) found nothing", block)
	}
	for attempt := 0; attempt < 16; attempt++ {
		again, ok := DetectToolCall(block)
		if !ok || !reflect.DeepEqual(again, first) {
			t.Fatalf("DetectToolCall returned %+v (found=%v) on attempt %d, want %+v",
				again, ok, attempt, first)
		}
	}
}

func TestToolCallRisk(t *testing.T) {
	tests := []struct {
		name   string
		server string
		tool   string
		risk   RiskLevel
	}{
		{name: "delete is high", server: "github", tool: "delete_branch", risk: RiskHigh},
		{name: "write is high", server: "fs", tool: "write_file", risk: RiskHigh},
		{name: "exec is high", server: "shell", tool: "exec", risk: RiskHigh},
		{name: "send is high", server: "gmail", tool: "send_message", risk: RiskHigh},
		{name: "secret is high", server: "vault", tool: "read_secret", risk: RiskHigh},
		{name: "get is low", server: "github", tool: "get_issue", risk: RiskLow},
		{name: "list is low", server: "github", tool: "list_issues", risk: RiskLow},
		{name: "read is low", server: "fs", tool: "read_file", risk: RiskLow},
		{name: "describe is low", server: "db", tool: "describe_table", risk: RiskLow},
		{name: "neither list matches", server: "ccd_session", tool: "spawn_task", risk: RiskUnknown},
		{name: "empty tool", server: "github", tool: "", risk: RiskUnknown},
		{name: "case is ignored", server: "GitHub", tool: "DELETE_BRANCH", risk: RiskHigh},
		{name: "high server outranks a read tool", server: "payment_api", tool: "get_status", risk: RiskHigh},
		{name: "read tool on a neutral server stays low", server: "docs", tool: "get_status", risk: RiskLow},
		// A safe word buried inside a longer word is not a safe tool. Reading
		// "spreadsheet" as "read" or "forget_session" as "get" turned a tool
		// nothing is known about into a low-risk one, which is the direction
		// this file is not allowed to err in.
		{name: "read inside a longer word stays unknown", server: "office", tool: "spreadsheet", risk: RiskUnknown},
		{name: "get inside a longer word stays unknown", server: "session", tool: "forget_session", risk: RiskUnknown},
		{name: "list inside a longer word stays unknown", server: "ui", tool: "listener", risk: RiskUnknown},
		{name: "safe word as a dotted token is still low", server: "api", tool: "items.list", risk: RiskLow},
		{name: "safe word as a dashed token is still low", server: "api", tool: "read-file", risk: RiskLow},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ToolCallRisk(test.server, test.tool); got != test.risk {
				t.Fatalf("ToolCallRisk(%q, %q) = %q, want %q", test.server, test.tool, got, test.risk)
			}
		})
	}
}

// TestDetectToolCallRiskIgnoresArguments pins the rule that matters for safety:
// arguments are agent-controlled, so they must not be able to talk a dangerous
// tool down to a low risk.
func TestDetectToolCallRiskIgnoresArguments(t *testing.T) {
	blocks := []string{
		`mcp__fs__delete_file {"mode": "read", "note": "safe", "risk": "low"}`,
		"mcp__fs__delete_file\nmode: read only, totally safe\n",
		`mcp__fs__delete_file {"tool": "mcp__docs__get_page"}`,
	}
	for _, block := range blocks {
		call, found := DetectToolCall(block)
		if !found {
			t.Fatalf("DetectToolCall(%q) found nothing", block)
		}
		if call.Tool != "delete_file" {
			t.Fatalf("DetectToolCall(%q) tool = %q, want delete_file", block, call.Tool)
		}
		if call.Risk != RiskHigh {
			t.Fatalf("DetectToolCall(%q) risk = %q, want %q", block, call.Risk, RiskHigh)
		}
	}
}

// TestDetectToolCallMalformedInput asserts the two properties that hold for any
// input at all: the parser never panics, and it never reports a call it could
// not name completely.
func TestDetectToolCallMalformedInput(t *testing.T) {
	blocks := []string{
		"",
		"\n\n\n",
		"mcp__",
		"mcp____",
		"mcp__a",
		"mcp__a__",
		"__mcp__a__b",
		"mcp" + strings.Repeat("_", 4096),
		strings.Repeat("mcp__", 4096),
		strings.Repeat("a", 64*1024),
		"mcp__a__b " + strings.Repeat("{", 4096),
		"mcp__a__b " + strings.Repeat("}", 4096),
		`mcp__a__b {"k": "` + strings.Repeat(`\`, 4096),
		`mcp__a__b {"k": "unterminated`,
		"mcp__a__b {\"k\": {\"n\": {\"d\": {\"e\": 1}}}}",
		"mcp__a__b \x00\x01\x02\x03\x1b\x1b[\x1b]",
		"mcp__a__b \xff\xfe\xfd",
		"mcp__a__b {\"\xff\": \"\xfe\"}",
		"mcp__a__b\n" + strings.Repeat("k: v\n", 4096),
		"mcp__a__b\n" + strings.Repeat(":", 4096),
		"mcp__" + strings.Repeat("a", 1024) + "__" + strings.Repeat("b", 1024),
		"\x1b[31mmcp__a__b\x1b[0m",
		"mcp__a__b {\"k\": ",
		"mcp__a__b {}",
		"mcp__a__b []",
	}
	for _, block := range blocks {
		call, found := DetectToolCall(block)
		if !found {
			if !reflect.DeepEqual(call, ToolCall{}) {
				t.Fatalf("DetectToolCall(%.32q) rejected but returned %+v", block, call)
			}
			continue
		}
		if !validToolCallName(call.Server) || !validToolCallName(call.Tool) {
			t.Fatalf("DetectToolCall(%.32q) returned invalid name %q/%q", block, call.Server, call.Tool)
		}
		if len(call.Params) > maxToolCallParams {
			t.Fatalf("DetectToolCall(%.32q) returned %d params", block, len(call.Params))
		}
		for _, param := range call.Params {
			if count := len([]rune(param.Value)); count > maxToolCallValueRunes {
				t.Fatalf("DetectToolCall(%.32q) param %q has %d runes", block, param.Name, count)
			}
			if strings.ContainsAny(param.Value, "\x00\x1b\n\r\t") {
				t.Fatalf("DetectToolCall(%.32q) param %q kept control text %q", block, param.Name, param.Value)
			}
		}
	}
}

// processorToolCallEvent runs a block through a real Processor and returns the
// occurrence it raised. It exercises the wiring, not the parser: the parser has
// its own tests above, and what matters here is that a detected prompt actually
// carries the tool call to whoever renders it.
func processorToolCallEvent(t *testing.T, block string) (Event, bool) {
	t.Helper()

	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatalf("NewGenericRegexAdapter: %v", err)
	}

	var (
		received Event
		seen     bool
	)
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session-mcp", "agent-mcp", GenericID),
		4096,
		Hooks{OnEvent: func(event Event) {
			received = event
			seen = true
		}},
	)
	if err != nil {
		t.Fatalf("NewProcessor: %v", err)
	}
	if err := processor.Consume([]byte(block)); err != nil {
		t.Fatalf("Consume: %v", err)
	}
	processor.WaitSemanticEvents()
	return received, seen
}

func TestProcessorAttachesToolCallToDetectedPrompt(t *testing.T) {
	event, seen := processorToolCallEvent(t,
		"Calling mcp__github__create_issue\n"+
			"  repo: Hocsman/Relayer\n"+
			"  title: Crash on start\n"+
			"Do you want to continue? [y/n] ")
	if !seen {
		t.Fatal("no occurrence was raised")
	}
	if event.ToolCall == nil {
		t.Fatal("the occurrence carries no tool call; nothing would reach the badge")
	}
	if event.ToolCall.Server != "github" || event.ToolCall.Tool != "create_issue" {
		t.Fatalf("tool call = %+v, want github/create_issue", *event.ToolCall)
	}
	if event.ToolCall.Risk != RiskHigh {
		t.Fatalf("risk = %q, want high for a create tool", event.ToolCall.Risk)
	}
	if len(event.ToolCall.Params) == 0 {
		t.Fatal("no argument was read from the block")
	}

	// The identity reaches the journal; the argument values must not. The
	// allowlist in internal/audit is the enforcing half -- this pins the
	// emitting half, so the two cannot drift apart unnoticed.
	if event.Metadata["mcp_server"] != "github" || event.Metadata["mcp_tool"] != "create_issue" {
		t.Fatalf("metadata = %v, want the tool identity", event.Metadata)
	}
	if event.Metadata["mcp_params"] == "" {
		t.Fatalf("metadata = %v, want an argument count", event.Metadata)
	}
	for key, value := range event.Metadata {
		if strings.Contains(value, "Hocsman/Relayer") || strings.Contains(value, "Crash on start") {
			t.Fatalf("an argument value reached the audit metadata: %q = %q", key, value)
		}
	}
}

func TestProcessorLeavesOrdinaryPromptsWithoutAToolCall(t *testing.T) {
	event, seen := processorToolCallEvent(t, "Overwrite generated file? [y/n] ")
	if !seen {
		t.Fatal("no occurrence was raised")
	}
	if event.ToolCall != nil {
		t.Fatalf("an ordinary prompt gained a tool call: %+v", *event.ToolCall)
	}
	for key := range event.Metadata {
		if strings.HasPrefix(key, "mcp_") {
			t.Fatalf("an ordinary prompt gained tool-call metadata: %v", event.Metadata)
		}
	}
}

func TestEventCloneDoesNotShareToolCallArguments(t *testing.T) {
	original := Event{
		ToolCall: &ToolCall{
			Server: "fs",
			Tool:   "write_file",
			Risk:   RiskHigh,
			Params: []ToolCallParam{{Name: "path", Value: "/tmp/a"}},
		},
	}

	clone := original.Clone()
	clone.ToolCall.Params[0].Value = "/etc/shadow"
	clone.ToolCall.Tool = "read_file"

	if original.ToolCall.Params[0].Value != "/tmp/a" {
		t.Fatal("a clone's edit reached the original's arguments")
	}
	if original.ToolCall.Tool != "write_file" {
		t.Fatal("a clone's edit reached the original's tool")
	}
}
