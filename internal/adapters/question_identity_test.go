package adapters

import "testing"

// A question is identified by the line it was asked on, not by the fragment a
// pattern captured.
//
// The shipped `confirmation` pattern captures the literal "[y/n]", so every
// yes/no question in a session used to produce the same signature and each one
// after the first was swallowed. The operator was never asked.
func TestASecondUnrelatedQuestionIsStillAsked(t *testing.T) {
	for _, testCase := range []struct {
		name   string
		first  string
		second string
	}{
		{
			// Same captured fragment, same signature, two different questions.
			name:   "the same fragment in two unrelated questions",
			first:  "Run 'npm test'? [y/n]",
			second: "Run 'rm -rf /' as root? [y/n]",
		},
		{
			// The general pattern's fragment is contained in the specific
			// pattern's: the more dangerous question was the one silenced.
			name:   "one captured fragment contained in the other",
			first:  "Overwrite file? [y/n]",
			second: "Delete the cache? [y/n]",
		},
		{
			// Identical casing. The shipped guard only ever passed its test
			// because that test wrote one question "[Y/n]" and the other
			// "[y/n]", and the comparison was case sensitive.
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

// What comparing lines has to keep doing.
//
// One line can match several patterns — the default set matches "Overwrite
// file? [Y/n]" as an overwrite AND as a yes/no confirmation. Suppressing only
// the answered signature let the same line come straight back under the other
// pattern's name, on the very next repaint.
func TestTheSameLineDoesNotComeBackUnderAnotherPattern(t *testing.T) {
	processor, raised := repaintProcessor(t)
	processor.Resize(70, 12)

	frame := "\x1b[2J\x1b[1;1HOverwrite file? [Y/n]\x1b[2;1H  [y] yes"
	if err := processor.Consume([]byte(frame)); err != nil {
		t.Fatal(err)
	}
	pending := processor.Pending()
	if pending == nil {
		t.Fatal("the question was not detected in the first place")
	}
	if err := processor.Resolve(pending.ID, func() error { return nil }); err != nil {
		t.Fatal(err)
	}
	// The agent has not redrawn without it yet, so the answered question is
	// still painted. It must not be asked again, under any pattern.
	for repaint := 0; repaint < 3; repaint++ {
		if err := processor.Consume([]byte(frame)); err != nil {
			t.Fatal(err)
		}
		if got := processor.Pending(); got != nil {
			t.Fatalf("repaint %d asked the answered question again as %q: %v",
				repaint+1, got.Metadata["pattern"], *raised)
		}
	}
}
