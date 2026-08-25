//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package app

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestRunRollsBackStartedAgentsWhenLaterStartupFailsWithoutLeakingSecrets(t *testing.T) {
	if _, err := exec.LookPath("ps"); err != nil {
		t.Skipf("ps is required for the rollback assertion: %v", err)
	}

	directory := t.TempDir()
	configPath := filepath.Join(directory, "config.yaml")
	missingExecutable := filepath.Join(directory, "missing-executable")
	processMarker := fmt.Sprintf("relayer-rollback-%d", time.Now().UnixNano())
	secret := "rollback-secret-must-not-leak"
	script := "while :; do sleep 30; done # " + processMarker
	configuration := fmt.Sprintf(`version: 1
backend: pty
agents:
  - id: rollback-first
    name: First rollback agent
    shell: %s
    env:
      OPENAI_API_KEY: %s
  - id: rollback-second
    name: Failing rollback agent
    command: [%s]
intercept_patterns:
  - pattern: '(?i)continue'
    description: Continue confirmation
`, strconv.Quote(script), strconv.Quote(secret), strconv.Quote(missingExecutable))
	if err := os.WriteFile(configPath, []byte(configuration), 0o600); err != nil {
		t.Fatalf("writing rollback config: %v", err)
	}

	var diagnostics bytes.Buffer
	err := Run([]string{"--config", configPath}, &diagnostics)
	if err == nil || !strings.Contains(err.Error(), "rollback-second") {
		t.Fatalf("Run error = %v, want the second agent startup failure", err)
	}
	combined := err.Error() + "\n" + diagnostics.String()
	if strings.Contains(combined, script) || strings.Contains(combined, secret) {
		t.Fatalf("shell or environment secret leaked on rollback: %q", combined)
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		output, psErr := exec.Command("ps", "-ax", "-o", "command=").Output()
		if psErr != nil {
			t.Skipf("process inspection is unavailable after rollback: %v", psErr)
		}
		if !bytes.Contains(output, []byte(processMarker)) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("first agent survived rollback; process marker %q is still present", processMarker)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
