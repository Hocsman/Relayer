// Package adapters defines backend-neutral agent events and the adapters that
// derive them from normalized terminal text.
package adapters

import (
	"errors"
	"strconv"
	"strings"
	"time"
)

// EventType describes an observation with a concrete meaning in Relayer.
// Output invalidations and backend failures intentionally remain transport
// signals: they are not agent events.
type EventType string

const (
	EventConfirmation EventType = "confirmation"
	EventPermission   EventType = "permission"
	EventCredential   EventType = "credential"
	EventProcessExit  EventType = "process_exit"
)

// RiskLevel is descriptive audit metadata. It does not apply a policy.
type RiskLevel string

const (
	RiskLow     RiskLevel = "low"
	RiskUnknown RiskLevel = "unknown"
	RiskHigh    RiskLevel = "high"
)

// Event is the single semantic representation shared by adapters, backends,
// snapshots and the TUI. ID identifies one occurrence; Signature is stable for
// equivalent normalized content and is used only while reconciling duplicates.
type Event struct {
	ID        string
	Signature string
	Sequence  uint64
	SessionID string
	AgentID   string
	Adapter   string
	Type      EventType
	Summary   string
	Match     string
	Sensitive bool
	Risk      RiskLevel
	Timestamp time.Time
	Metadata  map[string]string

	// questionLine is the logical terminal line the match sits on: the question
	// as it was asked, rather than the fragment a pattern happened to capture.
	//
	// It is unexported on purpose. It is terminal text, so it must not reach
	// the audit journal, a snapshot or any consumer outside this package; it
	// exists only so that the state can tell one question from another. Clone
	// copies it because two occurrences of the same struct describe the same
	// question, and a restored event simply arrives without it — which costs a
	// question being asked again, never one being swallowed.
	questionLine string
}

// NewProcessExitEvent creates the sole lifecycle event currently represented
// in the semantic stream. Metadata contains only a numeric exit code.
func NewProcessExitEvent(sessionID, agentID, adapterID string, sequence uint64, exitCode *int, failed bool) Event {
	if sequence == 0 {
		sequence = 1
	}
	summary := "process exited"
	metadata := make(map[string]string)
	if failed {
		summary = "process exited with error"
		metadata["failed"] = "true"
	}
	if exitCode != nil {
		metadata["exit_code"] = strconv.Itoa(*exitCode)
	}
	signature := stableSignature(sessionID, adapterID, EventProcessExit, "process_exit", summary)
	return Event{
		ID:        occurrenceID(signature, sequence),
		Signature: signature,
		Sequence:  sequence,
		SessionID: strings.TrimSpace(sessionID),
		AgentID:   strings.TrimSpace(agentID),
		Adapter:   strings.ToLower(strings.TrimSpace(adapterID)),
		Type:      EventProcessExit,
		Summary:   summary,
		Risk:      RiskUnknown,
		Timestamp: time.Now().UTC(),
		Metadata:  metadata,
	}
}

// Clone returns an event whose mutable metadata cannot alias the source.
func (e Event) Clone() Event {
	clone := e
	if e.Metadata != nil {
		clone.Metadata = make(map[string]string, len(e.Metadata))
		for key, value := range e.Metadata {
			clone.Metadata[key] = value
		}
	}
	return clone
}

// Actionable reports whether Relayer must pause for a human decision.
func (e Event) Actionable() bool {
	switch e.Type {
	case EventConfirmation, EventPermission, EventCredential:
		return true
	default:
		return false
	}
}

// Decision is deliberately limited to actions represented by current code.
// Adapters may reject allow or deny when they cannot encode the action
// reliably; callers must then retain the pending event for human input.
type Decision string

const (
	DecisionManual Decision = "manual"
	DecisionAllow  Decision = "allow"
	DecisionDeny   Decision = "deny"
)

var (
	ErrUnknownAdapter      = errors.New("unknown adapter")
	ErrAdapterUnavailable  = errors.New("adapter not implemented")
	ErrDecisionUnsupported = errors.New("unsupported decision")
	ErrEventMismatch       = errors.New("pending event mismatch")
	ErrProcessorTerminated = errors.New("adapter semantic stream terminated")
)
