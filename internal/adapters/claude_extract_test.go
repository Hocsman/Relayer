package adapters

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// runesThatGrowWhenLowered are runes whose lower case is not as long, in bytes,
// as they are. U+023A and U+023E are two bytes and their lower case three:
// every one in front of a header moved an index taken from strings.ToLower of
// the text one byte past the same place in the text itself, and the panic came
// from there. U+1E9E is three bytes and its lower case two, which moves the
// index the other way and silently cut the command short.
var runesThatGrowWhenLowered = []string{"Ⱥ", "Ⱦ", "ẞ"}

func TestIndexFoldASCIIReturnsAnOffsetInTheOriginalString(t *testing.T) {
	cases := []struct {
		name   string
		s      string
		needle string
		want   int
	}{
		{"exact", "run shell command", "run shell command", 0},
		{"mixed case", "xxRun Shell COMMAND", "run shell command", 2},
		{"absent", "run shell commands", "run shell command x", -1},
		{"shorter than the needle", "run", "run shell command", -1},
		{"empty needle", "anything", "", 0},
		{"empty haystack", "", "run", -1},
		{"a rune that grows when lowered before it", "ȺRun shell command", "run shell command", 2},
		{"many of them before it", strings.Repeat("Ⱥ", 300) + "RUN SHELL COMMAND", "run shell command", 600},
		{"a non-ASCII letter that is not an ASCII letter in disguise", "rún shell command", "run shell command", -1},
	}
	for _, c := range cases {
		got := indexFoldASCII(c.s, c.needle)
		if got != c.want {
			t.Fatalf("%s: indexFoldASCII(%q, %q) = %d, want %d", c.name, c.s, c.needle, got, c.want)
		}
		if got >= 0 && !strings.EqualFold(c.s[got:got+len(c.needle)], c.needle) {
			t.Fatalf("%s: the text at offset %d is %q, not the needle", c.name, got, c.s[got:got+len(c.needle)])
		}
	}
}

func claudeBashPrompt(beforeHeader, command string) string {
	return "Bash command\n" + beforeHeader + "\n" +
		"Run shell command ╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
		command + "\n" +
		"╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌╌\n" +
		"Do you want to proceed?\n" +
		"❯ 1. Yes\n  2. Yes, and always allow access to folder\n  3. No\n\n" +
		"Esc to cancel · Tab to amend"
}

// Before the fix the extraction panicked once the runes before the header
// outgrew the text after it (a few hundred bytes), and, short of that, sliced
// the command at the wrong place: the command a rule matches on was garbage.
func TestExtractClaudeBashCommandIgnoresRunesThatGrowWhenLowered(t *testing.T) {
	for _, r := range runesThatGrowWhenLowered {
		for count := 0; count <= 700; count++ {
			before := strings.Repeat(r, count)
			if got := extractClaudeBashCommand(claudeBashPrompt(before, "rm -rf build")); got != "rm -rf build" {
				t.Fatalf("%d x U+%04X before the header: command = %q", count, []rune(r)[0], got)
			}
			command := "echo " + before
			want := strings.TrimSpace(command)
			if got := extractClaudeBashCommand(claudeBashPrompt("", command)); got != want {
				t.Fatalf("%d x U+%04X in the command: command = %q, want %q", count, []rune(r)[0], got, want)
			}
		}
	}
	// No header at all: the older layout, where the command precedes it.
	if got := extractClaudeBashCommand(strings.Repeat("Ⱥ", 500) + "\nls -la"); got == "" {
		t.Fatal("a match without the header lost its text")
	}
}

// The same text, reaching the adapter the way the agent's output does: through
// the Processor that reads it, which is where the panic ended the process.
func TestClaudeBashPromptWithRunesThatGrowWhenLoweredReachesTheOperator(t *testing.T) {
	for _, r := range runesThatGrowWhenLowered {
		var events []Event
		state := NewDetectionState("session-grow", "agent-claude", ClaudeID)
		processor, err := NewProcessor(newClaudeAdapterForTest(t, nil), state, 1<<16, Hooks{
			OnEvent: func(event Event) { events = append(events, event) },
		})
		if err != nil {
			t.Fatal(err)
		}
		prompt := claudeBashPrompt(strings.Repeat(r, 600), "rm -rf build")
		if err := processor.Run(context.Background(), strings.NewReader(prompt)); err != nil {
			t.Fatalf("U+%04X: Run = %v", []rune(r)[0], err)
		}
		if len(events) != 1 || events[0].Type != EventPermission || events[0].Command != "rm -rf build" {
			t.Fatalf("U+%04X: events = %#v", []rune(r)[0], events)
		}
	}
}

type panickingAdapter struct{ value any }

func (panickingAdapter) ID() string { return GenericID }

func (a panickingAdapter) Detect(*DetectionState, []byte) ([]Event, error) {
	if a.value != nil {
		panic(a.value)
	}
	var window []byte
	index := 5
	_ = window[index]
	return nil, nil
}

func (panickingAdapter) EncodeDecision(Event, Decision, string) ([]byte, error) { return nil, nil }

func TestRunReturnsAnAdapterPanicAsAnErrorAndReleasesTheLock(t *testing.T) {
	cases := []struct {
		name        string
		value       any
		wantInError []string
		notInError  []string
	}{
		{
			name:        "a runtime error keeps its message and says where it came from",
			wantInError: []string{"processing the terminal output failed", "index out of range", "panickingAdapter.Detect"},
		},
		{
			name:        "any other value is named by its type, never printed",
			value:       "api_key=sk-live-0123456789 from the agent's output",
			wantInError: []string{"processing the terminal output failed", "panic of type string", "panickingAdapter.Detect"},
			notInError:  []string{"sk-live", "api_key"},
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := NewDetectionState("session-panic", "agent", GenericID)
			processor, err := NewProcessor(panickingAdapter{value: c.value}, state, 4096, Hooks{})
			if err != nil {
				t.Fatal(err)
			}
			err = processor.Run(context.Background(), strings.NewReader("some output\n"))
			if err == nil {
				t.Fatal("Run returned nil after the adapter panicked")
			}
			for _, want := range c.wantInError {
				if !strings.Contains(err.Error(), want) {
					t.Fatalf("error %q does not contain %q", err, want)
				}
			}
			for _, unwanted := range c.notInError {
				if strings.Contains(err.Error(), unwanted) {
					t.Fatalf("error %q contains %q", err, unwanted)
				}
			}

			// The lock was held when the adapter panicked. Every reader of the
			// session (state, output, a decision) would block on it for good.
			done := make(chan struct{})
			go func() {
				defer close(done)
				_ = processor.Output()
				_ = processor.Pending()
				_ = processor.Revision()
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("the processor lock is still held after the panic")
			}
		})
	}
}

// The cursor-forward header is how a terminal that draws its spaces with
// "ESC [ 1 C" would spell it. Its length is not the length of the plain
// header, and an end offset taken from the plain one cut into the command.
func TestExtractClaudeBashCommandCursorForwardHeader(t *testing.T) {
	for _, header := range []string{"Run\x1b[1Cshell\x1b[1Ccommand", "RUN\x1b[1cSHELL\x1b[1cCOMMAND"} {
		prompt := "Bash command\n" + header + "\nrm -rf build\nDo you want to proceed?\n1. Yes\n2. No"
		if got := extractClaudeBashCommand(prompt); got != "rm -rf build" {
			t.Fatalf("header %q: command = %q", header, got)
		}
	}
}

type erroringAdapter struct{}

func (erroringAdapter) ID() string { return GenericID }

func (erroringAdapter) Detect(*DetectionState, []byte) ([]Event, error) {
	return nil, errors.New("detection failed")
}

func (erroringAdapter) EncodeDecision(Event, Decision, string) ([]byte, error) { return nil, nil }

// A panic ends the reader the way an ordinary processing error does; this is
// the ordinary one, which the panic test compares itself to.
func TestRunReturnsADetectionError(t *testing.T) {
	state := NewDetectionState("session-error", "agent", GenericID)
	processor, err := NewProcessor(erroringAdapter{}, state, 4096, Hooks{})
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.Run(context.Background(), strings.NewReader("some output\n")); err == nil || err.Error() != "detection failed" {
		t.Fatalf("Run = %v, want the adapter's error unchanged", err)
	}
}

// A hook that panics after an event was reserved must not leave it owed:
// WaitSemanticEvents holds back process_exit and the closing of the backend.
func TestRunPanicInAHookDoesNotStrandTheReservedEvents(t *testing.T) {
	cases := []struct {
		name  string
		hooks Hooks
	}{
		{"raw chunk", Hooks{OnRawChunk: func(time.Time, []byte) { panic("raw chunk hook") }}},
		{"output", Hooks{OnOutput: func() { panic("output hook") }}},
		{"event", Hooks{OnEvent: func(Event) { panic("event hook") }}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			state := NewDetectionState("session-hook", "agent-claude", ClaudeID)
			processor, err := NewProcessor(newClaudeAdapterForTest(t, nil), state, 1<<16, c.hooks)
			if err != nil {
				t.Fatal(err)
			}
			err = processor.Run(context.Background(), strings.NewReader(claudeBashPrompt("", "ls")))
			if err == nil || !strings.Contains(err.Error(), "processing the terminal output failed") {
				t.Fatalf("Run = %v, want the recovered panic", err)
			}
			done := make(chan struct{})
			go func() {
				defer close(done)
				processor.NewProcessExitEvent(nil, true)
			}()
			select {
			case <-done:
			case <-time.After(5 * time.Second):
				t.Fatal("process_exit is held back by an event that no hook will ever deliver")
			}
		})
	}
}

type candidateThenPanicAdapter struct{}

func (candidateThenPanicAdapter) ID() string { return GenericID }

// Stores the occurrence the way a vendor adapter does before it enriches it,
// then panics: the occurrence exists, and no hook was told about it.
func (candidateThenPanicAdapter) Detect(state *DetectionState, _ []byte) ([]Event, error) {
	state.replacePending(Event{
		Adapter:   GenericID,
		Type:      EventConfirmation,
		Risk:      RiskUnknown,
		Signature: "half-built",
		Metadata:  map[string]string{"pattern": "half-built"},
	})
	panic("after the candidate was stored")
}

func (candidateThenPanicAdapter) EncodeDecision(Event, Decision, string) ([]byte, error) {
	return []byte("y\r"), nil
}

func TestRunPanicDropsTheOccurrenceNobodyWasToldAbout(t *testing.T) {
	state := NewDetectionState("session-half", "agent", GenericID)
	processor, err := NewProcessor(candidateThenPanicAdapter{}, state, 4096, Hooks{
		OnEvent: func(Event) { t.Error("a hook was told about an occurrence of a read that panicked") },
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := processor.Run(context.Background(), strings.NewReader("some output\n")); err == nil {
		t.Fatal("Run returned nil after the adapter panicked")
	}
	if pending := processor.Pending(); pending != nil {
		t.Fatalf("a half-built occurrence is still pending after the reader died: %#v", pending)
	}
	if processor.IsBlocked() {
		t.Fatal("the processor still reports a blocking prompt after the reader died")
	}
}
