package config

import (
	"os"
	"path/filepath"
	"testing"
)

func patternsFromYAML(t *testing.T, body string) map[string]bool {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := LoadExisting(path)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	sensitivity := make(map[string]bool, len(result.Patterns))
	for _, pattern := range result.Patterns {
		sensitivity[pattern.Expression] = pattern.Sensitive
	}
	return sensitivity
}

const sensitivityConfigPrefix = `version: 1
backend: pty
agents: []
intercept_patterns:
`

// The inference is a word list and cannot recognize every prompt. Without a way
// to declare sensitivity, a missed word means the operator field is not masked
// and the event is not forced to a human, with no way to correct it.
func TestConfiguredSensitiveEscalatesAPatternTheInferenceMisses(t *testing.T) {
	sensitivity := patternsFromYAML(t, sensitivityConfigPrefix+`  - pattern: 'enter the six digits we sent you'
    description: "second factor challenge"
    sensitive: true
  - pattern: 'unrelated confirmation'
    description: "plain confirmation"
`)

	if !sensitivity["enter the six digits we sent you"] {
		t.Fatal("declared sensitive pattern was not marked sensitive")
	}
	if sensitivity["unrelated confirmation"] {
		t.Fatal("undeclared ordinary pattern became sensitive")
	}
}

// Declaring false must not be able to unmask a prompt the inference already
// recognized: the field escalates only.
func TestConfiguredSensitiveNeverDowngradesAnInferredSecret(t *testing.T) {
	sensitivity := patternsFromYAML(t, sensitivityConfigPrefix+`  - pattern: 'password:'
    description: "credential prompt"
    sensitive: false
`)

	if !sensitivity["password:"] {
		t.Fatal("sensitive: false downgraded an inferred credential prompt")
	}
}

// Omitting the field keeps the historical behaviour for existing files.
func TestOmittedSensitiveKeepsTheInferredValue(t *testing.T) {
	sensitivity := patternsFromYAML(t, sensitivityConfigPrefix+`  - pattern: 'passphrase:'
    description: "credential prompt"
  - pattern: 'overwrite\? \[y/n\]'
    description: "overwrite confirmation"
`)

	if !sensitivity["passphrase:"] {
		t.Fatal("inferred credential prompt lost its sensitivity")
	}
	if sensitivity[`overwrite\? \[y/n\]`] {
		t.Fatal("ordinary confirmation became sensitive")
	}
}
