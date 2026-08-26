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
	EventCredential   EventType = "credential"
	EventProcessExit  EventType = "process_exit"
)

// RiskLevel is descriptive audit metadata. It does not apply a policy.
type RiskLevel string

const (
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
}

// NewProcessExitEvent creates the sole lifecycle event currently represented
// in the semantic stream. Metadata contains only a numeric exit code.
func NewProcessExitEvent(sessionID, agentID, adapterID string, sequence uint64, exitCode *int, failed bool) Event {
	if sequence == 0 {
		sequence = 1
	}
	summary := "processus terminé"
	metadata := make(map[string]string)
	if failed {
		summary = "processus terminé avec erreur"
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
	case EventConfirmation, EventCredential:
		return true
	default:
		return false
	}
}

// Decision is deliberately limited to actions represented by current code.
// Automatic approval/denial policies are outside this phase.
type Decision string

const DecisionManual Decision = "manual"

var (
	ErrUnknownAdapter      = errors.New("adaptateur inconnu")
	ErrAdapterUnavailable  = errors.New("adaptateur non implémenté")
	ErrDecisionUnsupported = errors.New("décision non prise en charge")
	ErrEventMismatch       = errors.New("événement en attente différent")
	ErrProcessorTerminated = errors.New("flux sémantique de l'adaptateur terminé")
)
