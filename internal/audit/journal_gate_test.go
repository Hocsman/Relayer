package audit

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeJournalLine(t *testing.T, path string, terminated bool) {
	t.Helper()
	payload, err := json.Marshal(Entry{
		SchemaVersion: CurrentSchemaVersion,
		Sequence:      1,
		Timestamp:     time.Unix(1756382400, 0).UTC(),
		EntryID:       "entry-1",
		RunID:         "run-1",
		Kind:          KindRunStarted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if terminated {
		payload = append(payload, '\n')
	}
	if err := os.WriteFile(path, payload, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The sink takes ownership of the path it opens: it truncates a partial
// trailing line, and rotation removes surplus generations of the same base
// name. Pointing audit.path at an ordinary document therefore destroyed it —
// a file with no newline at all was truncated to nothing — and on a private
// home directory nothing else in the pipeline objected.
func TestOpenRefusesAndPreservesAForeignFileAtTheAuditPath(t *testing.T) {
	directory := t.TempDir()
	if err := os.Chmod(directory, 0o700); err != nil {
		t.Fatal(err)
	}

	notes := filepath.Join(directory, "notes.txt")
	const content = "meeting notes: ship the release on friday"
	if err := os.WriteFile(notes, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	// Neighbours that rotation matches purely by name. The second is beyond
	// max_files and used to be removed outright.
	companions := map[string]string{
		filepath.Join(directory, "notes.txt.1"): "older notes",
		filepath.Join(directory, "notes.txt.7"): "much older notes",
	}
	for path, body := range companions {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	recorder, err := Open(Config{
		Enabled: true, Mode: ModeMetadata, Path: notes, MaxFileSizeMB: 10, MaxFiles: 5,
	})
	if err == nil {
		_ = recorder.Close()
		t.Fatal("Open accepted an ordinary document as an audit journal")
	}
	if !errors.Is(err, ErrNotAuditJournal) {
		t.Fatalf("Open error = %v, want ErrNotAuditJournal", err)
	}

	preserved, readErr := os.ReadFile(notes)
	if readErr != nil {
		t.Fatalf("the foreign file was removed: %v", readErr)
	}
	if string(preserved) != content {
		t.Fatalf("the foreign file was modified: %q", string(preserved))
	}
	for path, body := range companions {
		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("companion %s was removed: %v", filepath.Base(path), err)
		}
		if string(got) != body {
			t.Fatalf("companion %s was modified: %q", filepath.Base(path), string(got))
		}
	}
}

// An empty file is a legitimate fresh journal and must still be accepted, as
// must a real one, including one whose last line was never terminated.
func TestOpenAcceptsEmptyAndGenuineJournals(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(t *testing.T, path string)
	}{
		{name: "absent", setup: func(*testing.T, string) {}},
		{name: "empty", setup: func(t *testing.T, path string) {
			if err := os.WriteFile(path, nil, 0o600); err != nil {
				t.Fatal(err)
			}
		}},
		{name: "terminated line", setup: func(t *testing.T, path string) {
			writeJournalLine(t, path, true)
		}},
		{name: "unterminated line", setup: func(t *testing.T, path string) {
			writeJournalLine(t, path, false)
		}},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.Chmod(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "audit.jsonl")
			test.setup(t, path)

			recorder, err := Open(Config{
				Enabled: true, Mode: ModeMetadata, Path: path, MaxFileSizeMB: 10, MaxFiles: 5,
			})
			if err != nil {
				t.Fatalf("Open rejected a valid journal: %v", err)
			}
			if err := recorder.Close(); err != nil {
				t.Fatalf("Close: %v", err)
			}
		})
	}
}

func TestRequireRelayerJournalRecognition(t *testing.T) {
	valid, err := json.Marshal(Entry{
		SchemaVersion: CurrentSchemaVersion, Sequence: 1,
		EntryID: "e", RunID: "r", Kind: KindRunStarted,
	})
	if err != nil {
		t.Fatal(err)
	}

	for _, test := range []struct {
		name    string
		content string
		wantErr bool
	}{
		{name: "empty", content: ""},
		{name: "entry", content: string(valid)},
		{name: "entry with following lines", content: string(valid) + "\n{\"schema_version\":1}\n"},
		{name: "plain text", content: "meeting notes", wantErr: true},
		{name: "json but not an entry", content: `{"hello":"world"}` + "\n", wantErr: true},
		{name: "entry without kind", content: `{"schema_version":1,"entry_id":"e"}` + "\n", wantErr: true},
		{name: "entry with unknown kind", content: `{"schema_version":1,"kind":"whatever"}` + "\n", wantErr: true},
		{name: "future schema version", content: `{"schema_version":99,"kind":"run_started"}` + "\n", wantErr: true},
		{name: "yaml document", content: "version: 1\nbackend: pty\n", wantErr: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "candidate")
			if err := os.WriteFile(path, []byte(test.content), 0o600); err != nil {
				t.Fatal(err)
			}
			file, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = file.Close() }()
			info, err := file.Stat()
			if err != nil {
				t.Fatal(err)
			}

			err = requireRelayerJournal(file, info.Size())
			if test.wantErr && !errors.Is(err, ErrNotAuditJournal) {
				t.Fatalf("requireRelayerJournal = %v, want ErrNotAuditJournal", err)
			}
			if !test.wantErr && err != nil {
				t.Fatalf("requireRelayerJournal = %v, want nil", err)
			}
		})
	}
}
