package record

import (
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"strings"
)

const (
	defaultMaxFileSizeMB  = 16
	defaultMaxRecordings  = 50
	defaultMaxTotalSizeMB = 512
	defaultRetentionDays  = 30
	maximumMaxRecordings  = 10000
	maximumRetentionDays  = 3650
)

// Config controls local session recording. It is disabled by default because a
// transcript retains far more than the audit trail does.
type Config struct {
	Enabled        bool   `json:"enabled" yaml:"enabled"`
	Path           string `json:"path" yaml:"path"`
	RecordInput    bool   `json:"record_input" yaml:"record_input"`
	Redact         bool   `json:"redact" yaml:"redact"`
	MaxFileSizeMB  int    `json:"max_file_size_mb" yaml:"max_file_size_mb"`
	MaxRecordings  int    `json:"max_recordings" yaml:"max_recordings"`
	MaxTotalSizeMB int    `json:"max_total_size_mb" yaml:"max_total_size_mb"`
	RetentionDays  int    `json:"retention_days" yaml:"retention_days"`
}

// DefaultConfig leaves recording off and keeps redaction on, so enabling the
// feature never silently captures typed secrets.
func DefaultConfig() Config {
	return Config{
		Enabled:        false,
		RecordInput:    false,
		Redact:         true,
		MaxFileSizeMB:  defaultMaxFileSizeMB,
		MaxRecordings:  defaultMaxRecordings,
		MaxTotalSizeMB: defaultMaxTotalSizeMB,
		RetentionDays:  defaultRetentionDays,
	}
}

// Validate rejects ambiguous or unbounded recording settings.
func Validate(config Config) error {
	if strings.IndexByte(config.Path, 0) >= 0 {
		return errors.New("recording path contains a NUL byte")
	}
	if config.MaxFileSizeMB <= 0 {
		return errors.New("max_file_size_mb must be strictly positive")
	}
	if int64(config.MaxFileSizeMB) > math.MaxInt64/(1024*1024) {
		return errors.New("max_file_size_mb exceeds the supported size")
	}
	if config.MaxRecordings <= 0 {
		return errors.New("max_recordings must be strictly positive")
	}
	if config.MaxRecordings > maximumMaxRecordings {
		return fmt.Errorf("max_recordings cannot exceed %d", maximumMaxRecordings)
	}
	if config.MaxTotalSizeMB <= 0 {
		return errors.New("max_total_size_mb must be strictly positive")
	}
	if int64(config.MaxTotalSizeMB) > math.MaxInt64/(1024*1024) {
		return errors.New("max_total_size_mb exceeds the supported size")
	}
	if config.MaxTotalSizeMB < config.MaxFileSizeMB {
		return errors.New("max_total_size_mb is smaller than max_file_size_mb")
	}
	// Zero disables age-based retention; only a negative value is ambiguous.
	if config.RetentionDays < 0 {
		return errors.New("retention_days must not be negative")
	}
	if config.RetentionDays > maximumRetentionDays {
		return fmt.Errorf("retention_days cannot exceed %d", maximumRetentionDays)
	}
	return nil
}

// DefaultDir returns the private per-user recording directory.
func DefaultDir() (string, error) {
	directory, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("resolve user configuration directory: %w", err)
	}
	if strings.TrimSpace(directory) == "" {
		return "", errors.New("empty user configuration directory")
	}
	return filepath.Join(directory, "relayer", "recordings"), nil
}

// ResolvePath returns an absolute recording directory, using DefaultDir for an
// empty configured value.
func ResolvePath(path string) (string, error) {
	if strings.IndexByte(path, 0) >= 0 {
		return "", errors.New("recording path contains a NUL byte")
	}
	if strings.TrimSpace(path) == "" {
		var err error
		path, err = DefaultDir()
		if err != nil {
			return "", err
		}
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("resolve recording path %q: %w", path, err)
	}
	return filepath.Clean(absolute), nil
}
