package main

import (
	"errors"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/notify"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/supervise"
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
	// HasHeaders says the webhook carries headers, usually a credential. Their
	// values never leave the engine, and a save keeps them.
	HasHeaders bool `json:"hasHeaders"`
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
		return FullSettingsView{}, errors.New(supervise.SafeDisplayError(err))
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
	current, updated, token, specsChanged, err := a.writeFullSettingsLocked(request)
	if err != nil {
		return FullSettingsView{}, err
	}

	// The top bar is not updated here. It shows the running engine's policy,
	// and the engine is built once per run: showing the saved values made
	// DRY RUN appear active while automatic approvals kept being delivered.

	if request.Notifications != nil {
		a.setNotifier(newNotifier(updated.Notifications))
	}

	// Notifications are applied above; agents and policies only take effect in
	// a new run. The running engine is therefore still current only when the
	// save changed neither, and nothing was already waiting for a restart.
	// v0.8.5 advanced it whenever the agents were unchanged, so a policy save
	// was announced as "applied immediately" while nothing enforced it.
	policiesChanged := !reflect.DeepEqual(current.Policies, updated.Policies)
	if !specsChanged && !policiesChanged && a.activeConfigRevision != "" && a.activeConfigRevision == current.Revision {
		a.activeConfigRevision = updated.Revision
	}
	profilesView := a.agentProfilesViewLocked(updated, token)
	if specsChanged || policiesChanged {
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

// writeFullSettingsLocked validates and publishes one settings request in a
// single atomic write, and keeps the opaque revision token in step. It is the
// write both the plain save and the save-and-restart transaction use; the
// transaction calls it after capturing its snapshot, so a rollback restores
// the file as it was before the whole request, security and notifications
// included.
func (a *App) writeFullSettingsLocked(request SaveFullSettingsRequest) (current, updated config.Result, token string, specsChanged bool, err error) {
	path, err := a.profileConfigPathLocked()
	if err != nil {
		return config.Result{}, config.Result{}, "", false, errProfilesSave
	}
	current, err = config.LoadExisting(path)
	if err != nil {
		return config.Result{}, config.Result{}, "", false, errProfilesSave
	}
	if current.Legacy {
		return config.Result{}, config.Result{}, "", false, errProfilesInvalid
	}
	if request.ExpectedRevision == "" || request.ExpectedRevision != a.profileRevisionToken ||
		a.profileRevisionHash == "" || current.Revision != a.profileRevisionHash {
		return config.Result{}, config.Result{}, "", false, errProfilesStale
	}

	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return config.Result{}, config.Result{}, "", false, errProfilesInvalid
	}

	update := config.FullConfigurationUpdate{}

	// 1. Process profiles if provided
	if len(request.Profiles) > 0 {
		if len(request.Profiles) < minimumAgentProfiles || len(request.Profiles) > maximumAgentProfiles {
			return config.Result{}, config.Result{}, "", false, errProfilesInvalid
		}
		specs, err := resolveProfileInputs(request.Profiles, current, baseDir)
		if err != nil {
			return config.Result{}, config.Result{}, "", false, errProfilesInvalid
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
			return config.Result{}, config.Result{}, "", false, err
		}
		update.Policies = &policyCfg
	}

	// 3. Process notification settings if provided
	if request.Notifications != nil {
		notifCfg := buildNotificationConfig(*request.Notifications)
		// The editor never receives header values, so it cannot send them
		// back; without this every save erased every webhook's credential,
		// and the rebuilt notifier sent the next alert unauthenticated.
		notifCfg.Webhooks = notify.MergeWebhookHeaders(current.Notifications.Webhooks, notifCfg.Webhooks)
		update.Notifications = &notifCfg
	}

	token, err = a.profileTokenGenerator()
	if err != nil {
		return config.Result{}, config.Result{}, "", false, errProfilesSave
	}

	updated, revision, err := config.UpdateFullConfiguration(path, current.Revision, update)
	if err != nil {
		if errors.Is(err, config.ErrRevisionMismatch) {
			return config.Result{}, config.Result{}, "", false, errProfilesStale
		}
		if reloaded, reloadErr := config.LoadExisting(path); reloadErr == nil && reloaded.Revision != current.Revision {
			a.profileRevisionHash = reloaded.Revision
			a.profileRevisionToken = token
		}
		return config.Result{}, config.Result{}, "", false, errProfilesSave
	}
	if revision == current.Revision {
		// Nothing was written: the revision, and the token the editor holds for
		// it, stay good. A new token here made any editor loaded before this
		// save stale although the file had not changed.
		return current, updated, a.profileRevisionToken, specsChanged, nil
	}

	a.profileRevisionHash = revision
	a.profileRevisionToken = token
	return current, updated, token, specsChanged, nil
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
			HasHeaders:  len(w.Headers) > 0,
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
