package server

import (
	"errors"
	"io"
	"sort"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/record"
)

// maxRecordingFrames bounds one readRecordingChunk page. The gateway's send
// queue drops a frame it cannot deliver rather than blocking, so a transcript
// is paged into messages a slow client can actually receive instead of being
// returned whole and silently discarded.
const maxRecordingFrames = 2000

// maxRecordingExportBytes bounds exportRecording. Beyond it the caller is told
// to page, because a multi-megabyte RPC result is exactly the payload the send
// queue throws away.
const maxRecordingExportBytes = 4 * 1024 * 1024

var errRecordingUnavailable = errors.New("session recording is not enabled")

func (c *Controller) recordingStore() *record.Store {
	c.mu.RLock()
	rt := c.runtime
	c.mu.RUnlock()

	if rt == nil {
		return nil
	}
	return rt.Recordings()
}

func recordingView(metadata record.Metadata) RecordingView {
	view := RecordingView{
		ID:            metadata.ID,
		RunID:         metadata.RunID,
		SessionID:     metadata.SessionID,
		AgentID:       metadata.AgentID,
		Name:          metadata.Name,
		Backend:       metadata.Backend,
		Adapter:       metadata.Adapter,
		StartedAt:     metadata.StartedAt.UTC().Format(time.RFC3339),
		Width:         metadata.Width,
		Height:        metadata.Height,
		Bytes:         metadata.Bytes,
		Frames:        metadata.Frames,
		Truncated:     metadata.Truncated,
		DroppedFrames: metadata.DroppedFrames,
		InputRecorded: metadata.InputRecorded,
		Redacted:      metadata.Redacted,
		ExitCode:      metadata.ExitCode,
		Active:        metadata.Active,
	}
	if !metadata.EndedAt.IsZero() {
		view.EndedAt = metadata.EndedAt.UTC().Format(time.RFC3339)
		if duration := metadata.EndedAt.Sub(metadata.StartedAt); duration > 0 {
			view.DurationSeconds = duration.Seconds()
		}
	}
	return view
}

// ListRecordings returns the stored transcripts, newest first.
func (c *Controller) ListRecordings(filter RecordingFilterInput) ([]RecordingView, error) {
	store := c.recordingStore()
	if store == nil {
		// An empty list rather than an error: a deployment with recording off is
		// a normal state, and the panel should say "nothing here" rather than
		// show a failure the operator cannot act on.
		return []RecordingView{}, nil
	}

	entries, err := store.List()
	if err != nil {
		return nil, err
	}

	runID := strings.ToLower(strings.TrimSpace(filter.RunID))
	sessionID := strings.ToLower(strings.TrimSpace(filter.SessionID))
	agentID := strings.ToLower(strings.TrimSpace(filter.AgentID))

	views := make([]RecordingView, 0, len(entries))
	for _, entry := range entries {
		if runID != "" && strings.ToLower(entry.RunID) != runID {
			continue
		}
		if sessionID != "" && strings.ToLower(entry.SessionID) != sessionID {
			continue
		}
		if agentID != "" && strings.ToLower(entry.AgentID) != agentID {
			continue
		}
		views = append(views, recordingView(entry))
	}

	sort.SliceStable(views, func(i, j int) bool {
		return views[i].StartedAt > views[j].StartedAt
	})
	if filter.Limit > 0 && len(views) > filter.Limit {
		views = views[:filter.Limit]
	}
	return views, nil
}

// GetRecording returns one transcript's metadata.
func (c *Controller) GetRecording(id string) (RecordingView, error) {
	store := c.recordingStore()
	if store == nil {
		return RecordingView{}, errRecordingUnavailable
	}

	metadata, err := store.Get(strings.TrimSpace(id))
	if err != nil {
		return RecordingView{}, err
	}
	return recordingView(metadata), nil
}

// ReadRecordingChunk returns one page of frames plus the header. A recording
// still being written is readable: the operator watching a session live is the
// one most likely to want the replay.
func (c *Controller) ReadRecordingChunk(id string, offset, limit int) (RecordingChunk, error) {
	store := c.recordingStore()
	if store == nil {
		return RecordingChunk{}, errRecordingUnavailable
	}

	id = strings.TrimSpace(id)
	metadata, err := store.Get(id)
	if err != nil {
		return RecordingChunk{}, err
	}
	if offset < 0 {
		offset = 0
	}
	if limit <= 0 || limit > maxRecordingFrames {
		limit = maxRecordingFrames
	}

	header, frames, nextOffset, complete, err := record.ReadRange(metadata.Path, offset, limit)
	if err != nil {
		return RecordingChunk{}, err
	}

	views := make([]RecordingFrameView, 0, len(frames))
	for _, frame := range frames {
		views = append(views, RecordingFrameView{
			Time: frame.Offset,
			Kind: string(frame.Kind),
			Data: frame.Data,
		})
	}

	return RecordingChunk{
		ID: metadata.ID,
		Header: RecordingHeaderView{
			Version:   header.Version,
			Width:     header.Width,
			Height:    header.Height,
			Timestamp: header.Timestamp,
			Title:     header.Title,
		},
		Frames:     views,
		Offset:     offset,
		NextOffset: nextOffset,
		Complete:   complete,
	}, nil
}

// ExportRecording returns a whole transcript as asciicast v2 text. Exporting is
// audited: it is the moment a transcript leaves the supervising host.
func (c *Controller) ExportRecording(id, operator string) (string, error) {
	store := c.recordingStore()
	if store == nil {
		return "", errRecordingUnavailable
	}

	id = strings.TrimSpace(id)
	metadata, err := store.Get(id)
	if err != nil {
		return "", err
	}
	if metadata.Bytes > maxRecordingExportBytes {
		return "", errors.New("recording is too large to export in one response, replay it in the panel instead")
	}

	reader, err := store.Open(id)
	if err != nil {
		return "", err
	}
	defer reader.Close()

	content, err := io.ReadAll(io.LimitReader(reader, maxRecordingExportBytes+1))
	if err != nil {
		return "", err
	}
	if len(content) > maxRecordingExportBytes {
		return "", errors.New("recording is too large to export in one response, replay it in the panel instead")
	}

	c.recordRecordingAudit(audit.KindRecordingExported, "recording_exported", metadata, operator)
	return string(content), nil
}

// DeleteRecording permanently removes a transcript. A recording still being
// written is refused by the store rather than deleted underneath its writer.
func (c *Controller) DeleteRecording(id, operator string) error {
	store := c.recordingStore()
	if store == nil {
		return errRecordingUnavailable
	}

	id = strings.TrimSpace(id)
	metadata, err := store.Get(id)
	if err != nil {
		return err
	}
	if err := store.Delete(id); err != nil {
		return err
	}

	c.recordRecordingAudit(audit.KindRecordingDeleted, "recording_deleted", metadata, operator)
	c.broadcast(eventRecording, RecordingEvent{
		Action:    "deleted",
		Recording: recordingView(metadata),
	})
	return nil
}

// recordRecordingAudit attributes a transcript action to the operator who took
// it. The transcript's own content never reaches the journal: only its identity
// and shape do.
func (c *Controller) recordRecordingAudit(kind audit.Kind, reason string, metadata record.Metadata, operator string) {
	c.mu.RLock()
	rt := c.runtime
	c.mu.RUnlock()

	if rt == nil {
		return
	}
	if strings.TrimSpace(operator) == "" {
		operator = "operator"
	}

	_ = rt.RecordAudit(audit.Entry{
		Kind:       kind,
		SessionID:  metadata.SessionID,
		AgentID:    metadata.AgentID,
		Backend:    strings.ToLower(strings.TrimSpace(metadata.Backend)),
		Adapter:    strings.ToLower(strings.TrimSpace(metadata.Adapter)),
		DecisionBy: audit.DecisionByHuman,
		Operator:   operator,
		Outcome:    audit.OutcomeApplied,
		Reason:     reason,
		Metadata: map[string]string{
			"operator":     operator,
			"recording_id": metadata.ID,
		},
	})
}

// announceFinishedRecording tells connected clients that a session's transcript
// is complete, so the panel can offer a replay without polling the store.
//
// The recorder closes the transcript asynchronously after the exit, so the
// sidecar may still say "active" at the instant the exit reaches here. The
// announcement carries whatever the store currently knows and a later List
// reconciles it; it is a hint, not a source of truth.
func (c *Controller) announceFinishedRecording(sessionID string) {
	store := c.recordingStore()
	if store == nil {
		return
	}

	entries, err := store.List()
	if err != nil {
		return
	}

	c.mu.RLock()
	runID := c.runID
	c.mu.RUnlock()

	var newest record.Metadata
	for _, entry := range entries {
		if !strings.EqualFold(entry.SessionID, sessionID) || entry.RunID != runID {
			continue
		}
		if newest.ID == "" || entry.StartedAt.After(newest.StartedAt) {
			newest = entry
		}
	}
	if newest.ID == "" {
		return
	}

	c.broadcast(eventRecording, RecordingEvent{
		Action:    "finished",
		Recording: recordingView(newest),
	})
}
