package app

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/preflight"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

type doctorDetectorFunc func(context.Context, []string) (toolcatalog.Detection, error)

func (function doctorDetectorFunc) Detect(ctx context.Context, candidates []string) (toolcatalog.Detection, error) {
	return function(ctx, candidates)
}

func TestDoctorRendersDeterministicReadyAndWarningReports(t *testing.T) {
	tests := []struct {
		name       string
		report     preflight.Report
		wantStatus string
	}{
		{
			name:       "ready",
			report:     validDoctorReport(t, false),
			wantStatus: "— READY\n",
		},
		{
			name:       "warning",
			report:     validDoctorReport(t, true),
			wantStatus: "— WARNINGS\n",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var first, second bytes.Buffer
			runner := func(context.Context, PreflightOptions) (preflight.Report, error) {
				return test.report, nil
			}
			if err := runDoctor(nil, &first, io.Discard, runner); err != nil {
				t.Fatalf("runDoctor: %v", err)
			}
			if err := runDoctor(nil, &second, io.Discard, runner); err != nil {
				t.Fatalf("second runDoctor: %v", err)
			}
			if first.String() != second.String() || !strings.Contains(first.String(), test.wantStatus) ||
				!strings.Contains(first.String(), "[OK] Configuration — The configuration is valid.") {
				t.Fatalf("non-deterministic or incomplete output:\nfirst=%q\nsecond=%q", first.String(), second.String())
			}
		})
	}
}

func TestDoctorRendersSafeToolAndAgentInventory(t *testing.T) {
	report := validDoctorReport(t, true)
	var output bytes.Buffer
	if err := writeDoctorReport(&output, report); err != nil {
		t.Fatalf("writeDoctorReport: %v", err)
	}
	for _, expected := range []string{
		"  - Codex CLI: detected\n",
		"  - Agent #1: source=configured, command=direct, executable=detected, adapter=codex (experimental), backend=pty\n",
		"[WARNING] Adapter — The effective adapter is experimental.\n",
		"[WARNING] Backend — The auto backend will fall back to PTY because tmux is unavailable.\n",
	} {
		if !strings.Contains(output.String(), expected) {
			t.Fatalf("inventory output missing %q: %q", expected, output.String())
		}
	}
}

func TestDoctorWritesBlockedReportThenReturnsSilentExitSentinel(t *testing.T) {
	report := preflight.FailureReport(preflight.FailureConfigMissing, preflight.Options{GOOS: "darwin", GOARCH: "arm64"})
	var output bytes.Buffer
	err := runDoctor(nil, &output, io.Discard, func(context.Context, PreflightOptions) (preflight.Report, error) {
		return report, nil
	})
	if !errors.Is(err, ErrPreflightBlocked) {
		t.Fatalf("runDoctor error = %v", err)
	}
	want := "Relayer doctor\n" +
		"Checks:\n" +
		"[BLOCKED] Configuration — The configuration file is missing.\n" +
		"  Action: Create the configuration explicitly before running the diagnostic again.\n" +
		"Summary: 0 OK, 0 warning(s), 1 blocker(s) — BLOCKED\n"
	if output.String() != want {
		t.Fatalf("output = %q, want %q", output.String(), want)
	}
}

func TestRunWithOutputDoctorUsesDedicatedReadOnlyPath(t *testing.T) {
	var output, diagnostics bytes.Buffer
	called := 0
	requestedPath := ""
	err := runWithOutputAndPreflight(
		[]string{"doctor", "--config", "selected.yaml"},
		&output,
		&diagnostics,
		backendDependencies{lookup: func(string) (string, error) {
			t.Fatal("runtime backend lookup was called")
			return "", nil
		}},
		func(_ context.Context, options PreflightOptions) (preflight.Report, error) {
			called++
			requestedPath = options.ConfigPath
			return validDoctorReport(t, false), nil
		},
	)
	if err != nil {
		t.Fatalf("doctor: %v", err)
	}
	if called != 1 || requestedPath != "selected.yaml" || diagnostics.Len() != 0 {
		t.Fatalf("doctor calls=%d path=%q diagnostics=%q", called, requestedPath, diagnostics.String())
	}
	if !strings.Contains(output.String(), "— READY") {
		t.Fatalf("doctor output = %q", output.String())
	}
}

func TestDoctorRejectsRuntimeAndPositionalArgumentsBeforePreflight(t *testing.T) {
	for _, arguments := range [][]string{
		{"--pane1", "runner"},
		{"unexpected"},
		{"--config"},
	} {
		called := false
		err := runDoctor(arguments, io.Discard, io.Discard, func(context.Context, PreflightOptions) (preflight.Report, error) {
			called = true
			return preflight.Report{}, nil
		})
		if err == nil || called {
			t.Fatalf("arguments %q error=%v called=%t", arguments, err, called)
		}
	}
}

func TestDoctorHelpIsSuccessfulAndDoesNotRunPreflight(t *testing.T) {
	var diagnostics bytes.Buffer
	called := false
	err := runDoctor([]string{"--help"}, io.Discard, &diagnostics, func(context.Context, PreflightOptions) (preflight.Report, error) {
		called = true
		return preflight.Report{}, nil
	})
	if !errors.Is(err, flag.ErrHelp) || called {
		t.Fatalf("help error=%v called=%t", err, called)
	}
	if !strings.Contains(diagnostics.String(), "Usage: relayer doctor") {
		t.Fatalf("help = %q", diagnostics.String())
	}
}

func TestDoctorMissingConfigurationDoesNotCreateOrLeakPath(t *testing.T) {
	const secret = "doctor-private-path-sentinel-98c2"
	root := t.TempDir()
	path := filepath.Join(root, secret, "config.yaml")
	var output bytes.Buffer
	err := runWithOutput([]string{"doctor", "--config", path}, &output, io.Discard, backendDependencies{})
	if !errors.Is(err, ErrPreflightBlocked) {
		t.Fatalf("doctor error = %v", err)
	}
	if strings.Contains(output.String(), secret) {
		t.Fatalf("doctor leaked path: %q", output.String())
	}
	if _, statErr := os.Lstat(filepath.Dir(path)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("doctor created missing directory: %v", statErr)
	}
}

func TestDoctorConvertsRunnerAndWriterFailuresToSafeErrors(t *testing.T) {
	const secret = "doctor-internal-secret-sentinel-31af"
	t.Run("runner", func(t *testing.T) {
		var output bytes.Buffer
		err := runDoctor(nil, &output, io.Discard, func(context.Context, PreflightOptions) (preflight.Report, error) {
			return preflight.Report{}, errors.New(secret)
		})
		if !errors.Is(err, ErrPreflightBlocked) || strings.Contains(output.String(), secret) {
			t.Fatalf("runner error=%v output=%q", err, output.String())
		}
	})
	t.Run("writer", func(t *testing.T) {
		err := runDoctor(nil, doctorFailingWriter{err: errors.New(secret)}, io.Discard,
			func(context.Context, PreflightOptions) (preflight.Report, error) {
				return validDoctorReport(t, false), nil
			})
		if err == nil || strings.Contains(err.Error(), secret) {
			t.Fatalf("writer error = %v", err)
		}
	})
}

func TestDoctorRejectsMalformedReportWithoutPrintingSecret(t *testing.T) {
	const secret = "doctor-malformed-report-secret-sentinel-a6d4"
	report := validDoctorReport(t, false)
	report.Checks[0].Summary = secret
	var output bytes.Buffer
	err := runDoctor(nil, &output, io.Discard, func(context.Context, PreflightOptions) (preflight.Report, error) {
		return report, nil
	})
	if !errors.Is(err, ErrPreflightBlocked) || strings.Contains(output.String(), secret) {
		t.Fatalf("runDoctor error=%v output=%q", err, output.String())
	}
	if !strings.Contains(output.String(), "The diagnostic could not be completed safely.") {
		t.Fatalf("safe internal failure not rendered: %q", output.String())
	}

	output.Reset()
	if err := writeDoctorReport(&output, report); err == nil || strings.Contains(err.Error(), secret) || output.Len() != 0 {
		t.Fatalf("writeDoctorReport error=%v output=%q", err, output.String())
	}
}

func validDoctorReport(t *testing.T, warning bool) preflight.Report {
	t.Helper()
	auditConfiguration := audit.DefaultConfig()
	auditConfiguration.Enabled = false
	auditConfiguration.Mode = audit.ModeOff
	executable := "runner"
	adapterID := adapters.GenericID
	backend := agent.BackendPTY
	configurationBackend := agent.BackendPTY
	if warning {
		executable = "codex"
		adapterID = adapters.CodexID
		backend = agent.BackendAuto
		configurationBackend = agent.BackendAuto
	}
	spec := agent.Spec{
		ID: "configured", Name: "Configured", Command: []string{executable},
		Adapter: adapterID, Backend: backend,
	}
	report, err := preflight.Check(context.Background(), preflight.Input{
		Configuration: config.Result{
			Version: config.CurrentVersion, Backend: configurationBackend,
			Agents: []agent.Spec{spec}, Patterns: adapters.DefaultPatterns(),
			Policies: policy.DefaultConfig(), Audit: auditConfiguration,
		},
		Specs: []agent.Spec{spec},
	}, preflight.Options{
		GOOS: "darwin", GOARCH: "arm64",
		Detector: doctorDetectorFunc(func(ctx context.Context, candidates []string) (toolcatalog.Detection, error) {
			if err := ctx.Err(); err != nil {
				return toolcatalog.Detection{}, err
			}
			for _, candidate := range candidates {
				if candidate == executable {
					return toolcatalog.Detection{
						Status: toolcatalog.InstallInstalled, Executable: candidate, Path: "/detected/tool",
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

type doctorFailingWriter struct{ err error }

func (writer doctorFailingWriter) Write([]byte) (int, error) { return 0, writer.err }
