package app

import (
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/record"
)

func finishedTranscript() record.Metadata {
	return record.Metadata{
		ID:            "run-1-alpha-1700000000",
		RunID:         "run-1",
		SessionID:     "alpha",
		AgentID:       "alpha",
		Name:          "Agent Alpha",
		Backend:       "PTY",
		Adapter:       "Claude",
		StartedAt:     time.Unix(1700000000, 0),
		EndedAt:       time.Unix(1700000060, 0),
		Bytes:         4096,
		Frames:        12,
		Truncated:     true,
		InputRecorded: true,
		Redacted:      true,
	}
}

func TestRecordingAuditEntryDescribesTheTranscriptNotItsContent(t *testing.T) {
	transcript := finishedTranscript()

	started := recordingAuditEntry(record.LifecycleEvent{Metadata: transcript})
	if started.Kind != audit.KindRecordingStarted || started.Outcome != audit.OutcomeStarted {
		t.Fatalf("opening = %+v", started)
	}
	if started.DecisionBy != audit.DecisionBySystem || started.Operator != "" {
		t.Fatalf("opening attributed to %q/%q, want the system", started.DecisionBy, started.Operator)
	}
	if started.Backend != "pty" || started.Adapter != "claude" || started.AgentID != "alpha" {
		t.Fatalf("opening identity = %+v", started)
	}
	// The counters do not exist yet when a transcript opens.
	for _, key := range []string{"bytes", "frames", "truncated"} {
		if _, present := started.Metadata[key]; present {
			t.Errorf("opening carries %q before anything was written", key)
		}
	}

	finished := recordingAuditEntry(record.LifecycleEvent{Finished: true, Metadata: transcript})
	if finished.Kind != audit.KindRecordingFinished || finished.Outcome != audit.OutcomeFinished {
		t.Fatalf("closing = %+v", finished)
	}
	want := map[string]string{
		"recording_id":   transcript.ID,
		"input_recorded": "true",
		"redacted":       "true",
		"bytes":          "4096",
		"frames":         "12",
		"truncated":      "true",
	}
	if !reflect.DeepEqual(finished.Metadata, want) {
		t.Fatalf("closing metadata = %v, want %v", finished.Metadata, want)
	}
}

// TestRecordingAuditEntrySurvivesTheSanitizer guards against a key the
// allowlist does not know. The detailed-mode sanitizer drops such a key
// silently, so a misspelt one would vanish from every journal without a test
// noticing. (Metadata mode keeps no metadata at all, by design.)
func TestRecordingAuditEntrySurvivesTheSanitizer(t *testing.T) {
	events := []record.LifecycleEvent{
		{Metadata: finishedTranscript()},
		{Finished: true, Metadata: finishedTranscript()},
		{Metadata: record.Metadata{SessionID: "alpha"}, Err: errors.New("refused")},
		{Finished: true, Metadata: finishedTranscript(), Err: errors.New("refused")},
	}
	for _, event := range events {
		entry := recordingAuditEntry(event)
		kept := audit.SanitizeEntry(entry, audit.ModeDetailed)
		if len(entry.Metadata) > 0 && !reflect.DeepEqual(kept.Metadata, entry.Metadata) {
			t.Errorf("%s: metadata %v became %v", entry.Kind, entry.Metadata, kept.Metadata)
		}
		for _, mode := range []audit.Mode{audit.ModeMetadata, audit.ModeDetailed} {
			if kept := audit.SanitizeEntry(entry, mode); kept.Reason != entry.Reason {
				t.Errorf("%s in %s mode: reason %q became %q", entry.Kind, mode, entry.Reason, kept.Reason)
			}
		}
	}
}

func TestRecordingAuditEntryReportsAFailureWithoutCounts(t *testing.T) {
	refused := recordingAuditEntry(record.LifecycleEvent{
		Metadata: record.Metadata{SessionID: "alpha", AgentID: "alpha"},
		Err:      errors.New("store closed"),
	})
	if refused.Kind != audit.KindRecordingStarted || refused.Outcome != audit.OutcomeFailed ||
		refused.Reason != "recording_open_failed" {
		t.Fatalf("refused opening = %+v", refused)
	}
	if len(refused.Metadata) != 0 {
		t.Fatalf("refused opening metadata = %v, want nothing: no transcript exists", refused.Metadata)
	}

	unfinished := recordingAuditEntry(record.LifecycleEvent{
		Finished: true,
		Metadata: finishedTranscript(),
		Err:      errors.New("sidecar write failed"),
	})
	if unfinished.Kind != audit.KindRecordingFinished || unfinished.Outcome != audit.OutcomeFailed ||
		unfinished.Reason != "recording_finalize_failed" {
		t.Fatalf("failed closing = %+v", unfinished)
	}
	if want := map[string]string{"recording_id": finishedTranscript().ID}; !reflect.DeepEqual(unfinished.Metadata, want) {
		t.Fatalf("failed closing metadata = %v, want only the identity", unfinished.Metadata)
	}
}
