package supervise

import "errors"

// These are the refusals of the supervision core. Their text is the one the
// desktop has always returned across its bridge, which is why front ends that
// predate the core alias them rather than declare their own: a caller tests
// them with errors.Is, and the sentence an operator reads is produced where it
// is displayed.
var (
	ErrAuditUnavailable    = errors.New("audit journal unavailable, no decision was sent")
	ErrDecisionStale       = errors.New("request is no longer the awaited event")
	ErrDecisionInFlight    = errors.New("a decision is already in progress for this agent")
	ErrEmptyDecision       = errors.New("an empty answer is not a decision")
	ErrUnsupportedDecision = errors.New("answer cannot be encoded for this request")
	ErrDeliveryUncertain   = errors.New("delivery state is indeterminate, stop the session before further input")
	ErrLineInFlight        = errors.New("a line is already being delivered to this agent")
	ErrLinePromptPending   = errors.New("a supervision request must be answered before free-text input")
	ErrLineUnavailable     = errors.New("session does not accept free-text input in its current state")
	ErrLineInvalid         = errors.New("invalid line")
	ErrLineUnsupported     = errors.New("backend does not support free-text input")
	ErrRuntimeStopped      = errors.New("the Relayer engine is stopped")
	ErrRunStale            = errors.New("this Relayer run is no longer active")
	ErrAgentUnknown        = errors.New("unknown agent for this run")
	ErrAgentStillRunning   = errors.New("the agent process is still running")
)

// ErrReadOnlyActor refuses a decision or a line asked for by an Actor whose
// role may only watch. It is checked before anything else, so the request
// changes nothing and journals nothing, whatever else is wrong with it.
var ErrReadOnlyActor = errors.New("permission denied: this role is read-only")

// ErrNotHolder refuses a raw write to a session's terminal from a connection
// that does not hold its hand, including any connection while nobody does.
var ErrNotHolder = errors.New("this connection does not hold the session's terminal")
