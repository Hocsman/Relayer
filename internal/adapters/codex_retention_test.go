package adapters

import (
	"testing"
)

// A second prompt arriving while the first is pending must survive.
//
// The generic adapter normalizes the chunk into the detection window BEFORE it
// returns early on a pending occurrence, which is what lets
// rescanRetainedWindow recover the second prompt once the first is answered.
// The Codex adapter returned on `state.pending != nil` before any
// appendDetectionText call, so nothing was retained: the second prompt was
// never in the window, the rescan had nothing to find, and the request vanished
// with no trace.
//
// This is the fail-open direction. The agent is left waiting on a question no
// operator was ever shown, and the interface reports a calm session.
func TestCodexRetainsOutputWhileAPromptIsPending(t *testing.T) {
	adapter, err := NewCodexAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	state := NewDetectionState("session", "agent", CodexID)

	// A generic prompt blocks the session first.
	if _, err := adapter.Detect(state, []byte("Overwrite file? [Y/n]")); err != nil {
		t.Fatal(err)
	}
	if state.pending == nil {
		t.Fatal("the first prompt was not detected")
	}
	before := state.detectionText

	// The agent keeps writing while a human is deciding.
	if _, err := adapter.Detect(state, []byte("\nProceeding.\nDelete branch? [y/n]")); err != nil {
		t.Fatal(err)
	}

	if state.detectionText == before {
		t.Fatal("output produced while a prompt was pending was discarded: the " +
			"window is unchanged, so nothing can recover the second question")
	}
	if got := state.detectionText; !contains(got, "Delete branch?") {
		t.Fatalf("the second question is not in the retained window: %q", got)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}
