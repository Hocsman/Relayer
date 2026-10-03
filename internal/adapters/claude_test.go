package adapters

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

type claudeStreamFixture struct {
	Name      string    `json:"name"`
	Source    string    `json:"source"`
	FixtureID string    `json:"fixture_id"`
	Chunks    []string  `json:"chunks"`
	EventType EventType `json:"event_type"`
	Sensitive bool      `json:"sensitive"`
	Risk      RiskLevel `json:"risk"`
}

func TestClaudeAdapterObservedFixtures(t *testing.T) {
	fixtures := loadClaudeStreamFixtures(t)
	for fixtureIndex, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			adapter := newClaudeAdapterForTest(t, nil)
			state := NewDetectionState(fmt.Sprintf("session-%d", fixtureIndex), "agent-claude", ClaudeID)
			var events []Event
			processor, err := NewProcessor(adapter, state, 4096, Hooks{
				OnEvent: func(event Event) { events = append(events, event) },
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, chunk := range fixture.Chunks {
				if err := processor.Consume([]byte(chunk)); err != nil {
					t.Fatalf("Consume fixture chunk: %v", err)
				}
			}

			if len(events) != 1 {
				t.Fatalf("events = %#v, want one", events)
			}
			event := events[0]
			if event.Adapter != ClaudeID || event.Type != fixture.EventType ||
				event.Sensitive != fixture.Sensitive || event.Risk != fixture.Risk {
				t.Fatalf("event semantics = %#v", event)
			}
			if event.Metadata["fixture"] != fixture.FixtureID ||
				(event.Metadata["observed_cli_version"] != "2.1.59" &&
					event.Metadata["observed_cli_version"] != "2.1.285") {
				t.Fatalf("event provenance = %#v", event.Metadata)
			}
			if fixture.FixtureID == "claude-2.1.285-bash-command" {
				if event.Command != "rm -f some_file.txt" {
					t.Fatalf("expected command 'rm -f some_file.txt', got %q", event.Command)
				}
			}
			if fixture.FixtureID == "claude-2.1.285-write-file" || fixture.FixtureID == "claude-2.1.285-edit-file" {
				if event.Metadata["target_file"] != "test_claude_probe.txt" {
					t.Fatalf("expected target_file 'test_claude_probe.txt', got %q", event.Metadata["target_file"])
				}
			}
			if event.ID == "" || event.Signature == "" || event.Sequence != 1 || event.Timestamp.IsZero() {
				t.Fatalf("event identity = %#v", event)
			}
			if pending := processor.Pending(); pending == nil || pending.ID != event.ID || pending.Adapter != ClaudeID {
				t.Fatalf("pending event = %#v", pending)
			}
			if fixture.Sensitive {
				for _, forbidden := range []string{"<REDACTED>", "ANTHROPIC_API_KEY", "sk-ant-"} {
					if strings.Contains(event.Match, forbidden) {
						t.Fatalf("sensitive match contains %q: %q", forbidden, event.Match)
					}
				}
			}
		})
	}
}

func TestClaudeAdapterFragmentedANSIAndCRRewrite(t *testing.T) {
	fixture := loadClaudeStreamFixtures(t)[0]
	raw := strings.Join(fixture.Chunks, "")
	marker := "\x1b[38;5;220m"
	markerIndex := strings.Index(raw, marker)
	if markerIndex < 0 {
		t.Fatalf("fixture has no expected ANSI marker")
	}
	split := markerIndex + len("\x1b[38;5;")
	chunks := []string{
		"quoted old output that must be erased\r",
		raw[:split],
		raw[split:],
	}

	adapter := newClaudeAdapterForTest(t, nil)
	state := NewDetectionState("session-ansi", "agent-claude", ClaudeID)
	var events []Event
	processor, err := NewProcessor(adapter, state, 4096, Hooks{
		OnEvent: func(event Event) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range chunks {
		if err := processor.Consume([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if len(events) != 1 || events[0].Type != EventPermission {
		t.Fatalf("fragmented ANSI events = %#v", events)
	}
	if strings.Contains(processor.Output(), "quoted old output") == false {
		// Rendering is independent from CR detection normalization: the old
		// rendered line remains visible in bounded viewport history.
		t.Fatalf("rendered output unexpectedly rewrote terminal history: %q", processor.Output())
	}
}

func TestClaudeAdapterSuccessivePromptsAndRearming(t *testing.T) {
	fixtures := loadClaudeStreamFixtures(t)
	adapter := newClaudeAdapterForTest(t, nil)
	state := NewDetectionState("session-successive", "agent-claude", ClaudeID)
	var events []Event
	processor, err := NewProcessor(adapter, state, 8192, Hooks{
		OnEvent: func(event Event) { events = append(events, event) },
	})
	if err != nil {
		t.Fatal(err)
	}
	consumeClaudeFixture(t, processor, fixtures[0])
	if len(events) != 1 {
		t.Fatalf("first prompt events = %#v", events)
	}
	// A pending occurrence suppresses both replay and a different prompt.
	consumeClaudeFixture(t, processor, fixtures[0])
	consumeClaudeFixture(t, processor, fixtures[1])
	if len(events) != 1 {
		t.Fatalf("pending prompt emitted duplicates: %#v", events)
	}
	if err := processor.Acknowledge(events[0].ID); err != nil {
		t.Fatal(err)
	}
	consumeClaudeFixture(t, processor, fixtures[1])
	if len(events) != 2 || events[1].Type != EventCredential || events[1].Sequence != 2 ||
		events[1].ID == events[0].ID || events[1].Signature == events[0].Signature {
		t.Fatalf("successive events = %#v", events)
	}
	if err := processor.Acknowledge(events[1].ID); err != nil {
		t.Fatal(err)
	}
	consumeClaudeFixture(t, processor, fixtures[1])
	if len(events) != 3 || events[2].Sequence != 3 ||
		events[2].ID == events[1].ID || events[2].Signature != events[1].Signature {
		t.Fatalf("rearmed events = %#v", events)
	}
}

func TestClaudeAdapterIgnoresQuotedAndHistoricalPrompts(t *testing.T) {
	workspacePrompt := strings.Join([]string{
		"Quick safety check: Is this a project you created or one you trust?",
		"1. Yes, I trust this folder",
		"2. No, exit",
		"Enter to confirm Esc to cancel",
	}, " ")
	for _, test := range []struct {
		name  string
		input string
	}{
		{name: "log line", input: "Log: " + workspacePrompt},
		{name: "quoted output", input: `"` + workspacePrompt + `"`},
		{name: "markdown quote", input: "> " + workspacePrompt},
		{name: "completed history", input: workspacePrompt + "\nordinary active output"},
		{name: "code fence", input: "```text\n" + workspacePrompt},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := newClaudeAdapterForTest(t, nil)
			state := NewDetectionState("session-ignore", "agent-claude", ClaudeID)
			events, err := adapter.Detect(state, []byte(test.input))
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 || state.IsBlocked() {
				t.Fatalf("ignored context emitted %#v", events)
			}
		})
	}
}

func TestClaudeAdapterSnapshotDeduplicationAndDisappearance(t *testing.T) {
	fixture := loadClaudeStreamFixtures(t)[0]
	raw := []byte(strings.Join(fixture.Chunks, ""))
	adapter := newClaudeAdapterForTest(t, nil)
	state := NewDetectionState("session-snapshot", "agent-claude", ClaudeID)
	processor, err := NewProcessor(adapter, state, 4096, Hooks{})
	if err != nil {
		t.Fatal(err)
	}

	first, changed, err := processor.ReconcileSnapshot(raw)
	if err != nil || !changed || first == nil {
		t.Fatalf("first snapshot = %#v, %t, %v", first, changed, err)
	}
	second, changed, err := processor.ReconcileSnapshot(raw)
	if err != nil || changed || second == nil || second.ID != first.ID {
		t.Fatalf("identical snapshot = %#v, %t, %v", second, changed, err)
	}
	resized := []byte(strings.ReplaceAll(string(raw), "\x1b[1C", "\x1b[2C"))
	third, changed, err := processor.ReconcileSnapshot(resized)
	if err != nil || changed || third == nil || third.ID != first.ID {
		t.Fatalf("resized snapshot = %#v, %t, %v", third, changed, err)
	}
	resumedState := NewDetectionState("session-snapshot", "agent-claude", ClaudeID)
	resumed, err := NewProcessor(newClaudeAdapterForTest(t, nil), resumedState, 4096, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	// Another detection state is another process: the same screen is the same
	// question, but not the same occurrence, and a decision on the first must
	// not be accepted for this one.
	resumedEvent, changed, err := resumed.ReconcileSnapshot(resized)
	if err != nil || !changed || resumedEvent == nil || resumedEvent.ID == first.ID ||
		resumedEvent.Signature != first.Signature {
		t.Fatalf("resumed snapshot identity = %#v, %t, %v", resumedEvent, changed, err)
	}
	if err := processor.Acknowledge(first.ID); err != nil {
		t.Fatal(err)
	}
	if stale, changed, err := processor.ReconcileSnapshot(raw); err != nil || changed || stale != nil {
		t.Fatalf("acknowledged snapshot resurrected = %#v, %t, %v", stale, changed, err)
	}
	if pending, changed, err := processor.ReconcileSnapshot([]byte("ordinary active output")); err != nil || changed || pending != nil {
		t.Fatalf("prompt disappearance = %#v, %t, %v", pending, changed, err)
	}
	rearmed, changed, err := processor.ReconcileSnapshot(raw)
	if err != nil || !changed || rearmed == nil || rearmed.ID == first.ID || rearmed.Sequence != 2 {
		t.Fatalf("new snapshot occurrence = %#v, %t, %v", rearmed, changed, err)
	}
}

func TestClaudeSnapshotDistinguishesSuccessivePromptsWithSameFooter(t *testing.T) {
	fixtures := loadClaudeStreamFixtures(t)
	processor, err := NewProcessor(
		newClaudeAdapterForTest(t, nil),
		NewDetectionState("session-snapshot", "agent-claude", ClaudeID),
		4096,
		Hooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	first, changed, err := processor.ReconcileSnapshot([]byte(strings.Join(fixtures[0].Chunks, "")))
	if err != nil || first == nil || !changed {
		t.Fatalf("first snapshot = event %#v changed %t error %v", first, changed, err)
	}
	if err := processor.Acknowledge(first.ID); err != nil {
		t.Fatal(err)
	}
	second, changed, err := processor.ReconcileSnapshot([]byte(strings.Join(fixtures[1].Chunks, "")))
	if err != nil || second == nil || !changed || second.Type != EventCredential ||
		second.ID == first.ID || second.Sequence != 2 {
		t.Fatalf("successive same-footer snapshot = first %#v second %#v changed %t error %v", first, second, changed, err)
	}
}

func TestClaudeSnapshotReconcilesLiveSpacingAndDirectAttachResponse(t *testing.T) {
	fixture := loadClaudeStreamFixtures(t)[0]
	var live []Event
	processor, err := NewProcessor(
		newClaudeAdapterForTest(t, nil),
		NewDetectionState("session-live-snapshot", "agent-claude", ClaudeID),
		4096,
		Hooks{OnEvent: func(event Event) { live = append(live, event) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range fixture.Chunks {
		if err := processor.Consume([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if len(live) != 1 {
		t.Fatalf("live events = %#v", live)
	}
	raw := strings.Join(fixture.Chunks, "")
	tmuxScreen := strings.ReplaceAll(raw, "\x1b[1C", " ")
	repeated, changed, err := processor.ReconcileSnapshot([]byte(tmuxScreen))
	if err != nil || repeated == nil || changed || repeated.ID != live[0].ID {
		t.Fatalf("live-to-tmux snapshot = event %#v changed %t error %v", repeated, changed, err)
	}

	answeredScreen := tmuxScreen + "\nordinary active output"
	cleared, changed, err := processor.ReconcileSnapshot([]byte(answeredScreen))
	if err != nil || cleared != nil || !changed || processor.Pending() != nil {
		t.Fatalf("direct tmux response = event %#v changed %t pending %#v error %v", cleared, changed, processor.Pending(), err)
	}

	rearmed, changed, err := processor.ReconcileSnapshot([]byte(tmuxScreen))
	if err != nil || rearmed == nil || !changed || rearmed.ID == live[0].ID || rearmed.Sequence != 2 {
		t.Fatalf("rearmed trust occurrence = first %#v second %#v changed %t error %v", live[0], rearmed, changed, err)
	}

	secondScreen := strings.ReplaceAll(tmuxScreen, "<WORKSPACE>", "<SECOND-WORKSPACE>")
	second, changed, err := processor.ReconcileSnapshot([]byte(secondScreen))
	if err != nil || second == nil || !changed || second.ID == rearmed.ID || second.Sequence != 3 ||
		second.Signature != live[0].Signature {
		t.Fatalf("post-attach trust occurrence = rearmed %#v second %#v changed %t error %v", rearmed, second, changed, err)
	}
}

func TestClaudeAdapterGenericInterceptPatternFallback(t *testing.T) {
	patterns := []Pattern{{
		Name:        "configured-confirmation",
		Description: "configured legacy prompt",
		Expression:  `(?i)continue custom operation\?\s*\[y/n\]`,
	}}
	adapter := newClaudeAdapterForTest(t, patterns)
	patterns[0] = Pattern{Name: "mutated", Expression: "("}
	state := NewDetectionState("session-fallback", "agent-claude", ClaudeID)
	events, err := adapter.Detect(state, []byte("Continue custom operation? [Y/n]"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Adapter != ClaudeID || events[0].Type != EventConfirmation ||
		events[0].Summary != "configured legacy prompt" ||
		events[0].Metadata["pattern"] != "configured-confirmation" ||
		events[0].Metadata["fixture"] != "" {
		t.Fatalf("generic fallback event = %#v", events)
	}
	if _, err := NewClaudeAdapter([]Pattern{{Name: "broken", Expression: "("}}); err == nil {
		t.Fatal("invalid fallback regex was accepted")
	}
}

func TestClaudeConfiguredPatternTakesPriorityOverObservedRule(t *testing.T) {
	fixture := loadClaudeStreamFixtures(t)[0]
	adapter := newClaudeAdapterForTest(t, []Pattern{{
		Name:        "legacy-claude-trust",
		Description: "configured trust confirmation",
		Expression:  claudeObservedRules[0].pattern.Expression,
	}})
	var events []Event
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session-collision", "agent-claude", ClaudeID),
		4096,
		Hooks{OnEvent: func(event Event) { events = append(events, event) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range fixture.Chunks {
		if err := processor.Consume([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if len(events) != 1 || events[0].Type != EventConfirmation ||
		events[0].Summary != "configured trust confirmation" ||
		events[0].Metadata["pattern"] != "legacy-claude-trust" ||
		events[0].Metadata["fixture"] != "" {
		t.Fatalf("configured collision event = %#v", events)
	}
}

func TestClaudeConfiguredPatternCannotForgeObservedFixtureProvenance(t *testing.T) {
	vendor := claudeObservedRules[0]
	adapter := newClaudeAdapterForTest(t, []Pattern{{
		Name:        vendor.pattern.Name,
		Description: vendor.pattern.Description,
		Expression:  `legacy custom confirmation \[Y/n\]`,
	}})
	state := NewDetectionState("session-spoof", "agent-claude", ClaudeID)
	events, err := adapter.Detect(state, []byte("legacy custom confirmation [Y/n]"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventConfirmation || events[0].Risk != RiskUnknown ||
		events[0].Metadata["pattern"] != vendor.pattern.Name ||
		events[0].Metadata["fixture"] != "" || events[0].Metadata["observed_cli_version"] != "" {
		t.Fatalf("configured provenance collision = %#v", events)
	}
}

func TestClaudeAdapterEncodeDecisionIsConservative(t *testing.T) {
	adapter := newClaudeAdapterForTest(t, nil)
	actionable := Event{Adapter: ClaudeID, Type: EventPermission}
	for _, decision := range []Decision{DecisionAllow, DecisionDeny, Decision("approve")} {
		encoded, err := adapter.EncodeDecision(actionable, decision, "")
		if encoded != nil || !errors.Is(err, ErrDecisionUnsupported) {
			t.Fatalf("decision %q = %v, %v", decision, encoded, err)
		}
	}
	manual := "\x1b[B"
	encoded, err := adapter.EncodeDecision(actionable, DecisionManual, manual)
	if err != nil || string(encoded) != manual+"\r" {
		t.Fatalf("manual decision = %q, %v", encoded, err)
	}
	if encoded, err := adapter.EncodeDecision(actionable, DecisionManual, "bad\x00input"); encoded != nil || err == nil {
		t.Fatalf("manual NUL = %v, %v", encoded, err)
	}
	if encoded, err := adapter.EncodeDecision(Event{Type: EventProcessExit}, DecisionManual, "x"); encoded != nil || !errors.Is(err, ErrDecisionUnsupported) {
		t.Fatalf("terminal event decision = %v, %v", encoded, err)
	}
}

// No answer to a Claude Code Bash, create or edit prompt has been typed into a
// real Claude Code with its effect checked, so none is claimed: a policy
// decision on these prompts is a question for a person, and what a person types
// is sent exactly as typed, followed by Enter, never mapped to a menu option.
func TestClaudeAdapterAutomaticDecisionsAreUnsupportedOnVendorPrompts(t *testing.T) {
	adapter := newClaudeAdapterForTest(t, nil)

	detected := map[string]string{
		claudeBashCommandPattern: "Bash command\n" +
			"Run shell command ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
			"echo PROBE_BASH_OK > bash_probe.txt\n" +
			"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
			"Do you want to proceed?\n" +
			"❯ 1. Yes\n  2. Yes, and always allow access to folder\n  3. No\n\n" +
			"Esc to cancel · Tab to amend",
		claudeWriteFilePattern: "Create file\nnew_file.txt\n" +
			"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n  1 hello world\n" +
			"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
			"Do you want to create new_file.txt?\n❯ 1. Yes\n  2. No\n\n" +
			"Esc to cancel · Tab to amend",
		claudeEditFilePattern: "Edit file\nmain.go\n" +
			"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n- old\n+ new\n" +
			"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
			"Do you want to make this edit to main.go?\n❯ 1. Yes\n  2. No\n\n" +
			"Esc to cancel · Tab to amend",
	}
	for pattern, prompt := range detected {
		// An event the adapter itself raised, not one assembled by hand.
		state := NewDetectionState("session-"+pattern, "agent-claude", ClaudeID)
		events, err := adapter.Detect(state, []byte(prompt))
		if err != nil || len(events) != 1 || events[0].Metadata["pattern"] != pattern {
			t.Fatalf("pattern %s detected = %#v, %v", pattern, events, err)
		}
		for _, decision := range []Decision{DecisionAllow, DecisionDeny, Decision("other")} {
			encoded, err := adapter.EncodeDecision(events[0], decision, "")
			if encoded != nil || !errors.Is(err, ErrDecisionUnsupported) {
				t.Fatalf("pattern %s detected event, decision %q = %q, %v", pattern, decision, encoded, err)
			}
		}
	}

	for pattern := range detected {
		event := Event{Adapter: ClaudeID, Type: EventPermission, Metadata: map[string]string{"pattern": pattern}}
		for _, decision := range []Decision{DecisionAllow, DecisionDeny, Decision("other")} {
			encoded, err := adapter.EncodeDecision(event, decision, "")
			if encoded != nil || !errors.Is(err, ErrDecisionUnsupported) {
				t.Fatalf("pattern %s decision %q = %q, %v", pattern, decision, encoded, err)
			}
		}
		for _, input := range []string{"y", "yes", "1", "n", "no", "3", "4", "esc", "cancel", "2", "\x1b"} {
			encoded, err := adapter.EncodeDecision(event, DecisionManual, input)
			if err != nil || string(encoded) != input+"\r" {
				t.Fatalf("pattern %s manual %q = %q, %v, want it unchanged plus Enter", pattern, input, encoded, err)
			}
		}
		if encoded, err := adapter.EncodeDecision(event, DecisionManual, "bad\x00input"); encoded != nil || err == nil {
			t.Fatalf("pattern %s manual NUL = %q, %v", pattern, encoded, err)
		}
		nonActionable := event.Clone()
		nonActionable.Type = EventProcessExit
		if encoded, err := adapter.EncodeDecision(nonActionable, DecisionManual, "x"); encoded != nil || !errors.Is(err, ErrDecisionUnsupported) {
			t.Fatalf("pattern %s non-actionable = %q, %v", pattern, encoded, err)
		}
	}
}

func TestClaudeAdapterEmptyInvalidAndBoundedInput(t *testing.T) {
	adapter := newClaudeAdapterForTest(t, nil)
	if events, err := adapter.Detect(nil, []byte("prompt")); err == nil || len(events) != 0 {
		t.Fatalf("nil state = %#v, %v", events, err)
	}
	state := NewDetectionState("session-bounded", "agent-claude", ClaudeID)
	processor, err := NewProcessor(adapter, state, 1024, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume(nil); err != nil {
		t.Fatal(err)
	}
	if pending := processor.Pending(); pending != nil {
		t.Fatalf("empty input produced %#v", pending)
	}
	if err := processor.Consume([]byte(strings.Repeat("x", detectionWindowSize*4) + "\nordinary output")); err != nil {
		t.Fatal(err)
	}
	if length := processor.DetectionWindowLen(); length > detectionWindowSize {
		t.Fatalf("detection window = %d, limit %d", length, detectionWindowSize)
	}
	consumeClaudeFixture(t, processor, loadClaudeStreamFixtures(t)[0])
	if pending := processor.Pending(); pending == nil || pending.Type != EventPermission {
		t.Fatalf("bounded detector did not recover: %#v", pending)
	}

	var nilAdapter *ClaudeAdapter
	if events, err := nilAdapter.Detect(state, []byte("x")); err == nil || len(events) != 0 {
		t.Fatalf("nil adapter Detect = %#v, %v", events, err)
	}
	if encoded, err := nilAdapter.EncodeDecision(Event{Type: EventPermission}, DecisionManual, "x"); encoded != nil || err == nil {
		t.Fatalf("nil adapter EncodeDecision = %v, %v", encoded, err)
	}
}

func TestClaudeFixtureFileContainsNoIdentityOrSecretValue(t *testing.T) {
	payload, err := os.ReadFile(filepath.Join("testdata", "claude", "stream_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	for name, expression := range map[string]string{
		"personal path":  `/Users/|/home/`,
		"email":          `[A-Za-z0-9._%+-]+@[A-Za-z0-9.-]+\.[A-Za-z]{2,}`,
		"JWT":            `eyJ[A-Za-z0-9_-]{4,}\.[A-Za-z0-9_-]{4,}`,
		"API key value":  `sk-(?:ant-)?[A-Za-z0-9_-]{4,}`,
		"URL credential": `[A-Za-z][A-Za-z0-9+.-]*://[^\s/@:]+:[^\s/@]+@`,
	} {
		matched, compileErr := regexp.Match(expression, payload)
		if compileErr != nil {
			t.Fatal(compileErr)
		}
		if matched {
			t.Fatalf("fixture contains forbidden %s", name)
		}
	}
	if strings.Contains(string(payload), "<REDACTED>") == false {
		t.Fatal("sensitive fixture does not carry an explicit generic redaction marker")
	}
}

func loadClaudeStreamFixtures(t *testing.T) []claudeStreamFixture {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", "claude", "stream_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []claudeStreamFixture
	if err := json.Unmarshal(payload, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 5 {
		t.Fatalf("Claude fixture count = %d, want 5", len(fixtures))
	}
	for _, fixture := range fixtures {
		if (fixture.Source != "anonymized PTY observation from Claude Code 2.1.59" &&
			fixture.Source != "anonymized PTY observation from Claude Code 2.1.285") ||
			fixture.FixtureID == "" || len(fixture.Chunks) == 0 {
			t.Fatalf("fixture provenance is incomplete: %#v", fixture)
		}
	}
	return fixtures
}

func newClaudeAdapterForTest(t *testing.T, patterns []Pattern) *ClaudeAdapter {
	t.Helper()
	adapter, err := NewClaudeAdapter(patterns)
	if err != nil {
		t.Fatal(err)
	}
	if adapter.ID() != ClaudeID {
		t.Fatalf("adapter ID = %q", adapter.ID())
	}
	return adapter
}

func consumeClaudeFixture(t *testing.T, processor *Processor, fixture claudeStreamFixture) {
	t.Helper()
	for _, chunk := range fixture.Chunks {
		if err := processor.Consume([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
}

func TestClaudeAdapterReal21286Prompts(t *testing.T) {
	adapter := newClaudeAdapterForTest(t, nil)

	t.Run("workspace trust with unnumbered Ink menu", func(t *testing.T) {
		input := "Accessing workspace:\nC:\\Temp\\project\n" +
			"Quick safety check: Is this a project you created or one you trust? (Like your own code).\n" +
			"Claude Code'll be able to read, edit, and execute files here.\n" +
			"❯ No, exit\n  Yes, I trust this folder\n" +
			"Enter to confirm · Esc to cancel"
		state := NewDetectionState("s1", "a1", ClaudeID)
		events, err := adapter.Detect(state, []byte(input))
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if len(events) != 1 || events[0].Type != EventPermission {
			t.Fatalf("expected 1 permission event, got: %#v", events)
		}
	})

	t.Run("environment API key unnumbered Ink menu", func(t *testing.T) {
		input := "Detected a custom API key in your environment\n" +
			"ANTHROPIC_API_KEY: sk-ant-...\n" +
			"Do you want to use this API key?\n" +
			"  Yes\n❯ No (recommended)\n" +
			"Enter to confirm · Esc to cancel"
		state := NewDetectionState("s2", "a2", ClaudeID)
		events, err := adapter.Detect(state, []byte(input))
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if len(events) != 1 || events[0].Type != EventCredential {
			t.Fatalf("expected 1 credential event, got: %#v", events)
		}
	})

	t.Run("bash command 2.1.286 layout with 3 options", func(t *testing.T) {
		input := "Bash command\n" +
			"Run shell command ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
			"echo PROBE_BASH_OK > bash_probe.txt\n" +
			"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
			"│ Claude requested permissions to write to bash_probe.txt\n\n" +
			"Do you want to proceed?\n" +
			"❯ 1. Yes\n" +
			"  2. Yes, and always allow access to folder\n" +
			"  3. No\n\n" +
			"Esc to cancel · Tab to amend"
		state := NewDetectionState("s3", "a3", ClaudeID)
		events, err := adapter.Detect(state, []byte(input))
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if len(events) != 1 || events[0].Type != EventPermission {
			t.Fatalf("expected 1 permission event, got: %#v", events)
		}
		if events[0].Command != "echo PROBE_BASH_OK > bash_probe.txt" {
			t.Fatalf("expected extracted command 'echo PROBE_BASH_OK > bash_probe.txt', got %q", events[0].Command)
		}
	})

	t.Run("write file with 2 options", func(t *testing.T) {
		input := "Create file\nnew_file.txt\n" +
			"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
			"  1 hello world\n" +
			"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
			"Do you want to create new_file.txt?\n" +
			"❯ 1. Yes\n" +
			"  2. No\n\n" +
			"Esc to cancel · Tab to amend"
		state := NewDetectionState("s4", "a4", ClaudeID)
		events, err := adapter.Detect(state, []byte(input))
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if len(events) != 1 || events[0].Type != EventConfirmation {
			t.Fatalf("expected 1 confirmation event, got: %#v", events)
		}
		if events[0].Metadata["target_file"] != "new_file.txt" {
			t.Fatalf("expected target_file 'new_file.txt', got %q", events[0].Metadata["target_file"])
		}
	})

	t.Run("bash command real ConPTY artifacts with missing dot and trailing ink footer", func(t *testing.T) {
		input := "Bash command\nTip: auto mode handles these prompts for you — choose \"switch to auto mode\" below\n" +
			"Run shell command\n" +
			"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
			"echo BASH_ONCE_OK>test_bash_once.txt\n" +
			"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
			"Do you want to proeed?\n" +
			"❯1Yes\n" +
			"2. Yes,andalwaysallowaccesstoC:\\Temp fromthisproject\n" +
			"3. Yes, and switch to automode · auto mode handles these prompts for you\n" +
			"4. No\n" +
			"Esc to cancel · Tab to amend ● ● ● ● ● ●  ❯ 1. Yes ❯ 1. Yes ❯ 1. Yes"
		state := NewDetectionState("s5", "a5", ClaudeID)
		events, err := adapter.Detect(state, []byte(input))
		if err != nil {
			t.Fatalf("Detect: %v", err)
		}
		if len(events) != 1 || events[0].Type != EventPermission {
			t.Fatalf("expected 1 permission event, got: %#v", events)
		}
		if events[0].Command != "echo BASH_ONCE_OK>test_bash_once.txt" {
			t.Fatalf("expected command 'echo BASH_ONCE_OK>test_bash_once.txt', got %q", events[0].Command)
		}
	})
}

func TestClaudeSuccessiveApiKeyThenBashProcessor(t *testing.T) {
	adapter := newClaudeAdapterForTest(t, nil)
	state := NewDetectionState("session-seq", "agent-claude", ClaudeID)
	var events []Event
	processor, err := NewProcessor(adapter, state, 16384, Hooks{
		OnEvent: func(e Event) { events = append(events, e) },
	})
	if err != nil {
		t.Fatal(err)
	}

	chunk1 := "Detected a custom API key in your environment\n" +
		"ANTHROPIC_API_KEY: sk-ant-...\n" +
		"Do you want to use this API key?\n" +
		"  Yes\n❯ No (recommended)\n" +
		"Enter to confirm · Esc to cancel"
	if err := processor.Consume([]byte(chunk1)); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventCredential {
		t.Fatalf("chunk 1 expected 1 credential event, got: %#v", events)
	}

	// Resolve the credential prompt
	pending := processor.Pending()
	if pending == nil {
		t.Fatal("expected pending credential event")
	}
	if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	chunk2 := "\n\nBash command\nTip: auto mode handles these prompts for you — choose \"switch to auto mode\" below\n" +
		"Run shell command\n" +
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
		"echo BASH_ONCE_OK>test_bash_once.txt\n" +
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
		"Do you want to proeed?\n" +
		"❯1Yes\n" +
		"2. Yes,andalwaysallowaccesstoC:\\Temp fromthisproject\n" +
		"3. Yes, and switch to automode · auto mode handles these prompts for you\n" +
		"4. No\n" +
		"Esc to cancel · Tab to amend ● ● ● ● ● ●  ❯ 1. Yes ❯ 1. Yes ❯ 1. Yes"
	if err := processor.Consume([]byte(chunk2)); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("chunk 2 expected 2 total events, got: %#v", events)
	}
	if events[1].Type != EventPermission {
		t.Fatalf("expected second event to be EventPermission, got: %#v", events[1])
	}
}

func TestClaudeSuccessiveBashPrompts(t *testing.T) {
	adapter := newClaudeAdapterForTest(t, nil)
	state := NewDetectionState("session-seq2", "agent-claude", ClaudeID)
	var events []Event
	processor, err := NewProcessor(adapter, state, 16384, Hooks{
		OnEvent: func(e Event) { events = append(events, e) },
	})
	if err != nil {
		t.Fatal(err)
	}

	chunk1 := "\n\nBash command\nTip: auto mode handles these prompts for you — choose \"switch to auto mode\" below\n" +
		"Run shell command\n" +
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
		"echo BASH_ONCE_OK>test_bash_once.txt\n" +
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
		"Do you want to proeed?\n" +
		"❯1Yes\n" +
		"2. Yes,andalwaysallowaccesstoC:\\Temp fromthisproject\n" +
		"3. Yes, and switch to automode · auto mode handles these prompts for you\n" +
		"4. No\n" +
		"Esc to cancel · Tab to amend ● ● ● ● ● ●  ❯ 1. Yes"
	if err := processor.Consume([]byte(chunk1)); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event, got: %#v", events)
	}

	// Resolve the first bash command
	pending := processor.Pending()
	if pending == nil {
		t.Fatal("expected pending event")
	}
	if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	// Chunk 2: output of first bash, then second bash command
	chunk2 := "\n● Running echo BASH_ONCE_OK > test_bash_once.txt\n" +
		"⎿  $ echo BASH_ONCE_OK > test_bash_once.txt\n\n" +
		"Bash command\nTip: auto mode handles these prompts for you — choose \"switch to auto mode\" below\n" +
		"Run shell command\n" +
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
		"echo BASH_ALWAYS_OK>test_bash_always.txt\n" +
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
		"Do you want to proeed?\n" +
		"❯1Yes\n" +
		"2. Yes,andalwaysallowaccesstoC:\\Temp fromthisproject\n" +
		"3. Yes, and switch to automode · auto mode handles these prompts for you\n" +
		"4. No\n" +
		"Esc to cancel · Tab to amend ● ● ● ● ● ●  ❯ 1. Yes"
	if err := processor.Consume([]byte(chunk2)); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("expected 2 total events, got: %#v", events)
	}
}
