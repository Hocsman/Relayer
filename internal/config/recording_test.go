package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/record"
)

func TestRecordingConfigurationDefaultsAndCompatibility(t *testing.T) {
	disabled := record.DefaultConfig()
	disabled.Enabled = false

	t.Run("version one without recording stays disabled", func(t *testing.T) {
		result := loadRecordingConfiguration(t, "")
		if result.Recording != disabled {
			t.Fatalf("recording = %#v, want disabled compatibility default %#v", result.Recording, disabled)
		}
	})

	t.Run("legacy stays disabled", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legacy.yaml")
		if err := os.WriteFile(path, []byte("- pattern: continue\n  description: Continue\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		result, err := LoadOrCreate(path)
		if err != nil {
			t.Fatal(err)
		}
		if result.Recording != disabled {
			t.Fatalf("legacy recording = %#v, want disabled", result.Recording)
		}
	})

	t.Run("legacy wrapper stays disabled", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "legacy-wrapper.yaml")
		if err := os.WriteFile(path, []byte("intercept_patterns:\n  - pattern: continue\n    description: Continue\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		result, err := LoadOrCreate(path)
		if err != nil {
			t.Fatal(err)
		}
		if result.Recording != disabled {
			t.Fatalf("legacy wrapper recording = %#v, want disabled", result.Recording)
		}
	})

	t.Run("generated config does not write a recording block", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "generated.yaml")
		result, err := LoadOrCreate(path)
		if err != nil {
			t.Fatal(err)
		}
		if !result.Created || result.Recording != disabled {
			t.Fatalf("generated recording = %#v", result.Recording)
		}
		contents, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(contents), "recording:") {
			t.Fatalf("generated config contains a recording block:\n%s", contents)
		}
	})
}

func TestRecordingConfigurationLoadsStrictSchemaAndResolvesRelativePath(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	contents := recordingVersionOne(`recording:
  enabled: true
  path: private/casts
  record_input: true
  redact: false
  max_file_size_mb: 4
  max_recordings: 7
  max_total_size_mb: 24
  retention_days: 3
`)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	want := record.Config{
		Enabled:        true,
		Path:           filepath.Join(directory, "private", "casts"),
		RecordInput:    true,
		Redact:         false,
		MaxFileSizeMB:  4,
		MaxRecordings:  7,
		MaxTotalSizeMB: 24,
		RetentionDays:  3,
	}
	if result.Recording != want {
		t.Fatalf("recording = %#v, want %#v", result.Recording, want)
	}
}

func TestRecordingConfigurationKeepsAbsolutePath(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	absolute := filepath.Join(directory, "casts")
	contents := recordingVersionOne(fmt.Sprintf("recording:\n  enabled: true\n  path: '%s'\n", absolute))
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	if result.Recording.Path != absolute {
		t.Fatalf("recording path = %q, want %q", result.Recording.Path, absolute)
	}
}

func TestRecordingConfigurationExplicitBlockUsesSafeFieldDefaults(t *testing.T) {
	result := loadRecordingConfiguration(t, "recording: {}\n")
	want := record.DefaultConfig()
	if result.Recording != want {
		t.Fatalf("recording defaults = %#v, want %#v", result.Recording, want)
	}

	result = loadRecordingConfiguration(t, "recording:\n  enabled: true\n  record_input: true\n")
	if !result.Recording.Enabled || !result.Recording.RecordInput || !result.Recording.Redact ||
		result.Recording.MaxFileSizeMB != want.MaxFileSizeMB || result.Recording.MaxRecordings != want.MaxRecordings ||
		result.Recording.MaxTotalSizeMB != want.MaxTotalSizeMB || result.Recording.RetentionDays != want.RetentionDays {
		t.Fatalf("partial recording = %#v", result.Recording)
	}
}

func TestRecordingConfigurationRejectsInvalidValuesBeforeStartup(t *testing.T) {
	tests := map[string]string{
		"recording is list":       "recording: []\n",
		"enabled wrong type":      "recording:\n  enabled: yes\n",
		"path wrong type":         "recording:\n  path: false\n",
		"record_input wrong type": "recording:\n  record_input: 1\n",
		"redact wrong type":       "recording:\n  redact: 'true'\n",
		"size wrong type":         "recording:\n  max_file_size_mb: '16'\n",
		"recordings wrong type":   "recording:\n  max_recordings: false\n",
		"total size wrong type":   "recording:\n  max_total_size_mb: 1.5\n",
		"retention wrong type":    "recording:\n  retention_days: '30'\n",
		"unknown field":           "recording:\n  destination: nowhere\n",
		"non-positive size":       "recording:\n  max_file_size_mb: 0\n",
		"non-positive recordings": "recording:\n  max_recordings: 0\n",
		"excessive recordings":    "recording:\n  max_recordings: 10001\n",
		"non-positive total size": "recording:\n  max_total_size_mb: 0\n",
		"total smaller than file": "recording:\n  max_file_size_mb: 32\n  max_total_size_mb: 16\n",
		"negative retention":      "recording:\n  retention_days: -1\n",
		"excessive retention":     "recording:\n  retention_days: 3651\n",
		"NUL path":                "recording:\n  path: \"bad\\0path\"\n",
	}
	for name, block := range tests {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(path, []byte(recordingVersionOne(block)), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := LoadOrCreate(path); err == nil {
				t.Fatal("invalid recording configuration was accepted")
			}
		})
	}
}

func TestRecordingConfigurationSurvivesFullConfigurationUpdate(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "config.yaml")
	initial := `version: 1
backend: pty

recording:
  enabled: true
  path: casts
  record_input: true
  redact: false
  max_file_size_mb: 4
  max_recordings: 7
  max_total_size_mb: 24
  retention_days: 3

agents:
  - id: alpha
    name: Agent Alpha
    command: ["echo", "hello"]

intercept_patterns:
  - pattern: '(?i)continue'
    description: continue prompt
`
	if err := os.WriteFile(path, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	loaded, err := LoadExisting(path)
	if err != nil {
		t.Fatal(err)
	}

	notifications := notify.DefaultConfig()
	notifications.Enabled = true
	updated, _, err := UpdateFullConfiguration(path, loaded.Revision, FullConfigurationUpdate{
		Notifications: &notifications,
	})
	if err != nil {
		t.Fatalf("update notifications: %v", err)
	}
	if updated.Recording != loaded.Recording {
		t.Fatalf("recording = %#v, want preserved %#v", updated.Recording, loaded.Recording)
	}
	contents, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(contents), "recording:") {
		t.Fatalf("saved configuration dropped the recording block:\n%s", contents)
	}
}

func loadRecordingConfiguration(t *testing.T, block string) Result {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(recordingVersionOne(block)), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := LoadOrCreate(path)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func recordingVersionOne(extra string) string {
	return "version: 1\n" +
		"backend: pty\n" +
		extra +
		"agents: []\n" +
		"intercept_patterns:\n" +
		"  - pattern: continue\n" +
		"    description: Continue\n"
}
