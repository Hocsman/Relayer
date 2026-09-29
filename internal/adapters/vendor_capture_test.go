package adapters

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// captureFixture is one question captured from a real agent CLI in a PTY: the
// bytes it wrote while it waited for the answer, and the answers that were
// observed to do what they say. See testdata/<vendor>/README.md.
type captureFixture struct {
	CLIVersion  string   `json:"cli_version"`
	Interaction string   `json:"interaction"`
	CaptureMode string   `json:"capture_mode"`
	Stripped    string   `json:"stripped"`
	ANSIChunks  []string `json:"ansi_chunks"`
	// AnsweredChunks, when present, are the bytes the CLI wrote after the
	// allow answer, up to its next input prompt.
	AnsweredChunks []string `json:"answered_chunks,omitempty"`
	AllowInputHex  string   `json:"allow_input_hex"`
	DenyInputHex   string   `json:"deny_input_hex"`
	ObservedAllow  string   `json:"observed_allow"`
	ObservedDeny   string   `json:"observed_deny"`
}

// capturedVendor is an adapter whose questions were captured from its CLI.
type capturedVendor struct {
	id  string
	dir string
	// newAdapter builds the adapter with the given fallback patterns.
	newAdapter func(patterns []Pattern) (Adapter, error)
	// questionEnd ends the question line in a capture's stripped text.
	questionEnd string
}

var capturedVendors = []capturedVendor{
	{
		id: AiderID, dir: "aider", questionEnd: "[Yes]:",
		newAdapter: func(patterns []Pattern) (Adapter, error) { return NewAiderAdapter(patterns) },
	},
	{
		id: OpenInterpreterID, dir: "interpreter", questionEnd: "(y/n)",
		newAdapter: func(patterns []Pattern) (Adapter, error) { return NewOpenInterpreterAdapter(patterns) },
	},
	{
		id: GooseID, dir: "goose", questionEnd: "?",
		newAdapter: func(patterns []Pattern) (Adapter, error) { return NewGooseAdapter(patterns) },
	},
}

func loadCaptureFixtures(t *testing.T, vendor capturedVendor) []captureFixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", vendor.dir, "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no %s fixtures: %v", vendor.id, err)
	}
	fixtures := make([]captureFixture, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		var fixture captureFixture
		if err := decoder.Decode(&fixture); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		// A second capture of the same question is named
		// <interaction>--<variant>.json.
		name, _, _ := strings.Cut(strings.TrimSuffix(filepath.Base(path), ".json"), "--")
		if fixture.Interaction != name {
			t.Fatalf("%s names interaction %q", path, fixture.Interaction)
		}
		fixtures = append(fixtures, fixture)
	}
	return fixtures
}

// playCapture feeds the captured bytes, cut into pieces of the given size, to
// a processor on a 100x30 terminal, as the capture's PTY was, and returns what
// it raised.
func playCapture(t *testing.T, vendor capturedVendor, fixture captureFixture, piece int) []Event {
	t.Helper()
	adapter, err := vendor.newAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	var raised []Event
	processor, err := NewProcessor(adapter, NewDetectionState("session-"+vendor.id, "agent-"+vendor.id, vendor.id), 64*1024,
		Hooks{OnEvent: func(event Event) { raised = append(raised, event) }})
	if err != nil {
		t.Fatal(err)
	}
	processor.Resize(100, 30)
	stream := strings.Join(fixture.ANSIChunks, "")
	if piece <= 0 {
		for _, chunk := range fixture.ANSIChunks {
			if err := processor.Consume([]byte(chunk)); err != nil {
				t.Fatal(err)
			}
		}
	} else {
		for start := 0; start < len(stream); start += piece {
			if err := processor.Consume([]byte(stream[start:min(start+piece, len(stream))])); err != nil {
				t.Fatal(err)
			}
		}
	}
	processor.WaitSemanticEvents()
	return raised
}

// Every captured question is read as its own interaction, once, however the
// bytes arrive: as captured, a byte at a time, or in pieces that cut escape
// sequences.
func TestCapturedQuestionsAreEachReadOnce(t *testing.T) {
	for _, vendor := range capturedVendors {
		for _, fixture := range loadCaptureFixtures(t, vendor) {
			for _, piece := range []int{0, 1, 7, 64} {
				events := playCapture(t, vendor, fixture, piece)
				if len(events) != 1 {
					t.Fatalf("%s %s in pieces of %d: %d events, want 1: %#v", vendor.id, fixture.Interaction, piece, len(events), events)
				}
				if got := events[0].Metadata["interaction"]; got != fixture.Interaction {
					t.Fatalf("%s %s in pieces of %d read as %q (%s)", vendor.id, fixture.Interaction, piece, got, events[0].Summary)
				}
				if !events[0].Actionable() {
					t.Fatalf("%s %s is not actionable", vendor.id, fixture.Interaction)
				}
			}
		}
	}
}

// The answers are the bytes observed to do what they say, for every captured
// question.
func TestCapturedAnswersAreTheObservedBytes(t *testing.T) {
	for _, vendor := range capturedVendors {
		adapter, err := vendor.newAdapter(nil)
		if err != nil {
			t.Fatal(err)
		}
		for _, fixture := range loadCaptureFixtures(t, vendor) {
			event := playCapture(t, vendor, fixture, 0)[0]
			for decision, want := range map[Decision]string{DecisionAllow: fixture.AllowInputHex, DecisionDeny: fixture.DenyInputHex} {
				encoded, err := adapter.EncodeDecision(event, decision, "")
				if err != nil {
					t.Fatalf("%s %s %s: %v", vendor.id, fixture.Interaction, decision, err)
				}
				if got := hex.EncodeToString(encoded); got != want {
					t.Fatalf("%s %s %s = %s, observed %s", vendor.id, fixture.Interaction, decision, got, want)
				}
			}
			if fixture.ObservedAllow == "" || fixture.ObservedDeny == "" {
				t.Fatalf("%s %s records no observed effect of its answers", vendor.id, fixture.Interaction)
			}
		}
	}
}

// The captured questions, quoted in a code fence, in backticks or after a log
// prefix, are not asked.
func TestCapturedQuestionsQuotedAreNotAsked(t *testing.T) {
	for _, vendor := range capturedVendors {
		for _, fixture := range loadCaptureFixtures(t, vendor) {
			question := ""
			for _, line := range strings.Split(fixture.Stripped, "\n") {
				if index := strings.Index(line, vendor.questionEnd); index >= 0 {
					question = strings.TrimSpace(line[:index+len(vendor.questionEnd)])
					break
				}
			}
			if question == "" {
				t.Fatalf("%s %s: no question line in %q", vendor.id, fixture.Interaction, fixture.Stripped)
			}
			for _, quoted := range []string{
				"```\n" + question + " \n```\n",
				"The agent will ask `" + question + "` next\n",
				"log: " + question + " \n",
				"> " + question + " \n",
			} {
				adapter, err := vendor.newAdapter(nil)
				if err != nil {
					t.Fatal(err)
				}
				events, err := adapter.Detect(NewDetectionState("session-quoted", "agent-"+vendor.id, vendor.id), []byte(quoted))
				if err != nil {
					t.Fatal(err)
				}
				if len(events) != 0 {
					t.Fatalf("%s %s quoted as %q raised %#v", vendor.id, fixture.Interaction, quoted, events)
				}
			}
		}
	}
}

// Once answered, the question the CLI redraws as answered is not asked again,
// and the same question asked after it is.
func TestCapturedAnswerRedrawIsNotAskedAgain(t *testing.T) {
	for _, vendor := range capturedVendors {
		for _, fixture := range loadCaptureFixtures(t, vendor) {
			if len(fixture.AnsweredChunks) == 0 {
				continue
			}
			adapter, err := vendor.newAdapter(DefaultPatterns())
			if err != nil {
				t.Fatal(err)
			}
			var raised []Event
			processor, err := NewProcessor(adapter, NewDetectionState("session-"+vendor.id, "agent-"+vendor.id, vendor.id), 64*1024,
				Hooks{OnEvent: func(event Event) { raised = append(raised, event) }})
			if err != nil {
				t.Fatal(err)
			}
			processor.Resize(100, 30)
			consume := func(chunks []string) {
				for _, chunk := range chunks {
					if err := processor.Consume([]byte(chunk)); err != nil {
						t.Fatal(err)
					}
				}
				processor.WaitSemanticEvents()
			}
			consume(fixture.ANSIChunks)
			if len(raised) != 1 {
				t.Fatalf("%s %s: %d events, want 1", vendor.id, fixture.Interaction, len(raised))
			}
			if err := processor.Resolve(raised[0].ID, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			consume(fixture.AnsweredChunks)
			if len(raised) != 1 {
				t.Fatalf("%s %s: the answered redraw raised %s", vendor.id, fixture.Interaction, describeRaised(raised[1:]))
			}
			consume(fixture.ANSIChunks)
			if len(raised) != 2 {
				t.Fatalf("%s %s asked again: %d events, want 2", vendor.id, fixture.Interaction, len(raised))
			}
		}
	}
}
