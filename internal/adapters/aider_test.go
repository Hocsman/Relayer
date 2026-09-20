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
		{
			name:        "commit changes",
			input:       "Commit changes? (Y)es/(N)o [Yes]: ",
			interaction: aiderGitCommit,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Aider asks to commit changes (y=allow, n=deny)",
		},
		{
			name:        "commit before the chat proceeds",
			input:       "Commit before the chat proceeds? (Y)es/(N)o [Yes]: ",
			interaction: aiderGitCommit,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Aider asks to commit changes (y=allow, n=deny)",
		},
		{
			name:        "push to remote",
			input:       "Push to remote? (Y)es/(N)o [Yes]: ",
			interaction: aiderGitPush,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Aider asks to push commits to the remote (y=allow, n=deny)",
		},
		{
			name:        "push commits to remote",
			input:       "Push commits to remote? (Y)es/(N)o [Yes]: ",
			interaction: aiderGitPush,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Aider asks to push commits to the remote (y=allow, n=deny)",
		},
		{
			name:        "add files to git",
			input:       "Add files to git? (Y)es/(N)o [Yes]: ",
			interaction: aiderGitAdd,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Aider asks to track files in git (y=allow, n=deny)",
		},
		// Ignoring a path is not tracking it, so it must not borrow the
		// tracking summary: the summary is what the operator answers.
		{
			name:        "add to gitignore",
			input:       "Add to .gitignore? (Y)es/(N)o [Yes]: ",
			interaction: aiderGitIgnore,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Aider asks to add a path to .gitignore (y=allow, n=deny)",
		},
		// A line that satisfies both the broad create marker and a git marker
		// resolves to the git reading, so a push is never hidden behind a
		// low-risk file confirmation.
		{
			name:        "create and push resolves to the push",
			input:       "Create branch and Push to remote? (Y)es/(N)o [Yes]: ",
			interaction: aiderGitPush,
			eventType:   EventPermission,
			risk:        RiskHigh,
			summary:     "Aider asks to push commits to the remote (y=allow, n=deny)",
		},
		{
			name:        "create and add to git resolves to the git add",
			input:       "Create file and Add to git? (Y)es/(N)o [Yes]: ",
			interaction: aiderGitAdd,
			eventType:   EventConfirmation,
			risk:        RiskLow,
			summary:     "Aider asks to track files in git (y=allow, n=deny)",
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

// TestAiderGitSuppression guards the git markers, which are heuristic and have
// no fixture behind them, against firing on text that only talks about git.
func TestAiderGitSuppression(t *testing.T) {
	cases := []struct {
		name  string
		input string
	}{
		{
			name:  "diff line mentioning commit",
			input: "@@ -8,7 +8,7 @@\n-  # Commit changes? is handled by the git layer\n",
		},
		{
			name:  "diff line with prompt shape but no options",
			input: "+  label = Commit changes? (y/n)\n",
		},
		{
			name:  "push prompt inside code fence",
			input: "```\nPush to remote? (Y)es/(N)o [Yes]: \n```\n",
		},
		{
			name:  "quoted push prompt in backticks",
			input: "Answer `Push to remote? (Y)es/(N)o [Yes]:` when the tests pass\n",
		},
		{
			name:  "prose about committing without prompt footer",
			input: "Aider will commit changes to git and push to remote once the edits apply.\n",
		},
		{
			name:  "log prefix commit prompt",
			input: "log: Commit changes? (Y)es/(N)o [Yes]: \n",
		},
		{
			name:  "quote block prefix push prompt",
			input: "> Push to remote? (Y)es/(N)o [Yes]: \n",
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			adapter, err := NewAiderAdapter(nil)
			if err != nil {
				t.Fatalf("NewAiderAdapter: %v", err)
			}
			state := NewDetectionState("session-git-suppress", "agent-aider", AiderID)

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
