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
	// promising a delivery that fails at the last step.
	Decisions []string `json:"decisions"`
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

// NewView builds the view of an event as evaluated by the policy. Only allow
// and deny are offered as answers: a free-text answer is not a button.
func NewView(
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
		RunID:     runID,
		ID:        event.ID,
		SessionID: event.SessionID,
		AgentID:   event.AgentID,
		Adapter:   event.Adapter,
		Type:      string(event.Type),
		Summary:   SafeEventSummary(event),
		Sensitive: RequiresSecretHandling(event),
		Risk:      string(event.Risk),
		Timestamp: timestamp.UTC().Format(eventTimestampLayout),
		Evaluation: EvaluationView{
			Action:         string(evaluation.Action),
			ProposedAction: string(evaluation.ProposedAction),
			RuleName:       SafeRuleName(evaluation.RuleName),
			Reason:         SafeReason(evaluation.Reason),
			Automatic:      evaluation.Automatic,
			DryRun:         evaluation.DryRun,
		},
		DeliveryStatus: delivery,
		Decisions:      offered,
	}
}
