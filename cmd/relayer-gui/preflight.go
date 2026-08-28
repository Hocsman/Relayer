package main

import (
	"context"
	"errors"

	appcore "github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/preflight"
)

//lint:ignore ST1005 This localized bridge error is rendered directly by Wails.
var errPreflightUnavailable = errors.New("The local diagnostic is unavailable.")

// RunPreflight delegates the complete effective-plan resolution to the shared
// application facade. This bridge neither loads configuration nor probes tools
// itself, and it never returns a raw operational error to the frontend.
func (a *App) RunPreflight() (PreflightReport, error) {
	a.lifecycleMu.Lock()
	if a.finalShutdown {
		a.lifecycleMu.Unlock()
		return PreflightReport{}, errRuntimeStopped
	}
	runner := a.runPreflight
	ctx := a.ctx
	a.lifecycleMu.Unlock()
	if runner == nil {
		return PreflightReport{}, errPreflightUnavailable
	}

	a.profilesMu.Lock()
	path, err := a.profileConfigPathLocked()
	a.profilesMu.Unlock()
	if err != nil {
		return PreflightReport{}, errPreflightUnavailable
	}
	if ctx == nil {
		ctx = context.Background()
	}
	report, err := runner(ctx, appcore.PreflightOptions{ConfigPath: path})
	if err != nil {
		return PreflightReport{}, errPreflightUnavailable
	}
	return preflightReportView(report)
}

func preflightReportView(report preflight.Report) (PreflightReport, error) {
	report = report.Clone()
	if err := preflight.ValidateReport(report); err != nil {
		return PreflightReport{}, errPreflightUnavailable
	}
	tools := make([]PreflightTool, len(report.Tools))
	for index, tool := range report.Tools {
		tools[index] = PreflightTool{
			ProfileID:    string(tool.ProfileID),
			Installation: string(tool.Installation),
		}
	}
	agents := make([]PreflightAgent, len(report.Agents))
	for index, item := range report.Agents {
		agents[index] = PreflightAgent{
			Ordinal:         item.Ordinal,
			Source:          string(item.Source),
			Command:         string(item.Command),
			Installation:    string(item.Installation),
			Adapter:         item.Adapter,
			AdapterMaturity: string(item.AdapterMaturity),
			Backend:         item.Backend,
		}
	}
	checks := make([]PreflightCheck, len(report.Checks))
	for index, check := range report.Checks {
		checks[index] = PreflightCheck{
			ID:          check.ID,
			Scope:       string(check.Scope),
			Status:      string(check.Status),
			Summary:     check.Summary,
			Remediation: check.Remediation,
		}
	}
	return PreflightReport{
		SchemaVersion: report.SchemaVersion,
		Status:        string(report.Status),
		Platform: PreflightPlatform{
			OS:        report.Platform.OS,
			Arch:      report.Platform.Arch,
			Supported: report.Platform.Supported,
		},
		Configuration: PreflightConfiguration{
			Version:         report.Configuration.Version,
			Legacy:          report.Configuration.Legacy,
			AgentCount:      report.Configuration.AgentCount,
			PolicyRuleCount: report.Configuration.PolicyRuleCount,
		},
		Audit: PreflightAudit{
			Enabled:       report.Audit.Enabled,
			Mode:          string(report.Audit.Mode),
			Location:      string(report.Audit.Location),
			MaxFileSizeMB: report.Audit.MaxFileSizeMB,
			MaxFiles:      report.Audit.MaxFiles,
		},
		Tools:  tools,
		Agents: agents,
		Checks: checks,
	}, nil
}
