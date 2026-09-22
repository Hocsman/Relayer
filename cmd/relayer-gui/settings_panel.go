package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/policy"
)

type SecuritySettings struct {
	Profile                     string `json:"profile"`
	DefaultAction               string `json:"defaultAction"`
	DryRun                      bool   `json:"dryRun"`
	BlockDestructive            bool   `json:"blockDestructive"`
	BlockExfiltration           bool   `json:"blockExfiltration"`
	BlockSensitivePaths         bool   `json:"blockSensitivePaths"`
	BlockOutsideWorkspace       bool   `json:"blockOutsideWorkspace"`
	WorkspaceRoot               string `json:"workspaceRoot"`
	RateLimitPerMinute          int    `json:"rateLimitPerMinute"`
	MaxConsecutiveAutoDecisions int    `json:"maxConsecutiveAutoDecisions"`
}

type NotificationWebhookSetting struct {
	Name        string `json:"name"`
	URL         string `json:"url"`
	Format      string `json:"format"`
	MinSeverity string `json:"minSeverity"`
	Timeout     string `json:"timeout"`
}

type NotificationSettings struct {
	Enabled     bool                         `json:"enabled"`
	Bell        bool                         `json:"bell"`
	Desktop     bool                         `json:"desktop"`
	MinSeverity string                       `json:"minSeverity"`
	Webhooks    []NotificationWebhookSetting `json:"webhooks"`
}

type FullSettingsView struct {
	ConfigPath string              `json:"configPath"`
	Revision   string              `json:"revision"`
	Catalog    []AgentCatalogEntry `json:"catalog"`
	Profiles   []AgentProfile      `json:"profiles"`
	Security   SecuritySettings    `json:"security"`
	// SecurityPresets are the values each preset fills in, from the same
	// source the configuration loader uses.
	SecurityPresets map[string]SecuritySettings `json:"securityPresets"`
	Notifications   NotificationSettings        `json:"notifications"`
	MinProfiles     int                         `json:"minProfiles"`
	MaxProfiles     int                         `json:"maxProfiles"`
	RestartRequired bool                        `json:"restartRequired"`
	Editable        bool                        `json:"editable"`
	ReadOnlyReason  string                      `json:"readOnlyReason,omitempty"`
}

type SaveFullSettingsRequest struct {
	ExpectedRevision string                `json:"expectedRevision"`
	Profiles         []AgentProfileInput   `json:"profiles,omitempty"`
	Security         *SecuritySettings     `json:"security,omitempty"`
	Notifications    *NotificationSettings `json:"notifications,omitempty"`
}

func (a *App) GetFullSettings() (FullSettingsView, error) {
	a.profilesMu.Lock()
	defer a.profilesMu.Unlock()
	return a.loadFullSettingsLocked()
}

func (a *App) SaveFullSettings(runID string, request SaveFullSettingsRequest) (FullSettingsView, error) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.finalShutdown {
		return FullSettingsView{}, errRuntimeStopped
	}
	a.mu.RLock()
	active := a.active
	a.mu.RUnlock()
	if active == nil {
		if strings.TrimSpace(runID) != "" {
			return FullSettingsView{}, errRunStale
		}
	} else if runID != active.id {
		return FullSettingsView{}, errRunStale
	}

	a.profilesMu.Lock()
	defer a.profilesMu.Unlock()
	return a.saveFullSettingsLocked(request)
}

func (a *App) loadFullSettingsLocked() (FullSettingsView, error) {
	profilesView, err := a.loadAgentProfilesLocked()
	if err != nil {
		return FullSettingsView{}, err
	}
	path, err := a.profileConfigPathLocked()
	if err != nil {
		return FullSettingsView{}, errProfilesSave
	}
	loaded, err := config.LoadExisting(path)
	if err != nil {
		return FullSettingsView{}, errors.New(safeDisplayError(err))
	}

	sec := extractSecuritySettings(loaded.Policies)
	notif := extractNotificationSettings(loaded.Notifications)

	return FullSettingsView{
		ConfigPath:      profilesView.ConfigPath,
		Revision:        profilesView.Revision,
		Catalog:         profilesView.Catalog,
		Profiles:        profilesView.Profiles,
		Security:        sec,
		SecurityPresets: securityPresetViews(),
		Notifications:   notif,
		MinProfiles:     profilesView.MinProfiles,
		MaxProfiles:     profilesView.MaxProfiles,
		RestartRequired: profilesView.RestartRequired,
		Editable:        profilesView.Editable,
		ReadOnlyReason:  profilesView.ReadOnlyReason,
	}, nil
}

func (a *App) saveFullSettingsLocked(request SaveFullSettingsRequest) (FullSettingsView, error) {
	path, err := a.profileConfigPathLocked()
	if err != nil {
		return FullSettingsView{}, errProfilesSave
	}
	current, err := config.LoadExisting(path)
	if err != nil {
		return FullSettingsView{}, errProfilesSave
	}
	if current.Legacy {
		return FullSettingsView{}, errProfilesInvalid
	}
	if request.ExpectedRevision == "" || request.ExpectedRevision != a.profileRevisionToken ||
		a.profileRevisionHash == "" || current.Revision != a.profileRevisionHash {
		return FullSettingsView{}, errProfilesStale
	}

	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return FullSettingsView{}, errProfilesInvalid
	}

	update := config.FullConfigurationUpdate{}

	// 1. Process profiles if provided
	var specsChanged bool
	if len(request.Profiles) > 0 {
		if len(request.Profiles) < minimumAgentProfiles || len(request.Profiles) > maximumAgentProfiles {
			return FullSettingsView{}, errProfilesInvalid
		}
		specs, err := resolveProfileInputs(request.Profiles, current, baseDir)
		if err != nil {
			return FullSettingsView{}, errProfilesInvalid
		}
		if !reflect.DeepEqual(specs, current.Agents) {
			update.Agents = specs
			update.UpdateAgents = true
			specsChanged = true
		}
	}

	// 2. Process security settings if provided
	if request.Security != nil {
		policyCfg, err := buildPolicyConfig(*request.Security, current.Policies, baseDir)
		if err != nil {
			return FullSettingsView{}, err
		}
		update.Policies = &policyCfg
	}

	// 3. Process notification settings if provided
	if request.Notifications != nil {
		notifCfg := buildNotificationConfig(*request.Notifications)
		update.Notifications = &notifCfg
	}

	token, err := a.profileTokenGenerator()
	if err != nil {
		return FullSettingsView{}, errProfilesSave
	}

	updated, revision, err := config.UpdateFullConfiguration(path, current.Revision, update)
	if err != nil {
		if errors.Is(err, config.ErrRevisionMismatch) {
			return FullSettingsView{}, errProfilesStale
		}
		if reloaded, reloadErr := config.LoadExisting(path); reloadErr == nil && reloaded.Revision != current.Revision {
			a.profileRevisionHash = reloaded.Revision
			a.profileRevisionToken = token
		}
		return FullSettingsView{}, errProfilesSave
	}

	a.profileRevisionHash = revision
	a.profileRevisionToken = token

	// Update GUI runtime topbar state if active
	if update.Policies != nil {
		a.mu.Lock()
		a.state.Policy.DefaultAction = string(update.Policies.DefaultAction)
		a.state.Policy.DryRun = update.Policies.DryRun
		a.mu.Unlock()
	}

	if update.Notifications != nil {
		a.notifier = notify.New(updated.Notifications, nil)
	}

	if !specsChanged && a.activeConfigRevision != "" {
		a.activeConfigRevision = updated.Revision
	}
	profilesView := a.agentProfilesViewLocked(updated, token)
	if specsChanged {
		profilesView.RestartRequired = true
	}

	return FullSettingsView{
		ConfigPath:      profilesView.ConfigPath,
		Revision:        profilesView.Revision,
		Catalog:         profilesView.Catalog,
		Profiles:        profilesView.Profiles,
		Security:        extractSecuritySettings(updated.Policies),
		SecurityPresets: securityPresetViews(),
		Notifications:   extractNotificationSettings(updated.Notifications),
		MinProfiles:     profilesView.MinProfiles,
		MaxProfiles:     profilesView.MaxProfiles,
		RestartRequired: profilesView.RestartRequired,
		Editable:        profilesView.Editable,
		ReadOnlyReason:  profilesView.ReadOnlyReason,
	}, nil
}

// extractSecuritySettings describes a policy for the settings editor. The
// conversion is a plain type conversion from policy.Settings, so the editor's
// fields cannot drift from the ones policy.ApplySettings understands.
func extractSecuritySettings(cfg policy.Config) SecuritySettings {
	return SecuritySettings(policy.SettingsFrom(cfg))
}

// securityPresetViews are the values the editor fills in when a preset is
// picked. They come from policy.ProfileConfig, like the loader's.
func securityPresetViews() map[string]SecuritySettings {
	presets := policy.PresetSettings()
	views := make(map[string]SecuritySettings, len(presets))
	for name, settings := range presets {
		views[name] = SecuritySettings(settings)
	}
	return views
}

func extractNotificationSettings(cfg notify.Config) NotificationSettings {
	hooks := make([]NotificationWebhookSetting, 0, len(cfg.Webhooks))
	for _, w := range cfg.Webhooks {
		hooks = append(hooks, NotificationWebhookSetting{
			Name:        w.Name,
			URL:         w.URL,
			Format:      w.Format,
			MinSeverity: w.MinSeverity,
			Timeout:     w.Timeout,
		})
	}
	return NotificationSettings{
		Enabled:     cfg.Enabled,
		Bell:        cfg.Bell,
		Desktop:     cfg.Desktop,
		MinSeverity: cfg.MinSeverity,
		Webhooks:    hooks,
	}
}

// buildPolicyConfig applies the editor's settings to the existing policy. It
// is policy.ApplySettings, shared with the other front end: rules and blocked
// patterns survive a save that did not touch them.
func buildPolicyConfig(sec SecuritySettings, existing policy.Config, baseDir string) (policy.Config, error) {
	return policy.ApplySettings(existing, policy.Settings(sec), baseDir)
}

func buildNotificationConfig(notif NotificationSettings) notify.Config {
	res := notify.Config{
		Enabled:     notif.Enabled,
		Bell:        notif.Bell,
		Desktop:     notif.Desktop,
		MinSeverity: notif.MinSeverity,
	}
	if res.MinSeverity == "" {
		res.MinSeverity = notify.SeverityInfo
	}
	for _, w := range notif.Webhooks {
		if strings.TrimSpace(w.URL) == "" {
			continue
		}
		format := strings.ToLower(strings.TrimSpace(w.Format))
		if format == "" {
			format = string(notify.FormatGeneric)
		}
		minSev := strings.ToLower(strings.TrimSpace(w.MinSeverity))
		if minSev == "" {
			minSev = notify.SeverityWarning
		}
		timeout := strings.TrimSpace(w.Timeout)
		if timeout == "" {
			timeout = "5s"
		}
		res.Webhooks = append(res.Webhooks, notify.WebhookConfig{
			Name:        strings.TrimSpace(w.Name),
			URL:         strings.TrimSpace(w.URL),
			Format:      format,
			MinSeverity: minSev,
			Timeout:     timeout,
		})
	}
	return res
}
