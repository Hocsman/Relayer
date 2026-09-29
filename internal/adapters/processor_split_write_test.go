package adapters

import (
	"strings"
	"testing"
)

// A PTY read can end anywhere: between the "\r" and the "\n" of a line break,
// or inside a UTF-8 character. The detection window reads each as if the write
// had not been cut. Read alone, the return erased the line it ended, and the
// bytes of a cut character were replaced.
func TestADetectionWindowReadsWritesCutAnywhere(t *testing.T) {
	const drawn = "◆  Question?\r\n│  ● Allow\r\n└  \r\n"
	for _, piece := range []int{1, 2, 3} {
		processor, err := NewProcessor(newAdapterForTest(t, GenericID), NewDetectionState("s", "a", GenericID), 64*1024, Hooks{})
		if err != nil {
			t.Fatal(err)
		}
		for start := 0; start < len(drawn); start += piece {
			if err := processor.Consume([]byte(drawn[start:min(start+piece, len(drawn))])); err != nil {
				t.Fatal(err)
			}
		}
		if got, want := processor.state.detectionText, strings.ReplaceAll(drawn, "\r\n", "\n"); got != want {
			t.Errorf("in pieces of %d: window = %q, want %q", piece, got, want)
		}
	}
}

// A lone return is still applied, once the next write comes.
func TestALoneCarriageReturnIsAppliedByTheNextWrite(t *testing.T) {
	processor, err := NewProcessor(newAdapterForTest(t, GenericID), NewDetectionState("s", "a", GenericID), 64*1024, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	for _, write := range []string{"working 10%\r", "working 90%\r", "done"} {
		if err := processor.Consume([]byte(write)); err != nil {
			t.Fatal(err)
		}
	}
	if got := processor.state.detectionText; got != "done" {
		t.Fatalf("window = %q, want %q", got, "done")
	}
}
