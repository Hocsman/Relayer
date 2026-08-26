package adapters

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

type genericStreamFixture struct {
	Name          string    `json:"name"`
	Chunks        []string  `json:"chunks"`
	WantEvents    int       `json:"want_events"`
	WantType      EventType `json:"want_type"`
	WantMatch     string    `json:"want_match"`
	WantSensitive bool      `json:"want_sensitive"`
	WantOutput    string    `json:"want_output"`
}

func TestGenericRegexAdapterSyntheticStreamFixtures(t *testing.T) {
	fixtures := loadGenericStreamFixtures(t)
	for fixtureIndex, fixture := range fixtures {
		fixture := fixture
		t.Run(fixture.Name, func(t *testing.T) {
			adapter, err := NewGenericRegexAdapter(DefaultPatterns())
			if err != nil {
				t.Fatal(err)
			}
			state := NewDetectionState(fmt.Sprintf("session-%d", fixtureIndex), "agent-a", GenericID)
			var events []Event
			processor, err := NewProcessor(adapter, state, 4096, Hooks{
				OnEvent: func(event Event) { events = append(events, event) },
			})
			if err != nil {
				t.Fatal(err)
			}
			for _, chunk := range fixture.Chunks {
				if err := processor.Consume([]byte(chunk)); err != nil {
					t.Fatalf("Consume(%q): %v", chunk, err)
				}
			}

			if got := processor.Output(); got != fixture.WantOutput {
				t.Fatalf("rendered output = %q, want %q", got, fixture.WantOutput)
			}
			if len(events) != fixture.WantEvents {
				t.Fatalf("events = %#v, want %d", events, fixture.WantEvents)
			}
			if fixture.WantEvents == 0 {
				if processor.Pending() != nil || processor.IsBlocked() {
					t.Fatalf("non-actionable fixture left pending state: %#v", processor.Pending())
				}
				return
			}
			event := events[0]
			if event.ID == "" || event.Signature == "" || event.Sequence != 1 || event.Timestamp.IsZero() {
				t.Fatalf("event identity is incomplete: %#v", event)
			}
			if event.SessionID != state.SessionID || event.AgentID != "agent-a" || event.Adapter != GenericID {
				t.Fatalf("event routing identity = %#v", event)
			}
			if event.Type != fixture.WantType || event.Match != fixture.WantMatch || event.Sensitive != fixture.WantSensitive {
				t.Fatalf("event semantics = %#v", event)
			}
			wantRisk := RiskUnknown
			if fixture.WantSensitive {
				wantRisk = RiskHigh
			}
			if event.Risk != wantRisk || event.Metadata["pattern"] == "" || !event.Actionable() {
				t.Fatalf("event risk/metadata = %#v", event)
			}
			if pending := processor.Pending(); pending == nil || pending.ID != event.ID {
				t.Fatalf("pending = %#v, want occurrence %q", pending, event.ID)
			}
		})
	}
}

func TestGenericRegexAdapterValidatesAndDefensivelyCopiesOrderedPatterns(t *testing.T) {
	if _, err := NewGenericRegexAdapter([]Pattern{{Name: "broken", Expression: "("}}); err == nil {
		t.Fatal("invalid regex was accepted")
	}

	patterns := []Pattern{
		{Name: "first", Description: "first in configured order", Expression: `(?i)\[y/n\]`},
		{Name: "second", Description: "more specific later expression", Expression: `(?i)overwrite.*\[y/n\]`},
	}
	adapter, err := NewGenericRegexAdapter(patterns)
	if err != nil {
		t.Fatal(err)
	}
	patterns[0] = Pattern{Name: "mutated", Expression: "("}
	state := NewDetectionState("session-a", "agent-a", GenericID)
	events, err := adapter.Detect(state, []byte("Overwrite draft? [Y/n]"))
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].Summary != "first in configured order" ||
		events[0].Match != "[Y/n]" || events[0].Metadata["pattern"] != "first" {
		t.Fatalf("ordered detection after caller mutation = %#v", events)
	}
	if events, err := adapter.Detect(state, []byte("Overwrite another? [Y/n]")); err != nil || len(events) != 0 {
		t.Fatalf("blocked adapter emitted duplicate events %#v, error %v", events, err)
	}
	if _, err := adapter.Detect(nil, []byte("Overwrite? [Y/n]")); err == nil {
		t.Fatal("nil detection state was accepted")
	}
}

func TestDefaultPatternsReturnsIndependentCopy(t *testing.T) {
	first := DefaultPatterns()
	if len(first) == 0 {
		t.Fatal("DefaultPatterns returned no patterns")
	}
	original := first[0]
	first[0] = Pattern{Name: "mutated", Expression: "("}
	second := DefaultPatterns()
	if second[0] != original {
		t.Fatalf("DefaultPatterns shares caller storage: %#v", second[0])
	}
}

func TestGenericEncodeDecision(t *testing.T) {
	adapter, err := NewGenericRegexAdapter(nil)
	if err != nil {
		t.Fatal(err)
	}
	actionable := Event{Type: EventConfirmation}
	for _, test := range []struct {
		name  string
		input string
		want  []byte
	}{
		{name: "ordinary", input: "Y", want: []byte("Y\r")},
		{name: "empty", input: "", want: []byte("\r")},
		{name: "embedded newline preserved", input: "line one\nline two", want: []byte("line one\nline two\r")},
	} {
		t.Run(test.name, func(t *testing.T) {
			got, err := adapter.EncodeDecision(actionable, DecisionManual, test.input)
			if err != nil || !reflect.DeepEqual(got, test.want) {
				t.Fatalf("EncodeDecision = %v, %v; want %v", got, err, test.want)
			}
			if len(got) > 0 {
				got[0] ^= 0xff
			}
		})
	}
	if got, err := adapter.EncodeDecision(actionable, Decision("approve"), "Y"); !errors.Is(err, ErrDecisionUnsupported) || got != nil {
		t.Fatalf("unsupported decision = %v, %v", got, err)
	}
	if got, err := adapter.EncodeDecision(Event{Type: EventProcessExit}, DecisionManual, "Y"); !errors.Is(err, ErrDecisionUnsupported) || got != nil {
		t.Fatalf("non-actionable event decision = %v, %v", got, err)
	}
	if got, err := adapter.EncodeDecision(actionable, DecisionManual, "before\x00after"); err == nil || got != nil {
		t.Fatalf("NUL input = %v, %v", got, err)
	}
}

func loadGenericStreamFixtures(t *testing.T) []genericStreamFixture {
	t.Helper()
	payload, err := os.ReadFile(filepath.Join("testdata", "generic", "stream_cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []genericStreamFixture
	if err := json.Unmarshal(payload, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) == 0 {
		t.Fatal("generic stream fixture file is empty")
	}
	return fixtures
}
