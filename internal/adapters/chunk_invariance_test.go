package adapters

import (
	"testing"
)

// feedInChunks replays the same bytes at a given chunk size and reports the
// matches that became actionable.
func feedInChunks(t *testing.T, output string, size int) []string {
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

	raw := []byte(output)
	if size <= 0 || size >= len(raw) {
		if err := processor.Consume(raw); err != nil {
			t.Fatal(err)
		}
		return matches
	}
	for start := 0; start < len(raw); start += size {
		end := start + size
		if end > len(raw) {
			end = len(raw)
		}
		if err := processor.Consume(raw[start:end]); err != nil {
			t.Fatal(err)
		}
		// A prompt that blocks must not stop the replay from delivering the
		// rest of the output, which is what a real session does.
		if pending := processor.Pending(); pending != nil {
			if err := processor.Acknowledge(pending.ID); err != nil {
				t.Fatal(err)
			}
		}
	}
	return matches
}

// An agent that draws its prompt inside an ASCII frame writes
// "| Overwrite file? [Y/n]        |". That line has the same "| " prefix as a
// markdown table row, so it was suppressed as quoted documentation — at every
// chunk size, deterministically, with no way for an operator to notice.
//
// A table row separates cells and therefore carries more pipes than the two a
// frame uses to close its sides.
func TestAsciiFramedPromptIsNotMistakenForATableRow(t *testing.T) {
	framed := "" +
		"+------------------------------+\n" +
		"| Overwrite file? [Y/n]        |\n"
	if matches := feedInChunks(t, framed, 0); len(matches) != 1 {
		t.Fatalf("framed prompt was suppressed as a table row: %v", matches)
	}

	// A genuine table stays quoted documentation.
	table := "| option | meaning |\n| --- | --- |\n| Overwrite file? [Y/n] | asks first |\n"
	if matches := feedInChunks(t, table, 0); len(matches) != 0 {
		t.Fatalf("a markdown table row became actionable: %v", matches)
	}
}

// Detection currently depends on how the operating system split the read: a
// match must reach the active line, so a prompt followed in the same write by
// its own frame, option list or footer is missed. Splitting the identical bytes
// finds it.
//
// This test pins the cases that already hold, so the boundary of the gap is
// explicit rather than assumed. docs/adapters.md records the rest.
func TestDetectionIsInvariantForAPromptOnTheActiveLine(t *testing.T) {
	for _, test := range []struct {
		name   string
		output string
	}{
		{name: "plain prompt at the end", output: "Building the project\nOverwrite file? [Y/n]"},
		{name: "prompt after a carriage return rewrite", output: "working 10%\rworking 90%\rOverwrite file? [Y/n]"},
		{name: "prompt split across a multi-byte rune", output: "Fichier déjà présent\nOverwrite file? [Y/n]"},
	} {
		t.Run(test.name, func(t *testing.T) {
			whole := feedInChunks(t, test.output, 0)
			if len(whole) == 0 {
				t.Fatal("the prompt was missed when the output arrived in one write")
			}
			for _, size := range []int{1, 7, 32} {
				if split := feedInChunks(t, test.output, size); len(split) != len(whole) {
					t.Fatalf("chunk size %d produced %d event(s), one write produced %d",
						size, len(split), len(whole))
				}
			}
		})
	}
}
