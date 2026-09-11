package adapters

import (
	"bytes"
	"reflect"
	"testing"
)

func TestAiderAdapterID(t *testing.T) {
	adapter, err := NewAiderAdapter(nil)
	if err != nil {
		t.Fatalf("NewAiderAdapter: %v", err)
	}
	if got := adapter.ID(); got != AiderID {
		t.Fatalf("adapter.ID() = %q, want %q", got, AiderID)
	}
}

func TestAiderPromptsDetection(t *testing.T) {
	tests := []struct {
		name        string
		input       string
		interaction string
		eventType   EventType
		risk        RiskLevel
		summary     string
	}{
		{
			name:        "apply changes basic",
			input:       "Apply changes? (Y)es/(N)o/(D)escribe [Yes]: ",
			interaction: aiderApplyChanges,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Aider asks to apply changes (y=allow, n=deny)",
		},
		{
			name:        "apply changes with chat option",
			input:       "Apply changes? (Y)es/(N)o/(D)escribe/(C)hat [Yes]: ",
			interaction: aiderApplyChanges,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Aider asks to apply changes (y=allow, n=deny)",
		},
		{
			name:        "apply these changes",
			input:       "Apply these changes? (Y)es/(N)o [Yes]: ",
			interaction: aiderApplyChanges,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Aider asks to apply changes (y=allow, n=deny)",
		},
		{
			name:        "run shell command",
			input:       "Run shell command? (Y)es/(N)o [Yes]: ",
			interaction: aiderRunCommand,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Aider asks to run shell command (y=allow, n=deny)",
		},
		{
			name:        "run tests",
			input:       "Run tests? (Y)es/(N)o [Yes]: ",
			interaction: aiderRunCommand,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Aider asks to run shell command (y=allow, n=deny)",
		},
		{
			name:        "add file to chat",
			input:       "Add src/main.py to the chat? (Y)es/(N)o [Yes]: ",
			interaction: aiderAddToChat,
			eventType:   EventPermission,
			risk:        RiskLow,
			summary:     "Aider asks to add file to chat (y=allow, n=deny)",
		},
		{
			name:        "create file",
			input:       "Create tests/test_new.py? (Y)es/(N)o [Yes]: ",
			interaction: aiderCreateFile,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Aider asks to create file (y=allow, n=deny)",
		},
	}

	for _, tc := range tests {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := NewAiderAdapter(nil)
			if err != nil {
				t.Fatalf("NewAiderAdapter: %v", err)
			}
			state := NewDetectionState("session-aider", "agent-aider", AiderID)

			events, err := adapter.Detect(state, []byte(tc.input))
			if err != nil {
				t.Fatalf("Detect error: %v", err)
			}
			if len(events) != 1 {
				t.Fatalf("events count = %d, want 1", len(events))
			}

			event := events[0]
			if event.Adapter != AiderID {
				t.Errorf("event.Adapter = %q, want %q", event.Adapter, AiderID)
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
			if event.Metadata[aiderInteractionMetadata] != tc.interaction {
				t.Errorf("interaction metadata = %q, want %q", event.Metadata[aiderInteractionMetadata], tc.interaction)
			}
			if !event.Actionable() {
				t.Errorf("event should be actionable")
			}
		})
	}
}

func TestAiderPromptsStreamingChunks(t *testing.T) {
	chunks := []string{
		"Tokens: 2.3k sent, 412 received.\n",
		"Applying edits...\n",
		"Run shell ",
		"command? (Y)es/(N)o ",
		"[Yes]: ",
	}

	adapter, err := NewAiderAdapter(nil)
	if err != nil {
		t.Fatalf("NewAiderAdapter: %v", err)
	}
	state := NewDetectionState("session-stream", "agent-aider", AiderID)

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

func TestAiderSuppression(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "code fence block",
			input: "```\nApply changes? (Y)es/(N)o/(D)escribe [Yes]: \n```\n",
		},
		{
			name:  "quoted prompt in backticks",
			input: "Run `Run shell command? (Y)es/(N)o [Yes]:` in the terminal\n",
		},
		{
			name:  "quoted prompt in single quotes",
			input: "Aider asked 'Add foo.py to the chat? (Y)es/(N)o [Yes]:' earlier\n",
		},
		{
			name:  "log prefix",
			input: "log: Apply changes? (Y)es/(N)o/(D)escribe [Yes]: \n",
		},
		{
			name:  "quote block prefix",
			input: "> Apply changes? (Y)es/(N)o/(D)escribe [Yes]: \n",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := NewAiderAdapter(nil)
			if err != nil {
				t.Fatalf("NewAiderAdapter: %v", err)
			}
			state := NewDetectionState("session-suppress", "agent-aider", AiderID)

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

func TestAiderEncodeDecision(t *testing.T) {
	adapter, err := NewAiderAdapter(nil)
	if err != nil {
		t.Fatalf("NewAiderAdapter: %v", err)
	}

	event := Event{
		Type: EventConfirmation,
		Metadata: map[string]string{
			aiderInteractionMetadata: aiderApplyChanges,
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
	manualEncoded, err := adapter.EncodeDecision(event, DecisionManual, "describe")
	if err != nil {
		t.Fatalf("EncodeDecision(DecisionManual) error: %v", err)
	}
	if !reflect.DeepEqual(manualEncoded, []byte("describe\r")) {
		t.Errorf("manualEncoded = %q, want %q", manualEncoded, "describe\r")
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

func TestAiderSnapshotFingerprintSource(t *testing.T) {
	adapter, err := NewAiderAdapter(nil)
	if err != nil {
		t.Fatalf("NewAiderAdapter: %v", err)
	}

	prompt := "Apply changes? (Y)es/(N)o/(D)escribe [Yes]: "
	source := adapter.snapshotFingerprintSource(prompt, prompt, false)
	if !bytes.Contains([]byte(source), []byte(aiderApplyChanges)) {
		t.Errorf("fingerprint source = %q, want to contain %q", source, aiderApplyChanges)
	}

	// Inside code fence, should return activeLine unchanged
	fenceSource := adapter.snapshotFingerprintSource(prompt, prompt, true)
	if fenceSource != prompt {
		t.Errorf("fenceSource = %q, want %q", fenceSource, prompt)
	}
}
