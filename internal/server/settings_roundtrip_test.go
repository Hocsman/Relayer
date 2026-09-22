package server

import (
	"context"
	"encoding/json"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/policy"
)

// startSettingsController writes cfg as the configuration's policies and boots a
// gateway controller on it.
func startSettingsController(t *testing.T, policies policy.Config) (*Controller, string) {
	t.Helper()

	configPath := filepath.Join(t.TempDir(), "config.yaml")
	created, err := config.LoadOrCreate(configPath)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	if _, _, err := config.UpdateFullConfiguration(configPath, created.Revision, config.FullConfigurationUpdate{Policies: &policies}); err != nil {
		t.Fatalf("write policies: %v", err)
	}

	ctrl, err := NewController(configPath, io.Discard)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := ctrl.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = ctrl.Close(shutdownCtx)
		cancel()
	})
	return ctrl, configPath
}

// TestWebSecuritySaveKeepsWhatTheFormDoesNotShow drives the web gateway's real
// save path the way the settings tab does: read the view, change one toggle,
// send the view back. v0.8.5 guessed a profile from the file and rebuilt the
// policy from that preset, so this dropped the deny rule and the blocked
// pattern and, for the default configuration, raised limits of 0 and 0 to 10 and 1.
func TestWebSecuritySaveKeepsWhatTheFormDoesNotShow(t *testing.T) {
	strict := policy.ProfileConfig(policy.ProfileStrict, "")
	strict.Guardrails.BlockOutsideWorkspace = false
	strict.Guardrails.BlockedPatterns = []string{`(?i)terraform\s+destroy`}
	strict.Rules = []policy.Rule{{
		Name:   "deny-terraform",
		Match:  policy.Match{CommandRegex: `(?i)^terraform\b`},
		Action: policy.ActionDeny,
	}}

	for name, policies := range map[string]policy.Config{
		"built-in default":            policy.DefaultConfig(),
		"limits, patterns and a deny": strict,
	} {
		t.Run(name, func(t *testing.T) {
			ctrl, configPath := startSettingsController(t, policies)
			before, err := config.LoadExisting(configPath)
			if err != nil {
				t.Fatalf("LoadExisting: %v", err)
			}

			view, err := ctrl.GetFullSettings()
			if err != nil {
				t.Fatalf("GetFullSettings: %v", err)
			}
			security := view.Security
			security.DryRun = !security.DryRun
			if _, err := ctrl.SaveFullSettings("", SaveFullSettingsRequest{
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
			want.DryRun = !want.DryRun
			if !reflect.DeepEqual(after.Policies, want) {
				t.Fatalf("a dry-run save changed the policy on disk:\n got  %+v\n want %+v", after.Policies, want)
			}
		})
	}
}

func TestWebSecurityViewCarriesTheGoPresets(t *testing.T) {
	ctrl, _ := startSettingsController(t, policy.DefaultConfig())
	view, err := ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}
	if view.Security.Profile != string(policy.ProfileCustom) {
		t.Errorf("default configuration shown as %q, want %q", view.Security.Profile, policy.ProfileCustom)
	}
	strict, ok := view.SecurityPresets[string(policy.ProfileStrict)]
	if !ok {
		t.Fatal("the view carries no strict preset")
	}
	if strict.RateLimitPerMinute != 10 || strict.MaxConsecutiveAutoDecisions != 1 {
		t.Fatalf("strict preset = %d/%d, want ProfileConfig's 10/1", strict.RateLimitPerMinute, strict.MaxConsecutiveAutoDecisions)
	}
}

// TestWebRestartRequiredFollowsWhatTheRunningEngineHas: a notification-only
// save is applied at once and owes no restart; a policy save does. v0.8.5
// asked for a restart after every save, notifications included.
func TestWebRestartRequiredFollowsWhatTheRunningEngineHas(t *testing.T) {
	ctrl, _ := startSettingsController(t, policy.DefaultConfig())
	view, err := ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}
	if view.RestartRequired {
		t.Fatal("a freshly started run already reports a restart as required")
	}

	notifications := view.Notifications
	notifications.Bell = !notifications.Bell
	saved, err := ctrl.SaveFullSettings("", SaveFullSettingsRequest{
		ExpectedRevision: view.Revision,
		Notifications:    &notifications,
	})
	if err != nil {
		t.Fatalf("SaveFullSettings notifications: %v", err)
	}
	if saved.RestartRequired {
		t.Fatal("a notification-only save asked for a restart, though it is applied at once")
	}

	security := saved.Security
	security.DryRun = true
	saved, err = ctrl.SaveFullSettings("", SaveFullSettingsRequest{
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

// TestWebNotificationSaveKeepsWebhookHeaders: the editor never receives header
// values, so every save used to write every webhook back without them.
func TestWebNotificationSaveKeepsWebhookHeaders(t *testing.T) {
	configPath := filepath.Join(t.TempDir(), "config.yaml")
	created, err := config.LoadOrCreate(configPath)
	if err != nil {
		t.Fatalf("LoadOrCreate: %v", err)
	}
	notifications := created.Notifications
	notifications.Enabled = true
	notifications.Webhooks = []notify.WebhookConfig{{
		Name:        "team",
		URL:         "https://hooks.example/team",
		Format:      "generic",
		MinSeverity: "warning",
		Timeout:     "5s",
		Headers:     map[string]string{"Authorization": "Bearer HEADER-PROBE-SECRET"},
	}}
	if _, _, err := config.UpdateFullConfiguration(configPath, created.Revision, config.FullConfigurationUpdate{Notifications: &notifications}); err != nil {
		t.Fatalf("write notifications: %v", err)
	}
	ctrl, err := NewController(configPath, io.Discard)
	if err != nil {
		t.Fatalf("NewController: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	if err := ctrl.Start(ctx); err != nil {
		cancel()
		t.Fatalf("Start: %v", err)
	}
	t.Cleanup(func() {
		shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer shutdownCancel()
		_ = ctrl.Close(shutdownCtx)
		cancel()
	})

	view, err := ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}
	if len(view.Notifications.Webhooks) != 1 || !view.Notifications.Webhooks[0].HasHeaders {
		t.Fatalf("webhooks = %+v, want one marked as having headers", view.Notifications.Webhooks)
	}
	if encoded, _ := json.Marshal(view); strings.Contains(string(encoded), "HEADER-PROBE-SECRET") {
		t.Fatal("the settings view carries a webhook header value")
	}

	edited := view.Notifications
	edited.Bell = !edited.Bell
	if _, err := ctrl.SaveFullSettings("", SaveFullSettingsRequest{ExpectedRevision: view.Revision, Notifications: &edited}); err != nil {
		t.Fatalf("SaveFullSettings: %v", err)
	}
	after, err := config.LoadExisting(configPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(after.Notifications.Webhooks) != 1 || after.Notifications.Webhooks[0].Headers["Authorization"] != "Bearer HEADER-PROBE-SECRET" {
		t.Fatalf("webhooks after a bell toggle = %+v, want the Authorization header kept", after.Notifications.Webhooks)
	}
}

func TestWebNotificationTestRefusesWhenNotificationsAreOff(t *testing.T) {
	ctrl, _ := startSettingsController(t, policy.DefaultConfig())
	view, err := ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}
	off := view.Notifications
	off.Enabled = false
	if _, err := ctrl.SaveFullSettings("", SaveFullSettingsRequest{ExpectedRevision: view.Revision, Notifications: &off}); err != nil {
		t.Fatalf("SaveFullSettings: %v", err)
	}
	if err := ctrl.TestNotification(); err == nil {
		t.Fatal("a test notification reported success with notifications switched off")
	}
}

// TestWebSaveRefusesAFormOlderThanTheFile: the save checked only an opaque
// token that changes when this gateway writes, so a form loaded before someone
// edited the YAML by hand was saved over the edit. A deny rule added by hand
// was dropped by a save that only toggled dry-run.
func TestWebSaveRefusesAFormOlderThanTheFile(t *testing.T) {
	strict := policy.ProfileConfig(policy.ProfileStrict, "")
	strict.Guardrails.BlockOutsideWorkspace = false
	ctrl, configPath := startSettingsController(t, strict)

	view, err := ctrl.GetFullSettings()
	if err != nil {
		t.Fatalf("GetFullSettings: %v", err)
	}

	// Someone edits the file by hand while the form is open.
	onDisk, err := config.LoadExisting(configPath)
	if err != nil {
		t.Fatalf("LoadExisting: %v", err)
	}
	edited := onDisk.Policies
	edited.Rules = append(edited.Rules, policy.Rule{
		Name:   "deny-terraform",
		Match:  policy.Match{CommandRegex: `(?i)^terraform\b`},
		Action: policy.ActionDeny,
	})
	if _, _, err := config.UpdateFullConfiguration(configPath, onDisk.Revision, config.FullConfigurationUpdate{Policies: &edited}); err != nil {
		t.Fatalf("edit by hand: %v", err)
	}

	security := view.Security
	security.DryRun = !security.DryRun
	if _, err := ctrl.SaveFullSettings("", SaveFullSettingsRequest{ExpectedRevision: view.Revision, Security: &security}); err == nil {
		t.Fatal("a form loaded before the file was edited was saved over the edit")
	}
	after, err := config.LoadExisting(configPath)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if len(after.Policies.Rules) != 1 || after.Policies.Rules[0].Name != "deny-terraform" {
		t.Fatalf("rules after the refused save = %+v, want the hand-added deny rule", after.Policies.Rules)
	}

	// An empty token is not a way around the check.
	if _, err := ctrl.SaveFullSettings("", SaveFullSettingsRequest{Security: &security}); err == nil {
		t.Fatal("a save with no revision token was accepted")
	}
}
