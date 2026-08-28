package audit

import (
	"bytes"
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
		return "", fmt.Errorf("résolution du dossier de configuration utilisateur: %w", err)
	}
	if strings.TrimSpace(directory) == "" {
		return "", errors.New("dossier de configuration utilisateur vide")
	}
	return filepath.Join(directory, "relayer", "audit", "audit.jsonl"), nil
}

// ResolvePath returns an absolute effective path, using DefaultPath for an
// empty configured value.
func ResolvePath(path string) (string, error) {
	if strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("chemin d'audit contenant un octet NUL")
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
		return "", fmt.Errorf("résolution du chemin d'audit %q: %w", path, err)
	}
	return filepath.Clean(absolute), nil
}

// Option customizes Open without weakening its filesystem invariants.
type Option func(*openOptions) error

// WithClock supplies a deterministic recorder clock.
func WithClock(clock func() time.Time) Option {
	return func(options *openOptions) error {
		if clock == nil {
			return errors.New("horloge d'audit nil")
		}
		options.clock = clock
		return nil
	}
}

// WithIDGenerator supplies run and entry identifiers.
func WithIDGenerator(generator func() (string, error)) Option {
	return func(options *openOptions) error {
		if generator == nil {
			return errors.New("générateur d'identifiants d'audit nil")
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
			return errors.New("run_id d'audit explicite invalide")
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
			return nil, fmt.Errorf("option d'audit %d nil", index+1)
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
		return nil, errors.New("chemin du fichier d'audit vide")
	}
	if strings.IndexByte(path, 0) >= 0 {
		return nil, errors.New("chemin du fichier d'audit contenant un octet NUL")
	}
	if maxBytes <= 0 {
		return nil, errors.New("taille maximale du fichier d'audit non positive")
	}
	if maxFiles <= 0 {
		return nil, errors.New("nombre maximal de fichiers d'audit non positif")
	}
	if maxFiles > maximumMaxFiles {
		return nil, fmt.Errorf("nombre maximal de fichiers d'audit supérieur à %d", maximumMaxFiles)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("résolution du fichier d'audit %q: %w", path, err)
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
		return errors.New("sink fichier d'audit nil")
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
		return errors.New("le sink d'audit exige exactement une ligne terminée par un saut de ligne")
	}
	if s.size > 0 && s.size+int64(len(line)) > s.maxBytes {
		if err := s.rotate(); err != nil {
			s.stickyErr = fmt.Errorf("rotation de l'audit: %w", err)
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
		s.stickyErr = fmt.Errorf("ajout d'une ligne d'audit: %w", writeErr)
		return s.stickyErr
	}
	s.size += int64(written)
	if err := s.file.Sync(); err != nil {
		s.stickyErr = fmt.Errorf("synchronisation d'une ligne d'audit: %w", err)
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
			closeErrors = append(closeErrors, fmt.Errorf("synchronisation finale de l'audit: %w", err))
		}
		if err := s.file.Close(); err != nil {
			closeErrors = append(closeErrors, fmt.Errorf("fermeture du fichier d'audit: %w", err))
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
		return fmt.Errorf("inspection du fichier d'audit %s: %w", s.path, err)
	}
	s.file = file
	s.size = info.Size()
	return nil
}

func (s *FileSink) rotate() error {
	if s.file == nil {
		return errors.New("fichier d'audit actif indisponible")
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
			return fmt.Errorf("création du dossier d'audit %s: %w", directory, err)
		}
		if err := os.Chmod(directory, 0o700); err != nil {
			return fmt.Errorf("permissions du dossier d'audit %s: %w", directory, err)
		}
		info, err = os.Lstat(directory)
	}
	if err != nil {
		return fmt.Errorf("inspection du dossier d'audit %s: %w", directory, err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("dossier d'audit %s non régulier", directory)
	}
	if err := requireCurrentUserOwner(info, directory); err != nil {
		return err
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("dossier d'audit %s non privé (permissions %04o)", directory, info.Mode().Perm())
	}
	return nil
}

func openPrivateRegularFile(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("fichier d'audit %s non régulier", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, fmt.Errorf("inspection du fichier d'audit %s: %w", path, err)
	}
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_APPEND, 0o600)
	if err != nil {
		return nil, fmt.Errorf("ouverture du fichier d'audit %s: %w", path, err)
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("permissions du fichier d'audit %s: %w", path, err)
	}
	opened, err := file.Stat()
	if err != nil || !opened.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("fichier d'audit %s ouvert non régulier", path)
	}
	if err := requireCurrentUserOwner(opened, path); err != nil {
		_ = file.Close()
		return nil, err
	}
	linked, err := os.Lstat(path)
	if err != nil || !linked.Mode().IsRegular() || !os.SameFile(opened, linked) {
		_ = file.Close()
		return nil, fmt.Errorf("identité du fichier d'audit %s non fiable", path)
	}
	if err := recoverPartialJSONL(file); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("récupération de la dernière ligne d'audit %s: %w", path, err)
	}
	return file, nil
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
		return fmt.Errorf("lecture du dossier d'audit %s: %w", directory, err)
	}
	for _, entry := range entries {
		index, matches := AuditGenerationIndex(base, entry.Name())
		if !matches {
			continue
		}
		candidate := filepath.Join(directory, entry.Name())
		info, err := os.Lstat(candidate)
		if err != nil {
			return fmt.Errorf("inspection de la génération d'audit %s: %w", candidate, err)
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("génération d'audit %s non régulière", candidate)
		}
		if err := requireCurrentUserOwner(info, candidate); err != nil {
			return err
		}
		if index >= maxFiles {
			if err := os.Remove(candidate); err != nil {
				return fmt.Errorf("suppression de la génération d'audit obsolète %s: %w", candidate, err)
			}
			continue
		}
		if err := os.Chmod(candidate, 0o600); err != nil {
			return fmt.Errorf("permissions de la génération d'audit %s: %w", candidate, err)
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
		return fmt.Errorf("génération d'audit %s non régulière", path)
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
		return fmt.Errorf("génération d'audit %s non régulière", source)
	}
	if err := requireCurrentUserOwner(info, source); err != nil {
		return err
	}
	if err := os.Remove(target); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return os.Rename(source, target)
}
