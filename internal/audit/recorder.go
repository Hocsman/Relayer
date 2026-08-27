package audit

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
)

var generatedIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,127}$`)

// Recorder assigns identities and a total order before writing sanitized JSONL.
type Recorder struct {
	mu          sync.Mutex
	config      Config
	sink        LineSink
	clock       func() time.Time
	idGenerator func() (string, error)
	runID       string
	sequence    uint64
	closed      bool
	closeErr    error
	writeErr    error
}

// NewRecorder constructs a recorder around an injectable line sink, clock,
// and ID generator. Nil clock and generator select secure production defaults.
func NewRecorder(
	config Config,
	sink LineSink,
	clock func() time.Time,
	idGenerator func() (string, error),
) (*Recorder, error) {
	return newRecorder(config, sink, clock, idGenerator, "")
}

func newRecorder(
	config Config,
	sink LineSink,
	clock func() time.Time,
	idGenerator func() (string, error),
	explicitRunID string,
) (*Recorder, error) {
	if err := Validate(config); err != nil {
		return nil, err
	}
	if clock == nil {
		clock = time.Now
	}
	if idGenerator == nil {
		idGenerator = randomID
	}

	result := &Recorder{
		config:      config,
		clock:       clock,
		idGenerator: idGenerator,
		runID:       explicitRunID,
	}
	if !result.Enabled() {
		return result, nil
	}
	if sink == nil {
		return nil, errors.New("sink d'audit nil")
	}
	runID := explicitRunID
	if runID == "" {
		var err error
		runID, err = idGenerator()
		if err != nil {
			return nil, fmt.Errorf("génération du run_id d'audit: %w", err)
		}
	}
	if !generatedIDPattern.MatchString(runID) {
		return nil, errors.New("générateur d'identifiants d'audit retournant un run_id invalide")
	}
	result.sink = sink
	result.runID = runID
	return result, nil
}

// Record sanitizes and synchronously persists one entry. Holding the recorder
// lock through WriteLine makes Sequence identical to accepted on-disk order.
func (r *Recorder) Record(entry Entry) error {
	if r == nil || !r.Enabled() {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return ErrClosed
	}
	if r.writeErr != nil {
		return r.writeErr
	}

	entryID, err := r.idGenerator()
	if err != nil {
		return fmt.Errorf("génération de l'entry_id d'audit: %w", err)
	}
	if !generatedIDPattern.MatchString(entryID) {
		return errors.New("générateur d'identifiants d'audit retournant un entry_id invalide")
	}

	nextSequence := r.sequence + 1
	entry = SanitizeEntry(entry, r.config.Mode)
	entry.SchemaVersion = CurrentSchemaVersion
	entry.Sequence = nextSequence
	entry.Timestamp = r.clock().UTC()
	entry.EntryID = entryID
	entry.RunID = r.runID
	encoded, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encodage JSON de l'audit: %w", err)
	}
	encoded = append(encoded, '\n')
	if err := r.sink.WriteLine(encoded); err != nil {
		r.writeErr = fmt.Errorf("écriture de l'audit: %w", err)
		return r.writeErr
	}
	r.sequence = nextSequence
	return nil
}

// Close is safe to call concurrently and closes the underlying sink once.
func (r *Recorder) Close() error {
	if r == nil || !r.Enabled() {
		return nil
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	r.closeErr = errors.Join(r.writeErr, r.sink.Close())
	return r.closeErr
}

// RunID returns the immutable identifier shared by this recorder's entries.
func (r *Recorder) RunID() string {
	if r == nil {
		return ""
	}
	return r.runID
}

// Path returns the configured or effective audit path.
func (r *Recorder) Path() string {
	if r == nil {
		return ""
	}
	return r.config.Path
}

// Enabled reports whether this recorder performs any work.
func (r *Recorder) Enabled() bool {
	return r != nil && r.config.Enabled && r.config.Mode != ModeOff
}

func randomID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}
