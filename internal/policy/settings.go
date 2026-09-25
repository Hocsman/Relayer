package policy

import (
	"fmt"
	"path/filepath"
	"reflect"
	"strings"
)

// Settings is the part of a Config the settings editors show and change. The
// web gateway and the Desktop GUI both convert to and from it, so the two
// cannot drift into different ideas of what saving a form does.
//
// Everything a Config holds that is not listed here — the rules and the
// guardrail blocked_patterns — is carried through a save unchanged.
type Settings struct {
	Profile                     string
	DefaultAction               string
	DryRun                      bool
	BlockDestructive            bool
	BlockExfiltration           bool
	BlockSensitivePaths         bool
	BlockOutsideWorkspace       bool
	WorkspaceRoot               string
	RateLimitPerMinute          int
	MaxConsecutiveAutoDecisions int
}

// presetProfiles are the profiles a settings editor offers, in display order.
var presetProfiles = []Profile{ProfileStrict, ProfileDeveloperFriendly, ProfilePermissive}

// SettingsFrom describes cfg for an editor.
//
// Profile names a preset only when cfg is exactly that preset, apart from the
// dry-run toggle, the workspace path and any blocked patterns, none of which a
// preset sets. Anything else is "custom". Earlier releases guessed: any "ask"
// configuration without rules was called strict — the built-in default, with
// every guardrail off and no limits, included — and a save then rebuilt the
// configuration from the guessed preset.
func SettingsFrom(cfg Config) Settings {
	return Settings{
		Profile:                     string(matchingProfile(cfg)),
		DefaultAction:               string(cfg.DefaultAction),
		DryRun:                      cfg.DryRun,
		BlockDestructive:            cfg.Guardrails.BlockDestructive,
		BlockExfiltration:           cfg.Guardrails.BlockExfiltration,
		BlockSensitivePaths:         cfg.Guardrails.BlockSensitivePaths,
		BlockOutsideWorkspace:       cfg.Guardrails.BlockOutsideWorkspace,
		WorkspaceRoot:               cfg.Guardrails.WorkspaceRoot,
		RateLimitPerMinute:          cfg.RateLimitPerMinute,
		MaxConsecutiveAutoDecisions: cfg.MaxConsecutiveAutoDecisions,
	}
}

// PresetSettings returns what an editor should fill in when the user picks each
// preset. They come from ProfileConfig, so the form and the loader agree on
// what "strict" means. WorkspaceRoot is left empty: picking a preset does not
// move the workspace.
func PresetSettings() map[string]Settings {
	presets := make(map[string]Settings, len(presetProfiles))
	for _, profile := range presetProfiles {
		// A non-empty root makes ProfileConfig report BlockOutsideWorkspace the
		// way it would for a configuration that has one.
		settings := SettingsFrom(ProfileConfig(profile, "workspace"))
		settings.Profile = string(profile)
		settings.WorkspaceRoot = ""
		presets[string(profile)] = settings
	}
	return presets
}

// ApplySettings returns existing with an editor's settings applied.
//
// It always starts from existing, so rules and blocked patterns survive a save
// that did not touch them; v0.8.5 started from a preset and dropped both. The
// preset's rules replace the existing ones only when the editor explicitly
// switched to a different preset. Blocked patterns are kept even then: they only
// ever block more.
//
// The limits are taken as given, zero included, which means unlimited; v0.8.5
// ignored zero, so a limit could never be removed and saving the default
// configuration raised it to 10 and 1. A workspace root is filled in only when
// the outside-workspace guardrail needs one, and is always made absolute
// against configDir: a relative root never matched an absolute path.
func ApplySettings(existing Config, settings Settings, configDir string) (Config, error) {
	requested, err := ParseProfile(settings.Profile)
	if err != nil {
		return Config{}, err
	}
	if settings.RateLimitPerMinute < 0 {
		return Config{}, fmt.Errorf("rate limit per minute cannot be negative: %d", settings.RateLimitPerMinute)
	}
	if settings.MaxConsecutiveAutoDecisions < 0 {
		return Config{}, fmt.Errorf("max consecutive automatic decisions cannot be negative: %d", settings.MaxConsecutiveAutoDecisions)
	}

	result := cloneConfig(existing)
	if requested != ProfileCustom && requested != matchingProfile(existing) {
		result.Rules = cloneConfig(ProfileConfig(requested, "")).Rules
	}

	if action := strings.ToLower(strings.TrimSpace(settings.DefaultAction)); action != "" {
		switch Action(action) {
		case ActionAsk, ActionAllow, ActionDeny:
			result.DefaultAction = Action(action)
		default:
			return Config{}, fmt.Errorf("invalid default action %q", settings.DefaultAction)
		}
	}
	result.DryRun = settings.DryRun
	result.Guardrails.BlockDestructive = settings.BlockDestructive
	result.Guardrails.BlockExfiltration = settings.BlockExfiltration
	result.Guardrails.BlockSensitivePaths = settings.BlockSensitivePaths
	result.Guardrails.BlockOutsideWorkspace = settings.BlockOutsideWorkspace
	result.RateLimitPerMinute = settings.RateLimitPerMinute
	result.MaxConsecutiveAutoDecisions = settings.MaxConsecutiveAutoDecisions

	root, err := workspaceRoot(settings, existing, configDir)
	if err != nil {
		return Config{}, err
	}
	result.Guardrails.WorkspaceRoot = root
	return result, nil
}

// workspaceRoot decides the root a save writes. An empty field keeps the
// existing root when the guardrail needs one, and falls back to the
// configuration's directory only then; with the guardrail off, clearing the
// field clears the root.
//
// The fallback is the directory itself, ".", resolved like any relative root.
// It was configDir, which the gateway passes as given on its command line: a
// relative "cfg" was then resolved against itself, as "<cwd>/cfg/cfg".
func workspaceRoot(settings Settings, existing Config, configDir string) (string, error) {
	root := strings.TrimSpace(settings.WorkspaceRoot)
	if root == "" && settings.BlockOutsideWorkspace {
		root = strings.TrimSpace(existing.Guardrails.WorkspaceRoot)
		if root == "" {
			root = "."
		}
	}
	if root == "" {
		return "", nil
	}
	if !isRootedPath(root) {
		base, err := filepath.Abs(configDir)
		if err != nil {
			return "", fmt.Errorf("resolve the configuration directory: %w", err)
		}
		root = filepath.Join(base, root)
	}
	return filepath.Clean(root), nil
}

// matchingProfile is the preset cfg is exactly, or ProfileCustom.
func matchingProfile(cfg Config) Profile {
	for _, profile := range presetProfiles {
		preset := ProfileConfig(profile, cfg.Guardrails.WorkspaceRoot)
		if cfg.DefaultAction != preset.DefaultAction ||
			cfg.RateLimitPerMinute != preset.RateLimitPerMinute ||
			cfg.MaxConsecutiveAutoDecisions != preset.MaxConsecutiveAutoDecisions ||
			cfg.Guardrails.BlockDestructive != preset.Guardrails.BlockDestructive ||
			cfg.Guardrails.BlockExfiltration != preset.Guardrails.BlockExfiltration ||
			cfg.Guardrails.BlockSensitivePaths != preset.Guardrails.BlockSensitivePaths ||
			cfg.Guardrails.BlockOutsideWorkspace != preset.Guardrails.BlockOutsideWorkspace {
			continue
		}
		if len(cfg.Rules) == 0 && len(preset.Rules) == 0 || reflect.DeepEqual(cfg.Rules, preset.Rules) {
			return profile
		}
	}
	return ProfileCustom
}
