// Package audit writes a bounded, local JSONL audit trail without retaining
// terminal input, raw prompt matches, environment values, or backend errors.
package audit

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
)

const (
	// CurrentSchemaVersion identifies the on-disk Entry representation.
	CurrentSchemaVersion = 1
	defaultMaxFileSizeMB = 10
	defaultMaxFiles      = 5
	maximumMaxFiles      = 100
)

// Mode controls how much already-sanitized event information reaches disk.
type Mode string

const (
	ModeOff      Mode = "off"
	ModeMetadata Mode = "metadata"
	ModeDetailed Mode = "detailed"
)

// Kind identifies a closed set of audit lifecycle records.
type Kind string

const (
	KindRunStarted     Kind = "run_started"
	KindRunFinished    Kind = "run_finished"
	KindSessionStarted Kind = "session_started"
	// KindSupervisionFinished means Relayer stopped supervising the session;
	// it does not claim that a persistent tmux process exited.
	KindSupervisionFinished Kind = "supervision_finished"
	KindSessionFinished     Kind = "session_finished"
	KindEventDetected       Kind = "event_detected"
	// KindEventWithdrawn records that an occurrence which was awaiting a human
	// stopped being pending without a decision being delivered. It is the only
	// evidence that a supervision gate opened on its own.
	KindEventWithdrawn  Kind = "event_withdrawn"
	KindPolicyEvaluated Kind = "policy_evaluated"
	KindDecision        Kind = "decision"
	KindDelivery        Kind = "delivery"
	// KindOperatorInput records only the lifecycle of a direct, human line
	// submission. Entry intentionally has no field for the submitted text,
	// its length, or the encoded terminal bytes.
	KindOperatorInput  Kind = "operator_input"
	KindAttachStarted  Kind = "attach_started"
	KindAttachFinished Kind = "attach_finished"
	KindBackendError   Kind = "backend_error"
	KindSessionCleanup Kind = "session_cleanup"
	KindUnknown        Kind = "unknown"
)

// DecisionBy identifies the actor without storing any submitted value.
type DecisionBy string

const (
	DecisionBySystem  DecisionBy = "system"
	DecisionByHuman   DecisionBy = "human"
	DecisionByPolicy  DecisionBy = "policy"
	DecisionByUnknown DecisionBy = "unknown"
)

// Decision is the audited policy outcome. It intentionally differs from an
// adapter's wire decision: asking a human is an explicit, content-free audit
// action rather than a copy of the submitted terminal input.
type Decision string

const (
	DecisionAllow   Decision = "allow"
	DecisionAsk     Decision = "ask"
	DecisionDeny    Decision = "deny"
	DecisionUnknown Decision = "unknown"
)

// Outcome is a safe, finite result vocabulary for lifecycle and policy audit.
type Outcome string

const (
	OutcomeStarted                   Outcome = "started"
	OutcomeFinished                  Outcome = "finished"
	OutcomeDetected                  Outcome = "detected"
	OutcomePending                   Outcome = "pending"
	OutcomeInFlight                  Outcome = "in_flight"
	OutcomeApplied                   Outcome = "applied"
	OutcomeAsk                       Outcome = "ask"
	OutcomeDryRun                    Outcome = "dry_run"
	OutcomeFallbackUnsupported       Outcome = "fallback_unsupported"
	OutcomeFallbackStale             Outcome = "fallback_stale"
	OutcomeFallbackDeliveryUncertain Outcome = "fallback_delivery_uncertain"
	OutcomeSucceeded                 Outcome = "succeeded"
	OutcomeFailed                    Outcome = "failed"
	OutcomeCancelled                 Outcome = "cancelled"
	OutcomeSkipped                   Outcome = "skipped"
	OutcomeUnknown                   Outcome = "unknown"
)

// Config controls local audit persistence. MaxFiles counts the active file as
// well as its rotated generations.
type Config struct {
	Enabled       bool   `json:"enabled" yaml:"enabled"`
	Mode          Mode   `json:"mode" yaml:"mode"`
	Path          string `json:"path" yaml:"path"`
	MaxFileSizeMB int    `json:"max_file_size_mb" yaml:"max_file_size_mb"`
	MaxFiles      int    `json:"max_files" yaml:"max_files"`
}

// DefaultConfig enables a conservative metadata-only audit trail.
func DefaultConfig() Config {
	return Config{
		Enabled:       true,
		Mode:          ModeMetadata,
		MaxFileSizeMB: defaultMaxFileSizeMB,
		MaxFiles:      defaultMaxFiles,
	}
}

// Validate rejects ambiguous or unsafe recorder settings.
func Validate(config Config) error {
	switch config.Mode {
	case ModeOff, ModeMetadata, ModeDetailed:
	default:
		return fmt.Errorf("invalid audit mode %q", config.Mode)
	}
	if strings.IndexByte(config.Path, 0) >= 0 {
		return errors.New("audit path contains a NUL byte")
	}
	if config.MaxFileSizeMB <= 0 {
		return errors.New("max_file_size_mb must be strictly positive")
	}
	if int64(config.MaxFileSizeMB) > math.MaxInt64/(1024*1024) {
		return errors.New("max_file_size_mb exceeds the supported size")
	}
	if config.MaxFiles <= 0 {
		return errors.New("max_files must be strictly positive")
	}
	if config.MaxFiles > maximumMaxFiles {
		return fmt.Errorf("max_files cannot exceed %d", maximumMaxFiles)
	}
	return nil
}

// Entry is the versioned JSONL record. It intentionally has no Match,
// manual-input, environment, or raw-error field.
type Entry struct {
	SchemaVersion int                `json:"schema_version"`
	Sequence      uint64             `json:"sequence"`
	Timestamp     time.Time          `json:"timestamp"`
	EntryID       string             `json:"entry_id"`
	RunID         string             `json:"run_id"`
	Kind          Kind               `json:"kind,omitempty"`
	SessionID     string             `json:"session_id,omitempty"`
	AgentID       string             `json:"agent_id,omitempty"`
	Backend       string             `json:"backend,omitempty"`
	Adapter       string             `json:"adapter,omitempty"`
	EventID       string             `json:"event_id,omitempty"`
	EventType     adapters.EventType `json:"event_type,omitempty"`
	Risk          adapters.RiskLevel `json:"risk,omitempty"`
	Rule          string             `json:"rule,omitempty"`
	Decision      Decision           `json:"decision,omitempty"`
	DecisionBy    DecisionBy         `json:"decision_by,omitempty"`
	Outcome       Outcome            `json:"outcome,omitempty"`
	Reason        string             `json:"reason,omitempty"`
	Summary       string             `json:"summary,omitempty"`
	Sensitive     bool               `json:"sensitive"`
	Metadata      map[string]string  `json:"metadata,omitempty"`
}

// LineSink owns persistence of complete JSONL lines.
type LineSink interface {
	WriteLine([]byte) error
	Close() error
}

var ErrClosed = errors.New("audit recorder is closed")
