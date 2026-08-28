// Package app composes Relayer's configuration, PTY sessions, and terminal UI.
// Keeping this wiring separate lets both the canonical cmd/relayer entrypoint
// and the documented root compatibility entrypoint stay tiny.
package app

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/Hocsman/Relayer/internal/tui"
	buildversion "github.com/Hocsman/Relayer/internal/version"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/creack/pty"
)

const (
	defaultRingCapacity  = 256 * 1024
	defaultEventCapacity = 256
)

// Run parses the public CLI, initializes the process owner and starts Bubble
// Tea. It never returns while sessions are still owned by the manager.
func Run(arguments []string, diagnostics io.Writer) error {
	return runWithOutput(
		arguments,
		diagnostics,
		diagnostics,
		productionBackendDependencies(),
	)
}

// RunWithOutput is the canonical public CLI entry. Version output is kept
// separate from diagnostics and is handled before configuration, audit, or
// backend initialization.
func RunWithOutput(arguments []string, output io.Writer, diagnostics io.Writer) error {
	return runWithOutput(
		arguments,
		output,
		diagnostics,
		productionBackendDependencies(),
	)
}

func runWithOutput(
	arguments []string,
	output io.Writer,
	diagnostics io.Writer,
	dependencies backendDependencies,
) error {
	return runWithOutputAndPreflight(arguments, output, diagnostics, dependencies, RunPreflight)
}

func runWithOutputAndPreflight(
	arguments []string,
	output io.Writer,
	diagnostics io.Writer,
	dependencies backendDependencies,
	preflightRun preflightRunner,
) error {
	versionRequested := false
	for _, argument := range arguments {
		if argument == "--version" || argument == "-version" {
			versionRequested = true
			break
		}
	}
	if versionRequested {
		if len(arguments) != 1 {
			return errors.New("l'option --version doit être utilisée seule")
		}
		if output == nil {
			output = io.Discard
		}
		if _, err := fmt.Fprintln(output, buildversion.String()); err != nil {
			return fmt.Errorf("écriture de la version: %w", err)
		}
		return nil
	}
	if len(arguments) > 0 && arguments[0] == "doctor" {
		return runDoctor(arguments[1:], output, diagnostics, preflightRun)
	}
	return run(arguments, diagnostics, dependencies)
}

func run(arguments []string, diagnostics io.Writer, dependencies backendDependencies) (returnErr error) {
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	options, err := parseOptions(arguments, diagnostics)
	if err != nil {
		return err
	}

	configuration, err := config.LoadOrCreate(options.configPath)
	if err != nil {
		return err
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		return fmt.Errorf("lecture du dossier courant: %w", err)
	}
	resolution, err := resolveAgentPlans(configuration, options, workingDirectory)
	if err != nil {
		return err
	}
	policyEngine, err := policy.New(configuration.Policies)
	if err != nil {
		return fmt.Errorf("initialisation des politiques: %w", err)
	}
	if err := validatePolicyAgentIDs(policyEngine.Config(), resolution.Specs); err != nil {
		return err
	}
	registry, err := adapters.NewRegistry(configuration.Patterns)
	if err != nil {
		return fmt.Errorf("initialisation des adaptateurs: %w", err)
	}
	resolution.Specs, err = resolveAgentAdapters(resolution.Specs, registry)
	if err != nil {
		return err
	}
	backendSelection, err := resolveAgentBackends(
		context.Background(), resolution.Specs, dependencies.lookup, dependencies.probeTmux)
	if err != nil {
		return err
	}
	resolution.Specs = backendSelection.Specs
	resolution.Warnings = append(resolution.Warnings, backendSelection.Warnings...)
	for _, warning := range resolution.Warnings {
		fmt.Fprintln(diagnostics, warning)
	}
	auditor, err := initializeAudit(configuration.Audit, dependencies)
	if err != nil {
		return fmt.Errorf("initialisation du journal d'audit: %w", err)
	}
	defer func() {
		outcome := audit.OutcomeSucceeded
		if returnErr != nil {
			outcome = audit.OutcomeFailed
		}
		if recordErr := auditor.Record(audit.Entry{
			Kind:       audit.KindRunFinished,
			DecisionBy: audit.DecisionBySystem,
			Outcome:    outcome,
		}); recordErr != nil {
			joinRunError(&returnErr, "écriture de la fin du run dans l'audit", recordErr)
		}
		if closeErr := auditor.Close(); closeErr != nil {
			joinRunError(&returnErr, "fermeture du journal d'audit", closeErr)
		}
	}()
	if err := auditor.Record(audit.Entry{
		Kind:       audit.KindRunStarted,
		DecisionBy: audit.DecisionBySystem,
		Outcome:    audit.OutcomeStarted,
	}); err != nil {
		return fmt.Errorf("écriture du démarrage du run dans l'audit: %w", err)
	}
	initialWidth, initialHeight := initialTerminalSize()

	events := make(chan session.Event, defaultEventCapacity)
	router, err := buildBackendRouter(
		context.Background(),
		events,
		registry,
		defaultRingCapacity,
		backendSelection,
		configuration.Sessions,
		dependencies,
	)
	if err != nil {
		if recordErr := auditor.Record(audit.Entry{
			Kind:       audit.KindBackendError,
			DecisionBy: audit.DecisionBySystem,
			Outcome:    audit.OutcomeFailed,
			Reason:     "backend_initialization_failed",
		}); recordErr != nil {
			return errors.Join(err, fmt.Errorf("audit de l'échec du backend: %w", recordErr))
		}
		return err
	}
	startedInfos := make([]session.Info, 0, len(resolution.Specs))
	defer func() {
		for _, info := range startedInfos {
			if recordErr := auditor.Record(audit.Entry{
				Kind:       audit.KindSupervisionFinished,
				SessionID:  info.ID,
				AgentID:    info.ID,
				Backend:    info.Backend,
				Adapter:    info.Adapter,
				DecisionBy: audit.DecisionBySystem,
				Outcome:    audit.OutcomeFinished,
				Reason:     "supervision_ended",
			}); recordErr != nil {
				joinRunError(&returnErr, "écriture de la fin de supervision dans l'audit", recordErr)
			}
		}
		closeContext, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		defer cancel()
		closeErr := router.Close(closeContext)
		if closeErr != nil {
			joinRunError(&returnErr, "fermeture des backends", closeErr)
		}
		for _, info := range startedInfos {
			closed, known := router.backendCloseStatus(info.Backend)
			cleanupOutcome, cleanupReason := auditCleanupResult(
				info,
				configuration.Sessions,
				closed,
				known,
			)
			if recordErr := auditor.Record(audit.Entry{
				Kind:       audit.KindSessionCleanup,
				SessionID:  info.ID,
				AgentID:    info.ID,
				Backend:    info.Backend,
				Adapter:    info.Adapter,
				DecisionBy: audit.DecisionBySystem,
				Outcome:    cleanupOutcome,
				Reason:     cleanupReason,
			}); recordErr != nil {
				joinRunError(&returnErr, "écriture du nettoyage des sessions dans l'audit", recordErr)
			}
		}
	}()

	panes, infos, err := startAgentSessionsObserved(
		router,
		resolution.Specs,
		initialWidth,
		initialHeight,
		func(spec agent.Spec, info session.Info) error {
			startedInfos = append(startedInfos, info)
			return auditor.Record(audit.Entry{
				Kind:       audit.KindSessionStarted,
				SessionID:  info.ID,
				AgentID:    spec.ID,
				Backend:    info.Backend,
				Adapter:    info.Adapter,
				DecisionBy: audit.DecisionBySystem,
				Outcome:    audit.OutcomeStarted,
			})
		},
	)
	if err != nil {
		if recordErr := auditor.Record(audit.Entry{
			Kind:       audit.KindBackendError,
			DecisionBy: audit.DecisionBySystem,
			Outcome:    audit.OutcomeFailed,
			Reason:     "session_start_failed",
		}); recordErr != nil {
			return errors.Join(err, fmt.Errorf("audit de l'échec de démarrage: %w", recordErr))
		}
		return err
	}

	startupLogs := buildStartupLogs(configuration, resolution, infos, options.configPath)
	if auditor.Enabled() {
		startupLogs = append(startupLogs, fmt.Sprintf(
			"Audit local: mode=%s, fichier=%s",
			configuration.Audit.Mode,
			auditor.Path(),
		))
	} else {
		startupLogs = append(startupLogs, "Audit local désactivé")
	}
	application, err := tui.NewModelWithPolicyAndAudit(
		&tuiBackendAdapter{router: router},
		events,
		panes,
		initialWidth,
		initialHeight,
		startupLogs,
		policyEngine,
		auditor,
	)
	if err != nil {
		return err
	}
	program := tea.NewProgram(
		application,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	_, err = program.Run()
	return err
}

func initializeAudit(configuration audit.Config, dependencies backendDependencies) (*audit.Recorder, error) {
	if dependencies.newAudit != nil {
		recorder, err := dependencies.newAudit(configuration)
		if err != nil {
			return nil, err
		}
		if recorder == nil {
			return nil, errors.New("fabrique d'audit ayant retourné un enregistreur nil")
		}
		return recorder, nil
	}
	if configuration.Enabled && configuration.Mode != audit.ModeOff {
		return nil, errors.New("fabrique d'audit indisponible")
	}
	return audit.NewRecorder(configuration, nil, nil, nil)
}

func initializeAuditForRun(configuration audit.Config, dependencies backendDependencies, runID string) (*audit.Recorder, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("run_id du runtime desktop vide")
	}
	if dependencies.newAuditForRun != nil {
		recorder, err := dependencies.newAuditForRun(configuration, runID)
		if err != nil {
			return nil, err
		}
		if recorder == nil {
			return nil, errors.New("fabrique d'audit desktop ayant retourné un enregistreur nil")
		}
		return recorder, nil
	}
	if dependencies.newAudit != nil {
		// Test dependencies predating externally reserved run IDs remain usable,
		// but production always supplies newAuditForRun.
		return initializeAudit(configuration, dependencies)
	}
	if configuration.Enabled && configuration.Mode != audit.ModeOff {
		return nil, errors.New("fabrique d'audit desktop indisponible")
	}
	return audit.Open(configuration, audit.WithRunID(runID))
}

func joinRunError(target *error, operation string, err error) {
	if err == nil {
		return
	}
	wrapped := fmt.Errorf("%s: %w", operation, err)
	if *target == nil {
		*target = wrapped
		return
	}
	*target = errors.Join(*target, wrapped)
}

func auditCleanupResult(
	info session.Info,
	sessionPolicy config.SessionPolicy,
	closed bool,
	known bool,
) (audit.Outcome, string) {
	if !known || !closed {
		// Backend.Close is aggregate: without a per-session result, never claim
		// that this particular session was removed, persisted, or failed.
		return audit.OutcomeUnknown, "backend_cleanup_incomplete"
	}
	if strings.EqualFold(info.Backend, agent.BackendTmux) && sessionPolicy.PersistOnExit {
		// This records the configured intent only; it does not assert that a
		// process which may already have exited is still alive.
		return audit.OutcomeSkipped, "persistence_requested"
	}
	return audit.OutcomeSucceeded, "backend_cleanup_completed"
}

func validatePolicyAgentIDs(configuration policy.Config, specs []agent.Spec) error {
	agentIDs := make(map[string]struct{}, len(specs))
	for _, spec := range specs {
		agentIDs[strings.ToLower(strings.TrimSpace(spec.ID))] = struct{}{}
	}
	for _, rule := range configuration.Rules {
		for _, configuredID := range rule.Match.AgentIDs {
			id := strings.ToLower(strings.TrimSpace(configuredID))
			if _, exists := agentIDs[id]; !exists {
				return fmt.Errorf(
					"politique %q: agent_id inconnu %q",
					rule.Name,
					configuredID,
				)
			}
		}
	}
	return nil
}

type sessionStarter interface {
	Start(context.Context, agent.Spec, terminal.Size) (terminal.Info, error)
	Close(context.Context) error
}

// startAgentSessions makes partial startup transactional: once an owner has
// accepted any sessions, a later failure synchronously closes the complete
// owner before the error can escape. Run keeps its defer as a second,
// idempotent lifecycle guard for every later return path.
func startAgentSessions(
	owner sessionStarter,
	specs []agent.Spec,
	initialWidth int,
	initialHeight int,
) ([]tui.Pane, []session.Info, error) {
	return startAgentSessionsObserved(owner, specs, initialWidth, initialHeight, nil)
}

type sessionStartedObserver func(agent.Spec, session.Info) error

func startAgentSessionsObserved(
	owner sessionStarter,
	specs []agent.Spec,
	initialWidth int,
	initialHeight int,
	observer sessionStartedObserver,
) ([]tui.Pane, []session.Info, error) {
	panes := make([]tui.Pane, 0, len(specs))
	infos := make([]session.Info, 0, len(specs))
	for index, spec := range specs {
		columns, rows := tui.AgentViewportSize(
			initialWidth,
			initialHeight,
			len(specs),
			index,
		)
		info, startErr := owner.Start(
			context.Background(),
			spec,
			terminal.Size{Columns: columns, Rows: rows},
		)
		if startErr != nil {
			closeContext, cancel := context.WithTimeout(context.Background(), 6*time.Second)
			_ = owner.Close(closeContext)
			cancel()
			return nil, nil, fmt.Errorf("démarrage de l'agent %q: %w", spec.ID, startErr)
		}
		if observer != nil {
			if observeErr := observer(spec, info); observeErr != nil {
				closeContext, cancel := context.WithTimeout(context.Background(), 6*time.Second)
				_ = owner.Close(closeContext)
				cancel()
				return nil, nil, fmt.Errorf("audit du démarrage de l'agent %q: %w", spec.ID, observeErr)
			}
		}
		infos = append(infos, info)
		panes = append(panes, tui.Pane{
			ID:      info.ID,
			Name:    info.Name,
			Command: paneDisplayCommand(info),
			Backend: info.Backend,
			Adapter: info.Adapter,
			Shell:   info.Shell,
		})
	}
	return panes, infos, nil
}

func buildStartupLogs(
	configuration config.Result,
	resolution agentResolution,
	infos []session.Info,
	configPath string,
) []string {
	logs := make([]string, 0, len(resolution.Warnings)+6)
	logs = append(logs, resolution.Warnings...)
	if configuration.Legacy {
		logs = append(logs, "Configuration historique détectée: les deux agents de démonstration restent actifs")
	}
	if configuration.Created {
		logs = append(logs, fmt.Sprintf("Configuration par défaut créée: %s", configPath))
	}
	if len(resolution.MockAgentNames) > 0 {
		logs = append(logs, "Mode simulation actif: "+strings.Join(resolution.MockAgentNames, ", "))
	}
	for _, info := range infos {
		if info.Shell {
			logs = append(logs, fmt.Sprintf(
				"Mode shell explicite actif pour %s: les métacaractères sont interprétés",
				info.Name,
			))
		}
	}
	logs = append(logs,
		fmt.Sprintf("%d agent(s) démarré(s) via %s", len(infos), effectiveBackendLabel(infos)),
		fmt.Sprintf("Adaptateur(s) actif(s): %s", effectiveAdapterLabel(infos)),
		fmt.Sprintf("%d patterns chargés depuis %s", len(configuration.Patterns), configPath),
		fmt.Sprintf(
			"Politiques: default_action=%s, dry_run=%t, %d règle(s)",
			configuration.Policies.DefaultAction,
			configuration.Policies.DryRun,
			len(configuration.Policies.Rules),
		),
	)
	if strings.Contains(strings.ToLower(effectiveBackendLabel(infos)), agent.BackendTmux) {
		logs = append(logs, fmt.Sprintf(
			"Sessions tmux: persist_on_exit=%t, cleanup_on_success=%t",
			configuration.Sessions.PersistOnExit,
			configuration.Sessions.CleanupOnSuccess,
		))
	}
	return logs
}

func effectiveAdapterLabel(infos []session.Info) string {
	set := make(map[string]struct{})
	for _, info := range infos {
		name := strings.ToUpper(strings.TrimSpace(info.Adapter))
		if name != "" {
			set[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "INCONNU"
	}
	return strings.Join(names, "/")
}

func effectiveBackendLabel(infos []session.Info) string {
	set := make(map[string]struct{})
	for _, info := range infos {
		name := strings.ToUpper(strings.TrimSpace(info.Backend))
		if name != "" {
			set[name] = struct{}{}
		}
	}
	names := make([]string, 0, len(set))
	for name := range set {
		names = append(names, name)
	}
	sort.Strings(names)
	if len(names) == 0 {
		return "BACKEND INCONNU"
	}
	return strings.Join(names, "/")
}

func paneDisplayCommand(info session.Info) string {
	if info.Shell {
		// The UI must make the interpreted mode unmistakable without echoing a
		// potentially sensitive script into the persistent supervisor history.
		return "[shell explicite]"
	}
	return info.DisplayCommand
}

// initialTerminalSize avoids starting fast-producing CLIs with an arbitrary
// PTY size. Bubble Tea remains authoritative for all subsequent resizes.
func initialTerminalSize() (width, height int) {
	const fallbackWidth = 80
	const fallbackHeight = 24

	for _, terminal := range []*os.File{os.Stdout, os.Stdin, os.Stderr} {
		rows, columns, err := pty.Getsize(terminal)
		if err == nil && rows > 0 && columns > 0 {
			return columns, rows
		}
	}
	return fallbackWidth, fallbackHeight
}
