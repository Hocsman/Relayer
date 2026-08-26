package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/audit"
)

func TestAuditConfigurationDefaultsAndCompatibility(t *testing.T) {
	t.Run("version one without audit stays disabled", func(t *testing.T) {
		result := loadAuditConfiguration(t, "")
		if result.Audit.Enabled || result.Audit.Mode != audit.ModeOff {
			t.Fatalf("audit = %#v, want disabled compatibility default", result.Audit)
		}
	})

	t.Run("legacy stays disabled", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legacy.yaml")
		if err := os.WriteFile(path, []byte("- pattern: continue\n  description: Continue\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		result, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if result.Audit.Enabled || result.Audit.Mode != audit.ModeOff {
			t.Fatalf("legacy audit = %#v, want disabled", result.Audit)
		}
	})

	t.Run("legacy wrapper stays disabled", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legacy-wrapper.yaml")
		if err := os.WriteFile(path, []byte("intercept_patterns:\n  - pattern: continue\n    description: Continue\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		result, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if result.Audit.Enabled || result.Audit.Mode != audit.ModeOff {
			t.Fatalf("legacy wrapper audit = %#v, want disabled", result.Audit)
		}
	})

	t.Run("generated config enables metadata audit", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "generated.yaml")
		result, err := Load(path)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Created || !result.Audit.Enabled || result.Audit.Mode != audit.ModeMetadata ||
			result.Audit.MaxFileSizeMB != 10 || result.Audit.MaxFiles != 5 {
			t.Fatalf("generated audit = %#v", result.Audit)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for _, fragment := range []string{"audit:", "enabled: true", "mode: metadata", `path: ""`, "max_file_size_mb: 10", "max_files: 5"} {
			if !strings.Contains(string(contents), fragment) {
				t.Fatalf("generated config does not contain %q:\n%s", fragment, contents)
			}
		}
	})
}

func TestAuditConfigurationLoadsStrictSchemaAndResolvesRelativePath(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	contents := auditVersionOne(`audit:
  enabled: true
  mode: detailed
  path: private/audit.jsonl
  max_file_size_mb: 3
  max_files: 2
`)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	wantPath := filepath.Join(directory, "private", "audit.jsonl")
	if !result.Audit.Enabled || result.Audit.Mode != audit.ModeDetailed ||
		result.Audit.Path != wantPath || result.Audit.MaxFileSizeMB != 3 || result.Audit.MaxFiles != 2 {
		t.Fatalf("audit = %#v, want path %q", result.Audit, wantPath)
	}
}

func TestAuditConfigurationExplicitBlockUsesSafeFieldDefaults(t *testing.T) {
	result := loadAuditConfiguration(t, "audit: {}\n")
	want := audit.DefaultConfig()
	if result.Audit != want {
		t.Fatalf("audit defaults = %#v, want %#v", result.Audit, want)
	}

	result = loadAuditConfiguration(t, "audit:\n  enabled: false\n  mode: detailed\n")
	if result.Audit.Enabled || result.Audit.Mode != audit.ModeDetailed ||
		result.Audit.MaxFileSizeMB != want.MaxFileSizeMB || result.Audit.MaxFiles != want.MaxFiles {
		t.Fatalf("partial disabled audit = %#v", result.Audit)
	}
}

func TestAuditConfigurationRejectsInvalidValuesBeforeStartup(t *testing.T) {
	tests := map[string]string{
		"audit is list":           "audit: []\n",
		"enabled wrong type":      "audit:\n  enabled: yes\n",
		"mode wrong type":         "audit:\n  mode: 1\n",
		"path wrong type":         "audit:\n  path: false\n",
		"size wrong type":         "audit:\n  max_file_size_mb: '10'\n",
		"files wrong type":        "audit:\n  max_files: false\n",
		"unknown field":           "audit:\n  destination: nowhere\n",
		"invalid mode":            "audit:\n  mode: verbose\n",
		"non-positive size":       "audit:\n  max_file_size_mb: 0\n",
		"non-positive file count": "audit:\n  max_files: -1\n",
		"excessive file count":    "audit:\n  max_files: 101\n",
		"NUL path":                "audit:\n  path: \"bad\\0path\"\n",
	}
	for name, block := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(auditVersionOne(block)), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := Load(path); err == nil {
				t.Fatal("invalid audit configuration was accepted")
			}
		})
	}
}

func loadAuditConfiguration(t *testing.T, block string) Result {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(auditVersionOne(block)), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func auditVersionOne(extra string) string {
	return "version: 1\n" +
		"backend: pty\n" +
		extra +
		"agents: []\n" +
		"intercept_patterns:\n" +
		"  - pattern: continue\n" +
		"    description: Continue\n"
}
