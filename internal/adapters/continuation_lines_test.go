package adapters_test

import (
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
)

func TestQuestionSeparatedFromOptionsByMoreThanFourLines(t *testing.T) {
	adapter, err := adapters.NewGenericRegexAdapter([]adapters.Pattern{
		{
			Name:        "confirm",
			Description: "Confirmation",
			Expression:  `(?i)Apply changes to\s+\S+\?`,
		},
	})
	if err != nil {
		t.Fatalf("NewGenericRegexAdapter: %v", err)
	}

	state := adapters.NewDetectionState("session-1", "agent-1", adapters.GenericID)

	// Question followed by 8 lines of diff/preview, followed by options and footer.
	previewLines := []string{
		"Apply changes to main.go?",
		"--- a/main.go",
		"+++ b/main.go",
		"@@ -10,6 +10,8 @@",
		"+ func NewFeature() {",
		"+     // implementation",
		"+     return",
		"+ }",
		"1. Yes, apply changes",
		"2. No, cancel",
		"Press enter to confirm",
	}
	screen := strings.Join(previewLines, "\n")

	events, err := adapter.Detect(state, []byte(screen))
	if err != nil {
		t.Fatalf("Detect failed: %v", err)
	}
	if len(events) != 1 {
		t.Fatalf("expected 1 event for question with 8 lines of diff preview, got %d", len(events))
	}
	if events[0].Summary != "Confirmation" {
		t.Errorf("unexpected event summary: %q", events[0].Summary)
	}
}
