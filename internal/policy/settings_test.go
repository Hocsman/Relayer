package policy

import (
	"path/filepath"
	"reflect"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
)

// saveWith applies the editor's view of cfg back to cfg after one change, the
// way both front ends save a form.
func saveWith(t *testing.T, cfg Config, configDir string, change func(*Settings)) Config {
	t.Helper()
	settings := SettingsFrom(cfg)
	change(&settings)
	saved, err := ApplySettings(cfg, settings, configDir)
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	return saved
}

func toggleDryRun(settings *Settings) { settings.DryRun = !settings.DryRun }

// TestSavingOnlyDryRunChangesOnlyDryRun is the v0.8.5 regression: the web save
// guessed a profile and rebuilt the configuration from it, so a save that only
// toggled dry-run dropped deny rules and blocked patterns and rewrote limits.
func TestSavingOnlyDryRunChangesOnlyDryRun(t *testing.T) {
	configDir := t.TempDir()
	root := filepath.Join(configDir, "work")

	strictWithPatterns := ProfileConfig(ProfileStrict, root)
	strictWithPatterns.Guardrails.BlockedPatterns = []string{`(?i)terraform\s+destroy`}

	developerWithDeny := ProfileConfig(ProfileDeveloperFriendly, root)
	developerWithDeny.Rules = append(developerWithDeny.Rules, Rule{
		Name:   "deny-terraform",
		Match:  Match{CommandRegex: `(?i)^terraform\b`},
		Action: ActionDeny,
	})

	for name, cfg := range map[string]Config{
		"built-in default":             DefaultConfig(),
		"strict with blocked patterns": strictWithPatterns,
		"developer rules and a deny":   developerWithDeny,
	} {
		t.Run(name, func(t *testing.T) {
			saved := saveWith(t, cfg, configDir, toggleDryRun)
			want := cloneConfig(cfg)
			want.DryRun = !cfg.DryRun
			if !reflect.DeepEqual(saved, want) {
				t.Fatalf("a dry-run save changed more than dry-run:\n got  %+v\n want %+v", saved, want)
			}
		})
	}
}

func TestDefaultConfigIsNotCalledStrict(t *testing.T) {
	// The built-in default asks for everything but has every guardrail off and
	// no limits. v0.8.5 labelled it strict, and a save then applied strict.
	if got := SettingsFrom(DefaultConfig()).Profile; got != string(ProfileCustom) {
		t.Fatalf("default configuration profile = %q, want %q", got, ProfileCustom)
	}
	for _, profile := range presetProfiles {
		if got := SettingsFrom(ProfileConfig(profile, "/work")).Profile; got != string(profile) {
			t.Errorf("ProfileConfig(%s) is described as %q", profile, got)
		}
	}
}

func TestLimitsCanBeSetToZero(t *testing.T) {
	saved := saveWith(t, ProfileConfig(ProfileStrict, "/work"), t.TempDir(), func(settings *Settings) {
		settings.RateLimitPerMinute = 0
		settings.MaxConsecutiveAutoDecisions = 0
	})
	if saved.RateLimitPerMinute != 0 || saved.MaxConsecutiveAutoDecisions != 0 {
		t.Fatalf("limits = %d/%d, want 0/0: a limit could never be removed", saved.RateLimitPerMinute, saved.MaxConsecutiveAutoDecisions)
	}

	settings := SettingsFrom(DefaultConfig())
	settings.RateLimitPerMinute = -1
	if _, err := ApplySettings(DefaultConfig(), settings, t.TempDir()); err == nil {
		t.Fatal("a negative rate limit was accepted")
	}
}

func TestWorkspaceRootIsAbsoluteAndOnlySetWhenNeeded(t *testing.T) {
	configDir := t.TempDir()

	// Guardrail off and no root: a save must not invent one.
	saved := saveWith(t, DefaultConfig(), configDir, toggleDryRun)
	if saved.Guardrails.WorkspaceRoot != "" {
		t.Fatalf("a save with the guardrail off wrote workspace_root %q", saved.Guardrails.WorkspaceRoot)
	}

	// Guardrail on and no root: the configuration's directory, absolute even
	// when the configuration was addressed relatively.
	saved = saveWith(t, DefaultConfig(), ".", func(settings *Settings) { settings.BlockOutsideWorkspace = true })
	absoluteDot, _ := filepath.Abs(".")
	if saved.Guardrails.WorkspaceRoot != absoluteDot {
		t.Fatalf("default workspace root = %q, want %q", saved.Guardrails.WorkspaceRoot, absoluteDot)
	}

	// A relative root resolves against the configuration's directory.
	saved = saveWith(t, DefaultConfig(), configDir, func(settings *Settings) {
		settings.BlockOutsideWorkspace = true
		settings.WorkspaceRoot = "project"
	})
	if want := filepath.Join(configDir, "project"); saved.Guardrails.WorkspaceRoot != want {
		t.Fatalf("relative workspace root = %q, want %q", saved.Guardrails.WorkspaceRoot, want)
	}
	if !IsPathInsideWorkspace(filepath.Join(configDir, "project", "main.go"), saved.Guardrails.WorkspaceRoot) {
		t.Fatal("a file inside the saved workspace is not inside it")
	}
}

// The default workspace root of a configuration addressed by a relative path,
// as `relayer serve --config cfg/config.yaml` addresses it, is that
// configuration's directory. The directory was taken as the root and then
// resolved against itself, so turning the guardrail on with the root left
// empty wrote "<cwd>/cfg/cfg", a directory that does not exist, and every
// path an agent named was outside the workspace.
func TestTheDefaultWorkspaceRootOfARelativelyAddressedConfigurationIsItsDirectory(t *testing.T) {
	saved := saveWith(t, DefaultConfig(), "cfg", func(settings *Settings) { settings.BlockOutsideWorkspace = true })
	want, err := filepath.Abs("cfg")
	if err != nil {
		t.Fatal(err)
	}
	if saved.Guardrails.WorkspaceRoot != want {
		t.Fatalf("default workspace root = %q, want %q", saved.Guardrails.WorkspaceRoot, want)
	}
}

func TestExplicitPresetSwitchReplacesRulesButKeepsBlockedPatterns(t *testing.T) {
	custom := DefaultConfig()
	custom.Rules = []Rule{{Name: "deny-all-risky", Match: Match{RiskLevels: []adapters.RiskLevel{adapters.RiskHigh}}, Action: ActionDeny}}
	custom.Guardrails.BlockedPatterns = []string{`(?i)drop\s+table`}

	preset := PresetSettings()[string(ProfileDeveloperFriendly)]
	saved, err := ApplySettings(custom, preset, t.TempDir())
	if err != nil {
		t.Fatalf("ApplySettings: %v", err)
	}
	if !reflect.DeepEqual(saved.Rules, ProfileConfig(ProfileDeveloperFriendly, "").Rules) {
		t.Fatalf("rules after choosing developer-friendly = %+v, want the preset's rules", saved.Rules)
	}
	if !reflect.DeepEqual(saved.Guardrails.BlockedPatterns, custom.Guardrails.BlockedPatterns) {
		t.Fatalf("blocked patterns = %v, want them kept: they only ever block more", saved.Guardrails.BlockedPatterns)
	}
	if got := SettingsFrom(saved).Profile; got != string(ProfileDeveloperFriendly) {
		t.Fatalf("profile after choosing developer-friendly = %q", got)
	}

	// The source preset must not be reachable through the result.
	saved.Rules[0].Name = "edited"
	if ProfileConfig(ProfileDeveloperFriendly, "").Rules[0].Name == "edited" {
		t.Fatal("editing a saved configuration edited the preset")
	}
}

func TestPresetSettingsComeFromProfileConfig(t *testing.T) {
	presets := PresetSettings()
	strict := presets[string(ProfileStrict)]
	if strict.RateLimitPerMinute != 10 || strict.MaxConsecutiveAutoDecisions != 1 || !strict.BlockOutsideWorkspace {
		t.Fatalf("strict preset = %+v, want ProfileConfig's 10/1 with the workspace guardrail", strict)
	}
	permissive := presets[string(ProfilePermissive)]
	if permissive.DryRun || !permissive.BlockDestructive || !permissive.BlockExfiltration || !permissive.BlockSensitivePaths {
		t.Fatalf("permissive preset = %+v, want guardrails on and no dry run, as ProfileConfig defines it", permissive)
	}
	for name, preset := range presets {
		if preset.WorkspaceRoot != "" {
			t.Errorf("preset %s moves the workspace to %q", name, preset.WorkspaceRoot)
		}
	}
}
