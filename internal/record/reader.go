package record

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

// maxCastLineBytes bounds one transcript line. An output frame holds escaped
// terminal bytes, so the limit is well above the PTY read size while still
// refusing an unbounded line from a foreign file.
const maxCastLineBytes = 4 * 1024 * 1024

// maxReadRangeFrames bounds one ReadRange page so a replay request cannot pull
// a whole transcript into memory.
const maxReadRangeFrames = 5000

// Scanner streams a transcript one frame at a time. The header is decoded by
// NewScanner, so Header is available before the first Next.
type Scanner struct {
	lines  *bufio.Scanner
	header Header
	frame  Frame
	err    error
	line   int
}

// NewScanner decodes the header line and prepares frame iteration.
func NewScanner(reader io.Reader) (*Scanner, error) {
	if reader == nil {
		return nil, errors.New("nil recording reader")
	}
	lines := bufio.NewScanner(reader)
	lines.Buffer(make([]byte, 0, 64*1024), maxCastLineBytes)
	if !lines.Scan() {
		if err := lines.Err(); err != nil {
			return nil, errors.New("read the recording header")
		}
		return nil, errors.New("recording is empty")
	}
	header, err := decodeHeader(lines.Bytes())
	if err != nil {
		return nil, err
	}
	return &Scanner{lines: lines, header: header, line: 1}, nil
}

// Header returns the decoded transcript header.
func (s *Scanner) Header() Header {
	if s == nil {
		return Header{}
	}
	return s.header
}

// Next advances to the next frame, skipping blank separator lines. It reports
// false at the end of the transcript and on the first malformed line.
func (s *Scanner) Next() bool {
	if s == nil || s.err != nil {
		return false
	}
	for s.lines.Scan() {
		s.line++
		raw := bytes.TrimSpace(s.lines.Bytes())
		if len(raw) == 0 {
			continue
		}
		var frame Frame
		if err := json.Unmarshal(raw, &frame); err != nil {
			s.err = fmt.Errorf("recording line %d is malformed: %w", s.line, err)
			return false
		}
		s.frame = frame
		return true
	}
	if err := s.lines.Err(); err != nil {
		s.err = fmt.Errorf("read recording line %d", s.line+1)
	}
	return false
}

// Frame returns the frame decoded by the last successful Next.
func (s *Scanner) Frame() Frame {
	if s == nil {
		return Frame{}
	}
	return s.frame
}

// Err returns the first decoding error encountered.
func (s *Scanner) Err() error {
	if s == nil {
		return nil
	}
	return s.err
}

// ReadHeader decodes only the header line of a transcript stream.
func ReadHeader(reader io.Reader) (Header, error) {
	scanner, err := NewScanner(reader)
	if err != nil {
		return Header{}, err
	}
	return scanner.Header(), nil
}

// ReadRange returns at most limit frames starting at the zero-based frame
// index offset, the index to resume from, and whether the transcript ended
// within this page.
func ReadRange(path string, offset, limit int) (Header, []Frame, int, bool, error) {
	if offset < 0 {
		return Header{}, nil, 0, false, errors.New("recording offset is negative")
	}
	if limit <= 0 || limit > maxReadRangeFrames {
		limit = maxReadRangeFrames
	}
	file, err := openExistingRegularFile(path)
	if err != nil {
		return Header{}, nil, 0, false, err
	}
	defer func() { _ = file.Close() }()

	scanner, err := NewScanner(file)
	if err != nil {
		return Header{}, nil, 0, false, err
	}
	frames := make([]Frame, 0, 64)
	index := 0
	complete := true
	for scanner.Next() {
		if index < offset {
			index++
			continue
		}
		if len(frames) == limit {
			complete = false
			break
		}
		frames = append(frames, scanner.Frame())
		index++
	}
	if err := scanner.Err(); err != nil {
		return Header{}, nil, 0, false, err
	}
	return scanner.Header(), frames, offset + len(frames), complete, nil
}

// ValidateFile reports whether a path holds a transcript Relayer can replay:
// a known header, non-negative monotonic offsets, and only known kinds.
func ValidateFile(path string) error {
	file, err := openExistingRegularFile(path)
	if err != nil {
		return err
	}
	defer func() { _ = file.Close() }()

	scanner, err := NewScanner(file)
	if err != nil {
		return err
	}
	previous := 0.0
	index := 0
	for scanner.Next() {
		frame := scanner.Frame()
		index++
		if frame.Offset < 0 {
			return fmt.Errorf("recording frame %d has a negative offset", index)
		}
		if frame.Offset < previous {
			return fmt.Errorf("recording frame %d goes back in time", index)
		}
		previous = frame.Offset
	}
	return scanner.Err()
}

func decodeHeader(line []byte) (Header, error) {
	var header Header
	if err := json.Unmarshal(bytes.TrimSpace(line), &header); err != nil {
		return Header{}, errors.New("recording header is malformed")
	}
	if err := validateHeader(header); err != nil {
		return Header{}, err
	}
	return header, nil
}

func openExistingRegularFile(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect the recording file %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return nil, fmt.Errorf("recording file %s is not a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open the recording file %s: %w", path, err)
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() || !os.SameFile(info, opened) {
		_ = file.Close()
		return nil, fmt.Errorf("recording file %s identity is not trustworthy", path)
	}
	return file, nil
}
