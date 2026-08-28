package audit

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

type openOptions struct {
	clock       func() time.Time
	idGenerator func() (string, error)
	runID       string
}

// DefaultPath returns the private per-user audit file location.
func DefaultPath() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user configuration directory: %w", err)
	}
	if strings.TrimSpace(directory) == "" {
		return "", errors.New("empty user configuration directory")
	}
	return filepath.Join(directory, "relayer", "audit", "audit.jsonl"), nil
}

// ResolvePath returns an absolute effective path, using DefaultPath for an
// empty configured value.
func ResolvePath(path string) (string, error) {
	if strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("audit path contains a NUL byte")
	}
	if strings.TrimSpace(path) == "" {
		var err error
		path, err = DefaultPath()
		if err != nil {
			return "", err
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve audit path %q: %w", path, err)
	}
	return filepath.Clean(absolute), nil
}

// Option customizes Open without weakening its filesystem invariants.
type Option func(*openOptions) error

// WithClock supplies a deterministic recorder clock.
func WithClock(clock func() time.Time) Option {
	return func(options *openOptions) error {
		if clock == nil {
			return errors.New("nil audit clock")
		}
		options.clock = clock
		return nil
	}
}

// WithIDGenerator supplies run and entry identifiers.
func WithIDGenerator(generator func() (string, error)) Option {
	return func(options *openOptions) error {
		if generator == nil {
			return errors.New("audit ID generator must not be nil")
		}
		options.idGenerator = generator
		return nil
	}
}

// WithRunID binds audit records to an externally reserved runtime identity.
// This keeps GUI generation identity stable even when auditing is disabled.
func WithRunID(runID string) Option {
	return func(options *openOptions) error {
		runID = strings.TrimSpace(runID)
		if !generatedIDPattern.MatchString(runID) {
			return errors.New("invalid explicit audit run_id")
		}
		options.runID = runID
		return nil
	}
}

// Open creates a rotating FileSink and wraps it in a Recorder. Disabled and
// off configurations perform no filesystem or ID-generator work.
func Open(config Config, options ...Option) (*Recorder, error) {
	if err := Validate(config); err != nil {
		return nil, err
	}
	resolved := openOptions{}
	for index, option := range options {
		if option == nil {
			return nil, fmt.Errorf("audit option %d is nil", index+1)
		}
		if err := option(&resolved); err != nil {
			return nil, err
		}
	}
	if !config.Enabled || config.Mode == ModeOff {
		return newRecorder(config, nil, resolved.clock, resolved.idGenerator, resolved.runID)
	}

	absolute, err := ResolvePath(config.Path)
	if err != nil {
		return nil, err
	}
	config.Path = filepath.Clean(absolute)
	maximumBytes := int64(config.MaxFileSizeMB) * 1024 * 1024
	sink, err := NewFileSink(config.Path, maximumBytes, config.MaxFiles)
	if err != nil {
		return nil, err
	}
	recorder, err := newRecorder(config, sink, resolved.clock, resolved.idGenerator, resolved.runID)
	if err != nil {
		_ = sink.Close()
		return nil, err
	}
	return recorder, nil
}

// FileSink writes complete lines synchronously and rotates them by size.
type FileSink struct {
	mu        sync.Mutex
	path      string
	maxBytes  int64
	maxFiles  int
	file      *os.File
	size      int64
	closed    bool
	closeErr  error
	stickyErr error
}

// NewFileSink opens a private append-only audit file. maxFiles includes the
// active file, so maxFiles=1 retains no rotated generation.
func NewFileSink(path string, maxBytes int64, maxFiles int) (*FileSink, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("empty audit file path")
	}
	if strings.IndexByte(path, 0) >= 0 {
		return nil, errors.New("audit file path contains a NUL byte")
	}
	if maxBytes <= 0 {
		return nil, errors.New("maximum audit file size is not positive")
	}
	if maxFiles <= 0 {
		return nil, errors.New("maximum audit file count is not positive")
	}
	if maxFiles > maximumMaxFiles {
		return nil, fmt.Errorf("maximum audit file count is greater than %d", maximumMaxFiles)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve audit file %q: %w", path, err)
	}
	absolute = filepath.Clean(absolute)
	if err := ensurePrivateDirectory(filepath.Dir(absolute)); err != nil {
		return nil, err
	}
	if err := prepareExistingGenerations(absolute, maxFiles); err != nil {
		return nil, err
	}

	result := &FileSink{path: absolute, maxBytes: maxBytes, maxFiles: maxFiles}
	if err := result.openActive(); err != nil {
		return nil, err
	}
	return result, nil
}

// Path returns the absolute active file path.
func (s *FileSink) Path() string {
	if s == nil {
		return ""
	}
	return s.path
}

// WriteLine appends and syncs exactly one complete JSONL line.
func (s *FileSink) WriteLine(line []byte) error {
	if s == nil {
		return errors.New("nil audit file sink")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return ErrClosed
	}
	if s.stickyErr != nil {
		return s.stickyErr
	}
	if len(line) == 0 || line[len(line)-1] != '\n' || bytes.IndexByte(line[:len(line)-1], '\n') >= 0 {
		return errors.New("audit sink requires exactly one newline-terminated line")
	}
	if s.size > 0 && s.size+int64(len(line)) > s.maxBytes {
		if err := s.rotate(); err != nil {
			s.stickyErr = fmt.Errorf("rotate the audit: %w", err)
			return s.stickyErr
		}
	}

	start := s.size
	written, err := s.file.Write(line)
	if err != nil || written != len(line) {
		writeErr := err
		if writeErr == nil {
			writeErr = io.ErrShortWrite
		}
		if truncateErr := s.file.Truncate(start); truncateErr != nil {
			writeErr = errors.Join(writeErr, truncateErr)
		}
		s.stickyErr = fmt.Errorf("append an audit line: %w", writeErr)
		return s.stickyErr
	}
	s.size += int64(written)
	if err := s.file.Sync(); err != nil {
		s.stickyErr = fmt.Errorf("sync an audit line: %w", err)
		return s.stickyErr
	}
	return nil
}

// Close syncs and closes the active file once.
func (s *FileSink) Close() error {
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return s.closeErr
	}
	s.closed = true
	var closeErrors []error
	if s.stickyErr != nil {
		closeErrors = append(closeErrors, s.stickyErr)
	}
	if s.file != nil {
		if err := s.file.Sync(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("final audit sync: %w", err))
		}
		if err := s.file.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("close the audit file: %w", err))
		}
		s.file = nil
	}
	s.closeErr = errors.Join(closeErrors...)
	return s.closeErr
}

func (s *FileSink) openActive() error {
	file, err := openPrivateRegularFile(s.path)
	if err != nil {
		return err
	}
	info, err := file.Stat()
	if err != nil {
		_ = file.Close()
		return fmt.Errorf("inspect the audit file %s: %w", s.path, err)
	}
	s.file = file
	s.size = info.Size()
	return nil
}

func (s *FileSink) rotate() error {
	if s.file == nil {
		return errors.New("the active audit file is unavailable")
	}
	if err := s.file.Sync(); err != nil {
		return err
	}
	if err := s.file.Close(); err != nil {
		return err
	}
	s.file = nil

	if s.maxFiles == 1 {
		if err := os.Remove(s.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return err
		}
		return s.openActive()
	}

	oldest := generationPath(s.path, s.maxFiles-1)
	if err := removeGeneration(oldest); err != nil {
		return err
	}
	for index := s.maxFiles - 2; index >= 1; index-- {
		source := generationPath(s.path, index)
		target := generationPath(s.path, index+1)
		if err := renameGeneration(source, target); err != nil {
			return err
		}
	}
	if err := renameGeneration(s.path, generationPath(s.path, 1)); err != nil {
		return err
	}
	return s.openActive()
}

func ensurePrivateDirectory(directory string) error {
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return fmt.Errorf("create audit directory %s: %w", directory, err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("permissions on the audit directory %s: %w", directory, err)
		}
		info, err = os.Lstat(directory)
	}
	if err != nil {
		return fmt.Errorf("inspect the audit directory %s: %w", directory, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("audit directory %s is not a regular directory", directory)
	}
	if err := requireCurrentUserOwner(info, directory); err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("audit directory %s is not private (permissions %04o)", directory, info.Mode().Perm())
	}
	return nil
}

func openPrivateRegularFile(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("audit file %s is not a regular file", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspect the audit file %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open the audit file %s: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("permissions on the audit file %s: %w", path, err)
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("opened audit file %s is not a regular file", path)
	}
	if err := requireCurrentUserOwner(opened, path); err != nil {
		_ = file.Close()
		return nil, err
	}
	linked, err := os.Lstat(path)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		_ = file.Close()
		return nil, fmt.Errorf("audit file %s identity is not trustworthy", path)
	}
	if err := requireRelayerJournal(file, opened.Size()); err != nil {
		_ = file.Close()
		return nil, err
	}
	if err := recoverPartialJSONL(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("recover last audit line in %s: %w", path, err)
	}
	return file, nil
}

// ErrNotAuditJournal reports an audit path that already holds a file Relayer
// did not write.
var ErrNotAuditJournal = errors.New("audit path contains a foreign file")

// maxJournalHeaderBytes bounds the first-line read used for recognition. A
// Relayer entry is far smaller; anything larger is not one.
const maxJournalHeaderBytes = 64 * 1024

// journalEntryPrefix is how every entry begins: SchemaVersion is the first
// field of Entry, so encoding/json always emits it first.
var journalEntryPrefix = []byte(`{"schema_version":`)

// requireRelayerJournal refuses to touch a non-empty file that Relayer cannot
// recognize as one of its own journals.
//
// The sink takes ownership of what it opens: it truncates a partial trailing
// line, and rotation removes surplus generations of the same base name. Without
// this gate an ordinary document at audit.path - a typo, or a deliberate but
// mistaken choice - is silently destroyed, and a file with no newline at all is
// truncated to nothing.
//
// Recognition is deliberately narrow: the first line must decode as an Entry
// carrying a known schema version and a kind from the closed vocabulary.
func requireRelayerJournal(reader io.ReaderAt, size int64) error {
	if size == 0 {
		return nil
	}
	length := size
	if length > maxJournalHeaderBytes {
		length = maxJournalHeaderBytes
	}
	header := make([]byte, length)
	if _, err := reader.ReadAt(header, 0); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("%w: unreadable", ErrNotAuditJournal)
	}
	complete := true
	if index := bytes.IndexByte(header, '\n'); index >= 0 {
		header = header[:index]
	} else if size > maxJournalHeaderBytes {
		// A first line longer than any entry Relayer writes.
		return ErrNotAuditJournal
	} else {
		complete = false
	}

	header = bytes.TrimSpace(header)
	var entry Entry
	if err := json.Unmarshal(header, &entry); err != nil {
		// A journal interrupted while writing its very first entry has no
		// complete line to decode. Recovery of that case must survive, so accept
		// an unterminated line that is unmistakably the start of one of our
		// entries. Anything else is someone's file.
		if !complete && bytes.HasPrefix(header, journalEntryPrefix) {
			return nil
		}
		return ErrNotAuditJournal
	}
	if entry.SchemaVersion < 1 || entry.SchemaVersion > CurrentSchemaVersion {
		return ErrNotAuditJournal
	}
	if entry.Kind == "" || safeKind(entry.Kind) != entry.Kind || entry.Kind == KindUnknown {
		return ErrNotAuditJournal
	}
	return nil
}

// VerifyJournalFile reports whether an existing path holds a Relayer audit
// journal. It only reads, so read-only diagnostics can warn about a foreign
// file before startup refuses to open it. An absent path is not an error here:
// callers decide what a missing journal means.
func VerifyJournalFile(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("inspect audit generation %s: %w", path, err)
	}
	defer func() { _ = file.Close() }()
	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("inspect audit generation %s: %w", path, err)
	}
	if err := requireRelayerJournal(file, info.Size()); err != nil {
		return fmt.Errorf("%w: %s", err, path)
	}
	return nil
}

func recoverPartialJSONL(file *os.File) error {
	info, err := file.Stat()
	if err != nil {
		return err
	}
	size := info.Size()
	if size == 0 {
		return nil
	}
	var last [1]byte
	if _, err := file.ReadAt(last[:], size-1); err != nil {
		return err
	}
	if last[0] == '\n' {
		return nil
	}

	const blockSize = int64(4096)
	end := size
	for end > 0 {
		start := end - blockSize
		if start < 0 {
			start = 0
		}
		block := make([]byte, end-start)
		if _, err := file.ReadAt(block, start); err != nil {
			return err
		}
		if index := bytes.LastIndexByte(block, '\n'); index >= 0 {
			if err := file.Truncate(start + int64(index) + 1); err != nil {
				return err
			}
			return file.Sync()
		}
		end = start
	}
	if err := file.Truncate(0); err != nil {
		return err
	}
	return file.Sync()
}

func prepareExistingGenerations(path string, maxFiles int) error {
	directory := filepath.Dir(path)
	base := filepath.Base(path)
	entries, err := os.ReadDir(directory)
	if err != nil {
		return fmt.Errorf("read the audit directory %s: %w", directory, err)
	}
	for _, entry := range entries {
		index, matches := AuditGenerationIndex(base, entry.Name())
		if !matches {
			continue
		}
		candidate := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(candidate)
		if err != nil {
			return fmt.Errorf("inspect audit generation %s: %w", candidate, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("audit generation %s is not a regular file", candidate)
		}
		if err := requireCurrentUserOwner(info, candidate); err != nil {
			return err
		}
		// Rotation removes and re-permissions files purely by name. A user
		// document that happens to match `<base>.<n>` must never be touched.
		if err := VerifyJournalFile(candidate); err != nil {
			return err
		}
		if index >= maxFiles {
			if err := os.Remove(candidate); err != nil {
				return fmt.Errorf("remove obsolete audit generation %s: %w", candidate, err)
			}
			continue
		}
		if err := os.Chmod(candidate, 0o600); err != nil {
			return fmt.Errorf("restrict audit generation %s permissions: %w", candidate, err)
		}
	}
	return nil
}

// AuditGenerationIndex applies the exact filename recognition used by audit
// rotation. It is exported within Relayer's internal boundary so passive
// diagnostics can inspect precisely the files the runtime would mutate.
func AuditGenerationIndex(base, name string) (int, bool) {
	if name == base {
		return 0, true
	}
	prefix := base + "."
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	suffix := strings.TrimPrefix(name, prefix)
	if suffix == "" || suffix[0] == '0' {
		return 0, false
	}
	index, err := strconv.Atoi(suffix)
	if err != nil || index <= 0 {
		return 0, false
	}
	return index, true
}

func generationPath(path string, index int) string {
	return fmt.Sprintf("%s.%d", path, index)
}

func removeGeneration(path string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("audit generation %s is not a regular file", path)
	}
	if err := requireCurrentUserOwner(info, path); err != nil {
		return err
	}
	return os.Remove(path)
}

func renameGeneration(source, target string) error {
	info, err := os.Lstat(source)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("audit generation %s is not a regular file", source)
	}
	if err := requireCurrentUserOwner(info, source); err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(source, target)
}
