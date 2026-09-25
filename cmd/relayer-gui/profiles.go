package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/agentprofile"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/supervise"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

const (
	minimumAgentProfiles = agentprofile.MinProfiles
	maximumAgentProfiles = agentprofile.MaxProfiles
)

var (
	errProfilesStale   = errors.New("configuration has changed, reload the profiles before retrying")
	errProfilesInvalid = errors.New("one or more agent profiles are invalid")
	errProfilesSave    = errors.New("agent profiles could not be saved")
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
	current, err := config.LoadExisting(path)
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
		if reloaded, reloadErr := config.LoadExisting(path); reloadErr == nil && reloaded.Revision != current.Revision {
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
	loaded, err := config.LoadExisting(path)
	if err != nil {
		return AgentProfilesView{}, errors.New(supervise.SafeDisplayError(err))
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
	case toolcatalog.Aider:
		return "Aider coding assistant; interactive pair programming with terminal prompts."
	case toolcatalog.GooseCLI:
		return "Goose CLI; developer AI agent with automated tool and command execution prompts."
	case toolcatalog.OpenInterpreter:
		return "Open Interpreter; local code and command execution with human approval prompts."
	case toolcatalog.ClaudeCode:
		return "Claude Code; experimental rules verified on 2.1.59, then generic fallback."
	case toolcatalog.CodexCLI:
		return "Codex CLI; experimental rules verified on 0.148.0-alpha.21, then generic fallback."
	case toolcatalog.MimoCode:
		return "MiMo Code launch profile; local command and generic detection."
	case toolcatalog.Ollama:
		return "Local Ollama / DeepSeek; the run subcommand and the model stay explicit arguments."
	default:
		return "Any local interactive CLI with an explicit argv."
	}
}

// profileView is the shared editor view of spec, internal/agentprofile's, in
// this bridge's DTO.
func profileView(spec agent.Spec) AgentProfile {
	return AgentProfile(agentprofile.View(spec))
}

// resolveProfileInputs is internal/agentprofile's Resolve, which the web
// gateway uses too. Every refusal is errProfilesInvalid here: the desktop
// shows no detail.
func resolveProfileInputs(inputs []AgentProfileInput, current config.Result, baseDir string) ([]agent.Spec, error) {
	shared := make([]agentprofile.Input, len(inputs))
	for index, input := range inputs {
		shared[index] = agentprofile.Input(input)
	}
	specs, err := agentprofile.Resolve(shared, current.Agents, baseDir, current.Backend)
	if err != nil {
		return nil, errProfilesInvalid
	}
	return specs, nil
}
