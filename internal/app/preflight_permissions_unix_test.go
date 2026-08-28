//go:build unix

package app

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/preflight"
)

func TestRunPreflightInaccessibleAgentWorkingDirectoryIsConfigurationInvalid(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can traverse directories regardless of permission bits")
	}
	const secret = "inaccessible-cwd-private-sentinel-6ac3"
	directory := t.TempDir()
	blockedParent := filepath.Join(directory, secret)
	workingDirectory := filepath.Join(blockedParent, "agent")
	if err := os.MkdirAll(workingDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "config.yaml")
	contents := `version: 1
backend: pty
audit:
  enabled: false
  mode: off
agents:
  - id: cwd-validation
    name: CWD validation
    command: [runner]
    cwd: ` + workingDirectory + `
intercept_patterns:
  - pattern: continue
    description: Continue
`
	if err := os.WriteFile(configPath, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(blockedParent, 0); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blockedParent, 0o700) })

	report, err := runPreflight(context.Background(), PreflightOptions{ConfigPath: configPath}, preflightDependencies{
		getwd:   func() (string, error) { return directory, nil },
		options: preflight.Options{GOOS: "linux", GOARCH: "amd64"},
	})
	if err != nil || !report.HasBlockers() || len(report.Checks) != 1 || report.Checks[0].ID != "configuration.invalid" {
		t.Fatalf("inaccessible agent cwd report = %#v, error %v", report, err)
	}
	encoded, marshalErr := json.Marshal(report)
	if marshalErr != nil {
		t.Fatal(marshalErr)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(string(encoded), directory) {
		t.Fatalf("report leaked inaccessible cwd or configuration path: %s", encoded)
	}
}
