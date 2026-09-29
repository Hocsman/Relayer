package adapters

import (
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// aiderCaptureFixture is one question captured from a real Aider in a PTY:
// the bytes it wrote while it waited for the answer, and the answers that were
// observed to do what they say. See testdata/aider/README.md.
type aiderCaptureFixture struct {
	CLIVersion    string   `json:"cli_version"`
	Interaction   string   `json:"interaction"`
	CaptureMode   string   `json:"capture_mode"`
	Stripped      string   `json:"stripped"`
	ANSIChunks    []string `json:"ansi_chunks"`
	AllowInputHex string   `json:"allow_input_hex"`
	DenyInputHex  string   `json:"deny_input_hex"`
	ObservedAllow string   `json:"observed_allow"`
	ObservedDeny  string   `json:"observed_deny"`
}

func loadAiderCaptureFixtures(t *testing.T) []aiderCaptureFixture {
	t.Helper()
	paths, err := filepath.Glob(filepath.Join("testdata", "aider", "*.json"))
	if err != nil || len(paths) == 0 {
		t.Fatalf("no Aider fixtures: %v", err)
	}
	fixtures := make([]aiderCaptureFixture, 0, len(paths))
	for _, path := range paths {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(strings.NewReader(string(data)))
		decoder.DisallowUnknownFields()
		var fixture aiderCaptureFixture
		if err := decoder.Decode(&fixture); err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		if fixture.Interaction+".json" != filepath.Base(path) {
			t.Fatalf("%s names interaction %q", path, fixture.Interaction)
		}
		fixtures = append(fixtures, fixture)
	}
	return fixtures
}

// playAiderCapture feeds the captured bytes, cut into pieces of the given
// size, to a processor on a 100x30 terminal, as the capture's PTY was, and
// returns what it raised.
func playAiderCapture(t *testing.T, fixture aiderCaptureFixture, piece int) []Event {
	t.Helper()
	adapter, err := NewAiderAdapter(DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	var raised []Event
	processor, err := NewProcessor(adapter, NewDetectionState("session-aider", "agent-aider", AiderID), 64*1024,
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
func TestAiderCapturedQuestionsAreEachReadOnce(t *testing.T) {
	for _, fixture := range loadAiderCaptureFixtures(t) {
		for _, piece := range []int{0, 1, 7, 64} {
			events := playAiderCapture(t, fixture, piece)
			if len(events) != 1 {
				t.Fatalf("%s in pieces of %d: %d events, want 1: %#v", fixture.Interaction, piece, len(events), events)
			}
			if got := events[0].Metadata[aiderInteractionMetadata]; got != fixture.Interaction {
				t.Fatalf("%s in pieces of %d read as %q (%s)", fixture.Interaction, piece, got, events[0].Summary)
			}
			if !events[0].Actionable() {
				t.Fatalf("%s is not actionable", fixture.Interaction)
			}
		}
	}
}

// The answers are the bytes observed to do what they say: y and Enter
// allowed, n and Enter denied, for every captured question.
func TestAiderAnswersAreTheObservedBytes(t *testing.T) {
	adapter, err := NewAiderAdapter(nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, fixture := range loadAiderCaptureFixtures(t) {
		event := playAiderCapture(t, fixture, 0)[0]
		for decision, want := range map[Decision]string{DecisionAllow: fixture.AllowInputHex, DecisionDeny: fixture.DenyInputHex} {
			encoded, err := adapter.EncodeDecision(event, decision, "")
			if err != nil {
				t.Fatalf("%s %s: %v", fixture.Interaction, decision, err)
			}
			if got := hex.EncodeToString(encoded); got != want {
				t.Fatalf("%s %s = %s, observed %s", fixture.Interaction, decision, got, want)
			}
		}
		if fixture.ObservedAllow == "" || fixture.ObservedDeny == "" {
			t.Fatalf("%s records no observed effect of its answers", fixture.Interaction)
		}
	}
}

// The captured questions, quoted in a code fence, in backticks or after a log
// prefix, are not asked.
func TestAiderCapturedQuestionsQuotedAreNotAsked(t *testing.T) {
	for _, fixture := range loadAiderCaptureFixtures(t) {
		question := ""
		for _, line := range strings.Split(fixture.Stripped, "\n") {
			if strings.Contains(line, "[Yes]:") {
				question = strings.TrimSpace(line[:strings.Index(line, "[Yes]:")+len("[Yes]:")])
				break
			}
		}
		if question == "" {
			t.Fatalf("%s: no question line in %q", fixture.Interaction, fixture.Stripped)
		}
		for _, quoted := range []string{
			"```\n" + question + " \n```\n",
			"Aider will ask `" + question + "` next\n",
			"log: " + question + " \n",
			"> " + question + " \n",
		} {
			adapter, err := NewAiderAdapter(nil)
			if err != nil {
				t.Fatal(err)
			}
			events, err := adapter.Detect(NewDetectionState("session-quoted", "agent-aider", AiderID), []byte(quoted))
			if err != nil {
				t.Fatal(err)
			}
			if len(events) != 0 {
				t.Fatalf("%s quoted as %q raised %#v", fixture.Interaction, quoted, events)
			}
		}
	}
}
