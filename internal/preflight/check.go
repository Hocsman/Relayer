package preflight

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/intercept"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

var ErrNilContext = errors.New("preflight context is nil")

const (
	summaryConfigurationValid      = "La configuration est valide."
	summaryConfigurationInvalid    = "La configuration effective est invalide."
	remediationConfiguration       = "Corrigez la configuration avant de démarrer un agent."
	summaryPlatformSupported       = "La plateforme permet l'exécution supervisée."
	summaryPlatformUnsupported     = "L'exécution supervisée n'est pas disponible sur cette plateforme."
	remediationPlatform            = "Utilisez une plateforme prise en charge ou attendez un backend natif compatible."
	summaryPolicyValid             = "Les politiques sont valides."
	summaryPolicyInvalid           = "Les politiques sont invalides."
	remediationPolicy              = "Corrigez les règles de politique avant de démarrer un agent."
	summaryPolicyReferencesValid   = "Les références d'agents des politiques sont valides."
	summaryPolicyReferencesBad     = "Une politique référence un agent absent."
	remediationPolicyReferences    = "Référencez uniquement des agents présents dans la configuration effective."
	summaryAuditDisabled           = "Le journal d'audit est désactivé."
	summaryAuditConfigured         = "La configuration du journal d'audit est valide."
	summaryAuditInvalid            = "La configuration du journal d'audit est invalide."
	remediationAudit               = "Corrigez la section audit avant de démarrer un agent."
	summaryAuditPathReady          = "L'emplacement d'audit sera vérifié et créé au démarrage."
	summaryAuditPathExisting       = "L'emplacement d'audit existant est privé."
	summaryAuditPathHarden         = "Les permissions du fichier d'audit seront renforcées à l'ouverture."
	remediationAuditPermissions    = "Limitez l'accès au fichier d'audit à l'utilisateur courant."
	summaryAuditPathUnsafe         = "L'emplacement d'audit existant n'est pas sûr."
	remediationAuditPath           = "Choisissez un fichier régulier dans un dossier privé appartenant à l'utilisateur courant."
	summaryAuditGenerationsNone    = "Aucune génération d'audit existante n'est détectée."
	summaryAuditGenerationsReady   = "Les générations d'audit existantes sont privées et régulières."
	summaryAuditGenerationsHarden  = "Les permissions de générations d'audit seront renforcées au démarrage."
	summaryAuditGenerationsUnsafe  = "Une génération d'audit existante n'est pas sûre."
	summaryAuditGenerationsUnread  = "Les générations d'audit ne peuvent pas être inspectées."
	remediationAuditGenerations    = "Vérifiez que chaque génération est un fichier régulier privé appartenant à l'utilisateur courant."
	remediationAuditReadDir        = "Vérifiez les permissions de lecture du dossier d'audit."
	summaryToolInstalled           = "L'outil optionnel est détecté."
	summaryToolMissing             = "L'outil optionnel n'est pas installé."
	summaryToolUnknown             = "Cet outil optionnel nécessite une sélection explicite."
	summaryToolInconclusive        = "La détection de l'outil optionnel est indisponible."
	remediationToolDetection       = "Vérifiez la variable PATH et relancez le diagnostic."
	summaryAgentExecutableReady    = "L'exécutable de l'agent est détecté."
	summaryAgentExecutableMissing  = "L'exécutable de l'agent est introuvable."
	summaryAgentExecutableUnknown  = "La présence de l'exécutable de l'agent ne peut pas être confirmée."
	remediationAgentExecutable     = "Installez l'outil requis ou corrigez sa commande avant le démarrage."
	summaryAdapterReady            = "L'adaptateur effectif est disponible."
	summaryAdapterExperimental     = "L'adaptateur effectif est expérimental."
	remediationAdapterExperimental = "Conservez une décision humaine pour les interactions non couvertes."
	summaryAdapterUnavailable      = "L'adaptateur demandé n'est pas disponible."
	remediationAdapter             = "Choisissez un adaptateur enregistré et implémenté."
	summaryBackendPTY              = "Le backend effectif est PTY."
	summaryBackendTmux             = "Le backend effectif est tmux."
	summaryBackendAutoFallback     = "Le backend auto se repliera sur PTY car tmux est indisponible."
	remediationBackendAuto         = "Installez tmux pour activer les sessions détachables."
	summaryBackendUnavailable      = "Le backend tmux demandé est indisponible."
	remediationBackend             = "Installez tmux ou choisissez PTY ou auto."
)

// Check passively validates one effective plan. It never opens an audit sink,
// constructs a terminal backend, starts a process or invokes an executable.
func Check(ctx context.Context, input Input, options Options) (Report, error) {
	if ctx == nil {
		return Report{}, ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return Report{}, err
	}

	input = cloneInput(input)
	options = normalizeOptions(options)
	report := baseReport(options)
	report.Configuration = ConfigurationInfo{
		Version:         input.Configuration.Version,
		Legacy:          input.Configuration.Legacy,
		AgentCount:      len(input.Specs),
		PolicyRuleCount: len(input.Configuration.Policies.Rules),
	}
	report.Audit = auditInfo(input.Configuration.Audit)

	configurationValid := validateConfigurationShape(input)
	if configurationValid {
		addCheck(&report, "configuration.valid", ScopeConfiguration, CheckPass, summaryConfigurationValid, "")
	} else {
		addCheck(&report, "configuration.valid", ScopeConfiguration, CheckBlock, summaryConfigurationInvalid, remediationConfiguration)
	}

	if report.Platform.Supported {
		addCheck(&report, "platform.execution", ScopePlatform, CheckPass, summaryPlatformSupported, "")
	} else {
		addCheck(&report, "platform.execution", ScopePlatform, CheckBlock, summaryPlatformUnsupported, remediationPlatform)
	}

	if _, err := policy.New(input.Configuration.Policies); err != nil {
		addCheck(&report, "policy.valid", ScopePolicy, CheckBlock, summaryPolicyInvalid, remediationPolicy)
	} else {
		addCheck(&report, "policy.valid", ScopePolicy, CheckPass, summaryPolicyValid, "")
	}
	if validPolicyAgentReferences(input.Configuration.Policies, input.Specs) {
		addCheck(&report, "policy.agent_references", ScopePolicy, CheckPass, summaryPolicyReferencesValid, "")
	} else {
		addCheck(&report, "policy.agent_references", ScopePolicy, CheckBlock, summaryPolicyReferencesBad, remediationPolicyReferences)
	}

	checkAudit(&report, input.Configuration.Audit, options)
	if err := checkContext(ctx); err != nil {
		return Report{}, err
	}
	checkTools(ctx, &report, options.Detector)
	if err := checkContext(ctx); err != nil {
		return Report{}, err
	}
	checkAgents(ctx, &report, input, options.Detector)
	if err := checkContext(ctx); err != nil {
		return Report{}, err
	}

	finalizeStatus(&report)
	return report.Clone(), nil
}

// FailureReport builds a static, blocked report for facade failures that occur
// before an effective Input exists. Unknown kinds collapse to an internal
// failure and never become caller-controlled display text.
func FailureReport(kind FailureKind, options Options) Report {
	options = normalizeOptions(options)
	report := baseReport(options)
	type failureText struct {
		id          string
		scope       Scope
		summary     string
		remediation string
	}
	texts := map[FailureKind]failureText{
		FailureConfigMissing: {
			id: "configuration.missing", scope: ScopeConfiguration,
			summary:     "Le fichier de configuration est absent.",
			remediation: "Créez explicitement la configuration avant de relancer le diagnostic.",
		},
		FailureConfigInvalid: {
			id: "configuration.invalid", scope: ScopeConfiguration,
			summary: summaryConfigurationInvalid, remediation: remediationConfiguration,
		},
		FailureConfigUnreadable: {
			id: "configuration.unreadable", scope: ScopeConfiguration,
			summary:     "Le fichier de configuration ne peut pas être lu.",
			remediation: "Vérifiez l'existence et les permissions du fichier de configuration.",
		},
		FailureWorkingDirectory: {
			id: "configuration.working_directory", scope: ScopeConfiguration,
			summary:     "Le dossier de travail ne peut pas être vérifié.",
			remediation: "Choisissez un dossier de travail existant et accessible.",
		},
		FailureAgentResolution: {
			id: "configuration.agents", scope: ScopeAgent,
			summary: summaryConfigurationInvalid, remediation: remediationConfiguration,
		},
		FailurePolicyResolution: {
			id: "policy.initialization", scope: ScopePolicy,
			summary: summaryPolicyInvalid, remediation: remediationPolicy,
		},
		FailureAdapterResolution: {
			id: "adapter.initialization", scope: ScopeAdapter,
			summary: summaryAdapterUnavailable, remediation: remediationAdapter,
		},
		FailurePreflightInternal: {
			id: "preflight.internal", scope: ScopeConfiguration,
			summary:     "Le diagnostic n'a pas pu être terminé de façon sûre.",
			remediation: "Relancez le diagnostic avant de démarrer un agent.",
		},
	}
	selected, ok := texts[kind]
	if !ok {
		selected = texts[FailurePreflightInternal]
	}
	addCheck(&report, selected.id, selected.scope, CheckBlock, selected.summary, selected.remediation)
	finalizeStatus(&report)
	return report.Clone()
}

func normalizeOptions(options Options) Options {
	options.GOOS = normalizeGOOS(options.GOOS)
	options.GOARCH = normalizeGOARCH(options.GOARCH)
	if options.Detector == nil {
		options.Detector = toolcatalog.DefaultDetector()
	}
	if options.Lstat == nil {
		options.Lstat = os.Lstat
	}
	if options.ReadDir == nil {
		options.ReadDir = os.ReadDir
	}
	if options.ResolveAuditPath == nil {
		options.ResolveAuditPath = audit.ResolvePath
	}
	if options.OwnerCheck == nil {
		options.OwnerCheck = currentOwnerStatus
	}
	return options
}

func baseReport(options Options) Report {
	return Report{
		SchemaVersion: CurrentSchemaVersion,
		Status:        StatusReady,
		Platform: PlatformInfo{
			OS:        options.GOOS,
			Arch:      options.GOARCH,
			Supported: supportedPlatform(options.GOOS),
		},
		Audit: AuditInfo{Location: AuditLocationDisabled, Mode: audit.ModeOff},
	}
}

func normalizeGOOS(value string) string {
	if strings.TrimSpace(value) == "" {
		value = runtime.GOOS
	}
	value = strings.ToLower(strings.TrimSpace(value))
	for _, known := range []string{
		"aix", "android", "darwin", "dragonfly", "freebsd", "illumos", "ios",
		"js", "linux", "netbsd", "openbsd", "plan9", "solaris", "wasip1", "windows",
	} {
		if value == known {
			return value
		}
	}
	return "unknown"
}

func normalizeGOARCH(value string) string {
	if strings.TrimSpace(value) == "" {
		value = runtime.GOARCH
	}
	value = strings.ToLower(strings.TrimSpace(value))
	for _, known := range []string{
		"386", "amd64", "arm", "arm64", "loong64", "mips", "mips64", "mips64le",
		"mipsle", "ppc64", "ppc64le", "riscv64", "s390x", "wasm",
	} {
		if value == known {
			return value
		}
	}
	return "unknown"
}

func supportedPlatform(goos string) bool {
	switch goos {
	case "darwin", "linux":
		return true
	default:
		return false
	}
}

func validateConfigurationShape(input Input) bool {
	if !input.Configuration.Legacy && input.Configuration.Version != config.CurrentVersion {
		return false
	}
	if !agent.IsSupportedBackend(input.Configuration.Backend) {
		return false
	}
	if len(input.Specs) == 0 || len(input.Specs) > 8 {
		return false
	}
	_, err := agent.ValidateAll(input.Specs, ".", input.Configuration.Backend)
	return err == nil
}

func validPolicyAgentReferences(configuration policy.Config, specs []agent.Spec) bool {
	known := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		known[strings.ToLower(strings.TrimSpace(spec.ID))] = struct{}{}
	}
	for _, rule := range configuration.Rules {
		for _, id := range rule.Match.AgentIDs {
			if _, ok := known[strings.ToLower(strings.TrimSpace(id))]; !ok {
				return false
			}
		}
	}
	return true
}

func auditInfo(configuration audit.Config) AuditInfo {
	result := AuditInfo{
		Enabled:       configuration.Enabled && configuration.Mode != audit.ModeOff,
		MaxFileSizeMB: configuration.MaxFileSizeMB,
		MaxFiles:      configuration.MaxFiles,
	}
	switch configuration.Mode {
	case audit.ModeOff, audit.ModeMetadata, audit.ModeDetailed:
		result.Mode = configuration.Mode
	default:
		result.Mode = audit.ModeOff
		result.Enabled = false
	}
	if !result.Enabled {
		result.Location = AuditLocationDisabled
	} else if strings.TrimSpace(configuration.Path) == "" {
		result.Location = AuditLocationDefault
	} else {
		result.Location = AuditLocationCustom
	}
	return result
}

func checkAudit(report *Report, configuration audit.Config, options Options) {
	if err := audit.Validate(configuration); err != nil {
		addCheck(report, "audit.configuration", ScopeAudit, CheckBlock, summaryAuditInvalid, remediationAudit)
		return
	}
	if !configuration.Enabled || configuration.Mode == audit.ModeOff {
		addCheck(report, "audit.configuration", ScopeAudit, CheckPass, summaryAuditDisabled, "")
		return
	}
	addCheck(report, "audit.configuration", ScopeAudit, CheckPass, summaryAuditConfigured, "")
	path, err := options.ResolveAuditPath(configuration.Path)
	if err != nil || strings.TrimSpace(path) == "" {
		addCheck(report, "audit.path", ScopeAudit, CheckBlock, summaryAuditPathUnsafe, remediationAuditPath)
		return
	}
	directory := filepath.Dir(path)
	directoryInfo, directoryErr := options.Lstat(directory)
	directoryExists := directoryErr == nil
	if directoryErr == nil {
		if !safeAuditDirectory(directoryInfo, options) {
			addCheck(report, "audit.path", ScopeAudit, CheckBlock, summaryAuditPathUnsafe, remediationAuditPath)
			return
		}
	} else if !errors.Is(directoryErr, fs.ErrNotExist) {
		addCheck(report, "audit.path", ScopeAudit, CheckBlock, summaryAuditPathUnsafe, remediationAuditPath)
		return
	}
	if directoryExists {
		if !checkAuditGenerations(report, directory, filepath.Base(path), options) {
			return
		}
	} else {
		addCheck(report, "audit.generations", ScopeAudit, CheckPass, summaryAuditGenerationsNone, "")
	}

	fileInfo, fileErr := options.Lstat(path)
	if errors.Is(fileErr, fs.ErrNotExist) {
		addCheck(report, "audit.path", ScopeAudit, CheckPass, summaryAuditPathReady, "")
		return
	}
	if fileErr != nil || fileInfo.Mode()&os.ModeSymlink != 0 || !fileInfo.Mode().IsRegular() ||
		ownerRejected(fileInfo, options) || !hasUnixOwnerPermissions(fileInfo, options, 0o600) {
		addCheck(report, "audit.path", ScopeAudit, CheckBlock, summaryAuditPathUnsafe, remediationAuditPath)
		return
	}
	if unixPermissionModel(options.GOOS) && fileInfo.Mode().Perm()&0o077 != 0 {
		addCheck(report, "audit.path", ScopeAudit, CheckWarning, summaryAuditPathHarden, remediationAuditPermissions)
		return
	}
	addCheck(report, "audit.path", ScopeAudit, CheckPass, summaryAuditPathExisting, "")
}

func checkAuditGenerations(report *Report, directory, activeBase string, options Options) bool {
	entries, err := options.ReadDir(directory)
	if err != nil {
		addCheck(report, "audit.generations", ScopeAudit, CheckBlock, summaryAuditGenerationsUnread, remediationAuditReadDir)
		return false
	}
	found := false
	needsHardening := false
	for _, entry := range entries {
		if entry == nil {
			addCheck(report, "audit.generations", ScopeAudit, CheckBlock, summaryAuditGenerationsUnread, remediationAuditReadDir)
			return false
		}
		index, recognized := audit.AuditGenerationIndex(activeBase, entry.Name())
		if !recognized || index == 0 {
			continue
		}
		found = true
		info, statErr := options.Lstat(filepath.Join(directory, entry.Name()))
		if statErr != nil || info == nil || info.Mode()&os.ModeSymlink != 0 ||
			!info.Mode().IsRegular() || ownerRejected(info, options) {
			addCheck(report, "audit.generations", ScopeAudit, CheckBlock, summaryAuditGenerationsUnsafe, remediationAuditGenerations)
			return false
		}
		if unixPermissionModel(options.GOOS) && info.Mode().Perm()&0o077 != 0 {
			needsHardening = true
		}
	}
	if !found {
		addCheck(report, "audit.generations", ScopeAudit, CheckPass, summaryAuditGenerationsNone, "")
	} else if needsHardening {
		addCheck(report, "audit.generations", ScopeAudit, CheckWarning, summaryAuditGenerationsHarden, remediationAuditPermissions)
	} else {
		addCheck(report, "audit.generations", ScopeAudit, CheckPass, summaryAuditGenerationsReady, "")
	}
	return true
}

func safeAuditDirectory(info fs.FileInfo, options Options) bool {
	if info == nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() || ownerRejected(info, options) {
		return false
	}
	return !unixPermissionModel(options.GOOS) ||
		info.Mode().Perm()&0o077 == 0 && hasUnixOwnerPermissions(info, options, 0o300)
}

func hasUnixOwnerPermissions(info fs.FileInfo, options Options, required fs.FileMode) bool {
	return !unixPermissionModel(options.GOOS) || info.Mode().Perm()&required == required
}

func ownerRejected(info fs.FileInfo, options Options) bool {
	if !unixPermissionModel(options.GOOS) {
		return false
	}
	return options.OwnerCheck(info) != OwnerCurrent
}

func unixPermissionModel(goos string) bool {
	switch goos {
	case "aix", "darwin", "dragonfly", "freebsd", "illumos", "linux", "netbsd", "openbsd", "solaris":
		return true
	default:
		return false
	}
}

func checkTools(ctx context.Context, report *Report, detector toolcatalog.Detector) {
	for _, descriptor := range toolcatalog.Descriptors() {
		if ctx.Err() != nil {
			return
		}
		detection, err := toolcatalog.Detect(ctx, descriptor.ID, "", detector)
		status := detection.Status
		checkStatus := CheckPass
		summary := summaryToolUnknown
		remediation := ""
		if err != nil {
			status = toolcatalog.InstallUnknown
			checkStatus = CheckWarning
			summary = summaryToolInconclusive
			remediation = remediationToolDetection
		} else {
			switch status {
			case toolcatalog.InstallInstalled:
				summary = summaryToolInstalled
			case toolcatalog.InstallNotInstalled:
				summary = summaryToolMissing
			case toolcatalog.InstallUnknown:
				summary = summaryToolUnknown
			}
		}
		report.Tools = append(report.Tools, ToolInfo{ProfileID: descriptor.ID, Installation: status})
		addCheck(report, "tool."+string(descriptor.ID), ScopeTool, checkStatus, summary, remediation)
	}
}

func checkAgents(ctx context.Context, report *Report, input Input, detector toolcatalog.Detector) {
	registry, registryErr := adapters.NewRegistry(input.Configuration.Patterns)
	needsTmux := false
	for _, spec := range input.Specs {
		if spec.Backend == agent.BackendTmux || spec.Backend == agent.BackendAuto {
			needsTmux = true
			break
		}
	}
	tmuxStatus := toolcatalog.InstallUnknown
	if needsTmux {
		tmuxStatus, _ = detectStatus(ctx, detector, []string{"tmux"})
	}

	for index, spec := range input.Specs {
		if ctx.Err() != nil {
			return
		}
		ordinal := index + 1
		prefix := "agent." + strconv.Itoa(ordinal)
		info := AgentInfo{Ordinal: ordinal, Source: AgentConfigured}
		if input.DemoAgents {
			info.Source = AgentDemo
		}

		if len(spec.Command) > 0 {
			info.Command = CommandDirect
		} else {
			info.Command = CommandShell
		}
		effectiveBackend := spec.Backend
		if spec.Backend == agent.BackendAuto {
			if tmuxStatus == toolcatalog.InstallInstalled {
				effectiveBackend = agent.BackendTmux
			} else {
				effectiveBackend = agent.BackendPTY
			}
		}
		info.Installation = agentExecutableStatus(ctx, detector, spec, effectiveBackend)
		switch info.Installation {
		case toolcatalog.InstallInstalled:
			addCheck(report, prefix+".executable", ScopeAgent, CheckPass, summaryAgentExecutableReady, "")
		case toolcatalog.InstallNotInstalled:
			addCheck(report, prefix+".executable", ScopeAgent, CheckBlock, summaryAgentExecutableMissing, remediationAgentExecutable)
		default:
			addCheck(report, prefix+".executable", ScopeAgent, CheckBlock, summaryAgentExecutableUnknown, remediationAgentExecutable)
		}

		if registryErr != nil {
			addCheck(report, prefix+".adapter", ScopeAdapter, CheckBlock, summaryAdapterUnavailable, remediationAdapter)
		} else {
			executable := ""
			if len(spec.Command) > 0 {
				executable = spec.Command[0]
			}
			resolved, descriptor, err := registry.Resolve(spec.Adapter, executable)
			if err != nil || resolved == nil || !descriptor.Implemented {
				addCheck(report, prefix+".adapter", ScopeAdapter, CheckBlock, summaryAdapterUnavailable, remediationAdapter)
			} else {
				info.Adapter = descriptor.ID
				info.AdapterMaturity = descriptor.Status
				if descriptor.Status == adapters.StatusExperimental {
					addCheck(report, prefix+".adapter", ScopeAdapter, CheckWarning, summaryAdapterExperimental, remediationAdapterExperimental)
				} else {
					addCheck(report, prefix+".adapter", ScopeAdapter, CheckPass, summaryAdapterReady, "")
				}
			}
		}

		switch spec.Backend {
		case agent.BackendPTY:
			info.Backend = agent.BackendPTY
			addCheck(report, prefix+".backend", ScopeBackend, CheckPass, summaryBackendPTY, "")
		case agent.BackendTmux:
			if tmuxStatus == toolcatalog.InstallInstalled {
				info.Backend = agent.BackendTmux
				addCheck(report, prefix+".backend", ScopeBackend, CheckPass, summaryBackendTmux, "")
			} else {
				addCheck(report, prefix+".backend", ScopeBackend, CheckBlock, summaryBackendUnavailable, remediationBackend)
			}
		case agent.BackendAuto:
			if tmuxStatus == toolcatalog.InstallInstalled {
				info.Backend = agent.BackendTmux
				addCheck(report, prefix+".backend", ScopeBackend, CheckPass, summaryBackendTmux, "")
			} else {
				info.Backend = agent.BackendPTY
				addCheck(report, prefix+".backend", ScopeBackend, CheckWarning, summaryBackendAutoFallback, remediationBackendAuto)
			}
		default:
			addCheck(report, prefix+".backend", ScopeBackend, CheckBlock, summaryBackendUnavailable, remediationBackend)
		}
		report.Agents = append(report.Agents, info)
	}
}

func detectStatus(ctx context.Context, detector toolcatalog.Detector, candidates []string) (toolcatalog.InstallStatus, bool) {
	if len(candidates) == 0 {
		return toolcatalog.InstallUnknown, true
	}
	detection, err := detector.Detect(ctx, append([]string(nil), candidates...))
	if err != nil || !validPassiveDetection(detection) {
		return toolcatalog.InstallUnknown, true
	}
	return detection.Status, false
}

func validPassiveDetection(detection toolcatalog.Detection) bool {
	switch detection.Status {
	case toolcatalog.InstallUnknown, toolcatalog.InstallNotInstalled:
		return detection.Executable == "" && detection.Path == "" && detection.Version == "" && !detection.VersionKnown
	case toolcatalog.InstallInstalled:
		return strings.TrimSpace(detection.Executable) != "" && strings.TrimSpace(detection.Path) != "" &&
			!strings.ContainsRune(detection.Executable, '\x00') && !strings.ContainsRune(detection.Path, '\x00') &&
			!strings.ContainsRune(detection.Version, '\x00') &&
			detection.VersionKnown == (strings.TrimSpace(detection.Version) != "")
	default:
		return false
	}
}

func addCheck(report *Report, id string, scope Scope, status CheckStatus, summary, remediation string) {
	report.Checks = append(report.Checks, CheckResult{
		ID: id, Scope: scope, Status: status, Summary: summary, Remediation: remediation,
	})
}

func finalizeStatus(report *Report) {
	report.Status = StatusReady
	for _, check := range report.Checks {
		if check.Status == CheckBlock {
			report.Status = StatusBlocked
			return
		}
		if check.Status == CheckWarning {
			report.Status = StatusWarning
		}
	}
}

func checkContext(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return nil
}

func cloneInput(input Input) Input {
	result := input
	result.Configuration = cloneConfiguration(input.Configuration)
	result.Specs = cloneSpecs(input.Specs)
	return result
}

func cloneConfiguration(configuration config.Result) config.Result {
	result := configuration
	result.Agents = cloneSpecs(configuration.Agents)
	result.Patterns = append([]intercept.Pattern(nil), configuration.Patterns...)
	result.Policies = clonePolicyConfig(configuration.Policies)
	return result
}

func cloneSpecs(specs []agent.Spec) []agent.Spec {
	result := make([]agent.Spec, len(specs))
	for index, spec := range specs {
		result[index] = spec
		result[index].Command = append([]string(nil), spec.Command...)
		if spec.Env != nil {
			result[index].Env = make(map[string]string, len(spec.Env))
			for name, value := range spec.Env {
				result[index].Env[name] = value
			}
		}
	}
	return result
}

func clonePolicyConfig(configuration policy.Config) policy.Config {
	result := configuration
	result.Rules = make([]policy.Rule, len(configuration.Rules))
	for index, rule := range configuration.Rules {
		result.Rules[index] = rule
		result.Rules[index].Match.EventTypes = append([]adapters.EventType(nil), rule.Match.EventTypes...)
		result.Rules[index].Match.AgentIDs = append([]string(nil), rule.Match.AgentIDs...)
		result.Rules[index].Match.RiskLevels = append([]adapters.RiskLevel(nil), rule.Match.RiskLevels...)
		if rule.Match.Sensitive != nil {
			value := *rule.Match.Sensitive
			result.Rules[index].Match.Sensitive = &value
		}
	}
	return result
}
