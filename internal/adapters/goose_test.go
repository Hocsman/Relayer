package adapters

import (
	"strings"
	"testing"
)

// The menus as Goose 1.52.0 draws them, with the escapes removed, in a
// terminal that takes Unicode and in one that does not.
const (
	gooseToolMenu = "  Tool approval request\n" +
		"    {\n      \"tool_name\": \"shell\",\n    }\n\n" +
		"◆  Goose would like to call the above tool, do you allow?\n" +
		"│  ● Allow (Allow the tool call once)\n" +
		"│  ○ Always Allow \n" +
		"│  ○ Deny \n" +
		"│  ○ Cancel \n" +
		"└  \n"
	gooseToolMenuASCII = "*  Goose would like to call the above tool, do you allow?\n" +
		"|  > Allow (Allow the tool call once)\n" +
		"|    Always Allow \n" +
		"|    Deny \n" +
		"|    Cancel \n" +
		"—  \n"
	gooseNoticeMenu = "  Provider-provided approval notice\n" +
		"    Extension management requires approval for security\n\n" +
		"◆  Do you allow this tool call?\n" +
		"│  ● Allow (Allow the tool call once)\n" +
		"│  ○ Deny \n" +
		"│  ○ Cancel \n" +
		"└  \n"
	gooseNoticeMenuASCII = "*  Do you allow this tool call?\n" +
		"|  > Allow (Allow the tool call once)\n" +
		"|    Deny \n" +
		"|    Cancel \n" +
		"—  \n"
)

func detectGoose(t *testing.T, input string) []Event {
	t.Helper()
	adapter, err := NewGooseAdapter(nil)
	if err != nil {
		t.Fatal(err)
	}
	events, err := adapter.Detect(NewDetectionState("session-goose", "agent-goose", GooseID), []byte(input))
	if err != nil {
		t.Fatal(err)
	}
	return events
}

func TestGooseAdapterID(t *testing.T) {
	adapter, err := NewGooseAdapter(nil)
	if err != nil {
		t.Fatalf("NewGooseAdapter: %v", err)
	}
	if got := adapter.ID(); got != GooseID {
		t.Fatalf("adapter.ID() = %q, want %q", got, GooseID)
	}
}

func TestGooseMenusAreRead(t *testing.T) {
	for input, interaction := range map[string]string{
		gooseToolMenu:        gooseToolCall,
		gooseToolMenuASCII:   gooseToolCall,
		gooseNoticeMenu:      gooseToolCallWithNotice,
		gooseNoticeMenuASCII: gooseToolCallWithNotice,
	} {
		events := detectGoose(t, input)
		if len(events) != 1 {
			t.Fatalf("%q: %d events, want 1", input, len(events))
		}
		event := events[0]
		if event.Metadata[gooseInteractionMetadata] != interaction || event.Type != EventPermission ||
			event.Risk != RiskHigh || !event.Actionable() {
			t.Fatalf("%q read as %#v", input, event)
		}
	}
}

// Whatever option is highlighted, the question is the same one.
func TestGooseMenuIsReadWhateverIsHighlighted(t *testing.T) {
	moved := strings.NewReplacer("│  ● Allow", "│  ○ Allow", "│  ○ Deny", "│  ● Deny").Replace(gooseToolMenu)
	if events := detectGoose(t, moved); len(events) != 1 {
		t.Fatalf("highlight on Deny: %d events, want 1", len(events))
	}
}

func TestGooseMenusThatAreNotAskingAreNotRead(t *testing.T) {
	for name, input := range map[string]string{
		// Answered: cliclack redraws the question after the submitted symbol
		// and the chosen option alone.
		"answered":          "◇  Goose would like to call the above tool, do you allow?\n│  Allow \n│\n",
		"answered in ASCII": "o  Do you allow this tool call?\n|  Deny \n|\n",
		// Not all drawn yet.
		"no bar end":        strings.TrimSuffix(gooseToolMenu, "└  \n"),
		"an option missing": strings.Replace(gooseToolMenu, "│  ○ Deny \n", "", 1),
		"options of the other menu": strings.Replace(gooseNoticeMenu, "Do you allow this tool call?",
			"Goose would like to call the above tool, do you allow?", 1),
		"output after the menu": gooseToolMenu + "Running the tool...\n",
		"a question alone":      "Goose would like to call the above tool, do you allow?\n",
		"quoted question":       "> Goose would like to call the above tool, do you allow?\n",
		"in a code fence":       "```\n" + gooseToolMenu,
	} {
		if events := detectGoose(t, input); len(events) != 0 {
			t.Errorf("%s: raised %#v", name, events)
		}
	}
}

// Nothing is raised until the bar's end is drawn, however the menu arrives.
func TestGooseMenuStreamedIsReadOnceComplete(t *testing.T) {
	adapter, err := NewGooseAdapter(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := NewDetectionState("session-goose-stream", "agent-goose", GooseID)
	lines := strings.SplitAfter(gooseToolMenu, "\n")
	var raised []Event
	for index, line := range lines {
		events, err := adapter.Detect(state, []byte(line))
		if err != nil {
			t.Fatal(err)
		}
		if len(events) > 0 && index < len(lines)-2 {
			t.Fatalf("raised after line %d of %d: %q", index, len(lines), line)
		}
		raised = append(raised, events...)
	}
	if len(raised) != 1 {
		t.Fatalf("raised %d events, want 1", len(raised))
	}
}

// Allow and Deny are the keys that pick them wherever the highlight is: k to
// the top, where cliclack stops it, j down, Enter. y and n pick nothing.
func TestGooseEncodeDecision(t *testing.T) {
	adapter, err := NewGooseAdapter(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		interaction string
		decision    Decision
		manual      string
		want        string
	}{
		{gooseToolCall, DecisionAllow, "", "kkk\r"},
		{gooseToolCall, DecisionDeny, "", "kkkjj\r"},
		{gooseToolCall, DecisionManual, "allow", "kkk\r"},
		{gooseToolCall, DecisionManual, "Deny", "kkkjj\r"},
		{gooseToolCall, DecisionManual, "cancel", "kkkjjj\r"},
		{gooseToolCallWithNotice, DecisionAllow, "", "kk\r"},
		{gooseToolCallWithNotice, DecisionDeny, "", "kkj\r"},
		{gooseToolCallWithNotice, DecisionManual, "cancel", "kkjj\r"},
	} {
		event := Event{Type: EventPermission, Metadata: map[string]string{gooseInteractionMetadata: tc.interaction}}
		got, err := adapter.EncodeDecision(event, tc.decision, tc.manual)
		if err != nil {
			t.Fatalf("%s %s %q: %v", tc.interaction, tc.decision, tc.manual, err)
		}
		if string(got) != tc.want {
			t.Errorf("%s %s %q = %q, want %q", tc.interaction, tc.decision, tc.manual, got, tc.want)
		}
	}

	event := Event{Type: EventPermission, Metadata: map[string]string{gooseInteractionMetadata: gooseToolCall}}
	for name, call := range map[string]func() ([]byte, error){
		"automatic decision with manual input": func() ([]byte, error) { return adapter.EncodeDecision(event, DecisionAllow, "extra") },
		"NUL byte":                             func() ([]byte, error) { return adapter.EncodeDecision(event, DecisionManual, "bad\x00input") },
		// Typed into the menu, text moves the highlight and Enter picks
		// whatever it lands on.
		"free text":    func() ([]byte, error) { return adapter.EncodeDecision(event, DecisionManual, "deny tool") },
		"always allow": func() ([]byte, error) { return adapter.EncodeDecision(event, DecisionManual, "always allow") },
		"non-actionable": func() ([]byte, error) {
			return adapter.EncodeDecision(Event{Type: EventProcessExit}, DecisionAllow, "")
		},
	} {
		if got, err := call(); err == nil {
			t.Errorf("%s: encoded %q, want an error", name, got)
		}
	}
}

func TestGooseSnapshotFingerprintSource(t *testing.T) {
	adapter, err := NewGooseAdapter(nil)
	if err != nil {
		t.Fatal(err)
	}
	source := adapter.snapshotFingerprintSource(gooseToolMenu, "└", false)
	if !strings.HasPrefix(source, gooseToolCall+"\x00") {
		t.Errorf("fingerprint source = %q, want the tool_call menu", source)
	}
	if source := adapter.snapshotFingerprintSource(gooseToolMenu, "└", true); source != "└" {
		t.Errorf("in a code fence, fingerprint source = %q, want the active line", source)
	}
	if !adapter.snapshotOccurrenceAware(Event{Metadata: map[string]string{gooseInteractionMetadata: gooseToolCall}}) {
		t.Errorf("a menu occurrence is not occurrence aware")
	}
	if adapter.snapshotOccurrenceAware(Event{}) {
		t.Errorf("an event without interaction is occurrence aware")
	}
}
