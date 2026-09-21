package app

import (
	"strconv"
	"strings"

	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/record"
)

// recordingAuditEntry turns a transcript opening or closing into its journal
// record. It states that a transcript exists and how large it became, never
// what it holds: record.Metadata has no field for a frame, and this reads only
// counters and markers from it.
func recordingAuditEntry(event record.LifecycleEvent) audit.Entry {
	metadata := event.Metadata
	entry := audit.Entry{
		Kind:       audit.KindRecordingStarted,
		SessionID:  metadata.SessionID,
		AgentID:    metadata.AgentID,
		Backend:    strings.ToLower(strings.TrimSpace(metadata.Backend)),
		Adapter:    strings.ToLower(strings.TrimSpace(metadata.Adapter)),
		DecisionBy: audit.DecisionBySystem,
		Outcome:    audit.OutcomeStarted,
		Reason:     "recording_started",
		Metadata:   map[string]string{},
	}
	if event.Finished {
		entry.Kind = audit.KindRecordingFinished
		entry.Outcome = audit.OutcomeFinished
		entry.Reason = "recording_finished"
	}
	if metadata.ID != "" {
		entry.Metadata["recording_id"] = metadata.ID
	}

	if event.Err != nil {
		// The file's shape is unknown or unreliable once the store refused it,
		// so a failure carries the identity and nothing that reads as a count.
		entry.Outcome = audit.OutcomeFailed
		entry.Reason = "recording_open_failed"
		if event.Finished {
			entry.Reason = "recording_finalize_failed"
		}
		return entry
	}

	entry.Metadata["input_recorded"] = strconv.FormatBool(metadata.InputRecorded)
	entry.Metadata["redacted"] = strconv.FormatBool(metadata.Redacted)
	if event.Finished {
		entry.Metadata["bytes"] = strconv.FormatInt(metadata.Bytes, 10)
		entry.Metadata["frames"] = strconv.Itoa(metadata.Frames)
		entry.Metadata["truncated"] = strconv.FormatBool(metadata.Truncated)
	}
	return entry
}

// auditRecordingLifecycle journals a transcript opening or closing. A failed
// write is swallowed like every other recording failure: a recording is
// observability and must not be able to stop a run. The audit journal outlives
// the recorder at shutdown, since closeRecorder runs before the journal closes.
func (r *DesktopRuntime) auditRecordingLifecycle(event record.LifecycleEvent) {
	if r == nil || r.auditor == nil {
		return
	}
	_ = r.auditor.Record(recordingAuditEntry(event))
}
