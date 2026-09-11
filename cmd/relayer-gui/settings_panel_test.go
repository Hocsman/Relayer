package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"
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

	// Profiles were untouched, so restart should not be required
	if saved.RestartRequired {
		t.Errorf("expected RestartRequired false when profiles are unchanged")
	}
}
