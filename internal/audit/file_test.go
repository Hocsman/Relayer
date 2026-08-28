package audit

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestOpenUsesEffectivePathOptionsAndWritesJSONL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "audit.jsonl")
	config := DefaultConfig()
	config.Mode = ModeDetailed
	config.Path = path
	ids := sequentialIDGenerator()
	now := time.Date(2026, 8, 26, 8, 0, 0, 0, time.UTC)
	recorder, err := Open(config, WithClock(func() time.Time { return now }), WithIDGenerator(ids))
	if err != nil {
		t.Fatal(err)
	}
	absolute, _ := filepath.Abs(path)
	if recorder.Path() != filepath.Clean(absolute) || recorder.RunID() != "id-1" || !recorder.Enabled() {
		t.Fatalf("Open recorder = path %q run %q enabled %t", recorder.Path(), recorder.RunID(), recorder.Enabled())
	}
	if err := recorder.Record(Entry{Kind: KindRunStarted, Outcome: OutcomeStarted}); err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(recorder.Path())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(payload), "\n") != 1 {
		t.Fatalf("payload is not one JSONL line: %q", payload)
	}
	var entry Entry
	if err := json.Unmarshal(payload, &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Kind != KindRunStarted || entry.EntryID != "id-2" || !entry.Timestamp.Equal(now) {
		t.Fatalf("entry = %#v", entry)
	}

	defaultPath, err := DefaultPath()
	if err != nil {
		t.Fatal(err)
	}
	if !filepath.IsAbs(defaultPath) || !strings.HasSuffix(filepath.ToSlash(defaultPath), "/relayer/audit/audit.jsonl") {
		t.Fatalf("DefaultPath() = %q", defaultPath)
	}
	resolved, err := ResolvePath("")
	if err != nil || resolved != filepath.Clean(defaultPath) {
		t.Fatalf("ResolvePath empty = %q, %v", resolved, err)
	}
}

func TestOpenDisabledDoesNotTouchDiskOrGenerateIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "must-not-exist", "audit.jsonl")
	config := DefaultConfig()
	config.Enabled = false
	config.Path = path
	idCalls := 0
	recorder, err := Open(config, WithIDGenerator(func() (string, error) {
		idCalls++
		return "unexpected", nil
	}))
	if err != nil {
		t.Fatal(err)
	}
	if recorder.Enabled() || idCalls != 0 {
		t.Fatalf("disabled Open = enabled %t id calls %d", recorder.Enabled(), idCalls)
	}
	if _, err := os.Stat(filepath.Dir(path)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("disabled Open touched disk: %v", err)
	}
}

func TestFileSinkRotatesBeforeOverflowAndKeepsMaxFilesTotal(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "audit.jsonl")
	line := func(sequence int) []byte {
		return []byte(fmt.Sprintf(`{"sequence":%d,"pad":"xxxxxxxx"}`+"\n", sequence))
	}
	maximum := int64(len(line(1)) * 2)
	sink, err := NewFileSink(path, maximum, 3)
	if err != nil {
		t.Fatal(err)
	}
	for sequence := 1; sequence <= 2; sequence++ {
		if err := sink.WriteLine(line(sequence)); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := os.Stat(generationPath(path, 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("exact threshold rotated early: %v", err)
	}
	for sequence := 3; sequence <= 7; sequence++ {
		if err := sink.WriteLine(line(sequence)); err != nil {
			t.Fatal(err)
		}
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}

	paths := []string{generationPath(path, 2), generationPath(path, 1), path}
	var retained []int
	for _, candidate := range paths {
		payload, err := os.ReadFile(candidate)
		if err != nil {
			t.Fatalf("read %s: %v", candidate, err)
		}
		for _, raw := range strings.Split(strings.TrimSuffix(string(payload), "\n"), "\n") {
			var value struct {
				Sequence int `json:"sequence"`
			}
			if err := json.Unmarshal([]byte(raw), &value); err != nil {
				t.Fatalf("invalid rotated JSONL %s: %v: %q", candidate, err, raw)
			}
			retained = append(retained, value.Sequence)
		}
	}
	if fmt.Sprint(retained) != "[3 4 5 6 7]" {
		t.Fatalf("retained order = %#v", retained)
	}
	if _, err := os.Stat(generationPath(path, 3)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("maxFiles retained an extra generation: %v", err)
	}
}

func TestFileSinkPrunesGenerationsBeyondConfiguredRetention(t *testing.T) {
	directory := filepath.Join(t.TempDir(), "private")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(directory, "audit.jsonl")
	for index := 0; index <= 5; index++ {
		candidate := path
		if index > 0 {
			candidate = generationPath(path, index)
		}
		// Rotation only ever touches files it recognizes as its own journals.
		generation := []byte("{\"schema_version\":1,\"kind\":\"run_started\"}\n")
		if err := os.WriteFile(candidate, generation, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	sink, err := NewFileSink(path, 1024, 3)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	for index := 0; index <= 2; index++ {
		candidate := path
		if index > 0 {
			candidate = generationPath(path, index)
		}
		if _, err := os.Stat(candidate); err != nil {
			t.Fatalf("retained generation %d: %v", index, err)
		}
	}
	for index := 3; index <= 5; index++ {
		if _, err := os.Stat(generationPath(path, index)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("obsolete generation %d not pruned: %v", index, err)
		}
	}
}

func TestFileSinkRecoversAnIncompleteLastLine(t *testing.T) {
	for _, test := range []struct {
		name     string
		existing string
		want     string
	}{
		{
			// The retained prefix must be a real entry: startup refuses to take
			// ownership of a file it cannot recognize as its own journal.
			name:     "keeps complete prefix",
			existing: "{\"schema_version\":1,\"kind\":\"run_started\",\"sequence\":1}\n{\"schema_version\":",
			want:     "{\"schema_version\":1,\"kind\":\"run_started\",\"sequence\":1}\n{\"sequence\":2}\n",
		},
		{
			// A journal interrupted while writing its very first entry still
			// recovers, because the partial line is unmistakably one of ours.
			name:     "drops wholly partial file",
			existing: "{\"schema_version\":1,\"kind\":\"run_st",
			want:     "{\"sequence\":2}\n",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			directory := filepath.Join(t.TempDir(), "private")
			if err := os.Mkdir(directory, 0o700); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(directory, "audit.jsonl")
			if err := os.WriteFile(path, []byte(test.existing), 0o600); err != nil {
				t.Fatal(err)
			}

			sink, err := NewFileSink(path, 1024, 2)
			if err != nil {
				t.Fatal(err)
			}
			if err := sink.WriteLine([]byte("{\"sequence\":2}\n")); err != nil {
				t.Fatal(err)
			}
			if err := sink.Close(); err != nil {
				t.Fatal(err)
			}

			payload, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if string(payload) != test.want {
				t.Fatalf("recovered JSONL = %q, want %q", payload, test.want)
			}
			for _, line := range strings.Split(strings.TrimSuffix(string(payload), "\n"), "\n") {
				if !json.Valid([]byte(line)) {
					t.Fatalf("recovered line is invalid JSON: %q", line)
				}
			}
		})
	}
}

func TestFileSinkKeepsOversizeRecordWholeAndValidatesLines(t *testing.T) {
	if sink, err := NewFileSink(filepath.Join(t.TempDir(), "audit.jsonl"), 8, maximumMaxFiles+1); err == nil || sink != nil {
		t.Fatalf("NewFileSink accepted unbounded retention: %#v, %v", sink, err)
	}

	path := filepath.Join(t.TempDir(), "private", "audit.jsonl")
	sink, err := NewFileSink(path, 8, 1)
	if err != nil {
		t.Fatal(err)
	}
	for _, invalid := range [][]byte{nil, []byte("{}"), []byte("{}\n{}\n")} {
		if err := sink.WriteLine(invalid); err == nil {
			t.Fatalf("invalid line accepted: %q", invalid)
		}
	}
	large := []byte(`{"value":"` + strings.Repeat("x", 100) + `"}` + "\n")
	if err := sink.WriteLine(large); err != nil {
		t.Fatal(err)
	}
	if err := sink.Close(); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(payload) != string(large) {
		t.Fatalf("oversize record was split: %q", payload)
	}
	if _, err := os.Stat(generationPath(path, 1)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("oversize first record rotated: %v", err)
	}
}

func TestFileSinkRotationFailureIsStickyAndCloseConcurrent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "private", "audit.jsonl")
	line := []byte(`{"sequence":1}` + "\n")
	sink, err := NewFileSink(path, int64(len(line)), 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := sink.WriteLine(line); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(generationPath(path, 1), 0o700); err != nil {
		t.Fatal(err)
	}
	first := sink.WriteLine([]byte(`{"sequence":2}` + "\n"))
	second := sink.WriteLine([]byte(`{"sequence":3}` + "\n"))
	if first == nil || second == nil || first.Error() != second.Error() {
		t.Fatalf("rotation errors are not sticky: first %v second %v", first, second)
	}

	const closers = 24
	results := make(chan error, closers)
	var group sync.WaitGroup
	for index := 0; index < closers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			results <- sink.Close()
		}()
	}
	group.Wait()
	close(results)
	for closeErr := range results {
		if closeErr == nil || !strings.Contains(closeErr.Error(), "rotation") {
			t.Fatalf("concurrent Close error = %v", closeErr)
		}
	}
	if err := sink.WriteLine(line); !errors.Is(err, ErrClosed) {
		t.Fatalf("post-close WriteLine error = %v", err)
	}
}
