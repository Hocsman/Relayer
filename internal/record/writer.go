package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrClosed reports use of a recording writer after Close.
var ErrClosed = errors.New("recording writer is closed")

const (
	// truncatedMarker is the single marker appended when a transcript reaches
	// its byte cap, so a replay shows the loss instead of ending silently.
	truncatedMarker = "truncated"
	// droppedMarkerPrefix precedes the number of frames a saturated queue lost.
	droppedMarkerPrefix = "dropped:"
	// maxMarkerLabelRunes bounds a marker label; labels are internal, and a
	// bounded one cannot turn a transcript into an unbounded sink.
	maxMarkerLabelRunes = 128
)

// WriterOptions configures one streaming asciicast emitter. Base anchors every
// offset; Clock only times a call that supplies no instant of its own.
type WriterOptions struct {
	Path     string
	Header   Header
	MaxBytes int64
	Clock    func() time.Time
	Base     time.Time
}

// Writer appends asciicast frames to a private file under a hard byte cap. It
// is safe for concurrent use and never grows past MaxBytes.
type Writer struct {
	mu         sync.Mutex
	file       *os.File
	path       string
	maxBytes   int64
	clock      func() time.Time
	base       time.Time
	bytes      int64
	frames     int
	dropped    int
	lastOffset float64
	truncated  bool
	noted      bool
	closed     bool
	closeErr   error
	stickyErr  error
}

// NewWriter creates the transcript file and writes its header line. The file
// must not already exist: a recording never adopts a foreign file.
func NewWriter(options WriterOptions) (*Writer, error) {
	if strings.TrimSpace(options.Path) == "" {
		return nil, errors.New("empty recording file path")
	}
	if strings.IndexByte(options.Path, 0) >= 0 {
		return nil, errors.New("recording file path contains a NUL byte")
	}
	if options.MaxBytes <= 0 {
		return nil, errors.New("maximum recording size is not positive")
	}
	clock := options.Clock
	if clock == nil {
		clock = time.Now
	}
	base := options.Base
	if base.IsZero() {
		base = clock()
	}
	header := options.Header
	header.Version = CastVersion
	if header.Timestamp == 0 {
		header.Timestamp = base.Unix()
	}
	if err := validateHeader(header); err != nil {
		return nil, err
	}
	line, err := json.Marshal(header)
	if err != nil {
		return nil, errors.New("encode the recording header")
	}
	line = append(line, '\n')

	file, err := createPrivateRegularFile(options.Path)
	if err != nil {
		return nil, err
	}
	result := &Writer{
		file:     file,
		path:     options.Path,
		maxBytes: options.MaxBytes,
		clock:    clock,
		base:     base,
	}
	// The header is structural: it is written even when it alone exceeds the
	// cap, because a headerless file is not a transcript at all.
	if err := result.append(line); err != nil {
		_ = file.Close()
		_ = os.Remove(options.Path)
		return nil, err
	}
	if result.bytes >= result.maxBytes {
		result.truncated = true
		result.noted = true
	}
	return result, nil
}

// Path returns the transcript file path.
func (w *Writer) Path() string {
	if w == nil {
		return ""
	}
	return w.path
}

// WriteOutput appends terminal output produced at the given instant.
func (w *Writer) WriteOutput(at time.Time, data []byte) error {
	return w.write(at, KindOutput, string(data))
}

// WriteInput appends operator input produced at the given instant.
func (w *Writer) WriteInput(at time.Time, data []byte) error {
	return w.write(at, KindInput, string(data))
}

// WriteResize appends a terminal geometry change.
func (w *Writer) WriteResize(at time.Time, columns, rows int) error {
	return w.write(at, KindResize, fmt.Sprintf("%dx%d", clamp(columns, 1, 65535), clamp(rows, 1, 65535)))
}

// WriteMarker appends a bounded replay marker.
func (w *Writer) WriteMarker(at time.Time, label string) error {
	return w.write(at, KindMarker, safeLabel(label))
}

// RecordDropped accounts for frames a caller could not hand over. The count
// reaches the sidecar through Stats so the loss is never invisible.
func (w *Writer) RecordDropped(count int) {
	if w == nil || count <= 0 {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.dropped += count
}

// Stats reports the bytes written, the frames appended, whether the byte cap
// stopped the transcript, and how many frames were dropped before it.
func (w *Writer) Stats() (int64, int, bool, int) {
	if w == nil {
		return 0, 0, false, 0
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.bytes, w.frames, w.truncated, w.dropped
}

// Close syncs and closes the transcript once.
func (w *Writer) Close() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return w.closeErr
	}
	w.closed = true
	var closeErrors []error
	if w.stickyErr != nil {
		closeErrors = append(closeErrors, w.stickyErr)
	}
	if w.file != nil {
		if err := w.file.Sync(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("final recording sync: %w", err))
		}
		if err := w.file.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close the recording file: %w", err))
		}
		w.file = nil
	}
	w.closeErr = errors.Join(closeErrors...)
	return w.closeErr
}

func (w *Writer) write(at time.Time, kind Kind, data string) error {
	if w == nil {
		return ErrClosed
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return ErrClosed
	}
	if w.stickyErr != nil {
		return w.stickyErr
	}
	if w.truncated {
		return nil
	}
	offset := w.offsetLocked(at)
	line, err := json.Marshal(Frame{Offset: offset, Kind: kind, Data: data})
	if err != nil {
		return errors.New("encode a recording frame")
	}
	line = append(line, '\n')
	if w.bytes+int64(len(line)) > w.maxBytes {
		w.markTruncatedLocked(offset)
		return nil
	}
	if err := w.append(line); err != nil {
		return err
	}
	w.frames++
	w.lastOffset = offset
	return nil
}

// offsetLocked converts an instant to a transcript offset and clamps it to the
// last one written. A clock step backwards - NTP, a suspended host - must never
// produce a negative or non-monotonic asciicast, which no player can replay.
func (w *Writer) offsetLocked(at time.Time) float64 {
	if at.IsZero() {
		at = w.clock()
	}
	offset := at.Sub(w.base).Seconds()
	if math.IsNaN(offset) || math.IsInf(offset, 0) || offset < w.lastOffset {
		return w.lastOffset
	}
	return offset
}

// markTruncatedLocked stops the transcript and appends one final marker if it
// still fits under the cap.
func (w *Writer) markTruncatedLocked(offset float64) {
	w.truncated = true
	if w.noted {
		return
	}
	w.noted = true
	line, err := json.Marshal(Frame{Offset: offset, Kind: KindMarker, Data: truncatedMarker})
	if err != nil {
		return
	}
	line = append(line, '\n')
	if w.bytes+int64(len(line)) > w.maxBytes {
		return
	}
	if err := w.append(line); err != nil {
		return
	}
	w.frames++
	w.lastOffset = offset
}

func (w *Writer) append(line []byte) error {
	start := w.bytes
	written, err := w.file.Write(line)
	if err != nil || written != len(line) {
		writeErr := err
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		if truncateErr := w.file.Truncate(start); truncateErr != nil {
			writeErr = errors.Join(writeErr, truncateErr)
		}
		w.stickyErr = fmt.Errorf("append a recording frame: %w", writeErr)
		return w.stickyErr
	}
	w.bytes += int64(written)
	return nil
}

// safeLabel keeps a marker printable and bounded.
func safeLabel(label string) string {
	label = strings.TrimSpace(label)
	var builder strings.Builder
	count := 0
	for _, symbol := range label {
		if count >= maxMarkerLabelRunes {
			break
		}
		if symbol < 0x20 || symbol == 0x7f {
			continue
		}
		builder.WriteRune(symbol)
		count++
	}
	if builder.Len() == 0 {
		return "marker"
	}
	return builder.String()
}

// droppedMarker names the marker written once a saturated queue recovers.
func droppedMarker(count int) string {
	return droppedMarkerPrefix + strconv.Itoa(count)
}

func clamp(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}
