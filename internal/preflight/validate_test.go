package preflight

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/audit"
)

func TestValidateReportAcceptsCompleteAndFailureReports(t *testing.T) {
	ready, err := Check(context.Background(), validInput(), testOptions(detectorWithInstalled("runner")))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateReport(ready); err != nil {
		t.Fatalf("ready report rejected: %v", err)
	}

	enabledAudit := validInput()
	enabledAudit.Configuration.Audit = audit.DefaultConfig()
	options := testOptions(detectorWithInstalled("runner"))
	options.ResolveAuditPath = func(string) (string, error) { return "/private/audit.jsonl", nil }
	options.Lstat = func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }
	auditReport, err := Check(context.Background(), enabledAudit, options)
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateReport(auditReport); err != nil {
		t.Fatalf("enabled audit report rejected: %v", err)
	}

	for _, kind := range []FailureKind{
		FailureConfigMissing,
		FailureConfigInvalid,
		FailureConfigUnreadable,
		FailureWorkingDirectory,
		FailureAgentResolution,
		FailurePolicyResolution,
		FailureAdapterResolution,
		FailurePreflightInternal,
		FailureKind("unknown"),
	} {
		if err := ValidateReport(FailureReport(kind, Options{GOOS: "linux", GOARCH: "amd64"})); err != nil {
			t.Fatalf("failure report %q rejected: %v", kind, err)
		}
	}
}

func TestValidateReportAcceptsEveryAuditGenerationOutcome(t *testing.T) {
	tests := []struct {
		name    string
		mode    fs.FileMode
		readErr error
	}{
		{name: "private", mode: 0o600},
		{name: "harden", mode: 0o644},
		{name: "unsafe", mode: os.ModeSymlink | 0o777},
		{name: "unreadable", readErr: errors.New("private read failure")},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			input := validInput()
			input.Configuration.Audit = audit.DefaultConfig()
			path := filepath.Join(string(filepath.Separator), "private-audit", "audit.jsonl")
			directory := filepath.Dir(path)
			options := testOptions(detectorWithInstalled("runner"))
			options.ResolveAuditPath = func(string) (string, error) { return path, nil }
			options.ReadDir = func(string) ([]fs.DirEntry, error) {
				if test.readErr != nil {
					return nil, test.readErr
				}
				return []fs.DirEntry{fs.FileInfoToDirEntry(fakeFileInfo{name: "audit.jsonl.1", mode: test.mode})}, nil
			}
			options.Lstat = func(requested string) (fs.FileInfo, error) {
				switch requested {
				case directory:
					return fakeFileInfo{name: "private-audit", mode: os.ModeDir | 0o700}, nil
				case path:
					return nil, fs.ErrNotExist
				case path + ".1":
					return fakeFileInfo{name: "audit.jsonl.1", mode: test.mode}, nil
				default:
					return nil, fs.ErrNotExist
				}
			}
			report, err := Check(context.Background(), input, options)
			if err != nil {
				t.Fatal(err)
			}
			if err := ValidateReport(report); err != nil {
				t.Fatalf("audit generation report rejected: %#v: %v", report.Checks, err)
			}
		})
	}
}

func TestValidateReportRejectsEveryOpenDisplayField(t *testing.T) {
	base, err := Check(context.Background(), validInput(), testOptions(detectorWithInstalled("runner")))
	if err != nil {
		t.Fatal(err)
	}
	const secret = "validate-report-secret-sentinel-6f2a"
	tests := []struct {
		name   string
		mutate func(*Report)
	}{
		{name: "schema", mutate: func(report *Report) { report.SchemaVersion++ }},
		{name: "overall", mutate: func(report *Report) { report.Status = StatusWarning }},
		{name: "platform os", mutate: func(report *Report) { report.Platform.OS = secret }},
		{name: "platform arch", mutate: func(report *Report) { report.Platform.Arch = secret }},
		{name: "platform support", mutate: func(report *Report) { report.Platform.Supported = false }},
		{name: "configuration version", mutate: func(report *Report) { report.Configuration.Version++ }},
		{name: "agent count", mutate: func(report *Report) { report.Configuration.AgentCount++ }},
		{name: "audit mode", mutate: func(report *Report) { report.Audit.Mode = audit.Mode(secret) }},
		{name: "tool order", mutate: func(report *Report) {
			report.Tools[0], report.Tools[1] = report.Tools[1], report.Tools[0]
		}},
		{name: "tool status", mutate: func(report *Report) { report.Tools[0].Installation = secret }},
		{name: "agent ordinal", mutate: func(report *Report) { report.Agents[0].Ordinal = 2 }},
		{name: "agent source", mutate: func(report *Report) { report.Agents[0].Source = AgentSource(secret) }},
		{name: "agent command", mutate: func(report *Report) { report.Agents[0].Command = CommandKind(secret) }},
		{name: "agent adapter", mutate: func(report *Report) { report.Agents[0].Adapter = secret }},
		{name: "agent backend", mutate: func(report *Report) { report.Agents[0].Backend = secret }},
		{name: "check id", mutate: func(report *Report) { report.Checks[0].ID = secret }},
		{name: "check scope", mutate: func(report *Report) { report.Checks[0].Scope = Scope(secret) }},
		{name: "check status", mutate: func(report *Report) { report.Checks[0].Status = CheckStatus(secret) }},
		{name: "check summary", mutate: func(report *Report) { report.Checks[0].Summary = secret }},
		{name: "check remediation", mutate: func(report *Report) { report.Checks[0].Remediation = secret }},
		{name: "missing tool", mutate: func(report *Report) { report.Tools = report.Tools[1:] }},
		{name: "missing agent", mutate: func(report *Report) { report.Agents = nil }},
		{name: "extra check", mutate: func(report *Report) {
			report.Checks = append(report.Checks, CheckResult{
				ID: secret, Scope: ScopeConfiguration, Status: CheckPass, Summary: secret,
			})
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			report := base.Clone()
			test.mutate(&report)
			err := ValidateReport(report)
			if !errors.Is(err, ErrInvalidReport) {
				t.Fatalf("ValidateReport error = %v", err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("validation error leaked rejected value: %v", err)
			}
		})
	}
}

func TestValidateReportRejectsFailureTextMutation(t *testing.T) {
	const secret = "failure-report-secret-sentinel-319c"
	report := FailureReport(FailureConfigMissing, Options{GOOS: "darwin", GOARCH: "arm64"})
	report.Checks[0].Summary = secret
	err := ValidateReport(report)
	if !errors.Is(err, ErrInvalidReport) || strings.Contains(err.Error(), secret) {
		t.Fatalf("ValidateReport error = %v", err)
	}
}
