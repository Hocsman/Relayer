package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

const (
	minimumAgentProfiles = 1
	maximumAgentProfiles = 8
	maximumProfileArgs   = 64
	maximumProfileName   = 80
	maximumProfileValue  = 4096
)

var (
	profileIDPattern  = regexp.MustCompile(`^[a-z][a-z0-9_-]{0,63}$`)
	keyLikeArgPattern = regexp.MustCompile(`(?i)^(?:sk|pk|api)[-_][a-z0-9_-]{12,}$`)
)

var (
	errProfilesStale   = errors.New("La configuration a changé; rechargez les profils avant de réessayer.")
	errProfilesInvalid = errors.New("Un ou plusieurs profils d’agents sont invalides.")
	errProfilesSave    = errors.New("Impossible d’enregistrer les profils d’agents.")
)

type AgentCatalogEntry struct {
	ID                 string   `json:"id"`
	Name               string   `json:"name"`
	Description        string   `json:"description"`
	InstallStatus      string   `json:"installStatus"`
	Installed          bool     `json:"installed"`
	Adapter            string   `json:"adapter"`
	AdapterStatus      string   `json:"adapterStatus"`
	DefaultArgv        []string `json:"defaultArgv"`
	RequiresCustomArgv bool     `json:"requiresCustomArgv"`
}

// AgentProfile deliberately omits environment values and shell bodies. A
// locked profile is preserved by ID inside Go without exposing its argv.
type AgentProfile struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	PresetID        string   `json:"presetID"`
	Cwd             string   `json:"cwd"`
	Backend         string   `json:"backend"`
	Argv            []string `json:"argv,omitempty"`
	ExecutableLabel string   `json:"executableLabel"`
	ArgumentCount   int      `json:"argumentCount"`
	Locked          bool     `json:"locked"`
	ReadOnlyReason  string   `json:"readOnlyReason,omitempty"`
	PreserveOnSave  bool     `json:"preserveOnSave"`
}

type AgentProfilesView struct {
	ConfigPath      string              `json:"configPath"`
	Revision        string              `json:"revision"`
	Catalog         []AgentCatalogEntry `json:"catalog"`
	Profiles        []AgentProfile      `json:"profiles"`
	MinProfiles     int                 `json:"minProfiles"`
	MaxProfiles     int                 `json:"maxProfiles"`
	RestartRequired bool                `json:"restartRequired"`
	Editable        bool                `json:"editable"`
	ReadOnlyReason  string              `json:"readOnlyReason,omitempty"`
}

type AgentProfileInput struct {
	ID       string   `json:"id"`
	Name     string   `json:"name"`
	PresetID string   `json:"presetID"`
	Cwd      string   `json:"cwd"`
	Backend  string   `json:"backend"`
	Argv     []string `json:"argv"`
	Preserve bool     `json:"preserve"`
}

type SaveAgentProfilesRequest struct {
	ExpectedRevision string              `json:"expectedRevision"`
	Profiles         []AgentProfileInput `json:"profiles"`
}

func newOpaqueProfileToken() (string, error) {
	buffer := make([]byte, 24)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}

func (a *App) GetAgentProfiles() (AgentProfilesView, error) {
	a.profilesMu.Lock()
	defer a.profilesMu.Unlock()
	return a.loadAgentProfilesLocked()
}

func (a *App) SaveAgentProfiles(request SaveAgentProfilesRequest) (AgentProfilesView, error) {
	a.profilesMu.Lock()
	defer a.profilesMu.Unlock()

	path, err := a.profileConfigPathLocked()
	if err != nil {
		return AgentProfilesView{}, errProfilesSave
	}
	current, err := config.Load(path)
	if err != nil {
		return AgentProfilesView{}, errProfilesSave
	}
	if current.Legacy {
		return AgentProfilesView{}, errProfilesInvalid
	}
	if request.ExpectedRevision == "" || request.ExpectedRevision != a.profileRevisionToken ||
		a.profileRevisionHash == "" || current.Revision != a.profileRevisionHash {
		return AgentProfilesView{}, errProfilesStale
	}
	if len(request.Profiles) < minimumAgentProfiles || len(request.Profiles) > maximumAgentProfiles {
		return AgentProfilesView{}, errProfilesInvalid
	}

	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return AgentProfilesView{}, errProfilesInvalid
	}
	specs, err := resolveProfileInputs(request.Profiles, current, baseDir)
	if err != nil {
		return AgentProfilesView{}, errProfilesInvalid
	}
	if reflect.DeepEqual(specs, current.Agents) {
		return a.agentProfilesViewLocked(current, a.profileRevisionToken), nil
	}
	token, err := a.profileTokenGenerator()
	if err != nil {
		return AgentProfilesView{}, errProfilesSave
	}
	updated, revision, err := config.ReplaceAgents(path, current.Revision, specs)
	if err != nil {
		if errors.Is(err, config.ErrRevisionMismatch) {
			return AgentProfilesView{}, errProfilesStale
		}
		// Rename may have completed even when directory synchronization or the
		// post-commit read failed. Reconcile the opaque token before returning a
		// generic failure so a retry can never use stale authority.
		if reloaded, reloadErr := config.Load(path); reloadErr == nil && reloaded.Revision != current.Revision {
			a.profileRevisionHash = reloaded.Revision
			a.profileRevisionToken = token
		}
		return AgentProfilesView{}, errProfilesSave
	}
	a.profileRevisionHash = revision
	a.profileRevisionToken = token
	return a.agentProfilesViewLocked(updated, token), nil
}

func (a *App) loadAgentProfilesLocked() (AgentProfilesView, error) {
	path, err := a.profileConfigPathLocked()
	if err != nil {
		return AgentProfilesView{}, errProfilesSave
	}
	loaded, err := config.Load(path)
	if err != nil {
		return AgentProfilesView{}, errors.New(safeDisplayError(err))
	}
	if a.profileRevisionHash != loaded.Revision || a.profileRevisionToken == "" {
		token, tokenErr := a.profileTokenGenerator()
		if tokenErr != nil {
			return AgentProfilesView{}, errProfilesSave
		}
		a.profileRevisionHash = loaded.Revision
		a.profileRevisionToken = token
	}
	return a.agentProfilesViewLocked(loaded, a.profileRevisionToken), nil
}

func (a *App) profileConfigPathLocked() (string, error) {
	if strings.TrimSpace(a.configPath) != "" {
		return a.configPath, nil
	}
	path, err := desktopConfigPath()
	if err != nil {
		return "", err
	}
	a.configPath = path
	return path, nil
}

func (a *App) agentProfilesViewLocked(configuration config.Result, token string) AgentProfilesView {
	profiles := make([]AgentProfile, 0, len(configuration.Agents))
	for _, spec := range configuration.Agents {
		profiles = append(profiles, profileView(spec))
	}
	view := AgentProfilesView{
		ConfigPath:      a.configPath,
		Revision:        token,
		Catalog:         a.catalogViewLocked(),
		Profiles:        profiles,
		MinProfiles:     minimumAgentProfiles,
		MaxProfiles:     maximumAgentProfiles,
		RestartRequired: a.activeConfigRevision == "" || configuration.Revision != a.activeConfigRevision,
		Editable:        !configuration.Legacy,
	}
	if configuration.Legacy {
		view.ReadOnlyReason = "legacy_config"
	}
	return view
}

func (a *App) catalogViewLocked() []AgentCatalogEntry {
	descriptors := toolcatalog.Descriptors()
	result := make([]AgentCatalogEntry, 0, len(descriptors))
	detector := a.profileDetector
	if detector == nil {
		detector = toolcatalog.DefaultDetector()
	}
	ctx := a.ctx
	if ctx == nil || ctx.Err() != nil {
		ctx = context.Background()
	}
	ctx, cancel := context.WithTimeout(ctx, time.Second)
	defer cancel()
	for _, descriptor := range descriptors {
		detection, err := toolcatalog.Detect(ctx, descriptor.ID, "", detector)
		status := toolcatalog.InstallUnknown
		if err == nil {
			status = detection.Status
		}
		defaultArgv := []string{}
		if len(descriptor.Executables) > 0 {
			defaultArgv = []string{descriptor.Executables[0]}
		}
		result = append(result, AgentCatalogEntry{
			ID:                 string(descriptor.ID),
			Name:               descriptor.Name,
			Description:        profileDescription(descriptor.ID),
			InstallStatus:      string(status),
			Installed:          status == toolcatalog.InstallInstalled,
			Adapter:            descriptor.DefaultAdapter,
			AdapterStatus:      "stable",
			DefaultArgv:        defaultArgv,
			RequiresCustomArgv: descriptor.RequiresExecutable,
		})
	}
	return result
}

func profileDescription(id toolcatalog.ProfileID) string {
	switch id {
	case toolcatalog.ClaudeCode:
		return "Profil de lancement Claude Code; détection générique des demandes."
	case toolcatalog.CodexCLI:
		return "Profil de lancement Codex CLI; détection générique des demandes."
	case toolcatalog.MimoCode:
		return "Profil de lancement MiMo Code; détection générique des demandes."
	default:
		return "Toute CLI interactive locale avec un argv explicite."
	}
}

func profileView(spec agent.Spec) AgentProfile {
	profile := AgentProfile{
		ID:      spec.ID,
		Name:    spec.Name,
		Cwd:     spec.Cwd,
		Backend: spec.Backend,
	}
	reason := lockedProfileReason(spec)
	if reason != "" {
		profile.PresetID = string(toolcatalog.Custom)
		profile.Locked = true
		profile.PreserveOnSave = true
		profile.ReadOnlyReason = reason
		if spec.Shell != "" {
			profile.ExecutableLabel = "shell explicite"
		} else if len(spec.Command) > 0 {
			profile.ExecutableLabel = safeExecutableLabel(profileForExecutable(spec.Command[0]))
		}
		return profile
	}
	profile.PresetID = string(profileForExecutable(spec.Command[0]))
	// Existing argv may contain credentials that no heuristic can identify
	// reliably. Keep it authoritative in Go and require an explicit full
	// replacement before any command value crosses into the WebView.
	profile.PreserveOnSave = true
	profile.ExecutableLabel = safeExecutableLabel(toolcatalog.ProfileID(profile.PresetID))
	profile.ArgumentCount = len(spec.Command) - 1
	return profile
}

func safeExecutableLabel(profile toolcatalog.ProfileID) string {
	switch profile {
	case toolcatalog.ClaudeCode:
		return "claude"
	case toolcatalog.CodexCLI:
		return "codex"
	case toolcatalog.MimoCode:
		return "mimo"
	default:
		return "commande personnalisée"
	}
}

func profileForExecutable(executable string) toolcatalog.ProfileID {
	name := strings.ToLower(filepath.Base(strings.TrimSpace(executable)))
	for _, descriptor := range toolcatalog.Descriptors() {
		for _, candidate := range descriptor.Executables {
			if strings.EqualFold(name, filepath.Base(candidate)) {
				return descriptor.ID
			}
		}
	}
	return toolcatalog.Custom
}

func lockedProfileReason(spec agent.Spec) string {
	switch {
	case spec.Shell != "":
		return "advanced_shell"
	case len(spec.Env) > 0:
		return "advanced_environment"
	case strings.TrimSpace(spec.Adapter) != "" && !strings.EqualFold(spec.Adapter, agent.AdapterGeneric):
		return "advanced_adapter"
	case len(spec.Command) == 0:
		return "invalid_command"
	case !profileIDPattern.MatchString(spec.ID) ||
		utf8.RuneCountInString(spec.Name) > maximumProfileName ||
		utf8.RuneCountInString(spec.Cwd) > maximumProfileValue:
		return "legacy_profile_fields"
	default:
		return ""
	}
}

func resolveProfileInputs(inputs []AgentProfileInput, current config.Result, baseDir string) ([]agent.Spec, error) {
	currentByID := make(map[string]agent.Spec, len(current.Agents))
	lockedCurrent := make(map[string]struct{})
	for _, spec := range current.Agents {
		normalizedID := strings.ToLower(strings.TrimSpace(spec.ID))
		currentByID[normalizedID] = spec
		if lockedProfileReason(spec) != "" {
			lockedCurrent[normalizedID] = struct{}{}
		}
	}
	preservedLocked := make(map[string]struct{}, len(lockedCurrent))
	specs := make([]agent.Spec, 0, len(inputs))
	for _, input := range inputs {
		normalizedID := strings.ToLower(strings.TrimSpace(input.ID))
		existing, exists := currentByID[normalizedID]
		_, isLocked := lockedCurrent[normalizedID]
		if isLocked && !input.Preserve {
			return nil, errProfilesInvalid
		}
		if input.Preserve {
			if !exists || len(input.Argv) != 0 {
				return nil, errProfilesInvalid
			}
			if isLocked {
				specs = append(specs, existing)
				preservedLocked[normalizedID] = struct{}{}
				continue
			}
			if !validEditableProfileFields(input.ID, input.Name, input.Cwd) ||
				!agent.IsSupportedBackend(input.Backend) {
				return nil, errProfilesInvalid
			}
			preserved := existing
			preserved.Name = input.Name
			preserved.Cwd = input.Cwd
			preserved.Backend = input.Backend
			specs = append(specs, preserved)
			continue
		}
		if !validEditableProfileFields(input.ID, input.Name, input.Cwd) ||
			!agent.IsSupportedBackend(input.Backend) ||
			len(input.Argv) == 0 || len(input.Argv) > maximumProfileArgs ||
			argvContainsInvalidValue(input.Argv) || argvContainsSensitiveValue(input.Argv) {
			return nil, errProfilesInvalid
		}
		resolved, err := toolcatalog.Resolve(toolcatalog.LaunchRequest{
			ProfileID:  toolcatalog.ProfileID(input.PresetID),
			AgentID:    input.ID,
			Name:       input.Name,
			Executable: input.Argv[0],
			Args:       append([]string(nil), input.Argv[1:]...),
			Cwd:        input.Cwd,
			Adapter:    agent.AdapterGeneric,
			Backend:    input.Backend,
		})
		if err != nil {
			return nil, errProfilesInvalid
		}
		specs = append(specs, resolved)
	}
	if len(preservedLocked) != len(lockedCurrent) {
		return nil, errProfilesInvalid
	}
	_, err := agent.ValidateAll(specs, baseDir, current.Backend)
	if err != nil {
		return nil, errProfilesInvalid
	}
	return specs, nil
}

func validEditableProfileFields(id, name, cwd string) bool {
	return profileIDPattern.MatchString(strings.TrimSpace(id)) &&
		strings.TrimSpace(name) != "" &&
		utf8.RuneCountInString(name) <= maximumProfileName &&
		!strings.ContainsRune(name, '\x00') &&
		utf8.RuneCountInString(cwd) <= maximumProfileValue &&
		!strings.ContainsRune(cwd, '\x00')
}

func argvContainsInvalidValue(argv []string) bool {
	for index, argument := range argv {
		if utf8.RuneCountInString(argument) > maximumProfileValue || strings.ContainsRune(argument, '\x00') {
			return true
		}
		if index == 0 && strings.TrimSpace(argument) == "" {
			return true
		}
	}
	return false
}

func argvContainsSensitiveValue(argv []string) bool {
	markers := map[string]struct{}{
		"access-key": {}, "api-key": {}, "apikey": {}, "auth": {},
		"authentication": {}, "authorization": {}, "bearer": {},
		"client-secret": {}, "cookie": {}, "credential": {}, "key": {},
		"otp": {}, "passphrase": {}, "password": {}, "pin": {},
		"private-key": {}, "secret": {}, "session": {}, "token": {},
	}
	for index, argument := range argv {
		redacted := audit.Redact(argument)
		if strings.Contains(redacted, "[REDACTED]") ||
			strings.Contains(strings.ToUpper(redacted), "%5BREDACTED%5D") ||
			keyLikeArgPattern.MatchString(strings.TrimSpace(argument)) {
			return true
		}
		normalized := strings.ToLower(strings.TrimLeft(strings.TrimSpace(argument), "-"))
		name := normalized
		if separator := strings.IndexByte(name, '='); separator >= 0 {
			name = name[:separator]
		}
		name = strings.NewReplacer("_", "-", ".", "-").Replace(name)
		if _, sensitive := markers[name]; sensitive {
			if strings.Contains(normalized, "=") || index+1 < len(argv) {
				return true
			}
		}
	}
	return false
}
