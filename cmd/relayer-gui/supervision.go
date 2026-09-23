package main

import "github.com/Hocsman/Relayer/internal/supervise"

// supervisionEventFromView is the bridge's DTO for a core view. The two have
// the same fields and the same JSON; the bridge keeps a type of its own because
// Wails generates the frontend's models from the types its methods return, and
// a core type would move them into another namespace.
func supervisionEventFromView(view supervise.View) SupervisionEvent {
	return SupervisionEvent{
		RunID:          view.RunID,
		ID:             view.ID,
		SessionID:      view.SessionID,
		AgentID:        view.AgentID,
		Adapter:        view.Adapter,
		Type:           view.Type,
		Summary:        view.Summary,
		Sensitive:      view.Sensitive,
		Risk:           view.Risk,
		Timestamp:      view.Timestamp,
		Evaluation:     PolicyEvaluation(view.Evaluation),
		DeliveryStatus: view.DeliveryStatus,
		Decisions:      view.Decisions,
	}
}

// agentSpecOf is the journal identity of an agent the bridge displays.
func agentSpecOf(agent AgentState) supervise.AgentSpec {
	return supervise.AgentSpec{
		SessionID: agent.SessionID,
		AgentID:   agent.AgentID,
		Name:      agent.Name,
		Backend:   agent.Backend,
		Adapter:   agent.Adapter,
	}
}
