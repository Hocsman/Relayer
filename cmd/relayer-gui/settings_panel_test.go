package main

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Hocsman/Relayer/internal/config"
)

func TestGetAndSaveFullSettings(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")

	initialYAML := `version: 1
backend: pty

policies:
  default_action: ask
  dry_run: false
  rules: []

agents:
  - id: alpha
    name: Agent Alpha
    command: ["echo", "alpha"]

intercept_patterns:
  - pattern: '(?i)continue'
    description: continue prompt
`
	if err := os.WriteFile(configPath, []byte(initialYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	app := NewApp()
	app.configPath = configPath
	app.ctx = context.Background()

	// 1. GetFullSettings
	settings, err := app.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings failed: %v", err)
	}
	app.activeConfigRevision = app.profileRevisionHash
	if settings.Security.DefaultAction != "ask" {
		t.Errorf("expected defaultAction ask, got %s", settings.Security.DefaultAction)
	}
	if len(settings.Profiles) != 1 || settings.Profiles[0].ID != "alpha" {
		t.Errorf("expected 1 profile alpha, got %v", settings.Profiles)
	}

	// 2. SaveFullSettings: update security profile and notifications
	saveReq := SaveFullSettingsRequest{
		ExpectedRevision: settings.Revision,
		Security: &SecuritySettings{
			Profile:               "developer-friendly",
			DefaultAction:         "ask",
			BlockDestructive:      true,
			BlockSensitivePaths:   true,
			BlockOutsideWorkspace: false,
			WorkspaceRoot:         tempDir,
		},
		Notifications: &NotificationSettings{
			Enabled:     true,
			Desktop:     true,
			Bell:        true,
			MinSeverity: "warning",
			Webhooks: []NotificationWebhookSetting{
				{
					Name:        "team-slack",
					URL:         "https://hooks.slack.com/services/xxx",
					Format:      "slack",
					MinSeverity: "critical",
					Timeout:     "5s",
				},
			},
		},
	}

	saved, err := app.SaveFullSettings("", saveReq)
	if err != nil {
		t.Fatalf("SaveFullSettings failed: %v", err)
	}

	if !saved.Security.BlockDestructive {
		t.Errorf("expected BlockDestructive true")
	}
	if !saved.Security.BlockSensitivePaths {
		t.Errorf("expected BlockSensitivePaths true")
	}
	if !saved.Notifications.Desktop {
		t.Errorf("expected Notifications.Desktop true")
	}
	if len(saved.Notifications.Webhooks) != 1 || saved.Notifications.Webhooks[0].Name != "team-slack" {
		t.Errorf("expected 1 webhook team-slack")
	}

	// The policy changed, and the engine is built once per run: the save must
	// say a restart is needed. v0.8.5 asserted the opposite here, because only
	// agent changes were counted, and the panel said "applied immediately".
	if !saved.RestartRequired {
		t.Errorf("expected RestartRequired true after a policy change the running engine does not have")
	}
}

// TestNotificationOnlySaveNeedsNoRestart guards the other side of the
// restart-required rule: notifications are applied to the running app at once,
// so a save that changes only them must not ask for a restart.
func TestNotificationOnlySaveNeedsNoRestart(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	initialYAML := `version: 1
backend: pty

agents:
  - id: alpha
    name: Agent Alpha
    command: ["echo", "alpha"]

intercept_patterns:
  - pattern: '(?i)continue'
    description: continue prompt
`
	if err := os.WriteFile(configPath, []byte(initialYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	app := NewApp()
	app.configPath = configPath
	app.ctx = context.Background()

	view, err := app.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}
	// As if the running engine had been started from this file.
	app.activeConfigRevision = app.profileRevisionHash

	notifications := view.Notifications
	notifications.Bell = !notifications.Bell
	saved, err := app.SaveFullSettings("", SaveFullSettingsRequest{
		ExpectedRevision: view.Revision,
		Notifications:    &notifications,
	})
	if err != nil {
		t.Fatalf("SaveFullSettings: %v", err)
	}
	if saved.RestartRequired {
		t.Fatal("a notification-only save asked for a restart, though it is applied at once")
	}

	// A policy change after it still needs one.
	security := saved.Security
	security.DryRun = true
	saved, err = app.SaveFullSettings("", SaveFullSettingsRequest{
		ExpectedRevision: saved.Revision,
		Security:         &security,
	})
	if err != nil {
		t.Fatalf("SaveFullSettings security: %v", err)
	}
	if !saved.RestartRequired {
		t.Fatal("a dry-run change was reported as applied, but the running engine never sees it")
	}
}

// TestDesktopSecuritySaveKeepsWhatTheFormDoesNotShow is the desktop half of the
// settings round-trip fix: the GUI shared the web gateway's guess-and-rebuild
// save, so a form that only toggled dry-run dropped a deny rule and a blocked
// pattern and rewrote the limits.
func TestDesktopSecuritySaveKeepsWhatTheFormDoesNotShow(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	initialYAML := `version: 1
backend: pty

policies:
  default_action: ask
  dry_run: false
  rate_limit_per_minute: 10
  max_consecutive_auto_decisions: 1
  guardrails:
    block_destructive: true
    block_exfiltration: true
    block_sensitive_paths: true
    blocked_patterns: ['(?i)terraform\s+destroy']
  rules:
    - name: deny-terraform
      match:
        command_regex: '(?i)^terraform\b'
      action: deny

agents:
  - id: alpha
    name: Agent Alpha
    command: ["echo", "alpha"]

intercept_patterns:
  - pattern: '(?i)continue'
    description: continue prompt
`
	if err := os.WriteFile(configPath, []byte(initialYAML), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}
	before, err := config.LoadExisting(configPath)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}

	app := NewApp()
	app.configPath = configPath
	app.ctx = context.Background()

	view, err := app.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}
	if view.SecurityPresets["strict"].RateLimitPerMinute != 10 {
		t.Fatalf("strict preset = %+v, want the Go preset's values", view.SecurityPresets["strict"])
	}
	security := view.Security
	security.DryRun = true
	if _, err := app.SaveFullSettings("", SaveFullSettingsRequest{
		ExpectedRevision: view.Revision,
		Security:         &security,
	}); err != nil {
		t.Fatalf("SaveFullSettings: %v", err)
	}

	after, err := config.LoadExisting(configPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	want := before.Policies
	want.DryRun = true
	if !reflect.DeepEqual(after.Policies, want) {
		t.Fatalf("a dry-run save changed the policy on disk:\n got  %+v\n want %+v", after.Policies, want)
	}
}
