package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/preflight"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

func TestRunPreflightUsesEffectiveDemoAgentsWithoutMutatingConfiguration(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	contents := []byte(`version: 1
backend: pty
audit:
  enabled: false
  mode: off
agents: []
intercept_patterns:
  - pattern: continue
    description: Continue
`)
	if err := os.WriteFile(configPath, contents, 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := runPreflight(context.Background(), PreflightOptions{ConfigPath: configPath}, preflightDependencies{
		getwd: func() (string, error) { return directory, nil },
		options: preflight.Options{
			GOOS: "darwin", GOARCH: "arm64",
			Detector: installedPreflightDetector(),
		},
	})
	if err != nil {
		t.Fatalf("runPreflight: %v", err)
	}
	if !report.Ready() || report.Configuration.AgentCount != 2 || len(report.Agents) != 2 {
		t.Fatalf("effective demo report = %#v", report)
	}
	for index, agent := range report.Agents {
		if agent.Ordinal != index+1 || agent.Source != preflight.AgentDemo {
			t.Fatalf("agent %d = %#v", index, agent)
		}
	}
	after, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(contents) {
		t.Fatalf("preflight mutated configuration:\n%s", after)
	}
}

func TestRunPreflightMissingConfigurationIsStaticAndReadOnly(t *testing.T) {
	directory := t.TempDir()
	configPath := filepath.Join(directory, "private-segment", "config.yaml")
	report, err := runPreflight(context.Background(), PreflightOptions{ConfigPath: configPath}, preflightDependencies{
		getwd:   func() (string, error) { return directory, nil },
		options: preflight.Options{GOOS: "linux", GOARCH: "amd64"},
	})
	if err != nil || !report.HasBlockers() || len(report.Checks) != 1 || report.Checks[0].ID != "configuration.missing" {
		t.Fatalf("missing configuration report = %#v, error %v", report, err)
	}
	if _, statErr := os.Lstat(filepath.Dir(configPath)); !errors.Is(statErr, os.ErrNotExist) {
		t.Fatalf("read-only preflight created a directory: %v", statErr)
	}
	encoded, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), "private-segment") {
		t.Fatalf("report leaked configuration path: %s", encoded)
	}
}

func TestRunPreflightFailuresNeverExposeUnderlyingValues(t *testing.T) {
	const secret = "preflight-secret-sentinel-7d81"
	t.Run("invalid configuration", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), secret+".yaml")
		contents := []byte("version: " + secret + "\n")
		if err := os.WriteFile(path, contents, 0o600); err != nil {
			t.Fatal(err)
		}
		report, err := runPreflight(context.Background(), PreflightOptions{ConfigPath: path}, preflightDependencies{
			getwd: func() (string, error) { return t.TempDir(), nil },
		})
		if err != nil || !report.HasBlockers() || report.Checks[0].ID != "configuration.invalid" {
			t.Fatalf("invalid report = %#v, error %v", report, err)
		}
		encoded, marshalErr := json.Marshal(report)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("report leaked invalid input: %s", encoded)
		}
	})

	t.Run("working directory", func(t *testing.T) {
		path := writePreflightConfiguredAgent(t)
		report, err := runPreflight(context.Background(), PreflightOptions{ConfigPath: path}, preflightDependencies{
			getwd: func() (string, error) { return "", errors.New(secret) },
		})
		if err != nil || !report.HasBlockers() || report.Checks[0].ID != "configuration.working_directory" {
			t.Fatalf("working directory report = %#v, error %v", report, err)
		}
		encoded, marshalErr := json.Marshal(report)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(string(encoded), secret) {
			t.Fatalf("report leaked dependency error: %s", encoded)
		}
	})
}

func TestRunPreflightMissingAgentWorkingDirectoryIsConfigurationInvalid(t *testing.T) {
	const secret = "missing-cwd-private-sentinel-4f82"
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	contents := `version: 1
backend: pty
audit:
  enabled: false
  mode: off
agents:
  - id: cwd-validation
    name: CWD validation
    command: [runner]
    cwd: ` + secret + `
intercept_patterns:
  - pattern: continue
    description: Continue
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}

	report, err := runPreflight(context.Background(), PreflightOptions{ConfigPath: path}, preflightDependencies{
		getwd:   func() (string, error) { return directory, nil },
		options: preflight.Options{GOOS: "linux", GOARCH: "amd64"},
	})
	if err != nil || !report.HasBlockers() || len(report.Checks) != 1 || report.Checks[0].ID != "configuration.invalid" {
		t.Fatalf("missing agent cwd report = %#v, error %v", report, err)
	}
	encoded, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), directory) {
		t.Fatalf("report leaked agent cwd or configuration path: %s", encoded)
	}
}

func TestRunPreflightPreservesContextErrors(t *testing.T) {
	//lint:ignore SA1012 The nil-context boundary is part of the public facade contract.
	if report, err := runPreflight(nil, PreflightOptions{}, preflightDependencies{}); !errors.Is(err, preflight.ErrNilContext) || !reflect.DeepEqual(report, preflight.Report{}) {
		t.Fatalf("nil context report = %#v, error %v", report, err)
	}
	if report, err := runPreflight(cancelledPreflightContext(), PreflightOptions{}, preflightDependencies{}); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(report, preflight.Report{}) {
		t.Fatalf("cancelled context report = %#v, error %v", report, err)
	}
}

func cancelledPreflightContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}

func installedPreflightDetector() toolcatalog.Detector {
	return toolcatalog.PathDetector{LookPath: func(candidate string) (string, error) {
		return "/detected/" + filepath.Base(candidate), nil
	}}
}

func writePreflightConfiguredAgent(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	contents := `version: 1
backend: pty
audit:
  enabled: false
  mode: off
agents:
  - id: configured
    name: Configured
    command: [runner]
    adapter: generic
intercept_patterns:
  - pattern: continue
    description: Continue
`
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
