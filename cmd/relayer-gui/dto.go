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
	InputFrozen    bool   `json:"inputFrozen"`
	Simulated      bool   `json:"simulated"`
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
	// Decisions are the semantic answers this event's own adapter can encode,
	// probed per event rather than assumed per adapter. An interface that
	// offered an Allow button the adapter has no verified bytes for would be
	// promising a delivery that fails at the last step.
	Decisions []string `json:"decisions"`
}

type AppState struct {
	RunID         string             `json:"runID"`
	RunStatus     string             `json:"runStatus"`
	StartedAt     string             `json:"startedAt,omitempty"`
	Policy        PolicyState        `json:"policy"`
	Audit         AuditState         `json:"audit"`
	Agents        []AgentState       `json:"agents"`
	PendingEvents []SupervisionEvent `json:"pendingEvents"`

	// Notices are the resolution warnings and startup facts the terminal
	// interface prints. They previously went to standard error, which an
	// application launched from a file manager does not have, so "tmux is
	// unavailable, falling back to PTY" and "two demo agents were substituted"
	// reached nobody.
	Notices []string `json:"notices"`
}

type SnapshotEvent struct {
	RunID       string `json:"runID"`
	SessionID   string `json:"sessionID"`
	Revision    uint64 `json:"revision"`
	Output      string `json:"output"`
	Status      string `json:"status"`
	Running     bool   `json:"running"`
	Attached    bool   `json:"attached"`
	InputFrozen bool   `json:"inputFrozen"`
	ExitCode    *int   `json:"exitCode,omitempty"`
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

type PreflightPlatform struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Supported bool   `json:"supported"`
}

type PreflightConfiguration struct {
	Version         int  `json:"version"`
	Legacy          bool `json:"legacy"`
	AgentCount      int  `json:"agentCount"`
	PolicyRuleCount int  `json:"policyRuleCount"`
}

type PreflightAudit struct {
	Enabled       bool   `json:"enabled"`
	Mode          string `json:"mode"`
	Location      string `json:"location"`
	MaxFileSizeMB int    `json:"maxFileSizeMB"`
	MaxFiles      int    `json:"maxFiles"`
}

type PreflightTool struct {
	ProfileID    string `json:"profileID"`
	Installation string `json:"installation"`
}

type PreflightAgent struct {
	Ordinal         int    `json:"ordinal"`
	Source          string `json:"source"`
	Command         string `json:"command"`
	Installation    string `json:"installation"`
	Adapter         string `json:"adapter,omitempty"`
	AdapterMaturity string `json:"adapterMaturity,omitempty"`
	Backend         string `json:"backend,omitempty"`
}

type PreflightCheck struct {
	ID          string `json:"id"`
	Scope       string `json:"scope"`
	Status      string `json:"status"`
	Summary     string `json:"summary"`
	Remediation string `json:"remediation,omitempty"`
}

// PreflightReport is a direct camel-case projection of the versioned,
// display-safe core report. It intentionally adds no path, argv, environment,
// agent identity or raw error field.
type PreflightReport struct {
	SchemaVersion int                    `json:"schemaVersion"`
	Status        string                 `json:"status"`
	Platform      PreflightPlatform      `json:"platform"`
	Configuration PreflightConfiguration `json:"configuration"`
	Audit         PreflightAudit         `json:"audit"`
	Tools         []PreflightTool        `json:"tools"`
	Agents        []PreflightAgent       `json:"agents"`
	Checks        []PreflightCheck       `json:"checks"`
}

type AuditSummaryView struct {
	Path           string         `json:"path"`
	TotalEntries   int            `json:"totalEntries"`
	RunsCount      int            `json:"runsCount"`
	SessionsCount  int            `json:"sessionsCount"`
	AgentCounts    map[string]int `json:"agentCounts"`
	KindCounts     map[string]int `json:"kindCounts"`
	DecisionsCount map[string]int `json:"decisionsCount"`
	ActorsCount    map[string]int `json:"actorsCount"`
	OutcomesCount  map[string]int `json:"outcomesCount"`
	SensitiveCount int            `json:"sensitiveCount"`
	FirstTimestamp string         `json:"firstTimestamp,omitempty"`
	LastTimestamp  string         `json:"lastTimestamp,omitempty"`
}

type AuditVerificationIssueView struct {
	Line    int    `json:"line"`
	EntryID string `json:"entryID,omitempty"`
	Message string `json:"message"`
}

type AuditVerificationView struct {
	Path       string                       `json:"path"`
	TotalLines int                          `json:"totalLines"`
	TotalRuns  int                          `json:"totalRuns"`
	ValidLines int                          `json:"validLines"`
	Issues     []AuditVerificationIssueView `json:"issues"`
	Passed     bool                         `json:"passed"`
}

type AuditEntryView struct {
	Sequence   uint64            `json:"sequence"`
	Timestamp  string            `json:"timestamp"`
	EntryID    string            `json:"entryID"`
	RunID      string            `json:"runID"`
	Kind       string            `json:"kind"`
	SessionID  string            `json:"sessionID,omitempty"`
	AgentID    string            `json:"agentID,omitempty"`
	Backend    string            `json:"backend,omitempty"`
	Adapter    string            `json:"adapter,omitempty"`
	EventType  string            `json:"eventType,omitempty"`
	Risk       string            `json:"risk,omitempty"`
	Rule       string            `json:"rule,omitempty"`
	Decision   string            `json:"decision,omitempty"`
	DecisionBy string            `json:"decisionBy,omitempty"`
	Outcome    string            `json:"outcome,omitempty"`
	Reason     string            `json:"reason,omitempty"`
	Summary    string            `json:"summary,omitempty"`
	Sensitive  bool              `json:"sensitive"`
	Metadata   map[string]string `json:"metadata,omitempty"`
}

type AuditFilterInput struct {
	AgentID   string `json:"agentID,omitempty"`
	SessionID string `json:"sessionID,omitempty"`
	RunID     string `json:"runID,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}
