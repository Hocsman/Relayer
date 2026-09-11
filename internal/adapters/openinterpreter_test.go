package adapters

import (
	"bytes"
	"reflect"
	"testing"
)

func TestOpenInterpreterAdapterID(t *testing.T) {
	adapter, err := NewOpenInterpreterAdapter(nil)
	if err != nil {
		t.Fatalf("NewOpenInterpreterAdapter: %v", err)
	}
	if got := adapter.ID(); got != OpenInterpreterID {
		t.Fatalf("adapter.ID() = %q, want %q", got, OpenInterpreterID)
	}
}

func TestOpenInterpreterPromptsDetection(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		interaction string
		eventType   EventType
		risk        RiskLevel
		summary     string
	}{
		{
			name:        "run code basic",
			input:       "Would you like to run this code? (y/n) ",
			interaction: openinterpreterRunCode,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Open Interpreter asks to run code (y=allow, n=deny)",
		},
		{
			name:        "run code bracketed with colon",
			input:       "Would you like to run this code? [y/n]: ",
			interaction: openinterpreterRunCode,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Open Interpreter asks to run code (y=allow, n=deny)",
		},
		{
			name:        "execute python code",
			input:       "Execute this Python code? (y/n) ",
			interaction: openinterpreterRunCode,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Open Interpreter asks to run code (y=allow, n=deny)",
		},
		{
			name:        "run shell command",
			input:       "Run shell command? (y/n) ",
			interaction: openinterpreterRunCommand,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Open Interpreter asks to run shell command (y=allow, n=deny)",
		},
		{
			name:        "run this command",
			input:       "Would you like to run this command? [y/n] ",
			interaction: openinterpreterRunCommand,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Open Interpreter asks to run shell command (y=allow, n=deny)",
		},
		{
			name:        "install package",
			input:       "Would you like to install this package? (y/n) ",
			interaction: openinterpreterInstallPackage,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Open Interpreter asks to install package (y=allow, n=deny)",
		},
		{
			name:        "install dependencies",
			input:       "Install dependencies? [y/n] ",
			interaction: openinterpreterInstallPackage,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Open Interpreter asks to install package (y=allow, n=deny)",
		},
		{
			name:        "save file changes",
			input:       "Would you like to save these changes? (y/n) ",
			interaction: openinterpreterSaveFile,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Open Interpreter asks to save file changes (y=allow, n=deny)",
		},
		{
			name:        "save changes short",
			input:       "Save changes? [y/n] ",
			interaction: openinterpreterSaveFile,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Open Interpreter asks to save file changes (y=allow, n=deny)",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := NewOpenInterpreterAdapter(nil)
			if err != nil {
				t.Fatalf("NewOpenInterpreterAdapter: %v", err)
			}
			state := NewDetectionState("session-interpreter", "agent-interpreter", OpenInterpreterID)

			events, err := adapter.Detect(state, []byte(tc.input))
			if err != nil {
				t.Fatalf("Detect error: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("events count = %d, want 1", len(events))
			}

			event := events[0]
			if event.Adapter != OpenInterpreterID {
				t.Errorf("event.Adapter = %q, want %q", event.Adapter, OpenInterpreterID)
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
			if event.Metadata[openinterpreterInteractionMetadata] != tc.interaction {
				t.Errorf("interaction metadata = %q, want %q", event.Metadata[openinterpreterInteractionMetadata], tc.interaction)
			}
			if !event.Actionable() {
				t.Errorf("event should be actionable")
			}
		})
	}
}

func TestOpenInterpreterMultiLinePrompt(t *testing.T) {
	adapter, err := NewOpenInterpreterAdapter(nil)
	if err != nil {
		t.Fatalf("NewOpenInterpreterAdapter: %v", err)
	}
	state := NewDetectionState("session-interpreter-multiline", "agent-interpreter", OpenInterpreterID)

	input := "```python\nimport os\nos.listdir('.')\n```\nWould you like to run the following code?\n(y/n) "
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
	if events[0].Metadata[openinterpreterInteractionMetadata] != openinterpreterRunCode {
		t.Fatalf("unexpected interaction: %q", events[0].Metadata[openinterpreterInteractionMetadata])
	}
}

func TestOpenInterpreterPromptsStreamingChunks(t *testing.T) {
	chunks := []string{
		"Open Interpreter is ready.\n",
		"Generated code block:\n",
		"Would you like to ",
		"run this code? ",
		"(y/n) ",
	}

	adapter, err := NewOpenInterpreterAdapter(nil)
	if err != nil {
		t.Fatalf("NewOpenInterpreterAdapter: %v", err)
	}
	state := NewDetectionState("session-stream", "agent-interpreter", OpenInterpreterID)

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

func TestOpenInterpreterSuppression(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "code fence block",
			input: "```\nWould you like to run this code? (y/n) \n```\n",
		},
		{
			name:  "quoted prompt in backticks",
			input: "Check `Would you like to run this code? (y/n)` in logs\n",
		},
		{
			name:  "quoted prompt in single quotes",
			input: "The system asked 'Would you like to run this command? (y/n)' earlier\n",
		},
		{
			name:  "log prefix",
			input: "log: Would you like to run this code? (y/n) \n",
		},
		{
			name:  "quote block prefix",
			input: "> Would you like to run this code? (y/n) \n",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := NewOpenInterpreterAdapter(nil)
			if err != nil {
				t.Fatalf("NewOpenInterpreterAdapter: %v", err)
			}
			state := NewDetectionState("session-suppress", "agent-interpreter", OpenInterpreterID)

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

func TestOpenInterpreterEncodeDecision(t *testing.T) {
	adapter, err := NewOpenInterpreterAdapter(nil)
	if err != nil {
		t.Fatalf("NewOpenInterpreterAdapter: %v", err)
	}

	event := Event{
		Type: EventPermission,
		Metadata: map[string]string{
			openinterpreterInteractionMetadata: openinterpreterRunCode,
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
	manualEncoded, err := adapter.EncodeDecision(event, DecisionManual, "use python instead")
	if err != nil {
		t.Fatalf("EncodeDecision(DecisionManual) error: %v", err)
	}
	if !reflect.DeepEqual(manualEncoded, []byte("use python instead\r")) {
		t.Errorf("manualEncoded = %q, want %q", manualEncoded, "use python instead\r")
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

func TestOpenInterpreterSnapshotFingerprintSource(t *testing.T) {
	adapter, err := NewOpenInterpreterAdapter(nil)
	if err != nil {
		t.Fatalf("NewOpenInterpreterAdapter: %v", err)
	}

	prompt := "Would you like to run this code? (y/n) "
	source := adapter.snapshotFingerprintSource(prompt, prompt, false)
	if !bytes.Contains([]byte(source), []byte(openinterpreterRunCode)) {
		t.Errorf("fingerprint source = %q, want to contain %q", source, openinterpreterRunCode)
	}

	// Inside code fence, should return activeLine unchanged
	fenceSource := adapter.snapshotFingerprintSource(prompt, prompt, true)
	if fenceSource != prompt {
		t.Errorf("fenceSource = %q, want %q", fenceSource, prompt)
	}

	// Occurrence aware
	if !adapter.snapshotOccurrenceAware(Event{Metadata: map[string]string{openinterpreterInteractionMetadata: openinterpreterRunCode}}) {
		t.Errorf("expected snapshotOccurrenceAware to be true for vendor interaction")
	}
	if adapter.snapshotOccurrenceAware(Event{}) {
		t.Errorf("expected snapshotOccurrenceAware to be false for empty metadata")
	}
}
