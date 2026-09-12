package server

// DTOs for client-server communication between web/desktop frontends and Relayer supervisor.
// All DTOs are intentionally closed, sanitised and display-safe.

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
	Decisions      []string         `json:"decisions"`
}

type AppState struct {
	RunID         string             `json:"runID"`
	RunStatus     string             `json:"runStatus"`
	StartedAt     string             `json:"startedAt,omitempty"`
	Policy        PolicyState        `json:"policy"`
	Audit         AuditState         `json:"audit"`
	Agents        []AgentState       `json:"agents"`
	PendingEvents []SupervisionEvent `json:"pendingEvents"`
	Notices       []string           `json:"notices"`
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

type AgentProfileInput struct {
	ID             string   `json:"id"`
	Name           string   `json:"name"`
	PresetID       string   `json:"presetID,omitempty"`
	Cwd            string   `json:"cwd,omitempty"`
	Backend        string   `json:"backend,omitempty"`
	Adapter        string   `json:"adapter,omitempty"`
	Argv           []string `json:"argv,omitempty"`
	PreserveOnSave bool     `json:"preserveOnSave,omitempty"`
}

type SaveAgentProfilesRequest struct {
	ExpectedRevision string              `json:"expectedRevision"`
	Profiles         []AgentProfileInput `json:"profiles"`
}

type SaveAgentProfilesAndRestartRequest struct {
	ExpectedRunID    string              `json:"expectedRunID,omitempty"`
	ExpectedRevision string              `json:"expectedRevision"`
	Profiles         []AgentProfileInput `json:"profiles"`
}

type LifecycleResult struct {
	Outcome  string            `json:"outcome"`
	State    AppState          `json:"state"`
	Profiles AgentProfilesView `json:"profiles"`
}

type AgentCatalogEntry struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	InstallStatus      string   `json:"installStatus"`
	Installed          bool     `json:"installed"`
	Adapter            string   `json:"adapter"`
	AdapterStatus      string   `json:"adapterStatus"`
	DefaultArgv        []string `json:"defaultArgv"`
	RequiresCustomArgv bool     `json:"requiresCustomArgv"`
	MinimumArguments   int      `json:"minimumArguments"`
	ArgumentPrefix     []string `json:"argumentPrefix"`
}

type AgentProfile struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	PresetID        string   `json:"presetID"`
	Cwd             string   `json:"cwd"`
	Backend         string   `json:"backend"`
	Adapter         string   `json:"adapter"`
	Argv            []string `json:"argv,omitempty"`
	ExecutableLabel string   `json:"executableLabel"`
	ArgumentCount   int      `json:"argumentCount"`
	Locked          bool     `json:"locked"`
	ReadOnlyReason  string   `json:"readOnlyReason,omitempty"`
	PreserveOnSave  bool     `json:"preserveOnSave"`
}

type AgentProfilesView struct {
	ConfigPath      string              `json:"configPath"`
	Revision        string              `json:"revision"`
	Catalog         []AgentCatalogEntry `json:"catalog"`
	Profiles        []AgentProfile      `json:"profiles"`
	MinProfiles     int                 `json:"minProfiles"`
	MaxProfiles     int                 `json:"maxProfiles"`
	RestartRequired bool                `json:"restartRequired"`
	Editable        bool                `json:"editable"`
	ReadOnlyReason  string              `json:"readOnlyReason,omitempty"`
}

type SecuritySettings struct {
	Profile                     string `json:"profile"`
	DefaultAction               string `json:"defaultAction"`
	DryRun                      bool   `json:"dryRun"`
	BlockDestructive            bool   `json:"blockDestructive"`
	BlockExfiltration           bool   `json:"blockExfiltration"`
	BlockSensitivePaths         bool   `json:"blockSensitivePaths"`
	BlockOutsideWorkspace       bool   `json:"blockOutsideWorkspace"`
	WorkspaceRoot               string `json:"workspaceRoot"`
	RateLimitPerMinute          int    `json:"rateLimitPerMinute"`
	MaxConsecutiveAutoDecisions int    `json:"maxConsecutiveAutoDecisions"`
}

type NotificationWebhookSetting struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Format      string `json:"format"`
	MinSeverity string `json:"minSeverity"`
	Timeout     string `json:"timeout"`
}

type NotificationSettings struct {
	Enabled     bool                         `json:"enabled"`
	Bell        bool                         `json:"bell"`
	Desktop     bool                         `json:"desktop"`
	MinSeverity string                       `json:"minSeverity"`
	Webhooks    []NotificationWebhookSetting `json:"webhooks"`
}

type FullSettingsView struct {
	AgentProfilesView
	Security      SecuritySettings     `json:"security"`
	Notifications NotificationSettings `json:"notifications"`
}

type SaveFullSettingsRequest struct {
	ExpectedRevision string                `json:"expectedRevision"`
	Profiles         []AgentProfileInput   `json:"profiles,omitempty"`
	Security         *SecuritySettings     `json:"security,omitempty"`
	Notifications    *NotificationSettings `json:"notifications,omitempty"`
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

type DecisionBreakdown struct {
	Allow     int64 `json:"allow"`
	Deny      int64 `json:"deny"`
	AutoAllow int64 `json:"autoAllow"`
	AutoDeny  int64 `json:"autoDeny"`
	Custom    int64 `json:"custom"`
}

type LatencyBucketView struct {
	Le    float64 `json:"le"`
	Label string  `json:"label"`
	Count uint64  `json:"count"`
}

type TelemetrySnapshotView struct {
	Timestamp            string              `json:"timestamp"`
	Enabled              bool                `json:"enabled"`
	PrometheusEnabled    bool                `json:"prometheusEnabled"`
	PrometheusAddress    string              `json:"prometheusAddress,omitempty"`
	OTLPEnabled          bool                `json:"otlpEnabled"`
	OTLPEndpoint         string              `json:"otlpEndpoint,omitempty"`
	SessionsActive       int64               `json:"sessionsActive"`
	EventsPending        int64               `json:"eventsPending"`
	SessionsTotal        int64               `json:"sessionsTotal"`
	EventsDetectedTotal  int64               `json:"eventsDetectedTotal"`
	EventsWithdrawnTotal int64               `json:"eventsWithdrawnTotal"`
	DecisionsTotal       int64               `json:"decisionsTotal"`
	DecisionsBreakdown   DecisionBreakdown   `json:"decisionsBreakdown"`
	OperatorInputsTotal  int64               `json:"operatorInputsTotal"`
	GuardrailsTotal      int64               `json:"guardrailsTotal"`
	GuardrailsBreakdown  map[string]int64    `json:"guardrailsBreakdown"`
	AverageReactionTime  float64             `json:"averageReactionTime"`
	DecisionDurations    []LatencyBucketView `json:"decisionDurations"`
}
