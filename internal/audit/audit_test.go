package audit

import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"
)

// recordingAndControlKinds is the vocabulary added for session recording and
// multi-operator control.
var recordingAndControlKinds = []Kind{
	KindRecordingStarted, KindRecordingFinished, KindRecordingExported, KindRecordingDeleted,
	KindControlRequested, KindControlGranted, KindControlDeclined, KindControlReleased, KindControlForced,
}

func TestSafeKindAcceptsRecordingAndControlKinds(t *testing.T) {
	for _, kind := range recordingAndControlKinds {
		if got := safeKind(kind); got != kind {
			t.Fatalf("safeKind(%q) = %q", kind, got)
		}
	}
	if got := safeKind(Kind("recording_paused")); got != KindUnknown {
		t.Fatalf("safeKind of an unlisted kind = %q, want %q", got, KindUnknown)
	}
}

func TestSafeCodeAcceptsRecordingAndControlReasons(t *testing.T) {
	for _, code := range []string{
		"recording_started", "recording_completed", "recording_truncated",
		"recording_exported", "recording_deleted",
		"control_requested", "control_granted", "control_declined",
		"control_released", "control_timeout", "control_disconnect", "control_forced",
	} {
		if got := safeCode(code); got != code {
			t.Fatalf("safeCode(%q) = %q", code, got)
		}
	}
}

// requireRelayerJournal recognizes a journal by the kind on its first line, so
// the closed vocabulary is also a compatibility surface: a binary built before
// a kind existed refuses a journal that opens on it, treats the path as someone
// else's file, and declines to write at all. That is tolerable only because the
// recorder always emits run_started first. This pins the half of the contract
// this binary can enforce - every kind it knows is structurally acceptable as a
// first line - so a future kind that becomes a journal's first entry fails here
// rather than in the field.
func TestRequireRelayerJournalAcceptsRecordingAndControlFirstLines(t *testing.T) {
	for _, kind := range recordingAndControlKinds {
		t.Run(string(kind), func(t *testing.T) {
			line, err := json.Marshal(Entry{
				SchemaVersion: CurrentSchemaVersion,
				Sequence:      1,
				EntryID:       "entry-1",
				RunID:         "run-1",
				Kind:          kind,
			})
			if err != nil {
				t.Fatal(err)
			}
			line = append(line, '\n')
			if err := requireRelayerJournal(bytes.NewReader(line), int64(len(line))); err != nil {
				t.Fatalf("requireRelayerJournal = %v, want nil", err)
			}
		})
	}
}

func TestSafeCodeBoundsReasonCodes(t *testing.T) {
	for _, code := range []string{
		"recording_started", "recording_completed", "recording_truncated",
		"recording_exported", "recording_deleted",
		"control_requested", "control_granted", "control_declined",
		"control_released", "control_timeout", "control_disconnect", "control_forced",
	} {
		if got := safeCode(code); got != code {
			t.Fatalf("safeCode(%q) = %q", code, got)
		}
	}
	for _, code := range []string{
		"control granted", "control.granted", "_control", "1control",
		"control/granted", "contrôle", strings.Repeat("c", 65),
	} {
		if got := safeCode(code); got != "unknown" {
			t.Fatalf("safeCode(%q) = %q, want %q", code, got, "unknown")
		}
	}
	if got := safeCode("Control_Granted"); got != "control_granted" {
		t.Fatalf("safeCode of a mixed-case code = %q", got)
	}
}

// A journal the recorder itself produced must verify. Human attribution reaches
// disk as allowlisted metadata, so a verifier that rejects any metadata on a
// human entry fails every journal the web gateway writes.
func TestVerifyJournalAcceptsRecorderOutput(t *testing.T) {
	sink := &bufferSink{}
	moment := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	counter := 0
	recorder, err := NewRecorder(
		Config{Enabled: true, Mode: ModeDetailed, MaxFileSizeMB: 1, MaxFiles: 1},
		sink,
		func() time.Time { moment = moment.Add(time.Second); return moment },
		func() (string, error) { counter++; return fmt.Sprintf("id-%02d", counter), nil },
	)
	if err != nil {
		t.Fatal(err)
	}
	entries := []Entry{
		{Kind: KindRunStarted, Outcome: OutcomeStarted},
		{
			Kind: KindDecision, Decision: DecisionAllow, DecisionBy: DecisionByHuman,
			Operator: "alice", Outcome: OutcomeInFlight, Reason: "decision_selected",
			Metadata: map[string]string{"operator": "alice", "role": "operator"},
		},
		{
			Kind: KindAttachStarted, DecisionBy: DecisionByHuman, Operator: "alice",
			Outcome: OutcomeApplied, Reason: "operator_interactive_attached",
			Metadata: map[string]string{
				"operator": "alice", "role": "operator", "conn_id": "conn-1", "active": "true",
			},
		},
	}
	for _, kind := range recordingAndControlKinds {
		entries = append(entries, Entry{
			Kind: kind, DecisionBy: DecisionByHuman, Operator: "alice",
			Outcome: OutcomeApplied, Reason: "control_granted",
			Metadata: map[string]string{"operator": "alice", "role": "operator"},
		})
	}
	for _, entry := range entries {
		if err := recorder.Record(entry); err != nil {
			t.Fatal(err)
		}
	}
	if err := requireRelayerJournal(bytes.NewReader(sink.buffer.Bytes()), int64(sink.buffer.Len())); err != nil {
		t.Fatalf("requireRelayerJournal = %v, want nil", err)
	}
	report, err := VerifyJournal(bytes.NewReader(sink.buffer.Bytes()))
	if err != nil {
		t.Fatal(err)
	}
	if !report.Passed || report.ValidLines != len(entries) {
		t.Fatalf("VerifyJournal = %#v", report)
	}
}

// The allowlist is the only description of what a kind may carry, so the
// verifier consults the same one the sanitizer does.
func TestVerifyJournalRejectsMetadataOutsideKindAllowlist(t *testing.T) {
	for _, test := range []struct {
		name     string
		kind     Kind
		metadata map[string]string
	}{
		{name: "foreign key", kind: KindControlGranted, metadata: map[string]string{"stdout": "rm -rf /"}},
		{name: "other kind key", kind: KindControlGranted, metadata: map[string]string{"frames": "128"}},
		{name: "reason is a field", kind: KindControlForced, metadata: map[string]string{"reason": "rm -rf /"}},
		{name: "operator input is closed", kind: KindOperatorInput, metadata: map[string]string{"operator": "alice"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			entry := makeTestEntry("run-1", 1, time.Now().UTC())
			entry.Kind = test.kind
			entry.Metadata = test.metadata
			report, err := VerifyJournal(bytes.NewReader(entriesToJSONL(t, []Entry{entry})))
			if err != nil {
				t.Fatal(err)
			}
			if report.Passed || len(report.Issues) != 1 ||
				!strings.Contains(report.Issues[0].Message, "metadata outside the allowlist") {
				t.Fatalf("VerifyJournal = %#v", report)
			}
			for key := range test.metadata {
				if strings.Contains(report.Issues[0].Message, key) ||
					strings.Contains(report.Issues[0].Message, test.metadata[key]) {
					t.Fatalf("issue message quotes journal content: %q", report.Issues[0].Message)
				}
			}
		})
	}
}

// Every metadata key an emitter writes must survive sanitization, or the audit
// trail silently loses the connection identity it was added to record.
func TestAllowedMetadataKeyCoversAttachAndControlEmitters(t *testing.T) {
	for _, kind := range []Kind{KindAttachStarted, KindAttachFinished} {
		got := SanitizeEntry(Entry{
			Kind: kind,
			Metadata: map[string]string{
				"operator": "alice", "role": "operator", "conn_id": "conn-1", "active": "true",
			},
		}, ModeDetailed)
		want := map[string]string{
			"operator": "alice", "role": "operator", "conn_id": "conn-1", "active": "true",
		}
		if !reflect.DeepEqual(got.Metadata, want) {
			t.Fatalf("kind %q metadata = %#v, want %#v", kind, got.Metadata, want)
		}
	}
}

type bufferSink struct {
	buffer bytes.Buffer
}

func (s *bufferSink) WriteLine(line []byte) error {
	s.buffer.Write(line)
	return nil
}

func (s *bufferSink) Close() error { return nil }
