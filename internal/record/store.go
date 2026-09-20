package record

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/platform"
)

const (
	// MetadataSchemaVersion identifies the sidecar representation on disk.
	MetadataSchemaVersion = 1
	castSuffix            = ".cast"
	metadataSuffix        = ".meta.json"
	temporarySuffix       = ".tmp"
	// maxSlugRunes bounds one path component built from a caller identity.
	maxSlugRunes = 64
)

// ErrRecordingNotFound reports an unknown recording identity.
var ErrRecordingNotFound = errors.New("recording not found")

// ErrRecordingActive rejects mutating a transcript still being written.
var ErrRecordingActive = errors.New("recording is open for writing")

// ErrStoreClosed reports use of a store after Close.
var ErrStoreClosed = errors.New("recording store is closed")

// Metadata is the sidecar a replay interface reads without parsing the whole
// transcript. Path is recomputed on every read so a moved directory still
// resolves.
type Metadata struct {
	SchemaVersion int       `json:"schema_version"`
	ID            string    `json:"id"`
	RunID         string    `json:"run_id"`
	SessionID     string    `json:"session_id"`
	AgentID       string    `json:"agent_id,omitempty"`
	Name          string    `json:"name,omitempty"`
	Backend       string    `json:"backend,omitempty"`
	Adapter       string    `json:"adapter,omitempty"`
	StartedAt     time.Time `json:"started_at"`
	EndedAt       time.Time `json:"ended_at,omitempty"`
	Width         int       `json:"width"`
	Height        int       `json:"height"`
	Bytes         int64     `json:"bytes"`
	Frames        int       `json:"frames"`
	Truncated     bool      `json:"truncated"`
	DroppedFrames int       `json:"dropped_frames"`
	InputRecorded bool      `json:"input_recorded"`
	Redacted      bool      `json:"redacted"`
	ExitCode      *int      `json:"exit_code,omitempty"`
	Active        bool      `json:"active"`
	Path          string    `json:"path,omitempty"`
}

// CreateOptions describes the session a new transcript belongs to.
type CreateOptions struct {
	RunID     string
	SessionID string
	AgentID   string
	Name      string
	Backend   string
	Adapter   string
	Title     string
	Width     int
	Height    int
	StartedAt time.Time
}

// Store owns the on-disk recording layout: one directory per run holding one
// transcript and one sidecar per session.
type Store struct {
	mu     sync.Mutex
	dir    string
	config Config
	clock  func() time.Time
	open   map[string]*Writer
	closed bool
}

// OpenStore prepares the private recording directory. It is usable while
// recording is disabled so a replay interface can still list past sessions.
func OpenStore(config Config) (*Store, error) {
	if err := Validate(config); err != nil {
		return nil, err
	}
	directory, err := ResolvePath(config.Path)
	if err != nil {
		return nil, err
	}
	if err := ensurePrivateDirectory(directory); err != nil {
		return nil, err
	}
	config.Path = directory
	return &Store{
		dir:    directory,
		config: config,
		clock:  time.Now,
		open:   make(map[string]*Writer),
	}, nil
}

// Dir returns the absolute recording directory.
func (s *Store) Dir() string {
	if s == nil {
		return ""
	}
	return s.dir
}

// RecordingID builds the filesystem-safe identity of one transcript. The start
// instant separates two sessions that reuse an identity within one run.
func RecordingID(runID, sessionID string, startedAt time.Time) string {
	return slug(runID) + "-" + sessionSlug(sessionID, startedAt)
}

// Create reserves a transcript, writes its opening sidecar and returns a
// writer the caller owns until Finish.
func (s *Store) Create(options CreateOptions) (Metadata, *Writer, error) {
	if s == nil {
		return Metadata{}, nil, ErrStoreClosed
	}
	startedAt := options.StartedAt
	if startedAt.IsZero() {
		startedAt = s.clock()
	}
	width := clamp(options.Width, 1, 65535)
	height := clamp(options.Height, 1, 65535)

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return Metadata{}, nil, ErrStoreClosed
	}
	runDirectory := filepath.Join(s.dir, slug(options.RunID))
	if err := ensurePrivateDirectory(runDirectory); err != nil {
		return Metadata{}, nil, err
	}
	base := filepath.Join(runDirectory, sessionSlug(options.SessionID, startedAt))
	metadata := Metadata{
		SchemaVersion: MetadataSchemaVersion,
		ID:            RecordingID(options.RunID, options.SessionID, startedAt),
		RunID:         options.RunID,
		SessionID:     options.SessionID,
		AgentID:       options.AgentID,
		Name:          options.Name,
		Backend:       options.Backend,
		Adapter:       options.Adapter,
		StartedAt:     startedAt,
		Width:         width,
		Height:        height,
		InputRecorded: s.config.RecordInput,
		Redacted:      s.config.Redact,
		Active:        true,
		Path:          base + castSuffix,
	}
	if _, exists := s.open[metadata.ID]; exists {
		return Metadata{}, nil, errors.New("recording identity is already open")
	}
	writer, err := NewWriter(WriterOptions{
		Path:     metadata.Path,
		MaxBytes: int64(s.config.MaxFileSizeMB) * 1024 * 1024,
		Base:     startedAt,
		Clock:    s.clock,
		Header: Header{
			Width:     width,
			Height:    height,
			Timestamp: startedAt.Unix(),
			Title:     safeLabel(options.Title),
		},
	})
	if err != nil {
		return Metadata{}, nil, err
	}
	if err := writeMetadata(base+metadataSuffix, metadata); err != nil {
		_ = writer.Close()
		_ = os.Remove(metadata.Path)
		return Metadata{}, nil, err
	}
	s.open[metadata.ID] = writer
	return metadata, writer, nil
}

// Finish closes the transcript and rewrites its sidecar with the final
// counters, so a crash is the only way a sidecar keeps no EndedAt.
func (s *Store) Finish(id string, endedAt time.Time, exitCode *int) (Metadata, error) {
	if s == nil {
		return Metadata{}, ErrStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	writer, open := s.open[id]
	if !open {
		return Metadata{}, ErrRecordingNotFound
	}
	delete(s.open, id)
	bytesWritten, frames, truncated, dropped := writer.Stats()
	closeErr := writer.Close()

	path, metadata, err := s.locateLocked(id)
	if err != nil {
		return Metadata{}, errors.Join(closeErr, err)
	}
	if endedAt.IsZero() {
		endedAt = s.clock()
	}
	if endedAt.Before(metadata.StartedAt) {
		endedAt = metadata.StartedAt
	}
	metadata.EndedAt = endedAt
	metadata.ExitCode = exitCode
	metadata.Bytes = bytesWritten
	metadata.Frames = frames
	metadata.Truncated = truncated
	metadata.DroppedFrames = dropped
	metadata.Active = false
	if err := writeMetadata(path, metadata); err != nil {
		return Metadata{}, errors.Join(closeErr, err)
	}
	return metadata, closeErr
}

// List returns every recording newest first. A sidecar that cannot be decoded
// is skipped so one damaged file never hides the rest.
func (s *Store) List() ([]Metadata, error) {
	if s == nil {
		return nil, ErrStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	recordings, err := s.scanLocked()
	if err != nil {
		return nil, err
	}
	sort.SliceStable(recordings, func(first, second int) bool {
		return recordings[first].StartedAt.After(recordings[second].StartedAt)
	})
	return recordings, nil
}

// Get returns one recording's metadata.
func (s *Store) Get(id string) (Metadata, error) {
	if s == nil {
		return Metadata{}, ErrStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	_, metadata, err := s.locateLocked(id)
	return metadata, err
}

// Open returns the transcript bytes of one recording.
func (s *Store) Open(id string) (io.ReadCloser, error) {
	if s == nil {
		return nil, ErrStoreClosed
	}
	s.mu.Lock()
	_, metadata, err := s.locateLocked(id)
	s.mu.Unlock()
	if err != nil {
		return nil, err
	}
	return openExistingRegularFile(metadata.Path)
}

// Delete removes one recording and its sidecar. A transcript still being
// written is refused rather than silently lost.
func (s *Store) Delete(id string) error {
	if s == nil {
		return ErrStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, open := s.open[id]; open {
		return ErrRecordingActive
	}
	path, metadata, err := s.locateLocked(id)
	if err != nil {
		return err
	}
	return removeRecording(path, metadata.Path)
}

// Prune enforces the retention limits oldest first and returns how many
// recordings it removed. A transcript currently open for writing is skipped:
// removing it loses the live session on Unix and fails outright on Windows.
func (s *Store) Prune() (int, error) {
	if s == nil {
		return 0, ErrStoreClosed
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return 0, ErrStoreClosed
	}
	recordings, err := s.scanLocked()
	if err != nil {
		return 0, err
	}
	sort.SliceStable(recordings, func(first, second int) bool {
		return recordings[first].StartedAt.Before(recordings[second].StartedAt)
	})

	candidates := make([]Metadata, 0, len(recordings))
	total := int64(0)
	for _, metadata := range recordings {
		total += metadata.Bytes
		if metadata.Active {
			continue
		}
		candidates = append(candidates, metadata)
	}
	maximumTotal := int64(s.config.MaxTotalSizeMB) * 1024 * 1024
	var cutoff time.Time
	if s.config.RetentionDays > 0 {
		cutoff = s.clock().AddDate(0, 0, -s.config.RetentionDays)
	}

	removed := 0
	count := len(recordings)
	for _, metadata := range candidates {
		expired := !cutoff.IsZero() && metadata.StartedAt.Before(cutoff)
		if !expired && count <= s.config.MaxRecordings && total <= maximumTotal {
			break
		}
		path, _, err := s.locateLocked(metadata.ID)
		if err != nil {
			continue
		}
		if err := removeRecording(path, metadata.Path); err != nil {
			return removed, err
		}
		total -= metadata.Bytes
		count--
		removed++
	}
	return removed, nil
}

// Close closes every transcript still open. It does not finalize their
// sidecars: an unfinished sidecar is exactly how a crash is reported.
func (s *Store) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	s.closed = true
	var closeErrors []error
	for id, writer := range s.open {
		if err := writer.Close(); err != nil {
			closeErrors = append(closeErrors, err)
		}
		delete(s.open, id)
	}
	return errors.Join(closeErrors...)
}

// scanLocked reads every sidecar under the recording root. The caller holds
// the store mutex.
func (s *Store) scanLocked() ([]Metadata, error) {
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return nil, fmt.Errorf("read the recording directory %s: %w", s.dir, err)
	}
	recordings := make([]Metadata, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runDirectory := filepath.Join(s.dir, entry.Name())
		files, err := os.ReadDir(runDirectory)
		if err != nil {
			continue
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), metadataSuffix) {
				continue
			}
			path := filepath.Join(runDirectory, file.Name())
			metadata, err := readMetadata(path)
			if err != nil {
				continue
			}
			recordings = append(recordings, s.reconcileLocked(path, metadata))
		}
	}
	return recordings, nil
}

// reconcileLocked replaces the persisted liveness fields with what the
// filesystem and this process actually show. A sidecar with no EndedAt that no
// writer owns any more belongs to a crashed run: it is inactive and its
// transcript is necessarily incomplete.
func (s *Store) reconcileLocked(sidecar string, metadata Metadata) Metadata {
	metadata.Path = strings.TrimSuffix(sidecar, metadataSuffix) + castSuffix
	writer, open := s.open[metadata.ID]
	metadata.Active = open
	if open {
		bytesWritten, frames, truncated, dropped := writer.Stats()
		metadata.Bytes = bytesWritten
		metadata.Frames = frames
		metadata.Truncated = truncated
		metadata.DroppedFrames = dropped
		return metadata
	}
	if info, err := os.Stat(metadata.Path); err == nil {
		metadata.Bytes = info.Size()
	}
	if metadata.EndedAt.IsZero() {
		metadata.Truncated = true
	}
	return metadata
}

// locateLocked resolves an identity to its sidecar path and reconciled
// metadata. The caller holds the store mutex.
func (s *Store) locateLocked(id string) (string, Metadata, error) {
	if strings.TrimSpace(id) == "" {
		return "", Metadata{}, ErrRecordingNotFound
	}
	entries, err := os.ReadDir(s.dir)
	if err != nil {
		return "", Metadata{}, fmt.Errorf("read the recording directory %s: %w", s.dir, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		runDirectory := filepath.Join(s.dir, entry.Name())
		files, err := os.ReadDir(runDirectory)
		if err != nil {
			continue
		}
		for _, file := range files {
			if file.IsDir() || !strings.HasSuffix(file.Name(), metadataSuffix) {
				continue
			}
			path := filepath.Join(runDirectory, file.Name())
			metadata, err := readMetadata(path)
			if err != nil || metadata.ID != id {
				continue
			}
			return path, s.reconcileLocked(path, metadata), nil
		}
	}
	return "", Metadata{}, ErrRecordingNotFound
}

func removeRecording(sidecar, transcript string) error {
	if err := os.Remove(transcript); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the recording %s: %w", transcript, err)
	}
	if err := os.Remove(sidecar); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove the recording sidecar %s: %w", sidecar, err)
	}
	// An empty run directory is layout residue, never a recording.
	_ = os.Remove(filepath.Dir(sidecar))
	return nil
}

func readMetadata(path string) (Metadata, error) {
	file, err := openExistingRegularFile(path)
	if err != nil {
		return Metadata{}, err
	}
	defer func() { _ = file.Close() }()
	var metadata Metadata
	if err := json.NewDecoder(io.LimitReader(file, maxCastLineBytes)).Decode(&metadata); err != nil {
		return Metadata{}, errors.New("recording sidecar is malformed")
	}
	if metadata.SchemaVersion < 1 || metadata.SchemaVersion > MetadataSchemaVersion {
		return Metadata{}, errors.New("unsupported recording sidecar version")
	}
	if strings.TrimSpace(metadata.ID) == "" {
		return Metadata{}, errors.New("recording sidecar has no identity")
	}
	return metadata, nil
}

// writeMetadata replaces a sidecar atomically so an interrupted rewrite never
// leaves a half-decoded record of a finished session.
func writeMetadata(path string, metadata Metadata) error {
	encoded, err := json.Marshal(metadata)
	if err != nil {
		return errors.New("encode the recording sidecar")
	}
	encoded = append(encoded, '\n')
	temporary := path + temporarySuffix
	if err := os.Remove(temporary); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("replace the recording sidecar %s: %w", path, err)
	}
	file, err := createPrivateRegularFile(temporary)
	if err != nil {
		return err
	}
	if _, err := file.Write(encoded); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return fmt.Errorf("write the recording sidecar %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(temporary)
		return fmt.Errorf("sync the recording sidecar %s: %w", path, err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("close the recording sidecar %s: %w", path, err)
	}
	// Windows refuses a rename while any other handle is open on either path,
	// and a sidecar is rewritten on every transcript finish -- including while
	// the Recordings panel may be listing it.
	if err := platform.PublishByRename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return fmt.Errorf("replace the recording sidecar %s: %w", path, err)
	}
	return nil
}

func ensurePrivateDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create recording directory %s: %w", directory, err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("permissions on the recording directory %s: %w", directory, err)
		}
		info, err = os.Lstat(directory)
	}
	if err != nil {
		return fmt.Errorf("inspect the recording directory %s: %w", directory, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("recording directory %s is not a regular directory", directory)
	}
	if err := requireCurrentUserOwner(info, directory); err != nil {
		return err
	}
	if runtime.GOOS != "windows" && info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("recording directory %s is not private (permissions %04o)", directory, info.Mode().Perm())
	}
	return nil
}

// createPrivateRegularFile creates a new private file and refuses an existing
// one. A transcript name carries its start instant, so the store never needs
// to adopt a file it did not create, and never destroys a foreign one.
func createPrivateRegularFile(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("recording file %s is not a regular file", path)
		}
		return nil, fmt.Errorf("recording file %s already exists", path)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect the recording file %s: %w", path, err)
	}
	flags := os.O_WRONLY | os.O_CREATE | os.O_EXCL
	if runtime.GOOS != "windows" {
		flags |= os.O_APPEND
	}
	file, err := os.OpenFile(path, flags, 0o600)
	if err != nil {
		return nil, fmt.Errorf("create the recording file %s: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("permissions on the recording file %s: %w", path, err)
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("created recording file %s is not a regular file", path)
	}
	if err := requireCurrentUserOwner(opened, path); err != nil {
		_ = file.Close()
		return nil, err
	}
	linked, err := os.Lstat(path)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		_ = file.Close()
		return nil, fmt.Errorf("recording file %s identity is not trustworthy", path)
	}
	return file, nil
}

func sessionSlug(sessionID string, startedAt time.Time) string {
	nanoseconds := startedAt.UnixNano()
	if nanoseconds < 0 {
		nanoseconds = 0
	}
	return slug(sessionID) + "-" + strconv.FormatInt(nanoseconds, 10)
}

// slug keeps only characters every supported filesystem accepts, so a session
// name can never escape the recording directory or collide with a device name.
func slug(value string) string {
	var builder strings.Builder
	count := 0
	previousDash := false
	for _, symbol := range value {
		if count >= maxSlugRunes {
			break
		}
		switch {
		case symbol >= 'a' && symbol <= 'z', symbol >= 'A' && symbol <= 'Z', symbol >= '0' && symbol <= '9', symbol == '_':
			builder.WriteRune(symbol)
			previousDash = false
		default:
			if previousDash {
				continue
			}
			builder.WriteByte('-')
			previousDash = true
		}
		count++
	}
	result := strings.Trim(builder.String(), "-")
	if result == "" {
		return "unknown"
	}
	if isReservedName(result) {
		return result + "_"
	}
	return result
}

// isReservedName reports the Windows device names no path component may carry.
// A run directory is named after a caller identity, and os.MkdirAll refuses
// such a name outright, which would silently disable recording for that run.
func isReservedName(value string) bool {
	lowered := strings.ToLower(value)
	switch lowered {
	case "con", "prn", "aux", "nul":
		return true
	}
	if len(lowered) != 4 || lowered[3] < '1' || lowered[3] > '9' {
		return false
	}
	return lowered[:3] == "com" || lowered[:3] == "lpt"
}
