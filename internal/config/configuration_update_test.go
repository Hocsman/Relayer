package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/policy"
)

func TestUpdateFullConfiguration_PoliciesAndNotifications(t *testing.T) {
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
    command: ["echo", "hello"]

intercept_patterns:
  - pattern: '(?i)continue'
    description: continue prompt
`
	if err := os.WriteFile(configPath, []byte(initialYAML), 0o600); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	loaded, err := LoadExisting(configPath)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}

	// 1. Update policies only (Developer-friendly profile + Guardrails)
	devConfig := policy.ProfileConfig(policy.ProfileDeveloperFriendly, tempDir)
	devConfig.Guardrails.BlockDestructive = true
	devConfig.Guardrails.BlockSensitivePaths = true

	updated, newRev, err := UpdateFullConfiguration(configPath, loaded.Revision, FullConfigurationUpdate{
		Policies: &devConfig,
	})
	if err != nil {
		t.Fatalf("update policies: %v", err)
	}
	if newRev == loaded.Revision {
		t.Errorf("expected revision to change after policies update")
	}
	if !updated.Policies.Guardrails.BlockDestructive {
		t.Errorf("expected BlockDestructive to be true")
	}
	if !updated.Policies.Guardrails.BlockSensitivePaths {
		t.Errorf("expected BlockSensitivePaths to be true")
	}

	// 2. Update notifications only (Enable desktop + Slack webhook)
	notifications := notify.Config{
		Enabled:     true,
		Desktop:     true,
		Bell:        true,
		MinSeverity: notify.SeverityWarning,
		Webhooks: []notify.WebhookConfig{
			{
				Name:        "slack-alerts",
				URL:         "https://hooks.slack.com/services/test",
				Format:      string(notify.FormatSlack),
				MinSeverity: notify.SeverityCritical,
				Timeout:     "5s",
			},
		},
	}

	updated2, newRev2, err := UpdateFullConfiguration(configPath, newRev, FullConfigurationUpdate{
		Notifications: &notifications,
	})
	if err != nil {
		t.Fatalf("update notifications: %v", err)
	}
	if newRev2 == newRev {
		t.Errorf("expected revision to change after notifications update")
	}
	if !updated2.Notifications.Desktop {
		t.Errorf("expected Desktop notifications to be enabled")
	}
	if len(updated2.Notifications.Webhooks) != 1 || updated2.Notifications.Webhooks[0].Name != "slack-alerts" {
		t.Errorf("expected 1 webhook named slack-alerts")
	}
	// Verify policies were preserved
	if !updated2.Policies.Guardrails.BlockDestructive {
		t.Errorf("expected preserved BlockDestructive")
	}
	// Verify agents were preserved
	if len(updated2.Agents) != 1 || updated2.Agents[0].ID != "alpha" {
		t.Errorf("expected preserved agent alpha")
	}

	// 3. Stale revision detection
	_, _, err = UpdateFullConfiguration(configPath, "stale-revision", FullConfigurationUpdate{
		Notifications: &notifications,
	})
	if err == nil {
		t.Fatalf("expected ErrRevisionMismatch on stale revision")
	}
}

func TestUpdateFullConfiguration_AllSections(t *testing.T) {
	tempDir := t.TempDir()
	configPath := filepath.Join(tempDir, "config.yaml")

	initialYAML := `version: 1
backend: pty

agents:
  - id: worker
    name: Worker
    command: ["echo", "1"]

intercept_patterns:
  - pattern: '(?i)continue'
    description: continue prompt
`
	if err := os.WriteFile(configPath, []byte(initialYAML), 0o600); err != nil {
		t.Fatalf("write initial config: %v", err)
	}

	loaded, err := LoadExisting(configPath)
	if err != nil {
		t.Fatalf("load initial config: %v", err)
	}

	strictConfig := policy.ProfileConfig(policy.ProfileStrict, tempDir)
	notifConfig := notify.DefaultConfig()
	notifConfig.Enabled = true

	newAgents := []agent.Spec{
		{
			ID:      "supervisor-test",
			Name:    "Supervisor Test Agent",
			Command: []string{"echo", "test"},
			Adapter: "generic",
			Backend: "pty",
		},
	}

	updated, _, err := UpdateFullConfiguration(configPath, loaded.Revision, FullConfigurationUpdate{
		UpdateAgents:  true,
		Agents:        newAgents,
		Policies:      &strictConfig,
		Notifications: &notifConfig,
	})
	if err != nil {
		t.Fatalf("update all sections: %v", err)
	}

	if len(updated.Agents) != 1 || updated.Agents[0].ID != "supervisor-test" {
		t.Errorf("expected agent supervisor-test, got %v", updated.Agents)
	}
	if updated.Policies.DefaultAction != policy.ActionAsk {
		t.Errorf("expected ActionAsk, got %v", updated.Policies.DefaultAction)
	}
	if !updated.Notifications.Enabled {
		t.Errorf("expected notifications enabled")
	}
}
