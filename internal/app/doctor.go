package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"strings"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/preflight"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

// ErrPreflightBlocked is returned only after a complete, display-safe doctor
// report has been written. Entrypoints use it to select a non-zero exit status
// without appending a second error message to the report.
var ErrPreflightBlocked = errors.New("relayer diagnostics detected a blocker")

type preflightRunner func(context.Context, PreflightOptions) (preflight.Report, error)

func runDoctor(
	arguments []string,
	output io.Writer,
	diagnostics io.Writer,
	runner preflightRunner,
) error {
	if output == nil {
		output = io.Discard
	}
	if diagnostics == nil {
		diagnostics = io.Discard
	}

	flags := flag.NewFlagSet("relayer doctor", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	flags.Usage = func() {
		_, _ = fmt.Fprintln(diagnostics, "Usage: relayer doctor [--config file]")
		flags.PrintDefaults()
	}
	configPath := flags.String("config", config.DefaultPath, "existing YAML configuration file")
	if err := flags.Parse(arguments); err != nil {
		return err
	}
	if flags.NArg() != 0 {
		return errors.New("doctor accepts no positional arguments")
	}

	var report preflight.Report
	if runner == nil {
		report = preflight.FailureReport(preflight.FailurePreflightInternal, preflight.Options{})
	} else {
		result, err := runner(context.Background(), PreflightOptions{ConfigPath: *configPath})
		if err != nil {
			report = preflight.FailureReport(preflight.FailurePreflightInternal, preflight.Options{})
		} else {
			result = result.Clone()
			if preflight.ValidateReport(result) != nil {
				report = preflight.FailureReport(preflight.FailurePreflightInternal, preflight.Options{})
			} else {
				report = result
			}
		}
	}
	if err := writeDoctorReport(output, report); err != nil {
		return err
	}
	if report.HasBlockers() {
		return ErrPreflightBlocked
	}
	return nil
}

func writeDoctorReport(output io.Writer, report preflight.Report) error {
	report = report.Clone()
	if err := preflight.ValidateReport(report); err != nil {
		return errors.New("invalid doctor report")
	}
	var rendered strings.Builder
	rendered.WriteString("Relayer doctor\n")
	if len(report.Tools) > 0 {
		rendered.WriteString("Tools:\n")
		for _, tool := range report.Tools {
			descriptor, ok := toolcatalog.Lookup(tool.ProfileID)
			installation, valid := doctorInstallationLabel(tool.Installation)
			if !ok || !valid {
				return errors.New("invalid doctor report")
			}
			_, _ = fmt.Fprintf(&rendered, "  - %s: %s\n", descriptor.Name, installation)
		}
	}
	if len(report.Agents) > 0 {
		rendered.WriteString("Agents:\n")
		for _, inspected := range report.Agents {
			line, ok := doctorAgentLine(inspected)
			if !ok {
				return errors.New("invalid doctor report")
			}
			_, _ = fmt.Fprintf(&rendered, "  - %s\n", line)
		}
	}
	if len(report.Checks) > 0 {
		rendered.WriteString("Checks:\n")
	}
	passCount, warningCount, blockCount := 0, 0, 0
	for _, check := range report.Checks {
		label := ""
		switch check.Status {
		case preflight.CheckPass:
			label = "OK"
			passCount++
		case preflight.CheckWarning:
			label = "WARNING"
			warningCount++
		case preflight.CheckBlock:
			label = "BLOCKED"
			blockCount++
		default:
			return errors.New("invalid doctor report")
		}
		scope, ok := doctorScopeLabel(check.Scope)
		if !ok || strings.TrimSpace(check.Summary) == "" {
			return errors.New("invalid doctor report")
		}
		_, _ = fmt.Fprintf(&rendered, "[%s] %s — %s\n", label, scope, check.Summary)
		if strings.TrimSpace(check.Remediation) != "" {
			_, _ = fmt.Fprintf(&rendered, "  Action: %s\n", check.Remediation)
		}
	}

	overall := ""
	switch report.Status {
	case preflight.StatusReady:
		overall = "READY"
	case preflight.StatusWarning:
		overall = "WARNINGS"
	case preflight.StatusBlocked:
		overall = "BLOCKED"
	default:
		return errors.New("invalid doctor report")
	}
	_, _ = fmt.Fprintf(
		&rendered,
		"Summary: %d OK, %d warning(s), %d blocker(s) — %s\n",
		passCount,
		warningCount,
		blockCount,
		overall,
	)
	if _, err := io.WriteString(output, rendered.String()); err != nil {
		return errors.New("cannot write the doctor report")
	}
	return nil
}

func doctorInstallationLabel(status toolcatalog.InstallStatus) (string, bool) {
	switch status {
	case toolcatalog.InstallInstalled:
		return "detected", true
	case toolcatalog.InstallNotInstalled:
		return "not installed", true
	case toolcatalog.InstallUnknown:
		return "unknown", true
	default:
		return "", false
	}
}

func doctorAgentLine(inspected preflight.AgentInfo) (string, bool) {
	if inspected.Ordinal < 1 {
		return "", false
	}
	source := ""
	switch inspected.Source {
	case preflight.AgentConfigured:
		source = "configured"
	case preflight.AgentDemo:
		source = "demo"
	default:
		return "", false
	}
	command := ""
	switch inspected.Command {
	case preflight.CommandDirect:
		command = "direct"
	case preflight.CommandShell:
		command = "shell"
	default:
		return "", false
	}
	installation, ok := doctorInstallationLabel(inspected.Installation)
	if !ok {
		return "", false
	}
	adapter := "unavailable"
	if inspected.Adapter != "" {
		switch inspected.Adapter {
		case adapters.GenericID, adapters.ClaudeID, adapters.CodexID:
			adapter = inspected.Adapter
		default:
			return "", false
		}
		maturity := ""
		switch inspected.AdapterMaturity {
		case adapters.StatusStable:
			maturity = "stable"
		case adapters.StatusExperimental:
			maturity = "experimental"
		default:
			return "", false
		}
		adapter += " (" + maturity + ")"
	} else if inspected.AdapterMaturity != "" {
		return "", false
	}
	backend := "unavailable"
	switch inspected.Backend {
	case agent.BackendPTY, agent.BackendTmux:
		backend = inspected.Backend
	case "":
	default:
		return "", false
	}
	return fmt.Sprintf(
		"Agent #%d: source=%s, command=%s, executable=%s, adapter=%s, backend=%s",
		inspected.Ordinal,
		source,
		command,
		installation,
		adapter,
		backend,
	), true
}

func doctorScopeLabel(scope preflight.Scope) (string, bool) {
	switch scope {
	case preflight.ScopeConfiguration:
		return "Configuration", true
	case preflight.ScopePlatform:
		return "Platform", true
	case preflight.ScopePolicy:
		return "Policies", true
	case preflight.ScopeAudit:
		return "Audit", true
	case preflight.ScopeTool:
		return "Tool", true
	case preflight.ScopeAgent:
		return "Agent", true
	case preflight.ScopeAdapter:
		return "Adapter", true
	case preflight.ScopeBackend:
		return "Backend", true
	default:
		return "", false
	}
}
