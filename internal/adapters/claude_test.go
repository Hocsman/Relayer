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
				event.Metadata["observed_cli_version"] != "2.1.59" {
				t.Fatalf("event provenance = %#v", event.Metadata)
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
	resumedEvent, changed, err := resumed.ReconcileSnapshot(resized)
	if err != nil || !changed || resumedEvent == nil || resumedEvent.ID != first.ID ||
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
	if len(fixtures) != 2 {
		t.Fatalf("Claude fixture count = %d, want 2", len(fixtures))
	}
	for _, fixture := range fixtures {
		if fixture.Source != "anonymized PTY observation from Claude Code 2.1.59" ||
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
