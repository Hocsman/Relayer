package record

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func writeTranscript(t *testing.T, lines ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "hand-written.cast")
	content := strings.Join(lines, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write transcript: %v", err)
	}
	return path
}

func buildTranscript(t *testing.T, frames int) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "built.cast")
	writer, err := NewWriter(WriterOptions{
		Path:     path,
		MaxBytes: 1 << 20,
		Base:     testBase,
		Header:   Header{Width: 100, Height: 30, Title: "demo"},
	})
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	for index := 0; index < frames; index++ {
		at := testBase.Add(time.Duration(index) * time.Second)
		if err := writer.WriteOutput(at, []byte(strings.Repeat("a", index%7+1))); err != nil {
			t.Fatalf("write frame %d: %v", index, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close writer: %v", err)
	}
	return path
}

func TestScannerReadsHeaderAndFrames(t *testing.T) {
	path := buildTranscript(t, 5)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer func() { _ = file.Close() }()
	scanner, err := NewScanner(file)
	if err != nil {
		t.Fatalf("new scanner: %v", err)
	}
	header := scanner.Header()
	if header.Width != 100 || header.Height != 30 || header.Title != "demo" {
		t.Fatalf("header = %+v", header)
	}
	count := 0
	for scanner.Next() {
		if scanner.Frame().Kind != KindOutput {
			t.Fatalf("frame %d = %+v", count, scanner.Frame())
		}
		count++
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("scan: %v", err)
	}
	if count != 5 {
		t.Fatalf("frames = %d, want 5", count)
	}
}

func TestNewScannerRejectsForeignAndEmptyStreams(t *testing.T) {
	cases := []string{
		"",
		"not json\n",
		`{"version":1,"width":80,"height":24}` + "\n",
		`{"version":2,"width":0,"height":24}` + "\n",
	}
	for index, content := range cases {
		if _, err := NewScanner(strings.NewReader(content)); err == nil {
			t.Fatalf("accepted foreign stream %d", index)
		}
	}
	if _, err := NewScanner(nil); err == nil {
		t.Fatal("accepted a nil reader")
	}
}

func TestScannerReportsAMalformedLineWithoutPanicking(t *testing.T) {
	path := writeTranscript(t,
		`{"version":2,"width":80,"height":24}`,
		`[0.000000,"o","fine"]`,
		`[1.000000,"z","unknown kind"]`,
		`[2.000000,"o","never reached"]`,
	)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer func() { _ = file.Close() }()
	scanner, err := NewScanner(file)
	if err != nil {
		t.Fatalf("new scanner: %v", err)
	}
	if !scanner.Next() {
		t.Fatal("first frame was not read")
	}
	if scanner.Next() {
		t.Fatal("malformed frame was accepted")
	}
	if scanner.Err() == nil {
		t.Fatal("malformed frame was not reported")
	}
	if !strings.Contains(scanner.Err().Error(), "line 3") {
		t.Fatalf("error does not name the line: %v", scanner.Err())
	}
}

func TestReadHeaderDecodesOnlyTheHeader(t *testing.T) {
	path := buildTranscript(t, 3)
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer func() { _ = file.Close() }()
	header, err := ReadHeader(file)
	if err != nil {
		t.Fatalf("read header: %v", err)
	}
	if header.Version != CastVersion || header.Width != 100 {
		t.Fatalf("header = %+v", header)
	}
}

func TestReadRangePagesThroughATranscript(t *testing.T) {
	path := buildTranscript(t, 10)
	header, frames, next, complete, err := ReadRange(path, 0, 4)
	if err != nil {
		t.Fatalf("read first page: %v", err)
	}
	if header.Width != 100 {
		t.Fatalf("header = %+v", header)
	}
	if len(frames) != 4 || next != 4 || complete {
		t.Fatalf("first page: frames=%d next=%d complete=%v", len(frames), next, complete)
	}
	total := len(frames)
	for !complete {
		_, frames, next, complete, err = ReadRange(path, next, 4)
		if err != nil {
			t.Fatalf("read page at %d: %v", next, err)
		}
		total += len(frames)
	}
	if total != 10 {
		t.Fatalf("read %d frames, want 10", total)
	}
	if next != 10 {
		t.Fatalf("next = %d, want 10", next)
	}
}

func TestReadRangeBeyondTheEndIsEmptyAndComplete(t *testing.T) {
	path := buildTranscript(t, 3)
	_, frames, next, complete, err := ReadRange(path, 99, 10)
	if err != nil {
		t.Fatalf("read past the end: %v", err)
	}
	if len(frames) != 0 || next != 99 || !complete {
		t.Fatalf("frames=%d next=%d complete=%v", len(frames), next, complete)
	}
}

func TestReadRangeRejectsANegativeOffset(t *testing.T) {
	path := buildTranscript(t, 1)
	if _, _, _, _, err := ReadRange(path, -1, 10); err == nil {
		t.Fatal("accepted a negative offset")
	}
}

func TestValidateFileAcceptsAWrittenTranscript(t *testing.T) {
	if err := ValidateFile(buildTranscript(t, 12)); err != nil {
		t.Fatalf("valid transcript rejected: %v", err)
	}
}

func TestValidateFileRejectsBrokenTranscripts(t *testing.T) {
	cases := [][]string{
		{`{"version":2,"width":80,"height":24}`, `[1.000000,"o","a"]`, `[0.500000,"o","back in time"]`},
		{`{"version":2,"width":80,"height":24}`, `[-1.000000,"o","negative"]`},
		{`{"version":2,"width":80,"height":24}`, `[0.000000,"q","unknown"]`},
		{`{"version":3,"width":80,"height":24}`, `[0.000000,"o","a"]`},
		{`[0.000000,"o","no header"]`},
	}
	for index, lines := range cases {
		if err := ValidateFile(writeTranscript(t, lines...)); err == nil {
			t.Fatalf("accepted broken transcript %d", index)
		}
	}
}

func TestValidateFileRejectsAMissingPath(t *testing.T) {
	if err := ValidateFile(filepath.Join(t.TempDir(), "absent.cast")); err == nil {
		t.Fatal("accepted a missing transcript")
	}
}

func TestScannerSkipsBlankSeparatorLines(t *testing.T) {
	path := writeTranscript(t,
		`{"version":2,"width":80,"height":24}`,
		``,
		`[0.000000,"o","a"]`,
		``,
		`[1.000000,"o","b"]`,
	)
	_, frames, _, complete, err := ReadRange(path, 0, 10)
	if err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	if len(frames) != 2 || !complete {
		t.Fatalf("frames=%d complete=%v", len(frames), complete)
	}
}
