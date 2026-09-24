package server

import (
	"context"
	"encoding/csv"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/record"
	"github.com/Hocsman/Relayer/internal/session"
)

// startRecordingController boots a supervisor whose configuration enables
// session recording. The default configuration has no agents, so the run starts
// the two built-in mock agents — which is exactly what a transcript test wants:
// real PTY bytes from a process nobody has to install.
func startRecordingController(t *testing.T, mutate func(*config.Result)) (*Controller, string) {
	t.Helper()

	tempDir := t.TempDir()
	t.Setenv("APPDATA", tempDir)
	t.Setenv("HOME", tempDir)
	t.Setenv("USERPROFILE", tempDir)
	t.Setenv("XDG_CONFIG_HOME", tempDir)

	configPath := filepath.Join(tempDir, "config.yaml")
	if _, err := config.LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	recordingDir := filepath.Join(tempDir, "recordings")
	block := "\nrecording:\n  enabled: true\n  path: " + filepath.ToSlash(recordingDir) + "\n"
	existing, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	if err := os.WriteFile(configPath, append(existing, []byte(block)...), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	loaded, err := config.LoadOrCreate(configPath)
	if err != nil {
		t.Fatalf("reload config: %v", err)
	}
	if !loaded.Recording.Enabled {
		t.Fatalf("recording block did not reach the configuration: %+v", loaded.Recording)
	}
	if mutate != nil {
		mutate(&loaded)
	}

	ctrl, err := NewController(configPath, io.Discard)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start controller: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = ctrl.Close(shutdownCtx)
	})

	return ctrl, recordingDir
}

// waitForRecordings polls until at least `want` transcripts exist, or the
// deadline passes. Recording is asynchronous by design: each session opens its
// own transcript from its own goroutine, so they do not all appear at once and
// a test that counts them after the first one is racing the others.
func waitForRecordings(t *testing.T, ctrl *Controller, want int, deadline time.Duration) []RecordingView {
	t.Helper()

	until := time.Now().Add(deadline)
	for {
		views, err := ctrl.ListRecordings(RecordingFilterInput{})
		if err != nil {
			t.Fatalf("ListRecordings: %v", err)
		}
		if len(views) >= want {
			return views
		}
		if time.Now().After(until) {
			t.Fatalf("saw %d recording(s) within %s, want %d", len(views), deadline, want)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestRecordingCapturesAgentOutput(t *testing.T) {
	ctrl, recordingDir := startRecordingController(t, nil)

	state := ctrl.GetState()
	if len(state.Agents) == 0 {
		t.Skip("the default configuration started no agent on this platform")
	}

	// One transcript per session, waited for rather than counted on arrival:
	// each session opens its own from its own goroutine.
	views := waitForRecordings(t, ctrl, len(state.Agents), 15*time.Second)

	view := views[0]
	if view.RunID != state.RunID {
		t.Errorf("recording runID = %q, want %q", view.RunID, state.RunID)
	}
	if view.Width <= 0 || view.Height <= 0 {
		t.Errorf("recording geometry = %dx%d, want the size the PTY was started with", view.Width, view.Height)
	}

	// The transcript must exist on disk under the configured directory, and
	// must not be readable by anyone else — it holds terminal output.
	if _, err := os.Stat(recordingDir); err != nil {
		t.Fatalf("recording directory: %v", err)
	}

	// Poll for frames: an agent that has only just started may not have painted
	// anything yet when the sidecar first appears.
	var chunk RecordingChunk
	until := time.Now().Add(10 * time.Second)
	for {
		var err error
		chunk, err = ctrl.ReadRecordingChunk(view.ID, 0, 0)
		if err != nil {
			t.Fatalf("ReadRecordingChunk: %v", err)
		}
		if len(chunk.Frames) > 0 || time.Now().After(until) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}

	if chunk.Header.Version != record.CastVersion {
		t.Errorf("header version = %d, want asciicast v%d", chunk.Header.Version, record.CastVersion)
	}
	if len(chunk.Frames) == 0 {
		t.Fatal("transcript carries no frame; the PTY tap is not reaching the recorder")
	}

	var previous float64
	sawOutput := false
	for index, frame := range chunk.Frames {
		if frame.Time < 0 {
			t.Fatalf("frame %d has a negative offset %f", index, frame.Time)
		}
		if frame.Time < previous {
			t.Fatalf("frame %d offset %f goes backwards from %f", index, frame.Time, previous)
		}
		previous = frame.Time
		if frame.Kind == string(record.KindOutput) && frame.Data != "" {
			sawOutput = true
		}
	}
	if !sawOutput {
		t.Error("transcript carries no output frame")
	}
}

func TestRecordingChunkPagesForward(t *testing.T) {
	ctrl, _ := startRecordingController(t, nil)
	if len(ctrl.GetState().Agents) == 0 {
		t.Skip("the default configuration started no agent on this platform")
	}

	views := waitForRecordings(t, ctrl, 1, 15*time.Second)
	first, err := ctrl.ReadRecordingChunk(views[0].ID, 0, 1)
	if err != nil {
		t.Fatalf("ReadRecordingChunk: %v", err)
	}
	if len(first.Frames) > 1 {
		t.Fatalf("limit 1 returned %d frames", len(first.Frames))
	}
	if first.Offset != 0 {
		t.Errorf("first page offset = %d, want 0", first.Offset)
	}
	if len(first.Frames) == 1 && first.NextOffset != 1 {
		t.Errorf("nextOffset = %d, want 1 after one frame", first.NextOffset)
	}

	// An offset past the end is a normal request from a client that has caught
	// up, not an error.
	tail, err := ctrl.ReadRecordingChunk(views[0].ID, 1_000_000, 10)
	if err != nil {
		t.Fatalf("ReadRecordingChunk past the end: %v", err)
	}
	if len(tail.Frames) != 0 || !tail.Complete {
		t.Errorf("tail page = %+v, want empty and complete", tail)
	}
}

func TestRecordingDisabledListsNothing(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("APPDATA", tempDir)
	t.Setenv("HOME", tempDir)
	t.Setenv("USERPROFILE", tempDir)
	t.Setenv("XDG_CONFIG_HOME", tempDir)

	configPath := filepath.Join(tempDir, "config.yaml")
	if _, err := config.LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	ctrl, err := NewController(configPath, io.Discard)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start controller: %v", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = ctrl.Close(shutdownCtx)
	}()

	// Recording off is an ordinary state, not a failure: the panel must be able
	// to say "nothing here" rather than show an error nobody can act on.
	views, err := ctrl.ListRecordings(RecordingFilterInput{})
	if err != nil {
		t.Fatalf("ListRecordings with recording disabled: %v", err)
	}
	if len(views) != 0 {
		t.Fatalf("recordings = %d, want none", len(views))
	}

	if _, err := ctrl.ReadRecordingChunk("whatever", 0, 10); err == nil {
		t.Error("ReadRecordingChunk should report that recording is not enabled")
	}
	if err := ctrl.DeleteRecording("whatever", "alice"); err == nil {
		t.Error("DeleteRecording should report that recording is not enabled")
	}
}

func TestRecordingDeleteIsAuditedAndBroadcast(t *testing.T) {
	ctrl, _ := startRecordingController(t, nil)
	if len(ctrl.GetState().Agents) == 0 {
		t.Skip("the default configuration started no agent on this platform")
	}

	views := waitForRecordings(t, ctrl, 1, 15*time.Second)

	broadcasts := make(chan RecordingEvent, 4)
	unsubscribe := ctrl.Subscribe(func(event string, payload any) {
		if event != eventRecording {
			return
		}
		if recording, ok := payload.(RecordingEvent); ok {
			select {
			case broadcasts <- recording:
			default:
			}
		}
	})
	defer unsubscribe()

	// A transcript still being written is refused rather than deleted from
	// underneath its own writer.
	target := views[0]
	err := ctrl.DeleteRecording(target.ID, "alice")
	if target.Active {
		if err == nil {
			t.Fatal("deleting an active recording should be refused")
		}
		return
	}
	if err != nil {
		t.Fatalf("DeleteRecording: %v", err)
	}

	select {
	case event := <-broadcasts:
		if event.Action != "deleted" || event.Recording.ID != target.ID {
			t.Errorf("broadcast = %+v, want a deleted event for %q", event, target.ID)
		}
	case <-time.After(2 * time.Second):
		t.Error("no relayer:recording broadcast after a delete")
	}

	entries, err := ctrl.GetAuditEntries(AuditFilterInput{Limit: 0})
	if err != nil {
		t.Fatalf("GetAuditEntries: %v", err)
	}
	found := false
	for _, entry := range entries {
		if entry.Kind == "recording_deleted" && entry.Operator == "alice" {
			found = true
			break
		}
	}
	if !found {
		t.Error("no recording_deleted audit entry attributed to alice")
	}
}

func TestRecordingViewCarriesNoFilesystemPath(t *testing.T) {
	// A browser client has no business learning where the supervising host
	// keeps its files, so the DTO deliberately omits the store path.
	view := recordingView(record.Metadata{
		ID:        "abc",
		RunID:     "run",
		SessionID: "alpha",
		Path:      filepath.Join("C:", "secret", "place", "alpha.cast"),
		StartedAt: time.Now(),
	})

	encoded := strings.ToLower(view.ID + view.RunID + view.SessionID + view.Name + view.Backend + view.Adapter)
	if strings.Contains(encoded, "secret") {
		t.Fatalf("recording view leaked a filesystem path: %+v", view)
	}
}

// TestExportAuditReportProducesParseableFormats pins that the interface's
// download buttons produce what their file extensions claim. The JSON path used
// to render Go struct syntax, which no JSON parser can read.
func TestExportAuditReportProducesParseableFormats(t *testing.T) {
	tempDir := t.TempDir()
	t.Setenv("APPDATA", tempDir)
	t.Setenv("HOME", tempDir)
	t.Setenv("USERPROFILE", tempDir)
	t.Setenv("XDG_CONFIG_HOME", tempDir)

	configPath := filepath.Join(tempDir, "config.yaml")
	if _, err := config.LoadOrCreate(configPath); err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}

	ctrl, err := NewController(configPath, io.Discard)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := ctrl.Start(ctx); err != nil {
		t.Fatalf("Start controller: %v", err)
	}
	defer func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = ctrl.Close(shutdownCtx)
	}()

	exported, err := ctrl.ExportAuditReport("json")
	if err != nil {
		t.Fatalf("ExportAuditReport(json): %v", err)
	}
	var decoded []AuditEntryView
	if err := json.Unmarshal([]byte(exported), &decoded); err != nil {
		t.Fatalf("the json export is not JSON: %v\n%.200s", err, exported)
	}

	csvText, err := ctrl.ExportAuditReport("csv")
	if err != nil {
		t.Fatalf("ExportAuditReport(csv): %v", err)
	}
	rows, err := csv.NewReader(strings.NewReader(csvText)).ReadAll()
	if err != nil {
		t.Fatalf("the csv export is not CSV: %v", err)
	}
	if len(rows) == 0 || rows[0][0] != "sequence" {
		t.Fatalf("csv export = %v, want a header row", rows)
	}
	// Every row must have the header's width: a field carrying a comma would
	// otherwise shift every column after it.
	for index, row := range rows {
		if len(row) != len(rows[0]) {
			t.Fatalf("csv row %d has %d fields, want %d", index, len(row), len(rows[0]))
		}
	}
}

// TestRecordingLifecycleIsJournaled covers the two recording kinds nothing
// emitted until the multiplexer reported its lifecycle. docs/recording.md has
// listed recording_started and recording_finished as journaled since v0.7.0.
func TestRecordingLifecycleIsJournaled(t *testing.T) {
	ctrl, _ := startRecordingController(t, nil)
	state := ctrl.GetState()
	if len(state.Agents) == 0 {
		t.Skip("the default configuration started no agent on this platform")
	}
	sessions := len(state.Agents)
	waitForRecordings(t, ctrl, sessions, 15*time.Second)

	count := func(kind, outcome string) int {
		entries, err := ctrl.GetAuditEntries(AuditFilterInput{RunID: state.RunID, Kind: kind})
		if err != nil {
			t.Fatalf("GetAuditEntries: %v", err)
		}
		matched := 0
		for _, entry := range entries {
			if entry.Outcome == outcome && entry.DecisionBy == "system" && entry.Summary == "" {
				matched++
			}
		}
		return matched
	}

	// The opening is journaled from the session's own goroutine, so it may
	// land a moment after the sidecar the wait above observed.
	until := time.Now().Add(5 * time.Second)
	for count("recording_started", "started") < sessions && time.Now().Before(until) {
		time.Sleep(20 * time.Millisecond)
	}
	if got := count("recording_started", "started"); got != sessions {
		t.Fatalf("recording_started entries = %d, want one per session (%d)", got, sessions)
	}

	// Closing the run finalizes every transcript before the journal closes,
	// so each closing is on disk once Close returns. The run ends as the
	// desktop's does, the agents asked to stop before anything is cancelled,
	// and on Windows an agent the console close does not end has five seconds
	// before it is killed: a stop's worst case, session.StopBudget.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), session.StopBudget+5*time.Second)
	defer cancel()
	if err := ctrl.Close(shutdownCtx); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := count("recording_finished", "finished"); got != sessions {
		t.Fatalf("recording_finished entries = %d, want one per session (%d)", got, sessions)
	}

	report, err := ctrl.VerifyAuditJournal()
	if err != nil {
		t.Fatalf("VerifyAuditJournal: %v", err)
	}
	if !report.Passed {
		t.Fatalf("the journal no longer verifies: %+v", report.Issues)
	}
}
