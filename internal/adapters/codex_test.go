package adapters

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type codexCaptureFixture struct {
	CLIVersion    string    `json:"cli_version"`
	Interaction   string    `json:"interaction"`
	CaptureMode   string    `json:"capture_mode"`
	Normalized    string    `json:"normalized"`
	Chunks        []string  `json:"chunks"`
	ANSIChunks    []string  `json:"ansi_chunks"`
	AllowInputHex string    `json:"allow_input_hex"`
	DenyInputHex  string    `json:"deny_input_hex"`
	Risk          RiskLevel `json:"risk"`
}

func TestCodexCapturedPromptsInSingleAndMultipleChunks(t *testing.T) {
	for _, fixture := range loadCodexCaptureFixtures(t) {
		fixture := fixture
		t.Run(fixture.Interaction, func(t *testing.T) {
			for _, test := range []struct {
				name   string
				chunks []string
			}{
				{name: "single chunk", chunks: []string{fixture.Normalized}},
				{name: "multiple chunks", chunks: fixture.Chunks},
			} {
				t.Run(test.name, func(t *testing.T) {
					adapter := newCodexTestAdapter(t, DefaultPatterns())
					state := NewDetectionState("session-codex", "reviewer", CodexID)
					var events []Event
					for chunkIndex, chunk := range test.chunks {
						detected, err := adapter.Detect(state, []byte(chunk))
						if err != nil {
							t.Fatalf("Detect chunk %d: %v", chunkIndex, err)
						}
						if chunkIndex < len(test.chunks)-1 && len(detected) != 0 {
							t.Fatalf("partial prompt emitted events: %#v", detected)
						}
						events = append(events, detected...)
					}
					assertCodexCaptureEvent(t, events, fixture)
				})
			}
		})
	}
}

func TestCodexCapturedPromptsWithFragmentedANSI(t *testing.T) {
	for _, fixture := range loadCodexCaptureFixtures(t) {
		fixture := fixture
		t.Run(fixture.Interaction, func(t *testing.T) {
			var events []Event
			processor, err := NewProcessor(
				newCodexTestAdapter(t, DefaultPatterns()),
				NewDetectionState("session-codex", "reviewer", CodexID),
				4096,
				Hooks{OnEvent: func(event Event) { events = append(events, event) }},
			)
			if err != nil {
				t.Fatal(err)
			}
			for index, chunk := range fixture.ANSIChunks {
				if err := processor.Consume([]byte(chunk)); err != nil {
					t.Fatalf("Consume ANSI chunk %d: %v", index, err)
				}
			}
			assertCodexCaptureEvent(t, events, fixture)
			if strings.Contains(processor.Output(), "\x1b") {
				t.Fatalf("rendered output retained ANSI: %q", processor.Output())
			}
		})
	}
}

func TestCodexCarriageReturnRewriteAndHistoricalOutput(t *testing.T) {
	fixture := loadCodexCaptureFixture(t, "command_approval.json")
	adapter := newCodexTestAdapter(t, DefaultPatterns())

	state := NewDetectionState("session-codex", "reviewer", CodexID)
	events, err := adapter.Detect(state, []byte("obsolete progress 10%\robs"))
	if err != nil || len(events) != 0 {
		t.Fatalf("partial rewritten line = events %#v error %v", events, err)
	}
	events, err = adapter.Detect(state, []byte("olete progress 20%\r"+fixture.Normalized))
	if err != nil {
		t.Fatal(err)
	}
	assertCodexCaptureEvent(t, events, fixture)

	historyState := NewDetectionState("session-history", "reviewer", CodexID)
	events, err = adapter.Detect(historyState, []byte(fixture.Normalized+"\ncommand completed\nshell ready"))
	if err != nil || len(events) != 0 || historyState.Pending() != nil {
		t.Fatalf("historical prompt = events %#v pending %#v error %v", events, historyState.Pending(), err)
	}
}

func TestCodexIgnoresQuotedLogsAndCodeFences(t *testing.T) {
	fixture := loadCodexCaptureFixture(t, "command_approval.json")
	prefixLines := func(prefix, value string) string {
		return prefix + strings.ReplaceAll(value, "\n", "\n"+prefix)
	}
	inlineQuoted := strings.ReplaceAll(fixture.Normalized, fixture.Normalized[strings.LastIndex(fixture.Normalized, "Press enter"):],
		"\"Press enter to confirm or esc to cancel\"")
	for _, test := range []struct {
		name string
		text string
	}{
		{name: "quoted transcript", text: prefixLines("> ", fixture.Normalized)},
		{name: "old log", text: prefixLines("log: ", fixture.Normalized)},
		{name: "code fence", text: "```text\n" + fixture.Normalized},
		{name: "inline quoted footer", text: inlineQuoted},
	} {
		t.Run(test.name, func(t *testing.T) {
			adapter := newCodexTestAdapter(t, DefaultPatterns())
			state := NewDetectionState("session-codex", "reviewer", CodexID)
			events, err := adapter.Detect(state, []byte(test.text))
			if err != nil || len(events) != 0 || state.Pending() != nil {
				t.Fatalf("ignored context = events %#v pending %#v error %v", events, state.Pending(), err)
			}
		})
	}
}

func TestCodexDisplayedCommandCannotShadowApprovalMarker(t *testing.T) {
	fixture := loadCodexCaptureFixture(t, "command_approval.json")
	withMarkerInCommand := strings.ReplaceAll(
		fixture.Normalized,
		"printf verified-content > verified.txt",
		`printf 'Would you like to run the following command?'`,
	)
	adapter := newCodexTestAdapter(t, DefaultPatterns())
	state := NewDetectionState("session-codex", "reviewer", CodexID)
	events, err := adapter.Detect(state, []byte(withMarkerInCommand))
	if err != nil || len(events) != 1 || events[0].Metadata[codexInteractionMetadata] != codexCommandApproval {
		t.Fatalf("marker inside displayed command = events %#v error %v", events, err)
	}
}

func TestCodexEmitsTwoSuccessivePromptsWithoutDuplicates(t *testing.T) {
	command := loadCodexCaptureFixture(t, "command_approval.json")
	trust := loadCodexCaptureFixture(t, "directory_trust.json")
	var events []Event
	processor, err := NewProcessor(
		newCodexTestAdapter(t, DefaultPatterns()),
		NewDetectionState("session-codex", "reviewer", CodexID),
		4096,
		Hooks{OnEvent: func(event Event) { events = append(events, event) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte(command.Normalized)); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("first prompt events = %#v", events)
	}
	// Repaints while pending cannot create duplicate occurrences.
	if err := processor.Consume([]byte("\r" + command.Normalized)); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 {
		t.Fatalf("pending repaint emitted duplicates: %#v", events)
	}
	if err := processor.Acknowledge(events[0].ID); err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte("\ndecision delivered\n")); err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte(trust.Normalized)); err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 || events[1].Metadata[codexInteractionMetadata] != codexDirectoryTrust ||
		events[1].Sequence != 2 || events[1].ID == events[0].ID {
		t.Fatalf("successive prompt occurrences = %#v", events)
	}
}

func TestCodexSnapshotDeduplicationAndRearming(t *testing.T) {
	fixture := loadCodexCaptureFixture(t, "command_approval.json")
	processor, err := NewProcessor(
		newCodexTestAdapter(t, DefaultPatterns()),
		NewDetectionState("session-codex", "reviewer", CodexID),
		4096,
		Hooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	if event, changed, err := processor.ReconcileSnapshot([]byte("old command log\nshell ready")); err != nil || event != nil || changed {
		t.Fatalf("baseline snapshot = event %#v changed %t error %v", event, changed, err)
	}
	first, changed, err := processor.ReconcileSnapshot([]byte(fixture.Normalized))
	if err != nil || first == nil || !changed {
		t.Fatalf("first snapshot = event %#v changed %t error %v", first, changed, err)
	}
	repeated, changed, err := processor.ReconcileSnapshot([]byte("\x1b[1m" + fixture.Normalized + "\x1b[0m"))
	if err != nil || repeated == nil || repeated.ID != first.ID || changed {
		t.Fatalf("repainted snapshot = event %#v changed %t error %v", repeated, changed, err)
	}
	if err := processor.Acknowledge(first.ID); err != nil {
		t.Fatal(err)
	}
	if event, changed, err := processor.ReconcileSnapshot([]byte(fixture.Normalized)); err != nil || event != nil || changed {
		t.Fatalf("retained snapshot after ack = event %#v changed %t error %v", event, changed, err)
	}
	if event, changed, err := processor.ReconcileSnapshot([]byte("decision delivered\nshell ready")); err != nil || event != nil || changed {
		t.Fatalf("cleared snapshot = event %#v changed %t error %v", event, changed, err)
	}
	second, changed, err := processor.ReconcileSnapshot([]byte(fixture.Normalized))
	if err != nil || second == nil || !changed || second.ID == first.ID || second.Sequence != 2 ||
		second.Signature != first.Signature {
		t.Fatalf("rearmed snapshot = first %#v second %#v changed %t error %v", first, second, changed, err)
	}
}

func TestCodexSnapshotDetectsSuccessivePromptWithSameFooter(t *testing.T) {
	fixture := loadCodexCaptureFixture(t, "command_approval.json")
	processor, err := NewProcessor(
		newCodexTestAdapter(t, DefaultPatterns()),
		NewDetectionState("session-codex", "reviewer", CodexID),
		4096,
		Hooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	first, changed, err := processor.ReconcileSnapshot([]byte(fixture.Normalized))
	if err != nil || first == nil || !changed {
		t.Fatalf("first snapshot = event %#v changed %t error %v", first, changed, err)
	}
	if err := processor.Acknowledge(first.ID); err != nil {
		t.Fatal(err)
	}
	secondScreen := strings.ReplaceAll(fixture.Normalized, "verified.txt", "second.txt")
	second, changed, err := processor.ReconcileSnapshot([]byte(secondScreen))
	if err != nil || second == nil || !changed || second.ID == first.ID || second.Sequence != 2 {
		t.Fatalf("successive same-footer snapshot = first %#v second %#v changed %t error %v", first, second, changed, err)
	}
}

func TestCodexSnapshotReplacesPendingPromptAnsweredDuringAttach(t *testing.T) {
	fixture := loadCodexCaptureFixture(t, "command_approval.json")
	var live []Event
	processor, err := NewProcessor(
		newCodexTestAdapter(t, DefaultPatterns()),
		NewDetectionState("session-codex", "reviewer", CodexID),
		4096,
		Hooks{OnEvent: func(event Event) { live = append(live, event) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range fixture.ANSIChunks {
		if err := processor.Consume([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if len(live) != 1 {
		t.Fatalf("live events = %#v", live)
	}
	first := live[0]
	repeated, changed, err := processor.ReconcileSnapshot([]byte(fixture.Normalized))
	if err != nil || repeated == nil || changed || repeated.ID != first.ID {
		t.Fatalf("same prompt live-to-snapshot = event %#v changed %t error %v", repeated, changed, err)
	}

	// The user answers the first approval inside the attached tmux client. No
	// acknowledgement crosses the Relayer bridge; the next visible approval has
	// the same semantic signature but a different verified prompt block.
	secondScreen := strings.ReplaceAll(fixture.Normalized, "verified.txt", "second.txt")
	second, changed, err := processor.ReconcileSnapshot([]byte(secondScreen))
	if err != nil || second == nil || !changed || second.ID == first.ID || second.Sequence != 2 ||
		second.Signature != first.Signature {
		t.Fatalf("post-attach occurrence = first %#v second %#v changed %t error %v", first, second, changed, err)
	}
	repeated, changed, err = processor.ReconcileSnapshot([]byte("\x1b[1m" + secondScreen + "\x1b[0m"))
	if err != nil || repeated == nil || repeated.ID != second.ID || changed {
		t.Fatalf("repainted second occurrence = event %#v changed %t error %v", repeated, changed, err)
	}
}

func TestCodexSnapshotFingerprintFollowsActivePromptNotOldHistory(t *testing.T) {
	command := loadCodexCaptureFixture(t, "command_approval.json")
	trust := loadCodexCaptureFixture(t, "directory_trust.json")
	var live []Event
	processor, err := NewProcessor(
		newCodexTestAdapter(t, DefaultPatterns()),
		NewDetectionState("session-codex", "reviewer", CodexID),
		4096,
		Hooks{OnEvent: func(event Event) { live = append(live, event) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, chunk := range trust.ANSIChunks {
		if err := processor.Consume([]byte(chunk)); err != nil {
			t.Fatal(err)
		}
	}
	if len(live) != 1 || live[0].Metadata[codexInteractionMetadata] != codexDirectoryTrust {
		t.Fatalf("live trust event = %#v", live)
	}
	snapshot := command.Normalized + "\n\ncompleted old interaction\n\n" + trust.Normalized
	reconciled, changed, err := processor.ReconcileSnapshot([]byte(snapshot))
	if err != nil || reconciled == nil || changed || reconciled.ID != live[0].ID {
		t.Fatalf("history plus active trust = event %#v changed %t error %v", reconciled, changed, err)
	}
}

func TestCodexSnapshotNeverCombinesHistoricalOptionsWithUnsupportedActiveFooter(t *testing.T) {
	command := loadCodexCaptureFixture(t, "command_approval.json")
	trust := loadCodexCaptureFixture(t, "directory_trust.json")
	processor, err := NewProcessor(
		newCodexTestAdapter(t, DefaultPatterns()),
		NewDetectionState("session-codex", "reviewer", CodexID),
		4096,
		Hooks{},
	)
	if err != nil {
		t.Fatal(err)
	}
	unsupported := command.Normalized + "\ncompleted old interaction\n" +
		"Unsupported selection\nA\nB\nPress enter to confirm or esc to cancel"
	if event, changed, err := processor.ReconcileSnapshot([]byte(unsupported)); err != nil || event != nil || changed {
		t.Fatalf("unsupported active prompt = event %#v changed %t error %v", event, changed, err)
	}

	activeTrust := command.Normalized + "\ncompleted old interaction\n" + trust.Normalized
	event, changed, err := processor.ReconcileSnapshot([]byte(activeTrust))
	if err != nil || event == nil || !changed || event.Metadata[codexInteractionMetadata] != codexDirectoryTrust {
		t.Fatalf("active trust after history = event %#v changed %t error %v", event, changed, err)
	}
}

func TestCodexConfiguredPatternTakesPriorityOverVendorRule(t *testing.T) {
	fixture := loadCodexCaptureFixture(t, "command_approval.json")
	adapter := newCodexTestAdapter(t, []Pattern{{
		Name:        "legacy-codex-command",
		Description: "configured command confirmation",
		Expression:  `(?s)Would you like to run the following command\?.*Press enter to confirm or esc to cancel`,
	}})
	state := NewDetectionState("session-codex", "reviewer", CodexID)
	events, err := adapter.Detect(state, []byte(fixture.Normalized))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Type != EventConfirmation ||
		events[0].Summary != "configured command confirmation" ||
		events[0].Metadata["pattern"] != "legacy-codex-command" ||
		events[0].Metadata[codexInteractionMetadata] != "" {
		t.Fatalf("configured collision event = %#v", events)
	}
}

func TestCodexGenericFallbackPreservesInterceptPatternsAndSensitiveInput(t *testing.T) {
	patterns := []Pattern{
		{Name: "legacy", Description: "legacy confirmation", Expression: `(?i)approve deployment\?`},
		{Name: "password", Description: "saisie d'un mot de passe", Expression: `(?im)password:[[:space:]]*$`, Sensitive: true},
	}
	adapter := newCodexTestAdapter(t, patterns)
	state := NewDetectionState("session-codex", "reviewer", CodexID)
	events, err := adapter.Detect(state, []byte("Approve deploy"))
	if err != nil || len(events) != 0 {
		t.Fatalf("partial fallback = events %#v error %v", events, err)
	}
	events, err = adapter.Detect(state, []byte("ment?"))
	if err != nil || len(events) != 1 || events[0].Adapter != CodexID ||
		events[0].Metadata["pattern"] != "legacy" || events[0].Type != EventConfirmation {
		t.Fatalf("fallback confirmation = events %#v error %v", events, err)
	}
	encoded, err := adapter.EncodeDecision(events[0], DecisionManual, "yes")
	if err != nil || !reflect.DeepEqual(encoded, []byte("yes\r")) {
		t.Fatalf("fallback manual encoding = %v, %v", encoded, err)
	}
	if got, err := adapter.EncodeDecision(events[0], DecisionAllow, ""); got != nil || !errors.Is(err, ErrDecisionUnsupported) {
		t.Fatalf("fallback automatic allow = %v, %v", got, err)
	}
	if _, err := state.acknowledge(events[0].ID); err != nil {
		t.Fatal(err)
	}
	// acknowledge no longer forgets the screen: the retained window is what
	// holds a prompt that arrived while this occurrence was pending. Processor
	// keeps it only when such a prompt survives and resets it otherwise, which
	// is the case here. This test drives the state directly, so it does the
	// same thing explicitly.
	state.resetWindow()

	events, err = adapter.Detect(state, []byte("Password:"))
	if err != nil || len(events) != 1 || events[0].Type != EventCredential ||
		!events[0].Sensitive || events[0].Risk != RiskHigh || events[0].Adapter != CodexID {
		t.Fatalf("sensitive fallback = events %#v error %v", events, err)
	}
	encoded, err = adapter.EncodeDecision(events[0], DecisionManual, "fixture-secret")
	if err != nil || !reflect.DeepEqual(encoded, []byte("fixture-secret\r")) {
		t.Fatalf("sensitive manual encoding = len %d error %v", len(encoded), err)
	}
}

func TestCodexDetectionWindowIsBounded(t *testing.T) {
	fixture := loadCodexCaptureFixture(t, "command_approval.json")
	var events []Event
	processor, err := NewProcessor(
		newCodexTestAdapter(t, DefaultPatterns()),
		NewDetectionState("session-codex", "reviewer", CodexID),
		64,
		Hooks{OnEvent: func(event Event) { events = append(events, event) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte(strings.Repeat("bounded history line\n", 4000))); err != nil {
		t.Fatal(err)
	}
	if got := processor.DetectionWindowLen(); got > detectionWindowSize {
		t.Fatalf("detection window len = %d, limit %d", got, detectionWindowSize)
	}
	if err := processor.Consume([]byte(fixture.Normalized)); err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || processor.DetectionWindowLen() > detectionWindowSize || len(processor.Output()) > 64 {
		t.Fatalf("bounded prompt = events %#v detection %d output %d", events, processor.DetectionWindowLen(), len(processor.Output()))
	}
}

func TestCodexEncodeOnlyCapturedDecisionSequences(t *testing.T) {
	adapter := newCodexTestAdapter(t, DefaultPatterns())
	for _, fixture := range loadCodexCaptureFixtures(t) {
		fixture := fixture
		t.Run(fixture.Interaction, func(t *testing.T) {
			event := Event{
				Adapter:  CodexID,
				Type:     EventPermission,
				Metadata: map[string]string{codexInteractionMetadata: fixture.Interaction},
			}
			tests := []struct {
				name     string
				decision Decision
				input    string
				wantHex  string
			}{
				{name: "automatic deny", decision: DecisionDeny, wantHex: fixture.DenyInputHex},
				{name: "manual allow", decision: DecisionManual, input: map[string]string{codexCommandApproval: "y", codexDirectoryTrust: "1"}[fixture.Interaction], wantHex: fixture.AllowInputHex},
				{name: "manual deny", decision: DecisionManual, input: map[string]string{codexCommandApproval: "esc", codexDirectoryTrust: "2"}[fixture.Interaction], wantHex: fixture.DenyInputHex},
			}
			if fixture.Interaction == codexCommandApproval {
				tests = append(tests, struct {
					name     string
					decision Decision
					input    string
					wantHex  string
				}{name: "automatic allow", decision: DecisionAllow, wantHex: fixture.AllowInputHex})
			} else if got, err := adapter.EncodeDecision(event, DecisionAllow, ""); got != nil || !errors.Is(err, ErrDecisionUnsupported) {
				t.Fatalf("selection-dependent automatic allow = %v, %v", got, err)
			}
			for _, test := range tests {
				t.Run(test.name, func(t *testing.T) {
					want, err := hex.DecodeString(test.wantHex)
					if err != nil {
						t.Fatal(err)
					}
					got, err := adapter.EncodeDecision(event, test.decision, test.input)
					if err != nil || !reflect.DeepEqual(got, want) {
						t.Fatalf("EncodeDecision = %x, %v; want %x", got, err, want)
					}
					if len(got) > 0 {
						got[0] ^= 0xff
					}
				})
			}
			if got, err := adapter.EncodeDecision(event, DecisionManual, "invented-answer"); got != nil || !errors.Is(err, ErrDecisionUnsupported) {
				t.Fatalf("invented manual response = %v, %v", got, err)
			}
			if got, err := adapter.EncodeDecision(event, DecisionAllow, "unexpected"); got != nil || err == nil {
				t.Fatalf("automatic decision with input = %v, %v", got, err)
			}
			if got, err := adapter.EncodeDecision(event, DecisionManual, "before\x00after"); got != nil || err == nil {
				t.Fatalf("manual NUL = %v, %v", got, err)
			}
		})
	}

	unknown := Event{Type: EventPermission, Metadata: map[string]string{codexInteractionMetadata: "not_captured"}}
	if got, err := adapter.EncodeDecision(unknown, DecisionAllow, ""); got != nil || !errors.Is(err, ErrDecisionUnsupported) {
		t.Fatalf("unknown interaction = %v, %v", got, err)
	}
	if got, err := adapter.EncodeDecision(Event{Type: EventProcessExit}, DecisionManual, "y"); got != nil || !errors.Is(err, ErrDecisionUnsupported) {
		t.Fatalf("non-actionable event = %v, %v", got, err)
	}
}

func TestCodexConstructorAndEmptyOrInvalidDetection(t *testing.T) {
	if _, err := NewCodexAdapter([]Pattern{{Name: "broken", Expression: "("}}); err == nil {
		t.Fatal("invalid fallback regex was accepted")
	}
	adapter := newCodexTestAdapter(t, nil)
	if events, err := adapter.Detect(nil, []byte("output")); err == nil || events != nil {
		t.Fatalf("nil state = events %#v error %v", events, err)
	}
	state := NewDetectionState("session-codex", "reviewer", CodexID)
	if events, err := adapter.Detect(state, nil); err != nil || events != nil || state.Pending() != nil {
		t.Fatalf("empty output = events %#v pending %#v error %v", events, state.Pending(), err)
	}
	if events, err := adapter.Detect(state, []byte("\x00\x01\x02")); err != nil || len(events) != 0 || state.Pending() != nil {
		t.Fatalf("invalid normalized output = events %#v pending %#v error %v", events, state.Pending(), err)
	}
}

func assertCodexCaptureEvent(t *testing.T, events []Event, fixture codexCaptureFixture) {
	t.Helper()
	if len(events) != 1 {
		t.Fatalf("events = %#v, want one", events)
	}
	event := events[0]
	if event.ID == "" || event.Signature == "" || event.Sequence != 1 || event.Timestamp.IsZero() {
		t.Fatalf("event identity = %#v", event)
	}
	if event.SessionID != "session-codex" || event.AgentID != "reviewer" || event.Adapter != CodexID ||
		event.Type != EventPermission || !event.Actionable() || event.Sensitive || event.Risk != fixture.Risk ||
		event.Metadata[codexInteractionMetadata] != fixture.Interaction {
		t.Fatalf("event semantics = %#v", event)
	}
	if event.Match == "" || strings.Contains(event.Match, "verified-content") ||
		strings.Contains(event.Summary, "verified-content") || strings.Contains(event.Summary, "fixture-directory") {
		t.Fatalf("event retained command or path content: %#v", event)
	}
	if len(event.Metadata) != 1 {
		t.Fatalf("event metadata contains unverified content: %#v", event.Metadata)
	}
}

func newCodexTestAdapter(t *testing.T, patterns []Pattern) *CodexAdapter {
	t.Helper()
	adapter, err := NewCodexAdapter(patterns)
	if err != nil {
		t.Fatal(err)
	}
	return adapter
}

func loadCodexCaptureFixtures(t *testing.T) []codexCaptureFixture {
	t.Helper()
	return []codexCaptureFixture{
		loadCodexCaptureFixture(t, "command_approval.json"),
		loadCodexCaptureFixture(t, "directory_trust.json"),
	}
}

func loadCodexCaptureFixture(t *testing.T, name string) codexCaptureFixture {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", "codex", name))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "/Users/") || strings.Contains(string(payload), "@example") {
		t.Fatalf("fixture %q contains a personal path or email-like placeholder", name)
	}
	var fixture codexCaptureFixture
	if err := json.Unmarshal(payload, &fixture); err != nil {
		t.Fatal(err)
	}
	if fixture.CLIVersion != "codex-cli 0.148.0-alpha.21" || fixture.Interaction == "" ||
		fixture.Normalized == "" || len(fixture.Chunks) < 2 || len(fixture.ANSIChunks) < 2 ||
		fixture.AllowInputHex == "" || fixture.DenyInputHex == "" {
		t.Fatalf("incomplete Codex fixture %q: %#v", name, fixture)
	}
	if _, err := hex.DecodeString(fixture.AllowInputHex); err != nil {
		t.Fatalf("fixture %q allow input: %v", name, err)
	}
	if _, err := hex.DecodeString(fixture.DenyInputHex); err != nil {
		t.Fatalf("fixture %q deny input: %v", name, err)
	}
	return fixture
}

func TestCodexFixtureInventoryIsExact(t *testing.T) {
	entries, err := filepath.Glob(filepath.Join("testdata", "codex", "*.json"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		filepath.Join("testdata", "codex", "command_approval.json"),
		filepath.Join("testdata", "codex", "directory_trust.json"),
	}
	if !reflect.DeepEqual(entries, want) {
		t.Fatalf("captured fixture inventory = %v, want %v", entries, want)
	}
	for _, fixture := range loadCodexCaptureFixtures(t) {
		if fixture.CaptureMode != "interactive --no-alt-screen --ask-for-approval untrusted --sandbox workspace-write" {
			t.Errorf("unexpected capture mode for %s: %q", fixture.Interaction, fixture.CaptureMode)
		}
	}
	if fmt.Sprint(CodexID) != "codex" {
		t.Fatalf("CodexID = %q", CodexID)
	}
}
