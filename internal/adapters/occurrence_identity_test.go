package adapters

import (
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var legacyShortMatch = []Pattern{{
	Name:        "legacy-short-match",
	Description: "legacy confirmation",
	Expression:  `\[Y/n\]`,
}}

var everyAdapter = []struct {
	name string
	new  func([]Pattern) (Adapter, error)
}{
	{name: GenericID, new: func(patterns []Pattern) (Adapter, error) { return NewGenericRegexAdapter(patterns) }},
	{name: ClaudeID, new: func(patterns []Pattern) (Adapter, error) { return NewClaudeAdapter(patterns) }},
	{name: CodexID, new: func(patterns []Pattern) (Adapter, error) { return NewCodexAdapter(patterns) }},
	{name: AiderID, new: func(patterns []Pattern) (Adapter, error) { return NewAiderAdapter(patterns) }},
	{name: GooseID, new: func(patterns []Pattern) (Adapter, error) { return NewGooseAdapter(patterns) }},
	{name: OpenInterpreterID, new: func(patterns []Pattern) (Adapter, error) { return NewOpenInterpreterAdapter(patterns) }},
}

// oneProcessPrompt runs one process of an agent that asks one question, and
// returns what it emitted and what it holds as pending.
func oneProcessPrompt(t *testing.T, newAdapter func([]Pattern) (Adapter, error)) (Event, *Event) {
	t.Helper()
	adapter, err := newAdapter(legacyShortMatch)
	if err != nil {
		t.Fatal(err)
	}
	var emitted []Event
	processor, err := NewProcessor(
		adapter,
		NewDetectionState("session-restarted", "agent-restarted", adapter.ID()),
		4096,
		Hooks{OnEvent: func(event Event) { emitted = append(emitted, event) }},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.Consume([]byte("Overwrite the file? [Y/n] ")); err != nil {
		t.Fatal(err)
	}
	processor.WaitSemanticEvents()
	if len(emitted) == 0 {
		t.Fatalf("%s: the prompt was not detected", adapter.ID())
	}
	return emitted[0], processor.Pending()
}

// TestEveryAdapterGivesEachProcessItsOwnPromptIDs: an occurrence ID was its
// signature and a sequence that starts again with each process, and a
// signature can be as little as the "[Y/n]" a pattern captured. A restarted
// agent's first prompt then had its predecessor's ID: a decision on the old
// prompt passed the ID check and was delivered to the new one. Five adapters
// rebuild the ID after the generic detection, so each is checked, and so is
// that what an adapter emits is what it holds as pending: those two are built
// separately in several adapters, and a mismatch makes every decision fail.
func TestEveryAdapterGivesEachProcessItsOwnPromptIDs(t *testing.T) {
	for _, adapter := range everyAdapter {
		t.Run(adapter.name, func(t *testing.T) {
			first, firstPending := oneProcessPrompt(t, adapter.new)
			second, secondPending := oneProcessPrompt(t, adapter.new)
			if first.Signature != second.Signature || first.Sequence != second.Sequence {
				t.Fatalf("the two processes did not see the same question: %#v and %#v", first, second)
			}
			if first.ID == second.ID {
				t.Fatalf("two processes gave their first prompt the same ID %q", first.ID)
			}
			if firstPending == nil || firstPending.ID != first.ID || secondPending == nil || secondPending.ID != second.ID {
				t.Fatalf("emitted and pending IDs differ: %q / %v and %q / %v", first.ID, firstPending, second.ID, secondPending)
			}
		})
	}
}

// TestAProcessRefusesADecisionOnItsPredecessorsPrompt: two processes of one
// agent asked different questions that the pattern reduced to the same
// signature, and the replacement delivered the answer meant for the first.
func TestAProcessRefusesADecisionOnItsPredecessorsPrompt(t *testing.T) {
	previous := newGenericTestProcessor(t, 4096, Hooks{})
	replacement := newGenericTestProcessor(t, 4096, Hooks{})
	old, _, err := previous.ReconcileSnapshot([]byte("Run 'npm test'? [y/n]"))
	if err != nil || old == nil {
		t.Fatalf("previous prompt = %#v, %v", old, err)
	}
	current, _, err := replacement.ReconcileSnapshot([]byte("Run 'rm -rf build' as root? [y/n]"))
	if err != nil || current == nil {
		t.Fatalf("replacement prompt = %#v, %v", current, err)
	}
	if old.Signature != current.Signature {
		t.Skipf("the patterns now tell these questions apart (%q, %q); the ID no longer carries the risk", old.Signature, current.Signature)
	}
	delivered := false
	err = replacement.Resolve(old.ID, func() error {
		delivered = true
		return nil
	})
	if delivered || !errors.Is(err, ErrEventMismatch) {
		t.Fatalf("a decision on the previous process's prompt: delivered=%v, err=%v; want refused with %v", delivered, err, ErrEventMismatch)
	}
}

// TestEveryOccurrenceIDCarriesTheProcessInstance keeps a new adapter from
// building an ID without the instance, which the compiler cannot catch: an
// unsalted ID collides with the previous process's again.
func TestEveryOccurrenceIDCarriesTheProcessInstance(t *testing.T) {
	sources, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	bare := regexp.MustCompile(`(^|[^.\w])occurrenceID\(`)
	for _, source := range sources {
		if strings.HasSuffix(source, "_test.go") {
			continue
		}
		data, err := os.ReadFile(source)
		if err != nil {
			t.Fatal(err)
		}
		for number, line := range strings.Split(string(data), "\n") {
			if !bare.MatchString(line) || strings.Contains(line, "func occurrenceID(") {
				continue
			}
			// The two places allowed to use the unsalted form: the salted
			// helper itself, and the provisional exit ID that
			// MarkProcessExitEvent replaces.
			if (source == "adapter.go" && strings.Contains(line, "return occurrenceID(")) ||
				(source == "event.go" && strings.Contains(line, "ID:        occurrenceID(signature, sequence)")) {
				continue
			}
			t.Errorf("%s:%d builds an occurrence ID without the process instance; use DetectionState.occurrenceIDFor: %s", source, number+1, strings.TrimSpace(line))
		}
	}
}
