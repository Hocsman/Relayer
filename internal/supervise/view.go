package supervise

import (
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/policy"
)

// View is the display-safe form of one supervised prompt: what a front end may
// put on screen or send to a browser. Its JSON is the desktop's SupervisionEvent,
// field for field. It has no field for terminal input, adapter matches or raw
// backend text, and its summary, rule and reason have been bounded and redacted.
type View struct {
	RunID          string         `json:"runID"`
	ID             string         `json:"id"`
	SessionID      string         `json:"sessionID"`
	AgentID        string         `json:"agentID"`
	Adapter        string         `json:"adapter"`
	Type           string         `json:"type"`
	Summary        string         `json:"summary"`
	Sensitive      bool           `json:"sensitive"`
	Risk           string         `json:"risk"`
	Timestamp      string         `json:"timestamp"`
	Evaluation     EvaluationView `json:"evaluation"`
	DeliveryStatus string         `json:"deliveryStatus"`
	// Decisions are the semantic answers this event's own adapter can encode,
	// probed per event rather than assumed per adapter. An interface that
	// offered an Allow button the adapter has no verified bytes for would be
	// promising a delivery that fails at the last step. The core only ever
	// takes answers away: one the adapter could not encode when it was tried,
	// and every answer but deny when the policy denies the prompt but it goes
	// to the operator all the same. An answer not offered is refused.
	Decisions []string `json:"decisions"`
	// toolCall is the MCP tool call the prompt asks about, in the form that
	// may be shown (displayToolCall), or nil. It is not part of the JSON: the
	// desktop's interface has no badge for it, and its SupervisionEvent stays
	// what it always was. The web gateway shows it, through ToolCall.
	toolCall *adapters.ToolCall
	// resolution says how a prompt shown delivered left the run (Resolution),
	// and denyOnly that the policy denies it although it went to a person
	// (DenyOnly). Neither is part of the JSON, for the reason toolCall is not.
	resolution string
	denyOnly   bool
}

// How a prompt shown delivered left the run, as View.Resolution reports it.
const (
	// ResolutionAnswered: the answer, the policy's or a person's, was written
	// and applied.
	ResolutionAnswered = "answered"
	// ResolutionNotApplied: the agent no longer showed the prompt when its
	// answer was to be written, and nothing was written. The prompt it shows
	// instead, if any, is taken in on its own.
	ResolutionNotApplied = "not_applied"
	// ResolutionWithdrawn: the prompt went before an answer through the core
	// was applied to it. The agent took the question back, possibly while an
	// answer to it was being written, whose outcome the journal gives, or a
	// resynchronisation no longer found it on screen, most likely because it
	// was answered inside the terminal.
	ResolutionWithdrawn = "withdrawn"
)

// Resolution is how the prompt left the run when the view shows it delivered:
// ResolutionAnswered, ResolutionNotApplied or ResolutionWithdrawn. It is empty
// for a prompt still pending, delivering, uncertain or failed. "delivered" only
// says the prompt is gone, and a front end that read it as answered called a
// stale answer, which was never written, applied.
func (v View) Resolution() string { return v.resolution }

// DenyOnly reports that the policy denies the prompt, on its own or but for
// one of its limits, and that it went to a person all the same: for the hand,
// a repeat, a limit or an answer the adapter could not encode. Its Decisions
// then offer deny alone. While they offer it, a typed answer is refused
// (ErrDenyOnly); where the adapter encodes no deny they are empty, and a typed
// answer is taken and journaled with ReasonTypedOverPolicyDeny. A front end
// tells the two apart from Decisions, and says why the prompt offers what it
// does as the core decided it, instead of guessing it from the rule. A prompt
// its terminal's holder typed into (Evaluation.Reason ReasonTypedAtTerminal)
// stays DenyOnly but offers nothing, whatever the adapter encodes, and a typed
// answer to it is refused (ErrTypedAtTerminal): a front end reads that reason
// before it takes empty Decisions for an adapter that encodes no deny.
func (v View) DenyOnly() bool { return v.denyOnly }

// ToolCall is the MCP tool call the prompt asks about, as it may be shown to
// anyone who may see the prompt, or nil. A prompt whose text must not be shown
// carries none, and a call's names and parameter values are redacted as the
// journal redacts text and bounded. The result is the caller's own copy.
func (v View) ToolCall() *adapters.ToolCall {
	if v.toolCall == nil {
		return nil
	}
	call := *v.toolCall
	call.Params = append([]adapters.ToolCallParam(nil), v.toolCall.Params...)
	return &call
}

// EvaluationView is the display-safe form of a policy evaluation.
type EvaluationView struct {
	Action         string `json:"action"`
	ProposedAction string `json:"proposedAction"`
	RuleName       string `json:"ruleName,omitempty"`
	Reason         string `json:"reason"`
	Automatic      bool   `json:"automatic"`
	DryRun         bool   `json:"dryRun"`
}

// eventTimestampLayout is RFC 3339 with a fixed nine-digit fraction. Prompts are
// ordered by comparing their timestamps as text, here and in the interface;
// RFC3339Nano drops trailing zeros, so a prompt at .1 sorted after one at .12.
const eventTimestampLayout = "2006-01-02T15:04:05.000000000Z07:00"

// supervisionView builds the view of an event as evaluated by the policy. Only
// allow and deny are offered as answers: a free-text answer is not a button.
func supervisionView(
	runID string,
	event adapters.Event,
	evaluation policy.Evaluation,
	delivery string,
	decisions []adapters.Decision,
) View {
	offered := make([]string, 0, len(decisions))
	for _, decision := range decisions {
		switch decision {
		case adapters.DecisionAllow, adapters.DecisionDeny:
			offered = append(offered, string(decision))
		}
	}
	timestamp := event.Timestamp
	if timestamp.IsZero() {
		timestamp = time.Now().UTC()
	}
	return View{
		RunID:          runID,
		ID:             event.ID,
		SessionID:      event.SessionID,
		AgentID:        event.AgentID,
		Adapter:        event.Adapter,
		Type:           string(event.Type),
		Summary:        safeEventSummary(event),
		Sensitive:      requiresSecretHandling(event),
		Risk:           string(event.Risk),
		Timestamp:      timestamp.UTC().Format(eventTimestampLayout),
		Evaluation:     evaluationView(evaluation),
		DeliveryStatus: delivery,
		Decisions:      offered,
		toolCall:       displayToolCall(event),
	}
}

// evaluationView is the display form of a policy evaluation: its rule name and
// reason bounded and redacted. The core decides from the evaluation itself,
// never from this form.
func evaluationView(evaluation policy.Evaluation) EvaluationView {
	return EvaluationView{
		Action:         string(evaluation.Action),
		ProposedAction: string(evaluation.ProposedAction),
		RuleName:       safeRuleName(evaluation.RuleName),
		Reason:         safeReason(evaluation.Reason),
		Automatic:      evaluation.Automatic,
		DryRun:         evaluation.DryRun,
	}
}
