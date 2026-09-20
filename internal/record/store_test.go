package record

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func testConfig(t *testing.T) Config {
	t.Helper()
	config := DefaultConfig()
	config.Enabled = true
	// A directory the store creates itself, not t.TempDir() directly: temporary
	// directories are 0755 on Unix and the store refuses a transcript directory
	// anyone else can read. internal/audit's tests are shaped the same way.
	config.Path = filepath.Join(t.TempDir(), "recordings")
	config.MaxFileSizeMB = 1
	config.MaxTotalSizeMB = 1
	config.RetentionDays = 0
	return config
}

func openTestStore(t *testing.T, config Config) *Store {
	t.Helper()
	store, err := OpenStore(config)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = store.Close() })
	return store
}

func createRecording(t *testing.T, store *Store, sessionID string, startedAt time.Time) (Metadata, *Writer) {
	t.Helper()
	metadata, writer, err := store.Create(CreateOptions{
		RunID:     "run-1",
		SessionID: sessionID,
		Name:      sessionID,
		Backend:   "pty",
		Adapter:   "claude",
		Width:     80,
		Height:    24,
		StartedAt: startedAt,
	})
	if err != nil {
		t.Fatalf("create recording %s: %v", sessionID, err)
	}
	return metadata, writer
}

func TestRecordingIDIsFilesystemSafe(t *testing.T) {
	at := time.Unix(0, 1_700_000_000_000_000_000).UTC()
	id := RecordingID("run/../1", "agent one:*?", at)
	if strings.ContainsAny(id, `/\:*?"<>| `) {
		t.Fatalf("id %q holds an unsafe character", id)
	}
	if !strings.HasSuffix(id, "1700000000000000000") {
		t.Fatalf("id %q does not end with the start instant", id)
	}
	if RecordingID("", "", time.Time{}) != "unknown-unknown-0" {
		t.Fatalf("empty identities = %q", RecordingID("", "", time.Time{}))
	}
	if RecordingID("run", "a", at) == RecordingID("run", "a", at.Add(time.Nanosecond)) {
		t.Fatal("two start instants share one identity")
	}
}

func TestStoreCreatesTheDocumentedLayout(t *testing.T) {
	config := testConfig(t)
	store := openTestStore(t, config)
	metadata, writer := createRecording(t, store, "session-a", testBase)

	if metadata.Path != filepath.Join(config.Path, "run-1", "session-a-"+timeSlug(testBase)+castSuffix) {
		t.Fatalf("transcript path = %s", metadata.Path)
	}
	sidecar := strings.TrimSuffix(metadata.Path, castSuffix) + metadataSuffix
	if _, err := os.Stat(sidecar); err != nil {
		t.Fatalf("sidecar is missing: %v", err)
	}
	if err := writer.WriteOutput(testBase.Add(time.Second), []byte("hello")); err != nil {
		t.Fatalf("write output: %v", err)
	}
	finished, err := store.Finish(metadata.ID, testBase.Add(2*time.Second), intPointer(0))
	if err != nil {
		t.Fatalf("finish: %v", err)
	}
	if finished.Active || finished.EndedAt.IsZero() || finished.Truncated {
		t.Fatalf("finished metadata = %+v", finished)
	}
	if finished.Frames != 1 || finished.Bytes == 0 {
		t.Fatalf("finished counters = %+v", finished)
	}
	if finished.ExitCode == nil || *finished.ExitCode != 0 {
		t.Fatalf("exit code = %v", finished.ExitCode)
	}
	if err := ValidateFile(finished.Path); err != nil {
		t.Fatalf("transcript is invalid: %v", err)
	}
}

func TestStoreOpenReturnsTheTranscript(t *testing.T) {
	store := openTestStore(t, testConfig(t))
	metadata, writer := createRecording(t, store, "session-open", testBase)
	if err := writer.WriteOutput(testBase, []byte("payload")); err != nil {
		t.Fatalf("write output: %v", err)
	}
	if _, err := store.Finish(metadata.ID, testBase.Add(time.Second), nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	reader, err := store.Open(metadata.ID)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer func() { _ = reader.Close() }()
	content, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if !strings.Contains(string(content), `"payload"`) {
		t.Fatalf("transcript = %q", content)
	}
	if _, err := store.Open("absent"); !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("open absent = %v, want %v", err, ErrRecordingNotFound)
	}
}

func TestStoreReportsACrashedRecordingAsTruncated(t *testing.T) {
	config := testConfig(t)
	store := openTestStore(t, config)
	metadata, writer := createRecording(t, store, "session-crash", testBase)
	if err := writer.WriteOutput(testBase, []byte("partial")); err != nil {
		t.Fatalf("write output: %v", err)
	}
	// A crash closes the file without ever rewriting the sidecar.
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}

	recovered := openTestStore(t, config)
	listed, err := recovered.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 {
		t.Fatalf("listed %d recordings, want 1", len(listed))
	}
	if listed[0].ID != metadata.ID {
		t.Fatalf("id = %s, want %s", listed[0].ID, metadata.ID)
	}
	if listed[0].Active {
		t.Fatal("a crashed recording is reported as active")
	}
	if !listed[0].Truncated {
		t.Fatal("a crashed recording is not reported as truncated")
	}
	if !listed[0].EndedAt.IsZero() {
		t.Fatalf("crashed recording has an end instant %v", listed[0].EndedAt)
	}
	if listed[0].Bytes == 0 {
		t.Fatal("crashed recording reports no bytes")
	}
}

func TestStoreListsAnOpenRecordingAsActive(t *testing.T) {
	store := openTestStore(t, testConfig(t))
	metadata, writer := createRecording(t, store, "session-live", testBase)
	if err := writer.WriteOutput(testBase, []byte("live")); err != nil {
		t.Fatalf("write output: %v", err)
	}
	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 1 || !listed[0].Active || listed[0].Truncated {
		t.Fatalf("listed = %+v", listed)
	}
	if listed[0].Frames != 1 {
		t.Fatalf("frames = %d, want 1", listed[0].Frames)
	}
	if _, err := store.Finish(metadata.ID, testBase.Add(time.Second), nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestStoreListsNewestFirst(t *testing.T) {
	store := openTestStore(t, testConfig(t))
	for index := 0; index < 3; index++ {
		startedAt := testBase.Add(time.Duration(index) * time.Hour)
		metadata, _ := createRecording(t, store, "session", startedAt)
		if _, err := store.Finish(metadata.ID, startedAt.Add(time.Minute), nil); err != nil {
			t.Fatalf("finish %d: %v", index, err)
		}
	}
	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 3 {
		t.Fatalf("listed %d recordings, want 3", len(listed))
	}
	for index := 1; index < len(listed); index++ {
		if listed[index].StartedAt.After(listed[index-1].StartedAt) {
			t.Fatalf("recordings are not newest first: %v", listed)
		}
	}
}

func TestStoreDeleteRefusesAnOpenRecording(t *testing.T) {
	store := openTestStore(t, testConfig(t))
	metadata, _ := createRecording(t, store, "session-busy", testBase)
	if err := store.Delete(metadata.ID); !errors.Is(err, ErrRecordingActive) {
		t.Fatalf("delete open = %v, want %v", err, ErrRecordingActive)
	}
	if _, err := store.Finish(metadata.ID, testBase.Add(time.Second), nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := store.Delete(metadata.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := os.Stat(metadata.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("transcript survived deletion: %v", err)
	}
	if err := store.Delete(metadata.ID); !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("second delete = %v, want %v", err, ErrRecordingNotFound)
	}
}

func TestPruneRemovesTheOldestOverTheCountLimit(t *testing.T) {
	config := testConfig(t)
	config.MaxRecordings = 2
	store := openTestStore(t, config)
	kept := make([]string, 0, 4)
	for index := 0; index < 4; index++ {
		startedAt := testBase.Add(time.Duration(index) * time.Hour)
		metadata, _ := createRecording(t, store, "session", startedAt)
		if _, err := store.Finish(metadata.ID, startedAt.Add(time.Minute), nil); err != nil {
			t.Fatalf("finish %d: %v", index, err)
		}
		kept = append(kept, metadata.ID)
	}
	removed, err := store.Prune()
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 2 {
		t.Fatalf("removed = %d, want 2", removed)
	}
	listed, err := store.List()
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(listed) != 2 {
		t.Fatalf("listed %d recordings, want 2", len(listed))
	}
	surviving := map[string]bool{listed[0].ID: true, listed[1].ID: true}
	if !surviving[kept[2]] || !surviving[kept[3]] {
		t.Fatalf("prune removed the wrong recordings: %v", surviving)
	}
}

func TestPruneSkipsARecordingOpenForWriting(t *testing.T) {
	config := testConfig(t)
	config.MaxRecordings = 1
	store := openTestStore(t, config)
	open, writer := createRecording(t, store, "session-open", testBase)
	for index := 0; index < 3; index++ {
		startedAt := testBase.Add(time.Duration(index+1) * time.Hour)
		metadata, _ := createRecording(t, store, "session", startedAt)
		if _, err := store.Finish(metadata.ID, startedAt.Add(time.Minute), nil); err != nil {
			t.Fatalf("finish %d: %v", index, err)
		}
	}
	if _, err := store.Prune(); err != nil {
		t.Fatalf("prune: %v", err)
	}
	if _, err := os.Stat(open.Path); err != nil {
		t.Fatalf("prune removed the live transcript: %v", err)
	}
	// The live writer must still work after a prune.
	if err := writer.WriteOutput(testBase.Add(time.Second), []byte("still alive")); err != nil {
		t.Fatalf("write after prune: %v", err)
	}
	if _, err := store.Finish(open.ID, testBase.Add(2*time.Second), nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
}

func TestPruneRemovesExpiredRecordings(t *testing.T) {
	config := testConfig(t)
	config.RetentionDays = 7
	config.MaxRecordings = 100
	store := openTestStore(t, config)
	store.clock = func() time.Time { return testBase }
	old, _ := createRecording(t, store, "session-old", testBase.AddDate(0, 0, -30))
	if _, err := store.Finish(old.ID, testBase.AddDate(0, 0, -30), nil); err != nil {
		t.Fatalf("finish old: %v", err)
	}
	recent, _ := createRecording(t, store, "session-recent", testBase.AddDate(0, 0, -1))
	if _, err := store.Finish(recent.ID, testBase.AddDate(0, 0, -1), nil); err != nil {
		t.Fatalf("finish recent: %v", err)
	}
	removed, err := store.Prune()
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed != 1 {
		t.Fatalf("removed = %d, want 1", removed)
	}
	if _, err := store.Get(old.ID); !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("expired recording survived: %v", err)
	}
	if _, err := store.Get(recent.ID); err != nil {
		t.Fatalf("recent recording was removed: %v", err)
	}
}

func TestPruneRemovesTheOldestOverTheTotalSizeLimit(t *testing.T) {
	config := testConfig(t)
	config.MaxRecordings = 100
	store := openTestStore(t, config)
	payload := []byte(strings.Repeat("z", 64*1024))
	ids := make([]string, 0, 3)
	for index := 0; index < 3; index++ {
		startedAt := testBase.Add(time.Duration(index) * time.Hour)
		metadata, writer := createRecording(t, store, "session", startedAt)
		for block := 0; block < 8; block++ {
			if err := writer.WriteOutput(startedAt, payload); err != nil {
				t.Fatalf("write block: %v", err)
			}
		}
		if _, err := store.Finish(metadata.ID, startedAt.Add(time.Minute), nil); err != nil {
			t.Fatalf("finish %d: %v", index, err)
		}
		ids = append(ids, metadata.ID)
	}
	removed, err := store.Prune()
	if err != nil {
		t.Fatalf("prune: %v", err)
	}
	if removed == 0 {
		t.Fatal("prune kept more than the total size limit")
	}
	if _, err := store.Get(ids[0]); !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("the oldest recording survived: %v", err)
	}
	if _, err := store.Get(ids[len(ids)-1]); err != nil {
		t.Fatalf("the newest recording was removed: %v", err)
	}
}

func TestStoreRejectsAnUnusableConfiguration(t *testing.T) {
	config := testConfig(t)
	config.MaxRecordings = 0
	if _, err := OpenStore(config); err == nil {
		t.Fatal("accepted an unusable configuration")
	}
}

func TestStoreCloseClosesOpenTranscripts(t *testing.T) {
	store := openTestStore(t, testConfig(t))
	metadata, writer := createRecording(t, store, "session-close", testBase)
	if err := store.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}
	if err := writer.WriteOutput(testBase, []byte("x")); err != ErrClosed {
		t.Fatalf("write after store close = %v, want %v", err, ErrClosed)
	}
	if _, _, err := store.Create(CreateOptions{RunID: "run-1", SessionID: "later", Width: 80, Height: 24}); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("create after close = %v, want %v", err, ErrStoreClosed)
	}
	if _, err := store.Finish(metadata.ID, testBase, nil); !errors.Is(err, ErrRecordingNotFound) {
		t.Fatalf("finish after close = %v, want %v", err, ErrRecordingNotFound)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestNilStoreIsInert(t *testing.T) {
	var store *Store
	if store.Dir() != "" {
		t.Fatal("nil store reported a directory")
	}
	if _, _, err := store.Create(CreateOptions{}); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("create on a nil store = %v", err)
	}
	if _, err := store.List(); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("list on a nil store = %v", err)
	}
	if _, err := store.Prune(); !errors.Is(err, ErrStoreClosed) {
		t.Fatalf("prune on a nil store = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatalf("close a nil store: %v", err)
	}
}

func TestStoreKeepsRecordingsPrivate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits are not the Windows access model")
	}
	config := testConfig(t)
	store := openTestStore(t, config)
	metadata, writer := createRecording(t, store, "session-private", testBase)
	if err := writer.WriteOutput(testBase, []byte("secret")); err != nil {
		t.Fatalf("write output: %v", err)
	}
	if _, err := store.Finish(metadata.ID, testBase.Add(time.Second), nil); err != nil {
		t.Fatalf("finish: %v", err)
	}

	paths := []string{
		metadata.Path,
		strings.TrimSuffix(metadata.Path, castSuffix) + metadataSuffix,
	}
	for _, path := range paths {
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatalf("inspect %s: %v", path, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is not private (permissions %04o)", path, info.Mode().Perm())
		}
		if err := requireCurrentUserOwner(info, path); err != nil {
			t.Fatalf("ownership of %s: %v", path, err)
		}
	}
	for _, directory := range []string{config.Path, filepath.Dir(metadata.Path)} {
		info, err := os.Lstat(directory)
		if err != nil {
			t.Fatalf("inspect %s: %v", directory, err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("%s is not private (permissions %04o)", directory, info.Mode().Perm())
		}
	}
}

func TestEnsurePrivateDirectoryRejectsAFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(path, []byte("x"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	if err := ensurePrivateDirectory(path); err == nil {
		t.Fatal("accepted a file as a recording directory")
	}
}

func timeSlug(at time.Time) string {
	return strings.TrimPrefix(sessionSlug("", at), "unknown-")
}

func intPointer(value int) *int { return &value }

func TestPruneNeverRemovesATranscriptBeingWritten(t *testing.T) {
	config := testConfig(t)
	config.MaxRecordings = 1
	store := openTestStore(t, config)
	open, writer := createRecording(t, store, "session-live", testBase)
	for index := 0; index < 6; index++ {
		startedAt := testBase.Add(time.Duration(index+1) * time.Hour)
		metadata, _ := createRecording(t, store, "session-old", startedAt)
		if _, err := store.Finish(metadata.ID, startedAt.Add(time.Minute), nil); err != nil {
			t.Fatalf("finish %d: %v", index, err)
		}
	}

	var group sync.WaitGroup
	group.Add(2)
	go func() {
		defer group.Done()
		for pass := 0; pass < 50; pass++ {
			if _, err := store.Prune(); err != nil {
				t.Errorf("prune pass %d: %v", pass, err)
				return
			}
		}
	}()
	go func() {
		defer group.Done()
		for step := 0; step < 500; step++ {
			at := testBase.Add(time.Duration(step) * time.Millisecond)
			if err := writer.WriteOutput(at, []byte("live output")); err != nil {
				t.Errorf("write during prune at step %d: %v", step, err)
				return
			}
		}
	}()
	group.Wait()

	if _, err := os.Stat(open.Path); err != nil {
		t.Fatalf("prune removed the live transcript: %v", err)
	}
	metadata, err := store.Finish(open.ID, testBase.Add(time.Minute), nil)
	if err != nil {
		t.Fatalf("finish the live recording: %v", err)
	}
	if metadata.Frames != 500 {
		t.Fatalf("frames = %d, want 500", metadata.Frames)
	}
	if err := ValidateFile(open.Path); err != nil {
		t.Fatalf("live transcript is invalid: %v", err)
	}
}

func TestSlugNeverProducesAWindowsDeviceName(t *testing.T) {
	// os.MkdirAll refuses a reserved name, which would silently disable
	// recording for a whole run instead of degrading one transcript.
	for _, value := range []string{"nul", "CON", "Aux", "prn", "com1", "LPT9", "nul!"} {
		if name := slug(value); isReservedName(name) {
			t.Fatalf("slug(%q) = %q, still a device name", value, name)
		}
	}
	for _, value := range []string{"console", "com", "com10", "lpt0", "run-1", "nulls"} {
		if name := slug(value); strings.HasSuffix(name, "_") {
			t.Fatalf("slug(%q) = %q, an ordinary name was escaped", value, name)
		}
	}
	store := openTestStore(t, testConfig(t))
	metadata, writer, err := store.Create(CreateOptions{
		RunID:     "nul",
		SessionID: "con",
		Width:     80,
		Height:    24,
		StartedAt: testBase,
	})
	if err != nil {
		t.Fatalf("create under a reserved identity: %v", err)
	}
	if err := writer.WriteOutput(testBase, []byte("device name")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if _, err := store.Finish(metadata.ID, testBase.Add(time.Second), nil); err != nil {
		t.Fatalf("finish: %v", err)
	}
	if err := ValidateFile(metadata.Path); err != nil {
		t.Fatalf("transcript is invalid: %v", err)
	}
}
