// Package preflight performs read-only readiness checks for Relayer. Reports
// deliberately contain no configuration path, command arguments, environment
// values, agent identifiers, names or raw dependency errors.
//
// Every check is passive with one deliberate exception: when tmux is the
// effective backend, Options.TmuxProbe runs tmux inside a private socket to
// establish that it can actually serve a session. Nothing belonging to the
// user - configuration, audit journal, tmux server, agent process - is created,
// read or mutated.
package preflight

import (
	"context"
	"io/fs"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

const CurrentSchemaVersion = 1

// OverallStatus summarizes the strongest check status in a report.
type OverallStatus string

const (
	StatusReady   OverallStatus = "ready"
	StatusWarning OverallStatus = "warning"
	StatusBlocked OverallStatus = "blocked"
)

// CheckStatus is deliberately closed so presentation layers never need raw
// error strings to explain readiness.
type CheckStatus string

const (
	CheckPass    CheckStatus = "pass"
	CheckWarning CheckStatus = "warning"
	CheckBlock   CheckStatus = "block"
)

// Scope groups checks without disclosing user-controlled identifiers.
type Scope string

const (
	ScopeConfiguration Scope = "configuration"
	ScopePlatform      Scope = "platform"
	ScopePolicy        Scope = "policy"
	ScopeAudit         Scope = "audit"
	ScopeTool          Scope = "tool"
	ScopeAgent         Scope = "agent"
	ScopeAdapter       Scope = "adapter"
	ScopeBackend       Scope = "backend"
)

// CheckResult contains only finite, static display text selected by the package.
type CheckResult struct {
	ID          string      `json:"id"`
	Scope       Scope       `json:"scope"`
	Status      CheckStatus `json:"status"`
	Summary     string      `json:"summary"`
	Remediation string      `json:"remediation,omitempty"`
}

type PlatformInfo struct {
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Supported bool   `json:"supported"`
}

type ConfigurationInfo struct {
	Version         int  `json:"version"`
	Legacy          bool `json:"legacy"`
	AgentCount      int  `json:"agent_count"`
	PolicyRuleCount int  `json:"policy_rule_count"`
}

type AuditLocation string

const (
	AuditLocationDisabled AuditLocation = "disabled"
	AuditLocationDefault  AuditLocation = "default"
	AuditLocationCustom   AuditLocation = "custom"
)

type AuditInfo struct {
	Enabled       bool          `json:"enabled"`
	Mode          audit.Mode    `json:"mode"`
	Location      AuditLocation `json:"location"`
	MaxFileSizeMB int           `json:"max_file_size_mb"`
	MaxFiles      int           `json:"max_files"`
}

type ToolInfo struct {
	ProfileID    toolcatalog.ProfileID     `json:"profile_id"`
	Installation toolcatalog.InstallStatus `json:"installation"`
}

type AgentSource string

const (
	AgentConfigured AgentSource = "configured"
	AgentDemo       AgentSource = "demo"
)

type CommandKind string

const (
	CommandDirect CommandKind = "direct"
	CommandShell  CommandKind = "shell"
)

// AgentInfo uses a one-based ordinal in place of the configured agent ID or
// name. Executable names and resolved filesystem paths are also omitted.
type AgentInfo struct {
	Ordinal         int                       `json:"ordinal"`
	Source          AgentSource               `json:"source"`
	Command         CommandKind               `json:"command"`
	Installation    toolcatalog.InstallStatus `json:"installation"`
	Adapter         string                    `json:"adapter,omitempty"`
	AdapterMaturity adapters.Status           `json:"adapter_maturity,omitempty"`
	Backend         string                    `json:"backend,omitempty"`
}

// Report is a versioned, display-safe snapshot shared by CLI and GUI.
type Report struct {
	SchemaVersion int               `json:"schema_version"`
	Status        OverallStatus     `json:"status"`
	Platform      PlatformInfo      `json:"platform"`
	Configuration ConfigurationInfo `json:"configuration"`
	Audit         AuditInfo         `json:"audit"`
	Tools         []ToolInfo        `json:"tools"`
	Agents        []AgentInfo       `json:"agents"`
	Checks        []CheckResult     `json:"checks"`
}

// Ready reports whether no warning or blocker was found.
func (report Report) Ready() bool { return report.Status == StatusReady }

// HasBlockers reports whether at least one check prevents safe startup.
func (report Report) HasBlockers() bool { return report.Status == StatusBlocked }

// Clone returns independent slice storage for presentation-layer caching.
func (report Report) Clone() Report {
	result := report
	result.Tools = append([]ToolInfo(nil), report.Tools...)
	result.Agents = append([]AgentInfo(nil), report.Agents...)
	result.Checks = append([]CheckResult(nil), report.Checks...)
	return result
}

// Input contains an already effective, validated runtime plan. Application
// facades remain responsible for applying the empty-agent demo fallback before
// calling Check.
type Input struct {
	Configuration config.Result
	Specs         []agent.Spec
	DemoAgents    bool
}

type LstatFunc func(string) (fs.FileInfo, error)
type ReadDirFunc func(string) ([]fs.DirEntry, error)
type ResolveAuditPathFunc func(string) (string, error)

type OwnerStatus string

const (
	OwnerCurrent OwnerStatus = "current"
	OwnerOther   OwnerStatus = "other"
	OwnerUnknown OwnerStatus = "unknown"
)

type OwnerCheckFunc func(fs.FileInfo) OwnerStatus

// TmuxProbeFunc reports whether a discovered tmux can actually run a session.
type TmuxProbeFunc func(context.Context, string) error

// Options contains passive dependencies, plus the single deliberate exception
// described on TmuxProbe. Empty platform fields use the current runtime; a nil
// detector uses toolcatalog.DefaultDetector.
type Options struct {
	Detector toolcatalog.Detector

	// TmuxProbe is the one check in this package that executes a program.
	// Discovering the tmux binary is not evidence that it can serve Relayer's
	// machine-readable protocol, and a report that cannot observe that failure
	// tells the operator the backend is healthy right before startup fails with
	// an opaque identity error.
	//
	// The probe is bounded and self-contained: one short-lived session on a
	// private socket inside a 0700 temporary directory, removed by name. It
	// never reads, attaches to, or mutates the user's tmux server, and never
	// calls kill-server. A nil probe uses tmuxbackend.Probe.
	TmuxProbe TmuxProbeFunc

	GOOS             string
	GOARCH           string
	Lstat            LstatFunc
	ReadDir          ReadDirFunc
	ResolveAuditPath ResolveAuditPathFunc
	OwnerCheck       OwnerCheckFunc
}

// FailureKind is a closed classification used by the application facade when
// it cannot construct an effective Input. The underlying error is never put
// into the report.
type FailureKind string

const (
	FailureConfigMissing     FailureKind = "config_missing"
	FailureConfigInvalid     FailureKind = "config_invalid"
	FailureConfigUnreadable  FailureKind = "config_unreadable"
	FailureWorkingDirectory  FailureKind = "working_directory"
	FailureAgentResolution   FailureKind = "agent_resolution"
	FailurePolicyResolution  FailureKind = "policy_resolution"
	FailureAdapterResolution FailureKind = "adapter_resolution"
	FailurePreflightInternal FailureKind = "preflight_internal"
)
