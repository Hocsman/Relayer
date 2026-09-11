package adapters

import (
	"strings"
	"testing"
)

// Issue #29: Detection: a pinned footer with ordinary words hides every question painted above it
func TestPinnedFooterDoesNotHideQuestion(t *testing.T) {
	footers := []struct {
		name   string
		footer string
	}{
		{name: "esc to interrupt", footer: "esc to interrupt"},
		{name: "shortcuts hint", footer: "? for shortcuts"},
		{name: "context status", footer: "Context left until auto-compact: 42%"},
		{name: "shell prompt line", footer: "❯"},
		{name: "shell prompt with space", footer: "❯ "},
		{name: "bash prompt", footer: "user@box:~$ "},
	}

	for _, tc := range footers {
		t.Run(tc.name, func(t *testing.T) {
			processor, raised := repaintProcessor(t)
			processor.Resize(70, 12)

			// chunk 1: write footer near bottom
			chunk1 := "\x1b[2J\x1b[11;1H" + tc.footer
			if err := processor.Consume([]byte(chunk1)); err != nil {
				t.Fatal(err)
			}

			// chunk 2: write question above footer with choices
			chunk2 := "\x1b[2;1HDo you want to proceed? [y/n]\x1b[4;1H1. Yes\x1b[5;1H2. No"
			if err := processor.Consume([]byte(chunk2)); err != nil {
				t.Fatal(err)
			}

			if processor.Pending() == nil {
				t.Fatalf("question above footer %q was not detected; raised=%v", tc.footer, *raised)
			}
			if len(*raised) != 1 {
				t.Fatalf("expected 1 event, got %d: %v", len(*raised), *raised)
			}
		})
	}
}

// Issue #30: Detection: a question is identified by a text fragment, so the second yes/no question is swallowed
func TestSecondConfirmationQuestionIsNotSwallowed(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		first  string
		second string
	}{
		{
			name:   "the same fragment in two unrelated questions",
			first:  "Run 'npm test'? [y/n]",
			second: "Run 'rm -rf /' as root? [y/n]",
		},
		{
			name:   "one captured fragment contained in the other",
			first:  "Overwrite file? [y/n]",
			second: "Delete the cache? [y/n]",
		},
		{
			name:   "identical casing throughout",
			first:  "Overwrite file? [Y/n]",
			second: "Delete the cache? [Y/n]",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			processor, raised := repaintProcessor(t)
			processor.Resize(70, 12)

			if err := processor.Consume([]byte("\x1b[2J\x1b[1;1H" + testCase.first)); err != nil {
				t.Fatal(err)
			}
			pending := processor.Pending()
			if pending == nil {
				t.Fatalf("the first question was not detected: %v", *raised)
			}
			if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			if err := processor.Consume([]byte("\x1b[4;1H" + testCase.second)); err != nil {
				t.Fatal(err)
			}
			if processor.Pending() == nil {
				t.Fatalf("the second question was swallowed by the first; raised=%v", *raised)
			}
			if len(*raised) != 2 {
				t.Fatalf("raised %d occurrence(s), want 2: %v", len(*raised), *raised)
			}
		})
	}
}

// Issue #31: Detection: an escape-only write changes the screen but never reaches Detect
func TestEscapeOnlyWriteUncoveringQuestionTriggersDetection(t *testing.T) {
	processor, raised := repaintProcessor(t)
	processor.Resize(40, 10)

	// chunk 1: question covered by text below it
	chunk1 := "\x1b[2J\x1b[1;1HOverwrite file? [Y/n]\x1b[2;1HWorking on it now"
	if err := processor.Consume([]byte(chunk1)); err != nil {
		t.Fatal(err)
	}
	if processor.Pending() != nil {
		t.Fatalf("covered question was prematurely detected: %v", *raised)
	}

	// chunk 2: escape-only write erases the covering line (\x1b[2;1H\x1b[J)
	chunk2 := "\x1b[2;1H\x1b[J"
	if err := processor.Consume([]byte(chunk2)); err != nil {
		t.Fatal(err)
	}

	if processor.Pending() == nil {
		t.Fatalf("uncovered question was not detected after escape-only write; output=%q", processor.Output())
	}
	if len(*raised) != 1 {
		t.Fatalf("expected 1 event, got %d: %v", len(*raised), *raised)
	}
}

// Issue #32: Detection: an orphaned code fence silences all detection, generic and vendor
func TestOrphanedCodeFenceDoesNotSilenceDetection(t *testing.T) {
	t.Run("erased opening fence line", func(t *testing.T) {
		processor, raised := repaintProcessor(t)
		processor.Resize(60, 8)

		// 1) Output code block and Done
		chunk1 := "\x1b[2J\x1b[1;1H```sh\r\nrm -rf build\r\n```\r\nDone."
		if err := processor.Consume([]byte(chunk1)); err != nil {
			t.Fatal(err)
		}

		// 2) Agent erases the opening line at row 1
		chunk2 := "\x1b[1;1H\x1b[2Kbuilding"
		if err := processor.Consume([]byte(chunk2)); err != nil {
			t.Fatal(err)
		}

		// 3) Agent asks question at row 5
		chunk3 := "\x1b[5;1HOverwrite file? [y/n]"
		if err := processor.Consume([]byte(chunk3)); err != nil {
			t.Fatal(err)
		}

		if processor.Pending() == nil {
			t.Fatalf("question after orphaned closing fence was silenced: %v; output=%q", *raised, processor.Output())
		}
	})

	t.Run("code fence evicted from scrollback", func(t *testing.T) {
		processor, raised := repaintProcessor(t)
		processor.Resize(60, 10)

		// Create a scenario where closing fence is at the top of scrollback
		// with many lines of output afterwards
		var builder strings.Builder
		builder.WriteString("\x1b[2J\x1b[1;1H```\r\n")
		for i := 0; i < 30; i++ {
			builder.WriteString("log output line\r\n")
		}
		builder.WriteString("Overwrite file? [y/n]")

		if err := processor.Consume([]byte(builder.String())); err != nil {
			t.Fatal(err)
		}

		if processor.Pending() == nil {
			t.Fatalf("question was silenced by preceding single fence: %v", *raised)
		}
	})
}
