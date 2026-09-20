package record

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

var testBase = time.Date(2026, 3, 1, 12, 0, 0, 0, time.UTC)

func newTestWriter(t *testing.T, maxBytes int64) (*Writer, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "session.cast")
	writer, err := NewWriter(WriterOptions{
		Path:     path,
		MaxBytes: maxBytes,
		Base:     testBase,
		Header:   Header{Width: 80, Height: 24},
	})
	if err != nil {
		t.Fatalf("new writer: %v", err)
	}
	t.Cleanup(func() { _ = writer.Close() })
	return writer, path
}

func readFrames(t *testing.T, path string) (Header, []Frame) {
	t.Helper()
	file, err := os.Open(path)
	if err != nil {
		t.Fatalf("open transcript: %v", err)
	}
	defer func() { _ = file.Close() }()
	scanner, err := NewScanner(file)
	if err != nil {
		t.Fatalf("scan transcript: %v", err)
	}
	var frames []Frame
	for scanner.Next() {
		frames = append(frames, scanner.Frame())
	}
	if err := scanner.Err(); err != nil {
		t.Fatalf("read transcript: %v", err)
	}
	return scanner.Header(), frames
}

func TestNewWriterWritesTheHeaderFirst(t *testing.T) {
	writer, path := newTestWriter(t, 1<<20)
	if err := writer.WriteOutput(testBase.Add(time.Second), []byte("hi")); err != nil {
		t.Fatalf("write output: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	header, frames := readFrames(t, path)
	if header.Version != CastVersion || header.Width != 80 || header.Height != 24 {
		t.Fatalf("header = %+v", header)
	}
	if header.Timestamp != testBase.Unix() {
		t.Fatalf("header timestamp = %d, want %d", header.Timestamp, testBase.Unix())
	}
	if len(frames) != 1 || frames[0].Kind != KindOutput || frames[0].Data != "hi" {
		t.Fatalf("frames = %+v", frames)
	}
	if frames[0].Offset != 1 {
		t.Fatalf("offset = %v, want 1", frames[0].Offset)
	}
}

func TestWriterClampsNonMonotonicAndNegativeOffsets(t *testing.T) {
	writer, path := newTestWriter(t, 1<<20)
	// A clock stepped backwards by NTP must never produce a negative or
	// decreasing offset: no player can replay one.
	if err := writer.WriteOutput(testBase.Add(-5*time.Second), []byte("before")); err != nil {
		t.Fatalf("write before base: %v", err)
	}
	if err := writer.WriteOutput(testBase.Add(2*time.Second), []byte("later")); err != nil {
		t.Fatalf("write later: %v", err)
	}
	if err := writer.WriteOutput(testBase.Add(time.Second), []byte("stepped back")); err != nil {
		t.Fatalf("write stepped back: %v", err)
	}
	if err := writer.WriteOutput(testBase.Add(3*time.Second), []byte("forward again")); err != nil {
		t.Fatalf("write forward again: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, frames := readFrames(t, path)
	want := []float64{0, 2, 2, 3}
	if len(frames) != len(want) {
		t.Fatalf("frames = %d, want %d", len(frames), len(want))
	}
	previous := 0.0
	for index, frame := range frames {
		if frame.Offset != want[index] {
			t.Fatalf("offset %d = %v, want %v", index, frame.Offset, want[index])
		}
		if frame.Offset < previous {
			t.Fatalf("offset %d goes back in time", index)
		}
		previous = frame.Offset
	}
}

func TestWriterStopsAtTheByteCap(t *testing.T) {
	const maxBytes = 256
	writer, path := newTestWriter(t, maxBytes)
	payload := strings.Repeat("x", 64)
	for index := 0; index < 64; index++ {
		if err := writer.WriteOutput(testBase.Add(time.Duration(index)*time.Millisecond), []byte(payload)); err != nil {
			t.Fatalf("write %d: %v", index, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	bytesWritten, frames, truncated, dropped := writer.Stats()
	if !truncated {
		t.Fatal("writer did not report truncation")
	}
	if dropped != 0 {
		t.Fatalf("dropped = %d, want 0", dropped)
	}
	if bytesWritten > maxBytes {
		t.Fatalf("bytes = %d, want at most %d", bytesWritten, maxBytes)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	if info.Size() > maxBytes {
		t.Fatalf("file size = %d, want at most %d", info.Size(), maxBytes)
	}
	if info.Size() != bytesWritten {
		t.Fatalf("file size = %d, reported %d", info.Size(), bytesWritten)
	}
	_, decoded := readFrames(t, path)
	if len(decoded) != frames {
		t.Fatalf("decoded %d frames, reported %d", len(decoded), frames)
	}
	last := decoded[len(decoded)-1]
	if last.Kind != KindMarker || last.Data != truncatedMarker {
		t.Fatalf("last frame = %+v, want the truncated marker", last)
	}
	for _, frame := range decoded[:len(decoded)-1] {
		if frame.Kind == KindMarker {
			t.Fatal("more than one truncation marker was written")
		}
	}
}

func TestWriterKeepsTheCapAcrossLaterWrites(t *testing.T) {
	writer, path := newTestWriter(t, 128)
	for index := 0; index < 32; index++ {
		if err := writer.WriteOutput(testBase, []byte(strings.Repeat("y", 100))); err != nil {
			t.Fatalf("write %d: %v", index, err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	if info.Size() > 128 {
		t.Fatalf("file size = %d, want at most 128", info.Size())
	}
}

func TestWriterIsSafeForConcurrentWrites(t *testing.T) {
	writer, path := newTestWriter(t, 8<<20)
	const writers = 8
	const perWriter = 200
	var group sync.WaitGroup
	group.Add(writers)
	for index := 0; index < writers; index++ {
		go func(worker int) {
			defer group.Done()
			for step := 0; step < perWriter; step++ {
				at := testBase.Add(time.Duration(step) * time.Millisecond)
				switch step % 3 {
				case 0:
					_ = writer.WriteOutput(at, []byte("out"))
				case 1:
					_ = writer.WriteInput(at, []byte("in"))
				default:
					_ = writer.WriteResize(at, 80+worker, 24)
				}
			}
		}(index)
	}
	group.Wait()
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	bytesWritten, frames, truncated, _ := writer.Stats()
	if truncated {
		t.Fatal("transcript was truncated unexpectedly")
	}
	if frames != writers*perWriter {
		t.Fatalf("frames = %d, want %d", frames, writers*perWriter)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	if info.Size() != bytesWritten {
		t.Fatalf("file size = %d, reported %d", info.Size(), bytesWritten)
	}
	if err := ValidateFile(path); err != nil {
		t.Fatalf("concurrent transcript is invalid: %v", err)
	}
}

func TestWriterResizeAndMarkerFrames(t *testing.T) {
	writer, path := newTestWriter(t, 1<<20)
	if err := writer.WriteResize(testBase, 120, 40); err != nil {
		t.Fatalf("write resize: %v", err)
	}
	if err := writer.WriteResize(testBase, 0, -3); err != nil {
		t.Fatalf("write clamped resize: %v", err)
	}
	if err := writer.WriteMarker(testBase, "  a label\nwith control  "); err != nil {
		t.Fatalf("write marker: %v", err)
	}
	if err := writer.WriteMarker(testBase, ""); err != nil {
		t.Fatalf("write empty marker: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	_, frames := readFrames(t, path)
	want := []Frame{
		{Kind: KindResize, Data: "120x40"},
		{Kind: KindResize, Data: "1x1"},
		{Kind: KindMarker, Data: "a labelwith control"},
		{Kind: KindMarker, Data: "marker"},
	}
	if len(frames) != len(want) {
		t.Fatalf("frames = %+v", frames)
	}
	for index, frame := range frames {
		if frame.Kind != want[index].Kind || frame.Data != want[index].Data {
			t.Fatalf("frame %d = %+v, want %+v", index, frame, want[index])
		}
	}
}

func TestWriterRejectsUnusableOptions(t *testing.T) {
	directory := t.TempDir()
	cases := []WriterOptions{
		{Path: "", MaxBytes: 1024, Header: Header{Width: 80, Height: 24}},
		{Path: filepath.Join(directory, "a.cast"), MaxBytes: 0, Header: Header{Width: 80, Height: 24}},
		{Path: filepath.Join(directory, "b.cast"), MaxBytes: 1024, Header: Header{Width: 0, Height: 24}},
		{Path: filepath.Join(directory, "c\x00.cast"), MaxBytes: 1024, Header: Header{Width: 80, Height: 24}},
	}
	for index, options := range cases {
		writer, err := NewWriter(options)
		if err == nil {
			_ = writer.Close()
			t.Fatalf("accepted unusable options %d", index)
		}
	}
}

func TestNewWriterRefusesAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "taken.cast")
	if err := os.WriteFile(path, []byte("someone else's file\n"), 0o600); err != nil {
		t.Fatalf("seed file: %v", err)
	}
	writer, err := NewWriter(WriterOptions{Path: path, MaxBytes: 1024, Header: Header{Width: 80, Height: 24}})
	if err == nil {
		_ = writer.Close()
		t.Fatal("adopted an existing file")
	}
	content, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded file: %v", err)
	}
	if string(content) != "someone else's file\n" {
		t.Fatalf("seeded file was modified: %q", content)
	}
}

func TestWriterRejectsUseAfterClose(t *testing.T) {
	writer, _ := newTestWriter(t, 1<<20)
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	if err := writer.WriteOutput(testBase, []byte("x")); err != ErrClosed {
		t.Fatalf("write after close = %v, want %v", err, ErrClosed)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("second close: %v", err)
	}
}

func TestWriterRecordsDroppedFrames(t *testing.T) {
	writer, _ := newTestWriter(t, 1<<20)
	writer.RecordDropped(3)
	writer.RecordDropped(0)
	writer.RecordDropped(-1)
	writer.RecordDropped(4)
	if _, _, _, dropped := writer.Stats(); dropped != 7 {
		t.Fatalf("dropped = %d, want 7", dropped)
	}
}

func TestNilWriterIsInert(t *testing.T) {
	var writer *Writer
	if path := writer.Path(); path != "" {
		t.Fatalf("path = %q, want empty", path)
	}
	writer.RecordDropped(5)
	if bytesWritten, frames, truncated, dropped := writer.Stats(); bytesWritten != 0 || frames != 0 || truncated || dropped != 0 {
		t.Fatal("nil writer reported statistics")
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close nil writer: %v", err)
	}
	if err := writer.WriteOutput(testBase, []byte("x")); err != ErrClosed {
		t.Fatalf("write to nil writer = %v, want %v", err, ErrClosed)
	}
}

func TestNewWriterKeepsOnlyTheHeaderUnderATinyCap(t *testing.T) {
	// The header is structural: it is written even when it alone exceeds the
	// cap. Nothing else may follow it, and the overshoot is exactly one header.
	writer, path := newTestWriter(t, 1)
	if err := writer.WriteOutput(testBase, []byte("never stored")); err != nil {
		t.Fatalf("write under a tiny cap: %v", err)
	}
	if err := writer.WriteMarker(testBase, "never stored"); err != nil {
		t.Fatalf("marker under a tiny cap: %v", err)
	}
	if err := writer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	bytesWritten, frames, truncated, _ := writer.Stats()
	if !truncated {
		t.Fatal("writer did not report truncation")
	}
	if frames != 0 {
		t.Fatalf("frames = %d, want 0", frames)
	}
	header, decoded := readFrames(t, path)
	if len(decoded) != 0 {
		t.Fatalf("decoded %d frames, want 0", len(decoded))
	}
	if header.Width != 80 || header.Height != 24 {
		t.Fatalf("header = %+v, want the configured geometry", header)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	if info.Size() != bytesWritten {
		t.Fatalf("file size = %d, reported %d", info.Size(), bytesWritten)
	}
}

func TestWriterKeepsAStickyErrorAfterAFailedAppend(t *testing.T) {
	writer, path := newTestWriter(t, 1<<20)
	if err := writer.WriteOutput(testBase, []byte("stored")); err != nil {
		t.Fatalf("write: %v", err)
	}
	accounted, _, _, _ := writer.Stats()
	// A volume that disappears mid-session: the handle stops accepting writes
	// while the session keeps producing output.
	if err := writer.file.Close(); err != nil {
		t.Fatalf("close the underlying file: %v", err)
	}
	if err := writer.WriteOutput(testBase.Add(time.Second), []byte("lost")); err == nil {
		t.Fatal("a failed append was reported as a success")
	}
	if err := writer.WriteMarker(testBase.Add(2*time.Second), "lost"); err == nil {
		t.Fatal("the append failure was not sticky")
	}
	remaining, frames, _, _ := writer.Stats()
	if remaining != accounted {
		t.Fatalf("byte count drifted to %d from %d", remaining, accounted)
	}
	if frames != 1 {
		t.Fatalf("frames = %d, want 1", frames)
	}
	if err := writer.Close(); err == nil {
		t.Fatal("close hid the append failure")
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat transcript: %v", err)
	}
	if info.Size() != accounted {
		t.Fatalf("file size = %d, accounted %d", info.Size(), accounted)
	}
	_, decoded := readFrames(t, path)
	if len(decoded) != 1 {
		t.Fatalf("decoded %d frames, want 1", len(decoded))
	}
}
