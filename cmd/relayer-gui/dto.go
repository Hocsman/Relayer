package main

// These DTOs are intentionally closed and display-safe. In particular there
// is no field for terminal input, adapter matches, environment values or raw
// backend errors.

type PolicyState struct {
	DefaultAction string `json:"defaultAction"`
	DryRun        bool   `json:"dryRun"`
}

type AuditState struct {
	Enabled bool   `json:"enabled"`
	Mode    string `json:"mode"`
	Status  string `json:"status"`
	Path    string `json:"path,omitempty"`
}

type AgentState struct {
	SessionID      string `json:"sessionID"`
	AgentID        string `json:"agentID"`
	Name           string `json:"name"`
	DisplayCommand string `json:"displayCommand"`
	Backend        string `json:"backend"`
	Adapter        string `json:"adapter"`
	Status         string `json:"status"`
	Output         string `json:"output"`
	Revision       uint64 `json:"revision"`
	Running        bool   `json:"running"`
	Attached       bool   `json:"attached"`
	ExitCode       *int   `json:"exitCode,omitempty"`
}

type PolicyEvaluation struct {
	Action         string `json:"action"`
	ProposedAction string `json:"proposedAction"`
	RuleName       string `json:"ruleName,omitempty"`
	Reason         string `json:"reason"`
	Automatic      bool   `json:"automatic"`
	DryRun         bool   `json:"dryRun"`
}

type SupervisionEvent struct {
	RunID          string           `json:"runID"`
	ID             string           `json:"id"`
	SessionID      string           `json:"sessionID"`
	AgentID        string           `json:"agentID"`
	Adapter        string           `json:"adapter"`
	Type           string           `json:"type"`
	Summary        string           `json:"summary"`
	Sensitive      bool             `json:"sensitive"`
	Risk           string           `json:"risk"`
	Timestamp      string           `json:"timestamp"`
	Evaluation     PolicyEvaluation `json:"evaluation"`
	DeliveryStatus string           `json:"deliveryStatus"`
}

type AppState struct {
	RunID         string             `json:"runID"`
	RunStatus     string             `json:"runStatus"`
	StartedAt     string             `json:"startedAt,omitempty"`
	Policy        PolicyState        `json:"policy"`
	Audit         AuditState         `json:"audit"`
	Agents        []AgentState       `json:"agents"`
	PendingEvents []SupervisionEvent `json:"pendingEvents"`
}

type SnapshotEvent struct {
	RunID     string `json:"runID"`
	SessionID string `json:"sessionID"`
	Revision  uint64 `json:"revision"`
	Output    string `json:"output"`
	Status    string `json:"status"`
	Running   bool   `json:"running"`
	Attached  bool   `json:"attached"`
	ExitCode  *int   `json:"exitCode,omitempty"`
}

type StatusEvent struct {
	RunID     string `json:"runID"`
	Scope     string `json:"scope"`
	Status    string `json:"status"`
	SessionID string `json:"sessionID,omitempty"`
}

type SafeErrorEvent struct {
	RunID     string `json:"runID"`
	Code      string `json:"code"`
	Message   string `json:"message"`
	SessionID string `json:"sessionID,omitempty"`
	Timestamp string `json:"timestamp"`
}

type RestartAgentProfilesRequest struct {
	ExpectedRunID    string              `json:"expectedRunID"`
	ExpectedRevision string              `json:"expectedRevision"`
	Profiles         []AgentProfileInput `json:"profiles"`
}

type AgentLifecycleResult struct {
	Outcome  string            `json:"outcome"`
	State    AppState          `json:"state"`
	Profiles AgentProfilesView `json:"profiles"`
}
