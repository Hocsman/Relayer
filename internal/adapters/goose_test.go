package adapters

import (
	"bytes"
	"reflect"
	"testing"
)

func TestGooseAdapterID(t *testing.T) {
	adapter, err := NewGooseAdapter(nil)
	if err != nil {
		t.Fatalf("NewGooseAdapter: %v", err)
	}
	if got := adapter.ID(); got != GooseID {
		t.Fatalf("adapter.ID() = %q, want %q", got, GooseID)
	}
}

func TestGoosePromptsDetection(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		interaction string
		eventType   EventType
		risk        RiskLevel
		summary     string
	}{
		{
			name:        "approve tool execution",
			input:       "Approve tool execution? [y/n] ",
			interaction: gooseExecuteTool,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Goose asks to execute tool (y=allow, n=deny)",
		},
		{
			name:        "allow goose to run",
			input:       "Allow Goose to run: `ls -la` (y/n) ",
			interaction: gooseExecuteTool,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Goose asks to execute tool (y=allow, n=deny)",
		},
		{
			name:        "run shell command",
			input:       "Run shell command? (y/n) ",
			interaction: gooseRunCommand,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Goose asks to run command (y=allow, n=deny)",
		},
		{
			name:        "confirm command execution",
			input:       "Confirm command execution? [y/n]: ",
			interaction: gooseRunCommand,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Goose asks to run command (y=allow, n=deny)",
		},
		{
			name:        "approve file modification",
			input:       "Approve file modification? (y/n) ",
			interaction: gooseModifyFile,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Goose asks to modify file (y=allow, n=deny)",
		},
		{
			name:        "allow goose to write",
			input:       "Allow Goose to write: src/config.go [y/n] ",
			interaction: gooseModifyFile,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Goose asks to modify file (y=allow, n=deny)",
		},
		{
			name:        "approve extension access",
			input:       "Approve extension access? (y/n) ",
			interaction: gooseExtension,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Goose asks to approve extension (y=allow, n=deny)",
		},
		{
			name:        "enable extension",
			input:       "Enable extension? [y/n] ",
			interaction: gooseExtension,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Goose asks to approve extension (y=allow, n=deny)",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := NewGooseAdapter(nil)
			if err != nil {
				t.Fatalf("NewGooseAdapter: %v", err)
			}
			state := NewDetectionState("session-goose", "agent-goose", GooseID)

			events, err := adapter.Detect(state, []byte(tc.input))
			if err != nil {
				t.Fatalf("Detect error: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("events count = %d, want 1", len(events))
			}

			event := events[0]
			if event.Adapter != GooseID {
				t.Errorf("event.Adapter = %q, want %q", event.Adapter, GooseID)
			}
			if event.Type != tc.eventType {
				t.Errorf("event.Type = %q, want %q", event.Type, tc.eventType)
			}
			if event.Risk != tc.risk {
				t.Errorf("event.Risk = %q, want %q", event.Risk, tc.risk)
			}
			if event.Summary != tc.summary {
				t.Errorf("event.Summary = %q, want %q", event.Summary, tc.summary)
			}
			if event.Metadata[gooseInteractionMetadata] != tc.interaction {
				t.Errorf("interaction metadata = %q, want %q", event.Metadata[gooseInteractionMetadata], tc.interaction)
			}
			if !event.Actionable() {
				t.Errorf("event should be actionable")
			}
		})
	}
}

func TestGooseMultiLinePrompt(t *testing.T) {
	adapter, err := NewGooseAdapter(nil)
	if err != nil {
		t.Fatalf("NewGooseAdapter: %v", err)
	}
	state := NewDetectionState("session-goose-multiline", "agent-goose", GooseID)

	input := "Goose will run the following tool:\nApprove tool execution?\n[y/n] "
	events, err := adapter.Detect(state, []byte(input))
	if err != nil {
		t.Fatalf("Detect error: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("events count = %d, want 1", len(events))
	}
	if events[0].Type != EventPermission || events[0].Risk != RiskHigh {
		t.Fatalf("unexpected event: %#v", events[0])
	}
	if events[0].Metadata[gooseInteractionMetadata] != gooseExecuteTool {
		t.Fatalf("unexpected interaction: %q", events[0].Metadata[gooseInteractionMetadata])
	}
}

func TestGoosePromptsStreamingChunks(t *testing.T) {
	chunks := []string{
		"Tool request: developer__shell\n",
		"Command: npm test\n",
		"Approve tool ",
		"execution? ",
		"[y/n] ",
	}

	adapter, err := NewGooseAdapter(nil)
	if err != nil {
		t.Fatalf("NewGooseAdapter: %v", err)
	}
	state := NewDetectionState("session-stream", "agent-goose", GooseID)

	var allEvents []Event
	for i, chunk := range chunks {
		events, err := adapter.Detect(state, []byte(chunk))
		if err != nil {
			t.Fatalf("chunk %d Detect error: %v", i, err)
		}
		if i < len(chunks)-1 && len(events) != 0 {
			t.Fatalf("unexpected events before final chunk: %#v", events)
		}
		allEvents = append(allEvents, events...)
	}

	if len(allEvents) != 1 {
		t.Fatalf("total events = %d, want 1", len(allEvents))
	}
	if allEvents[0].Type != EventPermission || allEvents[0].Risk != RiskHigh {
		t.Fatalf("unexpected event: %#v", allEvents[0])
	}
}

func TestGooseSuppression(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "code fence block",
			input: "```\nApprove tool execution? [y/n] \n```\n",
		},
		{
			name:  "quoted prompt in backticks",
			input: "Run `Approve tool execution? [y/n]` check\n",
		},
		{
			name:  "quoted prompt in single quotes",
			input: "Goose logged 'Approve tool execution? [y/n]' earlier\n",
		},
		{
			name:  "log prefix",
			input: "log: Approve tool execution? [y/n] \n",
		},
		{
			name:  "quote block prefix",
			input: "> Approve tool execution? [y/n] \n",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := NewGooseAdapter(nil)
			if err != nil {
				t.Fatalf("NewGooseAdapter: %v", err)
			}
			state := NewDetectionState("session-suppress", "agent-goose", GooseID)

			events, err := adapter.Detect(state, []byte(tc.input))
			if err != nil {
				t.Fatalf("Detect error: %v", err)
			}
			if len(events) != 0 {
				t.Fatalf("expected no events, got: %#v", events)
			}
		})
	}
}

func TestGooseEncodeDecision(t *testing.T) {
	adapter, err := NewGooseAdapter(nil)
	if err != nil {
		t.Fatalf("NewGooseAdapter: %v", err)
	}

	event := Event{
		Type: EventPermission,
		Metadata: map[string]string{
			gooseInteractionMetadata: gooseExecuteTool,
		},
	}

	// DecisionAllow -> "y\r"
	allowEncoded, err := adapter.EncodeDecision(event, DecisionAllow, "")
	if err != nil {
		t.Fatalf("EncodeDecision(DecisionAllow) error: %v", err)
	}
	if !reflect.DeepEqual(allowEncoded, []byte("y\r")) {
		t.Errorf("allowEncoded = %q, want %q", allowEncoded, "y\r")
	}

	// DecisionDeny -> "n\r"
	denyEncoded, err := adapter.EncodeDecision(event, DecisionDeny, "")
	if err != nil {
		t.Fatalf("EncodeDecision(DecisionDeny) error: %v", err)
	}
	if !reflect.DeepEqual(denyEncoded, []byte("n\r")) {
		t.Errorf("denyEncoded = %q, want %q", denyEncoded, "n\r")
	}

	// DecisionManual -> "<input>\r"
	manualEncoded, err := adapter.EncodeDecision(event, DecisionManual, "deny tool")
	if err != nil {
		t.Fatalf("EncodeDecision(DecisionManual) error: %v", err)
	}
	if !reflect.DeepEqual(manualEncoded, []byte("deny tool\r")) {
		t.Errorf("manualEncoded = %q, want %q", manualEncoded, "deny tool\r")
	}

	// Automatic decision with manualInput must error
	if _, err := adapter.EncodeDecision(event, DecisionAllow, "extra"); err == nil {
		t.Errorf("expected error for automatic decision with non-empty manual input")
	}

	// Manual input with NUL byte must error
	if _, err := adapter.EncodeDecision(event, DecisionManual, "bad\x00input"); err == nil {
		t.Errorf("expected error for NUL byte in manual input")
	}

	// Non-actionable event must error
	nonActionable := Event{Type: EventProcessExit}
	if _, err := adapter.EncodeDecision(nonActionable, DecisionAllow, ""); err == nil {
		t.Errorf("expected error for non-actionable event")
	}
}

func TestGooseSnapshotFingerprintSource(t *testing.T) {
	adapter, err := NewGooseAdapter(nil)
	if err != nil {
		t.Fatalf("NewGooseAdapter: %v", err)
	}

	prompt := "Approve tool execution? [y/n] "
	source := adapter.snapshotFingerprintSource(prompt, prompt, false)
	if !bytes.Contains([]byte(source), []byte(gooseExecuteTool)) {
		t.Errorf("fingerprint source = %q, want to contain %q", source, gooseExecuteTool)
	}

	// Inside code fence, should return activeLine unchanged
	fenceSource := adapter.snapshotFingerprintSource(prompt, prompt, true)
	if fenceSource != prompt {
		t.Errorf("fenceSource = %q, want %q", fenceSource, prompt)
	}

	// Occurrence aware
	if !adapter.snapshotOccurrenceAware(Event{Metadata: map[string]string{gooseInteractionMetadata: gooseExecuteTool}}) {
		t.Errorf("expected snapshotOccurrenceAware to be true for vendor interaction")
	}
	if adapter.snapshotOccurrenceAware(Event{}) {
		t.Errorf("expected snapshotOccurrenceAware to be false for empty metadata")
	}
}
