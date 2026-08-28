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

	"github.com/Hocsman/Relayer/internal/adapters"
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
	MinimumArguments   int      `json:"minimumArguments"`
	ArgumentPrefix     []string `json:"argumentPrefix"`
}

// AgentProfile deliberately omits environment values and shell bodies. A
// locked profile is preserved by ID inside Go without exposing its argv.
type AgentProfile struct {
	ID              string   `json:"id"`
	Name            string   `json:"name"`
	PresetID        string   `json:"presetID"`
	Cwd             string   `json:"cwd"`
	Backend         string   `json:"backend"`
	Adapter         string   `json:"adapter"`
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
	Adapter  string   `json:"adapter"`
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

func (a *App) SaveAgentProfiles(runID string, request SaveAgentProfilesRequest) (AgentProfilesView, error) {
	a.lifecycleMu.Lock()
	defer a.lifecycleMu.Unlock()
	if a.finalShutdown {
		return AgentProfilesView{}, errRuntimeStopped
	}
	a.mu.RLock()
	active := a.active
	a.mu.RUnlock()
	if active == nil {
		if strings.TrimSpace(runID) != "" {
			return AgentProfilesView{}, errRunStale
		}
	} else if runID != active.id {
		return AgentProfilesView{}, errRunStale
	}
	a.profilesMu.Lock()
	defer a.profilesMu.Unlock()
	updated, token, err := a.saveAgentProfilesLocked(request)
	if err != nil {
		return AgentProfilesView{}, err
	}
	return a.agentProfilesViewLocked(updated, token), nil
}

func (a *App) saveAgentProfilesLocked(request SaveAgentProfilesRequest) (config.Result, string, error) {
	path, err := a.profileConfigPathLocked()
	if err != nil {
		return config.Result{}, "", errProfilesSave
	}
	current, err := config.Load(path)
	if err != nil {
		return config.Result{}, "", errProfilesSave
	}
	if current.Legacy {
		return config.Result{}, "", errProfilesInvalid
	}
	if request.ExpectedRevision == "" || request.ExpectedRevision != a.profileRevisionToken ||
		a.profileRevisionHash == "" || current.Revision != a.profileRevisionHash {
		return config.Result{}, "", errProfilesStale
	}
	if len(request.Profiles) < minimumAgentProfiles || len(request.Profiles) > maximumAgentProfiles {
		return config.Result{}, "", errProfilesInvalid
	}

	baseDir, err := filepath.Abs(filepath.Dir(path))
	if err != nil {
		return config.Result{}, "", errProfilesInvalid
	}
	specs, err := resolveProfileInputs(request.Profiles, current, baseDir)
	if err != nil {
		return config.Result{}, "", errProfilesInvalid
	}
	if reflect.DeepEqual(specs, current.Agents) {
		return current, a.profileRevisionToken, nil
	}
	token, err := a.profileTokenGenerator()
	if err != nil {
		return config.Result{}, "", errProfilesSave
	}
	updated, revision, err := config.ReplaceAgents(path, current.Revision, specs)
	if err != nil {
		if errors.Is(err, config.ErrRevisionMismatch) {
			return config.Result{}, "", errProfilesStale
		}
		// Rename may have completed even when directory synchronization or the
		// post-commit read failed. Reconcile the opaque token before returning a
		// generic failure so a retry can never use stale authority.
		if reloaded, reloadErr := config.Load(path); reloadErr == nil && reloaded.Revision != current.Revision {
			a.profileRevisionHash = reloaded.Revision
			a.profileRevisionToken = token
		}
		return config.Result{}, "", errProfilesSave
	}
	a.profileRevisionHash = revision
	a.profileRevisionToken = token
	return updated, token, nil
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
	adapterStatuses := make(map[string]string)
	if registry, err := adapters.NewRegistry(adapters.DefaultPatterns()); err == nil {
		for _, descriptor := range registry.Descriptors() {
			if descriptor.Implemented {
				adapterStatuses[descriptor.ID] = string(descriptor.Status)
			}
		}
	}
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
			defaultArgv = append(defaultArgv, descriptor.Executables[0])
			defaultArgv = append(defaultArgv, descriptor.ArgumentPrefix...)
			for len(defaultArgv)-1 < descriptor.MinimumArguments {
				// Required values such as an Ollama model remain deliberately
				// blank: the catalogue may guide the shape of argv but must never
				// invent a provider or model selection.
				defaultArgv = append(defaultArgv, "")
			}
		}
		adapterStatus := adapterStatuses[descriptor.DefaultAdapter]
		if adapterStatus == "" {
			// An unknown maturity must never be presented as stable.
			adapterStatus = string(adapters.StatusExperimental)
		}
		result = append(result, AgentCatalogEntry{
			ID:                 string(descriptor.ID),
			Name:               descriptor.Name,
			Description:        profileDescription(descriptor.ID),
			InstallStatus:      string(status),
			Installed:          status == toolcatalog.InstallInstalled,
			Adapter:            descriptor.DefaultAdapter,
			AdapterStatus:      adapterStatus,
			DefaultArgv:        defaultArgv,
			RequiresCustomArgv: descriptor.RequiresExecutable,
			MinimumArguments:   descriptor.MinimumArguments,
			ArgumentPrefix:     append([]string{}, descriptor.ArgumentPrefix...),
		})
	}
	return result
}

func profileDescription(id toolcatalog.ProfileID) string {
	switch id {
	case toolcatalog.ClaudeCode:
		return "Claude Code; règles expérimentales 2.1.59 vérifiées, puis fallback générique."
	case toolcatalog.CodexCLI:
		return "Codex CLI; règles expérimentales 0.148.0-alpha.21 vérifiées, puis fallback générique."
	case toolcatalog.MimoCode:
		return "Profil de lancement MiMo Code; commande locale et détection générique."
	case toolcatalog.Ollama:
		return "Ollama / DeepSeek local; run et le modèle restent des arguments explicites."
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
	if reason != "advanced_adapter" {
		// Known adapter IDs are safe bridge metadata. An unknown advanced ID is
		// intentionally kept on the Go side with the rest of its locked spec.
		profile.Adapter = effectiveProfileAdapter(spec)
	}
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
	case toolcatalog.Ollama:
		return "ollama"
	default:
		return "commande personnalisée"
	}
}

func effectiveProfileAdapter(spec agent.Spec) string {
	if adapterID := strings.ToLower(strings.TrimSpace(spec.Adapter)); adapterID != "" {
		return adapterID
	}
	if len(spec.Command) > 0 {
		if descriptor, ok := toolcatalog.Lookup(profileForExecutable(spec.Command[0])); ok {
			if adapterID := strings.ToLower(strings.TrimSpace(descriptor.DefaultAdapter)); adapterID != "" {
				return adapterID
			}
		}
	}
	return agent.AdapterGeneric
}

func profileForExecutable(executable string) toolcatalog.ProfileID {
	name := portableExecutableName(executable)
	for _, descriptor := range toolcatalog.Descriptors() {
		for _, candidate := range descriptor.Executables {
			if name != "" && name == portableExecutableName(candidate) {
				return descriptor.ID
			}
		}
	}
	return toolcatalog.Custom
}

func portableExecutableName(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return ""
	}
	// Configurations can be prepared on another OS. filepath.Base follows the
	// host separator, so normalize Windows paths before comparing catalog
	// candidates and ignore the executable suffix just like adapter hints do.
	value = strings.ReplaceAll(value, `\`, "/")
	name := strings.ToLower(strings.TrimSpace(filepath.Base(value)))
	return strings.TrimSuffix(name, ".exe")
}

func lockedProfileReason(spec agent.Spec) string {
	switch {
	case spec.Shell != "":
		return "advanced_shell"
	case len(spec.Env) > 0:
		return "advanced_environment"
	case len(spec.Command) == 0:
		return "invalid_command"
	case !editableProfileAdapter(spec):
		return "advanced_adapter"
	case !profileIDPattern.MatchString(spec.ID) ||
		utf8.RuneCountInString(spec.Name) > maximumProfileName ||
		utf8.RuneCountInString(spec.Cwd) > maximumProfileValue:
		return "legacy_profile_fields"
	default:
		return ""
	}
}

func editableProfileAdapter(spec agent.Spec) bool {
	adapterID := strings.ToLower(strings.TrimSpace(spec.Adapter))
	if adapterID == "" || adapterID == agent.AdapterGeneric {
		return true
	}
	switch profileForExecutable(spec.Command[0]) {
	case toolcatalog.ClaudeCode:
		return adapterID == adapters.ClaudeID
	case toolcatalog.CodexCLI:
		return adapterID == adapters.CodexID
	default:
		return false
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
			if input.Adapter != "" && !strings.EqualFold(strings.TrimSpace(input.Adapter), effectiveProfileAdapter(existing)) {
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
		adapterID, ok := validatedProfileAdapter(toolcatalog.ProfileID(input.PresetID), input.Adapter)
		if !ok {
			return nil, errProfilesInvalid
		}
		resolved, err := toolcatalog.Resolve(toolcatalog.LaunchRequest{
			ProfileID:  toolcatalog.ProfileID(input.PresetID),
			AgentID:    input.ID,
			Name:       input.Name,
			Executable: input.Argv[0],
			Args:       append([]string(nil), input.Argv[1:]...),
			Cwd:        input.Cwd,
			Adapter:    adapterID,
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

func validatedProfileAdapter(profileID toolcatalog.ProfileID, value string) (string, bool) {
	descriptor, ok := toolcatalog.Lookup(profileID)
	if !ok {
		return "", false
	}
	adapterID := strings.ToLower(strings.TrimSpace(value))
	if adapterID == "" {
		adapterID = strings.ToLower(strings.TrimSpace(descriptor.DefaultAdapter))
	}
	if adapterID == agent.AdapterGeneric || adapterID == strings.ToLower(strings.TrimSpace(descriptor.DefaultAdapter)) {
		return adapterID, adapterID != ""
	}
	return "", false
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
