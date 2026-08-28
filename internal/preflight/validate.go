package preflight

import (
	"errors"
	"strconv"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

// ErrInvalidReport is deliberately static. A rejected report may contain
// caller-controlled paths, commands, environment values or raw errors, so the
// validation error must never echo the field which caused the rejection.
var ErrInvalidReport = errors.New("invalid preflight report")

type checkVocabulary struct {
	scope       Scope
	status      CheckStatus
	summary     string
	remediation string
}

// ValidateReport verifies the complete display contract before a report may be
// rendered or cross a native/UI boundary. It accepts only reports produced by
// Check or FailureReport: every identifier, enum and displayed sentence belongs
// to a finite package-owned vocabulary.
func ValidateReport(report Report) error {
	if report.SchemaVersion != CurrentSchemaVersion ||
		normalizeGOOS(report.Platform.OS) != report.Platform.OS ||
		normalizeGOARCH(report.Platform.Arch) != report.Platform.Arch ||
		report.Platform.Supported != supportedPlatform(report.Platform.OS) ||
		len(report.Checks) == 0 || !validOverallStatus(report.Status) ||
		report.Status != overallStatusFor(report.Checks) {
		return ErrInvalidReport
	}

	if len(report.Checks) == 1 && isFailureCheckID(report.Checks[0].ID) {
		return validateFailureReport(report)
	}
	return validateCompleteReport(report)
}

func validateFailureReport(report Report) error {
	if report.Status != StatusBlocked || report.Configuration != (ConfigurationInfo{}) ||
		report.Audit != (AuditInfo{Mode: audit.ModeOff, Location: AuditLocationDisabled}) ||
		len(report.Tools) != 0 || len(report.Agents) != 0 {
		return ErrInvalidReport
	}
	allowed, ok := failureVocabulary()[report.Checks[0].ID]
	if !ok || !matchesVocabulary(report.Checks[0], allowed...) {
		return ErrInvalidReport
	}
	return nil
}

func validateCompleteReport(report Report) error {
	if report.Configuration.AgentCount < 1 || report.Configuration.AgentCount > 8 ||
		report.Configuration.AgentCount != len(report.Agents) ||
		report.Configuration.PolicyRuleCount < 0 {
		return ErrInvalidReport
	}
	if report.Configuration.Legacy {
		if report.Configuration.Version != 0 {
			return ErrInvalidReport
		}
	} else if report.Configuration.Version != config.CurrentVersion {
		return ErrInvalidReport
	}
	if !validAuditInfo(report.Audit) || !validTools(report.Tools) || !validAgents(report.Agents) {
		return ErrInvalidReport
	}

	cursor := 0
	next := func(id string, allowed ...checkVocabulary) bool {
		if cursor >= len(report.Checks) || report.Checks[cursor].ID != id ||
			!matchesVocabulary(report.Checks[cursor], allowed...) {
			return false
		}
		cursor++
		return true
	}

	platformCheck := checkVocabulary{ScopePlatform, CheckBlock, summaryPlatformUnsupported, remediationPlatform}
	if report.Platform.Supported {
		platformCheck = checkVocabulary{ScopePlatform, CheckPass, summaryPlatformSupported, ""}
	}
	auditCheck := checkVocabulary{ScopeAudit, CheckPass, summaryAuditDisabled, ""}
	if report.Audit.Enabled {
		auditCheck = checkVocabulary{ScopeAudit, CheckPass, summaryAuditConfigured, ""}
	}
	if !next("configuration.valid",
		checkVocabulary{ScopeConfiguration, CheckPass, summaryConfigurationValid, ""},
		checkVocabulary{ScopeConfiguration, CheckBlock, summaryConfigurationInvalid, remediationConfiguration},
	) || !next("platform.execution", platformCheck) || !next("policy.valid",
		checkVocabulary{ScopePolicy, CheckPass, summaryPolicyValid, ""},
		checkVocabulary{ScopePolicy, CheckBlock, summaryPolicyInvalid, remediationPolicy},
	) || !next("policy.agent_references",
		checkVocabulary{ScopePolicy, CheckPass, summaryPolicyReferencesValid, ""},
		checkVocabulary{ScopePolicy, CheckBlock, summaryPolicyReferencesBad, remediationPolicyReferences},
	) || !next("audit.configuration", auditCheck) {
		return ErrInvalidReport
	}
	if report.Audit.Enabled {
		pathVocabulary := []checkVocabulary{
			{ScopeAudit, CheckPass, summaryAuditPathReady, ""},
			{ScopeAudit, CheckPass, summaryAuditPathExisting, ""},
			{ScopeAudit, CheckWarning, summaryAuditPathHarden, remediationAuditPermissions},
			{ScopeAudit, CheckBlock, summaryAuditPathUnsafe, remediationAuditPath},
			{ScopeAudit, CheckBlock, summaryAuditPathForeign, remediationAuditForeign},
		}
		if cursor >= len(report.Checks) {
			return ErrInvalidReport
		}
		switch report.Checks[cursor].ID {
		case "audit.path":
			// Resolution and directory failures happen before generation
			// inspection and can only produce the closed unsafe-path blocker.
			if !next("audit.path", pathVocabulary[3]) {
				return ErrInvalidReport
			}
		case "audit.generations":
			if !next("audit.generations",
				checkVocabulary{ScopeAudit, CheckPass, summaryAuditGenerationsNone, ""},
				checkVocabulary{ScopeAudit, CheckPass, summaryAuditGenerationsReady, ""},
				checkVocabulary{ScopeAudit, CheckWarning, summaryAuditGenerationsHarden, remediationAuditPermissions},
				checkVocabulary{ScopeAudit, CheckBlock, summaryAuditGenerationsUnsafe, remediationAuditGenerations},
				checkVocabulary{ScopeAudit, CheckBlock, summaryAuditGenerationsUnread, remediationAuditReadDir},
			) {
				return ErrInvalidReport
			}
			if report.Checks[cursor-1].Status != CheckBlock && !next("audit.path", pathVocabulary...) {
				return ErrInvalidReport
			}
		default:
			return ErrInvalidReport
		}
	}

	for index, tool := range report.Tools {
		var allowed []checkVocabulary
		switch tool.Installation {
		case toolcatalog.InstallInstalled:
			allowed = []checkVocabulary{{ScopeTool, CheckPass, summaryToolInstalled, ""}}
		case toolcatalog.InstallNotInstalled:
			allowed = []checkVocabulary{{ScopeTool, CheckPass, summaryToolMissing, ""}}
		case toolcatalog.InstallUnknown:
			allowed = []checkVocabulary{
				{ScopeTool, CheckPass, summaryToolUnknown, ""},
				{ScopeTool, CheckWarning, summaryToolInconclusive, remediationToolDetection},
			}
		default:
			return ErrInvalidReport
		}
		if !next("tool."+string(report.Tools[index].ProfileID), allowed...) {
			return ErrInvalidReport
		}
	}

	for _, inspected := range report.Agents {
		prefix := "agent." + strconv.Itoa(inspected.Ordinal)
		if !next(prefix+".executable", executableVocabulary(inspected.Installation)...) ||
			!next(prefix+".adapter", adapterVocabulary(inspected)...) ||
			!next(prefix+".backend", backendVocabulary(inspected.Backend)...) {
			return ErrInvalidReport
		}
	}
	if cursor != len(report.Checks) {
		return ErrInvalidReport
	}
	return nil
}

func validAuditInfo(info AuditInfo) bool {
	if err := audit.Validate(audit.Config{
		Enabled: info.Enabled, Mode: info.Mode,
		MaxFileSizeMB: info.MaxFileSizeMB, MaxFiles: info.MaxFiles,
	}); err != nil {
		return false
	}
	if info.Enabled {
		return info.Mode != audit.ModeOff &&
			(info.Location == AuditLocationDefault || info.Location == AuditLocationCustom)
	}
	return info.Location == AuditLocationDisabled
}

func validTools(tools []ToolInfo) bool {
	descriptors := toolcatalog.Descriptors()
	if len(tools) != len(descriptors) {
		return false
	}
	for index, descriptor := range descriptors {
		if tools[index].ProfileID != descriptor.ID || !validInstallStatus(tools[index].Installation) {
			return false
		}
	}
	return true
}

func validAgents(agents []AgentInfo) bool {
	var source AgentSource
	for index, inspected := range agents {
		if inspected.Ordinal != index+1 || !validInstallStatus(inspected.Installation) {
			return false
		}
		switch inspected.Source {
		case AgentConfigured, AgentDemo:
		default:
			return false
		}
		if index == 0 {
			source = inspected.Source
		} else if inspected.Source != source {
			return false
		}
		switch inspected.Command {
		case CommandDirect, CommandShell:
		default:
			return false
		}
		switch inspected.Adapter {
		case "":
			if inspected.AdapterMaturity != "" {
				return false
			}
		case adapters.GenericID:
			if inspected.AdapterMaturity != adapters.StatusStable {
				return false
			}
		case adapters.ClaudeID, adapters.CodexID:
			if inspected.AdapterMaturity != adapters.StatusExperimental {
				return false
			}
		default:
			return false
		}
		switch inspected.Backend {
		case "", agent.BackendPTY, agent.BackendTmux:
		default:
			return false
		}
	}
	return true
}

func validInstallStatus(status toolcatalog.InstallStatus) bool {
	switch status {
	case toolcatalog.InstallInstalled, toolcatalog.InstallNotInstalled, toolcatalog.InstallUnknown:
		return true
	default:
		return false
	}
}

func executableVocabulary(status toolcatalog.InstallStatus) []checkVocabulary {
	switch status {
	case toolcatalog.InstallInstalled:
		return []checkVocabulary{{ScopeAgent, CheckPass, summaryAgentExecutableReady, ""}}
	case toolcatalog.InstallNotInstalled:
		return []checkVocabulary{{ScopeAgent, CheckBlock, summaryAgentExecutableMissing, remediationAgentExecutable}}
	case toolcatalog.InstallUnknown:
		return []checkVocabulary{{ScopeAgent, CheckBlock, summaryAgentExecutableUnknown, remediationAgentExecutable}}
	default:
		return nil
	}
}

func adapterVocabulary(inspected AgentInfo) []checkVocabulary {
	if inspected.Adapter == "" {
		return []checkVocabulary{{ScopeAdapter, CheckBlock, summaryAdapterUnavailable, remediationAdapter}}
	}
	if inspected.AdapterMaturity == adapters.StatusExperimental {
		return []checkVocabulary{{ScopeAdapter, CheckWarning, summaryAdapterExperimental, remediationAdapterExperimental}}
	}
	return []checkVocabulary{{ScopeAdapter, CheckPass, summaryAdapterReady, ""}}
}

func backendVocabulary(backend string) []checkVocabulary {
	switch backend {
	case "":
		return []checkVocabulary{
			{ScopeBackend, CheckBlock, summaryBackendUnavailable, remediationBackend},
			{ScopeBackend, CheckBlock, summaryBackendUnusable, remediationBackendUnusable},
		}
	case agent.BackendTmux:
		return []checkVocabulary{{ScopeBackend, CheckPass, summaryBackendTmux, ""}}
	case agent.BackendPTY:
		return []checkVocabulary{
			{ScopeBackend, CheckPass, summaryBackendPTY, ""},
			{ScopeBackend, CheckWarning, summaryBackendAutoFallback, remediationBackendAuto},
			{ScopeBackend, CheckWarning, summaryBackendAutoUnusable, remediationBackendUnusable},
		}
	default:
		return nil
	}
}

func failureVocabulary() map[string][]checkVocabulary {
	return map[string][]checkVocabulary{
		"configuration.missing":           {{ScopeConfiguration, CheckBlock, "The configuration file is missing.", "Create the configuration explicitly before running the diagnostic again."}},
		"configuration.invalid":           {{ScopeConfiguration, CheckBlock, summaryConfigurationInvalid, remediationConfiguration}},
		"configuration.unreadable":        {{ScopeConfiguration, CheckBlock, "The configuration file cannot be read.", "Check the existence and permissions of the configuration file."}},
		"configuration.working_directory": {{ScopeConfiguration, CheckBlock, "The working directory cannot be checked.", "Choose an existing, accessible working directory."}},
		"configuration.agents":            {{ScopeAgent, CheckBlock, summaryConfigurationInvalid, remediationConfiguration}},
		"policy.initialization":           {{ScopePolicy, CheckBlock, summaryPolicyInvalid, remediationPolicy}},
		"adapter.initialization":          {{ScopeAdapter, CheckBlock, summaryAdapterUnavailable, remediationAdapter}},
		"preflight.internal":              {{ScopeConfiguration, CheckBlock, "The diagnostic could not be completed safely.", "Run the diagnostic again before starting an agent."}},
	}
}

func isFailureCheckID(id string) bool {
	_, ok := failureVocabulary()[id]
	return ok
}

func matchesVocabulary(check CheckResult, allowed ...checkVocabulary) bool {
	for _, candidate := range allowed {
		if check.Scope == candidate.scope && check.Status == candidate.status &&
			check.Summary == candidate.summary && check.Remediation == candidate.remediation {
			return true
		}
	}
	return false
}

func validOverallStatus(status OverallStatus) bool {
	switch status {
	case StatusReady, StatusWarning, StatusBlocked:
		return true
	default:
		return false
	}
}

func overallStatusFor(checks []CheckResult) OverallStatus {
	status := StatusReady
	for _, check := range checks {
		switch check.Status {
		case CheckBlock:
			return StatusBlocked
		case CheckWarning:
			status = StatusWarning
		case CheckPass:
		default:
			return ""
		}
	}
	return status
}
