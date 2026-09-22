package server

import (
	"context"
	"io"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/config"
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
