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

// ErrAnswerInvalid refuses a typed answer that is not one line of text: it
// holds a control character, CR, LF and escape included, is not valid UTF-8,
// or is longer than a line may be. It is checked before anything is claimed,
// journaled or shown, as an empty answer is.
var ErrAnswerInvalid = errors.New("a typed answer must be one line of text, with no control characters and no more than 4096 bytes")

// ErrDenyOnly refuses a typed answer to a prompt the policy denies that went to
// a person all the same, because somebody holds the terminal, a limit was
// reached, it repeats an answer just written or the adapter could not encode
// the deny: such a prompt takes the adapter's deny alone. Typed text is
// whatever the adapter reads it as, its accept included. Where the adapter
// encodes no deny, the prompt is answered at the terminal, whose taking is
// journaled, or its agent is stopped.
var ErrDenyOnly = errors.New("the policy denies this request: only its deny is accepted")

// ErrTypedAtTerminal refuses an answer to a prompt that was shown while its
// terminal's holder typed into it (ReasonTypedAtTerminal): the keystrokes may
// have answered it, and an answer given now would reach whatever the agent
// asks next. It is answered at the terminal.
var ErrTypedAtTerminal = errors.New("keys were typed at this request's terminal while it was shown: answer it at the terminal")

// ErrReadOnlyActor refuses a decision or a line asked for by an Actor whose
// role may only watch. It is checked before anything else, so the request
// changes nothing and journals nothing, whatever else is wrong with it.
var ErrReadOnlyActor = errors.New("permission denied: this role is read-only")

// ErrNotHolder refuses a raw write to a session's terminal from a connection
// that does not hold its hand, including any connection while nobody does.
var ErrNotHolder = errors.New("this connection does not hold the session's terminal")

// ErrUnsupportedEntry refuses an entry a front end asked the core to journal
// (RecordAudit) whose kind is not one a front end writes: only who took or
// let go of a terminal, how its control changed hands, and the lifecycle of a
// recording. Every other kind is the core's own, or the runtime's.
var ErrUnsupportedEntry = errors.New("this entry is not one a front end journals through the core")
