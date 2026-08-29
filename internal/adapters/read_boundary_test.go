package adapters

import (
	"encoding/json"
	"os"
	"testing"
)

// detectOnce replays output through a fresh processor and reports the matches
// that became actionable. Unlike feedInChunks it does not acknowledge, because
// these cases are about whether anything is detected at all.
func detectOnce(t *testing.T, writes ...string) []string {
	t.Helper()
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	matches := []string{}
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session", "agent", GenericID),
		64*1024,
		Hooks{OnEvent: func(event Event) { matches = append(matches, event.Match) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, write := range writes {
		if err := processor.Consume([]byte(write)); err != nil {
			t.Fatal(err)
		}
	}
	return matches
}

// The gap this file exists to close.
//
// Detection keeps a match only when it overlaps the last non-empty line of the
// accumulated window, so whether a prompt is seen depends on where the
// operating system happened to end a read. An agent that writes its question
// and the furniture beneath it — a frame, an option list, a footer — in one
// write is not supervised at all. The identical bytes split across two writes
// are.
//
// It fails silently and in the unsafe direction: nothing is reported, so the
// agent proceeds unsupervised while the operator sees a calm screen.
func TestPromptIsFoundWhateverTheReadBoundary(t *testing.T) {
	for _, test := range []struct {
		name   string
		whole  string
		split  []string
		reason string
	}{
		{
			name:   "question above its own option list",
			whole:  "Overwrite file? [Y/n]\n  [y] yes\n  [n] no\n",
			split:  []string{"Overwrite file? [Y/n]", "\n  [y] yes\n  [n] no\n"},
			reason: "the option list is the trailing furniture of the question",
		},
		{
			name:   "question inside an ASCII frame",
			whole:  "+----------------------+\n| Overwrite file? [Y/n]|\n+----------------------+\n",
			split:  []string{"+----------------------+\n| Overwrite file? [Y/n]|", "\n+----------------------+\n"},
			reason: "the closing rule of the frame follows the question",
		},
		{
			name:   "question above a footer hint",
			whole:  "Do you want to continue\nPress enter to confirm or esc to cancel\n",
			split:  []string{"Do you want to continue", "\nPress enter to confirm or esc to cancel\n"},
			reason: "the CLI prints its key hint under the question",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			split := detectOnce(t, test.split...)
			if len(split) == 0 {
				t.Fatalf("the split replay found nothing, so this case does not "+
					"isolate the read boundary: %s", test.reason)
			}
			if whole := detectOnce(t, test.whole); len(whole) != len(split) {
				t.Fatalf("one write found %d prompt(s), the same bytes split found %d: %s",
					len(whole), len(split), test.reason)
			}
		})
	}
}

// The complement, and the reason this is delicate: widening what counts as
// actionable must not make documentation actionable. A prompt quoted inside a
// fenced block is an example, not a question, and an agent that explains what it
// is about to do must not trigger a supervision event by describing a prompt.
func TestQuotedAndFencedPromptsStayInert(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
	}{
		{
			name:   "prompt inside a fenced block",
			output: "Here is what you will see:\n```\nOverwrite file? [Y/n]\n```\nProceeding.\n",
		},
		{
			name:   "prompt quoted in prose",
			output: "I will answer `Overwrite file? [Y/n]` for you.\nDone.\n",
		},
		{
			name:   "prompt in a markdown table",
			output: "| option | meaning |\n| --- | --- |\n| Overwrite file? [Y/n] | asks first |\n",
		},
		{
			name:   "prompt on a quoted line",
			output: "> Overwrite file? [Y/n]\nThat was the question I saw.\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			if matches := detectOnce(t, test.output); len(matches) != 0 {
				t.Fatalf("documentation became actionable: %v", matches)
			}
		})
	}
}

// A prompt already answered must not come back when the region widens. This is
// the failure that killed the previous attempt: the earlier occurrence was still
// in the 16 KiB window, and widening what counts as actionable resurrected it.
func TestAnsweredPromptIsNotResurrectedByLaterOutput(t *testing.T) {
	adapter, err := NewGenericRegexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	emitted := []Event{}
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session", "agent", GenericID),
		64*1024,
		Hooks{OnEvent: func(event Event) { emitted = append(emitted, event) }},
	)
	if err != nil {
		t.Fatal(err)
	}

	if err := processor.Consume([]byte("Overwrite file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	first := processor.Pending()
	if first == nil {
		t.Fatal("the first prompt was not detected")
	}
	if err := processor.Resolve(first.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}

	// Ordinary work continues. The answered question is still in the window.
	if err := processor.Consume([]byte("\ny\nWriting the file\nDone.\n")); err != nil {
		t.Fatal(err)
	}
	if pending := processor.Pending(); pending != nil {
		t.Fatalf("the answered prompt came back: %#v", pending)
	}
	for _, event := range emitted {
		if event.ID != first.ID && event.Match == first.Match {
			t.Fatalf("the same question was emitted twice under different IDs: %#v", event)
		}
	}
}

// The shape that matters, taken from the real captured Codex screen rather than
// invented: a question that wraps across three lines, a blank, a numbered
// choice list, a blank, and a key hint.
//
// Before the region was widened this was detected in NO chunking at all — not
// whole, not as the chunks the capture actually recorded. A generic-adapter
// operator supervising any CLI that paints a prompt this way was supervising
// nothing, which is most modern agent CLIs.
func TestRealCapturedScreenIsDetectedInEveryChunking(t *testing.T) {
	raw, err := os.ReadFile("testdata/codex/directory_trust.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Normalized string   `json:"normalized"`
		Chunks     []string `json:"chunks"`
	}
	if err := json.Unmarshal(raw, &fixture); err != nil {
		t.Fatal(err)
	}

	// The pattern an operator would write for this CLI, not a vendor fixture:
	// the point is that the generic adapter finds it.
	patterns := append(DefaultPatterns(), Pattern{
		Name:        "directory-trust",
		Description: "directory trust",
		Expression:  `(?i)do you trust the contents`,
	})

	count := func(writes ...string) int {
		adapter, err := NewGenericRegexAdapter(patterns)
		if err != nil {
			t.Fatal(err)
		}
		found := 0
		processor, err := NewProcessor(adapter, NewDetectionState("s", "a", GenericID), 64*1024,
			Hooks{OnEvent: func(Event) { found++ }})
		if err != nil {
			t.Fatal(err)
		}
		for _, write := range writes {
			if err := processor.Consume([]byte(write)); err != nil {
				t.Fatal(err)
			}
		}
		return found
	}

	if whole := count(fixture.Normalized); whole != 1 {
		t.Fatalf("the whole screen in one write produced %d event(s), want 1", whole)
	}
	if chunked := count(fixture.Chunks...); chunked != 1 {
		t.Fatalf("the captured chunks produced %d event(s), want 1", chunked)
	}
	// Byte-at-a-time is the harshest split there is.
	single := make([]string, 0, len(fixture.Normalized))
	for _, character := range fixture.Normalized {
		single = append(single, string(character))
	}
	if byRune := count(single...); byRune != 1 {
		t.Fatalf("a rune-at-a-time replay produced %d event(s), want 1", byRune)
	}
}
