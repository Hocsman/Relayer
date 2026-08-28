package main

import (
	"context"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	appcore "github.com/Hocsman/Relayer/internal/app"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/preflight"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

type guiPreflightDetectorFunc func(context.Context, []string) (toolcatalog.Detection, error)

func (function guiPreflightDetectorFunc) Detect(ctx context.Context, candidates []string) (toolcatalog.Detection, error) {
	return function(ctx, candidates)
}

func TestRunPreflightDelegatesToSharedFacadeWithoutExposingConfigPath(t *testing.T) {
	application := NewApp()
	application.ctx = context.Background()
	application.configPath = "/private/hidden/fixture/config.yaml"
	var received appcore.PreflightOptions
	application.runPreflight = func(_ context.Context, options appcore.PreflightOptions) (preflight.Report, error) {
		received = options
		return preflight.FailureReport(preflight.FailureConfigMissing, preflight.Options{
			GOOS: "darwin", GOARCH: "arm64",
		}), nil
	}

	view, err := application.RunPreflight()
	if err != nil {
		t.Fatalf("RunPreflight: %v", err)
	}
	if received.ConfigPath != application.configPath {
		t.Fatalf("facade config path = %q", received.ConfigPath)
	}
	if view.Status != "blocked" || len(view.Checks) != 1 || view.Checks[0].Status != "block" {
		t.Fatalf("view = %#v", view)
	}
	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if strings.Contains(string(encoded), application.configPath) || strings.Contains(string(encoded), "hidden") {
		t.Fatalf("preflight DTO exposed config path: %s", encoded)
	}
}

func TestRunPreflightReplacesRawFacadeError(t *testing.T) {
	application := NewApp()
	application.configPath = "/fixture/config.yaml"
	application.runPreflight = func(context.Context, appcore.PreflightOptions) (preflight.Report, error) {
		return preflight.Report{}, errors.New("secret-native-error-sentinel")
	}

	_, err := application.RunPreflight()
	if !errors.Is(err, errPreflightUnavailable) || strings.Contains(err.Error(), "sentinel") {
		t.Fatalf("RunPreflight error = %v", err)
	}
}

func TestRunPreflightRejectsMalformedReportWithoutCrossingSecret(t *testing.T) {
	const secret = "malformed-preflight-secret-sentinel-889e"
	application := NewApp()
	application.configPath = "/fixture/config.yaml"
	application.runPreflight = func(context.Context, appcore.PreflightOptions) (preflight.Report, error) {
		report := validGUIPreflightReport(t)
		report.Checks[0].Summary = secret
		return report, nil
	}

	view, err := application.RunPreflight()
	if !errors.Is(err, errPreflightUnavailable) || strings.Contains(err.Error(), secret) {
		t.Fatalf("RunPreflight error = %v", err)
	}
	encoded, marshalErr := json.Marshal(view)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), secret) || !reflect.DeepEqual(view, PreflightReport{}) {
		t.Fatalf("malformed report crossed the bridge: %s", encoded)
	}
}

func TestPreflightReportViewProjectsSafeVersionedReport(t *testing.T) {
	report := validGUIPreflightReport(t)

	view, err := preflightReportView(report)
	if err != nil {
		t.Fatalf("preflightReportView: %v", err)
	}
	if view.SchemaVersion != preflight.CurrentSchemaVersion || view.Status != "warning" ||
		len(view.Tools) != len(toolcatalog.Descriptors()) || len(view.Agents) != 1 || len(view.Checks) == 0 {
		t.Fatalf("view = %#v", view)
	}
	if view.Configuration.PolicyRuleCount != 0 || view.Audit.Location != "disabled" ||
		view.Agents[0].AdapterMaturity != "experimental" || view.Checks[0].Scope != "configuration" {
		t.Fatalf("projected fields = %#v", view)
	}

	encoded, err := json.Marshal(view)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	for _, forbidden := range []string{`"path"`, `"argv"`, `"environment"`, `"error"`, `"agentID"`, `"name"`} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("preflight DTO contains forbidden field %s: %s", forbidden, encoded)
		}
	}

	view.Tools[0].ProfileID = "mutated"
	view.Agents[0].Backend = "mutated"
	view.Checks[0].Summary = "mutated"
	if report.Tools[0].ProfileID != toolcatalog.ClaudeCode || report.Agents[0].Backend != "pty" ||
		report.Checks[0].Summary != "The configuration is valid." {
		t.Fatalf("view aliases core report: %#v", report)
	}
}

func validGUIPreflightReport(t *testing.T) preflight.Report {
	t.Helper()
	auditConfiguration := audit.DefaultConfig()
	auditConfiguration.Enabled = false
	auditConfiguration.Mode = audit.ModeOff
	spec := agent.Spec{
		ID: "configured", Name: "Configured", Command: []string{"codex"},
		Adapter: adapters.CodexID, Backend: agent.BackendPTY,
	}
	report, err := preflight.Check(context.Background(), preflight.Input{
		Configuration: config.Result{
			Version: config.CurrentVersion, Backend: agent.BackendPTY,
			Agents: []agent.Spec{spec}, Patterns: adapters.DefaultPatterns(),
			Policies: policy.DefaultConfig(), Audit: auditConfiguration,
		},
		Specs: []agent.Spec{spec},
	}, preflight.Options{
		GOOS: "darwin", GOARCH: "arm64",
		Detector: guiPreflightDetectorFunc(func(ctx context.Context, candidates []string) (toolcatalog.Detection, error) {
			if err := ctx.Err(); err != nil {
				return toolcatalog.Detection{}, err
			}
			for _, candidate := range candidates {
				if candidate == "codex" {
					return toolcatalog.Detection{
						Status: toolcatalog.InstallInstalled, Executable: candidate, Path: "/detected/codex",
					}, nil
				}
			}
			return toolcatalog.Detection{Status: toolcatalog.InstallNotInstalled}, nil
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return report
}
