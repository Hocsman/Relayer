package adapters

import (
	"encoding/json"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestExpandCursorForward(t *testing.T) {
	for _, test := range []struct {
		name  string
		input string
		want  string
	}{
		{name: "no escape at all", input: "plain text", want: "plain text"},
		{name: "implicit single column", input: "a\x1b[Cb", want: "a b"},
		{name: "explicit single column", input: "a\x1b[1Cb", want: "a b"},
		{name: "several columns", input: "a\x1b[4Cb", want: "a    b"},
		{name: "zero columns collapses", input: "a\x1b[0Cb", want: "ab"},
		{name: "repeated", input: "a\x1b[1Cb\x1b[1Cc", want: "a b c"},
		{name: "leaves other escapes alone", input: "a\x1b[31mred\x1b[0m", want: "a\x1b[31mred\x1b[0m"},
		{name: "leaves vertical movement alone", input: "a\x1b[2Ab", want: "a\x1b[2Ab"},
		{name: "leaves absolute positioning alone", input: "a\x1b[10Gb", want: "a\x1b[10Gb"},
	} {
		t.Run(test.name, func(t *testing.T) {
			if got := expandCursorForward(test.input); got != test.want {
				t.Fatalf("expandCursorForward(%q) = %q, want %q", test.input, got, test.want)
			}
		})
	}
}

// Terminal output is untrusted and the column count is caller-controlled, so a
// single short sequence must not be able to inflate the detection window.
func TestExpandCursorForwardBoundsOneSubstitution(t *testing.T) {
	got := expandCursorForward("\x1b[999999999C")
	if len(got) != maxCursorForwardExpansion {
		t.Fatalf("expansion produced %d bytes, want the %d-byte bound", len(got), maxCursorForwardExpansion)
	}
	if strings.TrimSpace(got) != "" {
		t.Fatalf("expansion produced something other than spaces: %q", got)
	}
	// An unparsable parameter is left untouched rather than guessed at.
	if got := expandCursorForward("\x1b[99999999999999999999C"); !strings.Contains(got, "\x1b[") {
		t.Fatalf("overflowing parameter was substituted: %q", got)
	}
}

// Some agents lay a prompt out by moving the cursor instead of writing spaces.
// Stripping those escapes without substitution left the detector matching
// against text with no spaces in it, so any configured pattern containing a
// space could never fire — silently, on the documented compatibility path.
func TestConsumeRestoresSpacingFromRecordedVendorOutput(t *testing.T) {
	payload, err := os.ReadFile("testdata/claude/stream_cases.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []struct {
		Name   string   `json:"name"`
		Chunks []string `json:"chunks"`
	}
	if err := json.Unmarshal(payload, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) == 0 {
		t.Fatal("no recorded cases")
	}

	spaced := regexp.MustCompile(`(?i)do you want to use this`)
	matched := false
	for _, recorded := range cases {
		joined := strings.Join(recorded.Chunks, "")
		if strings.Count(joined, " ") != 0 {
			t.Fatalf("%s: the recording no longer exercises cursor-forward spacing", recorded.Name)
		}

		state := NewDetectionState("s", "a", GenericID)
		adapter, err := NewGenericRegexAdapter(DefaultPatterns())
		if err != nil {
			t.Fatal(err)
		}
		processor, err := NewProcessor(adapter, state, 256*1024, Hooks{})
		if err != nil {
			t.Fatal(err)
		}
		for _, chunk := range recorded.Chunks {
			if err := processor.Consume([]byte(chunk)); err != nil {
				t.Fatal(err)
			}
		}
		if strings.Count(state.detectionText, " ") == 0 {
			t.Fatalf("%s: normalized text still has no spaces: %q", recorded.Name, state.detectionText)
		}
		if spaced.MatchString(state.detectionText) {
			matched = true
		}
	}
	if !matched {
		t.Fatal("no recorded prompt was matched by a pattern written with ordinary spaces")
	}
}
