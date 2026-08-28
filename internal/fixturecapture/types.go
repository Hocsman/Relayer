// Package fixturecapture records bounded, anonymized terminal output for
// adapter fixtures. It deliberately has no input, shell-script, environment,
// provider, or credential API.
package fixturecapture

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"
)

const (
	// SchemaVersion is incremented when the persisted JSON contract changes.
	SchemaVersion = 1

	DefaultMaxBytes = 256 * 1024
	HardMaxBytes    = 4 * 1024 * 1024
	DefaultTimeout  = 20 * time.Second
	MaximumTimeout  = 5 * time.Minute

	artifactChunkBytes = 4096
	maxArtifactChunks  = (HardMaxBytes + artifactChunkBytes - 1) / artifactChunkBytes
)

var (
	ErrSensitiveContent = errors.New("fixture contains sensitive content")
	ErrUnsupported      = errors.New("fixture capture is unsupported on this platform")

	safeIdentifier = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,63}$`)
)

// Outcome describes why an otherwise successful output-only capture ended.
// A timeout and an output limit are expected capture outcomes, not claims
// about the tool that was observed.
type Outcome string

const (
	OutcomeExited      Outcome = "exited"
	OutcomeTimedOut    Outcome = "timed_out"
	OutcomeOutputLimit Outcome = "output_limit"
)

// Backend identifies the local terminal transport used for the observation.
type Backend string

const (
	BackendPTY  Backend = "pty"
	BackendTmux Backend = "tmux"
)

// Chunk is one bounded piece of already anonymized terminal output.
type Chunk struct {
	Sequence int    `json:"sequence"`
	Data     string `json:"data"`
}

// Fixture is the complete versioned on-disk contract. It intentionally omits
// argv, cwd, environment variables, raw input, timestamps, and user identity.
type Fixture struct {
	SchemaVersion int     `json:"schema_version"`
	Tool          string  `json:"tool"`
	Adapter       string  `json:"adapter"`
	Backend       Backend `json:"backend"`
	Outcome       Outcome `json:"outcome"`
	ExitCode      *int    `json:"exit_code,omitempty"`
	Truncated     bool    `json:"truncated,omitempty"`
	Chunks        []Chunk `json:"chunks"`
}

// Options configures an output-only PTY capture. Command is exact argv and the
// harness never wraps it in an implicit shell. A caller can still explicitly
// choose a shell executable, so argv remains security-sensitive. There is
// intentionally no environment or stdin field. The child receives a small internal allowlist of non-secret
// process environment variables plus disposable private HOME, TMPDIR, and XDG
// roots; it never inherits the caller's configuration directories.
type Options struct {
	Tool       string
	Adapter    string
	Backend    Backend
	Command    []string
	Cwd        string
	TmuxPath   string
	HelperPath string
	Timeout    time.Duration
	MaxBytes   int
	Anonymizer *Anonymizer
}

func normalizeOptions(options Options) (Options, error) {
	options.Tool = strings.ToLower(strings.TrimSpace(options.Tool))
	options.Adapter = strings.ToLower(strings.TrimSpace(options.Adapter))
	options.Backend = Backend(strings.ToLower(strings.TrimSpace(string(options.Backend))))
	if options.Backend == "" {
		options.Backend = BackendPTY
	}
	if !safeIdentifier.MatchString(options.Tool) {
		return Options{}, fmt.Errorf("tool must match %s", safeIdentifier)
	}
	if !safeIdentifier.MatchString(options.Adapter) {
		return Options{}, fmt.Errorf("adapter must match %s", safeIdentifier)
	}
	if options.Backend != BackendPTY && options.Backend != BackendTmux {
		return Options{}, fmt.Errorf("backend must be %q or %q", BackendPTY, BackendTmux)
	}
	if len(options.Command) == 0 || strings.TrimSpace(options.Command[0]) == "" {
		return Options{}, errors.New("an explicit argv with a non-blank executable is required")
	}
	options.Command = append([]string(nil), options.Command...)
	for index, argument := range options.Command {
		if strings.IndexByte(argument, 0) >= 0 {
			return Options{}, fmt.Errorf("argv entry %d contains a NUL byte", index)
		}
	}
	if strings.IndexByte(options.Cwd, 0) >= 0 {
		return Options{}, errors.New("working directory contains a NUL byte")
	}
	if strings.IndexByte(options.TmuxPath, 0) >= 0 {
		return Options{}, errors.New("tmux path contains a NUL byte")
	}
	if strings.IndexByte(options.HelperPath, 0) >= 0 {
		return Options{}, errors.New("helper path contains a NUL byte")
	}
	if options.Timeout == 0 {
		options.Timeout = DefaultTimeout
	}
	if options.Timeout < 0 || options.Timeout > MaximumTimeout {
		return Options{}, fmt.Errorf("timeout must be between 1ns and %s", MaximumTimeout)
	}
	if options.MaxBytes == 0 {
		options.MaxBytes = DefaultMaxBytes
	}
	if options.MaxBytes < 1 || options.MaxBytes > HardMaxBytes {
		return Options{}, fmt.Errorf("max bytes must be between 1 and %d", HardMaxBytes)
	}
	if options.Anonymizer == nil {
		anonymizer, err := NewDefaultAnonymizer()
		if err != nil {
			return Options{}, err
		}
		options.Anonymizer = anonymizer
	}
	if err := validatePersistedIdentifier("tool", options.Tool, options.Anonymizer); err != nil {
		return Options{}, err
	}
	if err := validatePersistedIdentifier("adapter", options.Adapter, options.Anonymizer); err != nil {
		return Options{}, err
	}
	if _, err := options.Anonymizer.Anonymize([]byte(options.Command[0])); err != nil {
		return Options{}, fmt.Errorf("argv executable: %w", ErrSensitiveContent)
	}
	for index, argument := range options.Command[1:] {
		sanitized, err := options.Anonymizer.Anonymize([]byte(argument))
		if err != nil || string(sanitized) != argument {
			return Options{}, fmt.Errorf("argv entry %d: %w", index+1, ErrSensitiveContent)
		}
	}
	if len(options.Command) > 1 {
		for _, separator := range []string{" ", ""} {
			arguments := strings.Join(options.Command[1:], separator)
			sanitized, err := options.Anonymizer.Anonymize([]byte(arguments))
			if err != nil || string(sanitized) != arguments {
				return Options{}, fmt.Errorf("combined argv: %w", ErrSensitiveContent)
			}
		}
	}
	return options, nil
}

func validatePersistedIdentifier(field, value string, anonymizer *Anonymizer) error {
	sanitized, err := anonymizer.Anonymize([]byte(value))
	if err != nil || string(sanitized) != value {
		return fmt.Errorf("%s: %w", field, ErrSensitiveContent)
	}
	return nil
}

func cloneFixture(fixture Fixture) Fixture {
	result := fixture
	if fixture.ExitCode != nil {
		code := *fixture.ExitCode
		result.ExitCode = &code
	}
	if fixture.Chunks != nil {
		result.Chunks = make([]Chunk, len(fixture.Chunks))
		copy(result.Chunks, fixture.Chunks)
	}
	return result
}
