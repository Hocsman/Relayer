package app

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
)

const (
	defaultDesktopColumns = 120
	defaultDesktopRows    = 32
)

// ErrCleanupUncertain marks a failed runtime start whose rollback could not
// prove that every partially started session was removed. Desktop controllers
// must quarantine the lifecycle and must not launch a candidate or rollback
// run while this sentinel is present.
var ErrCleanupUncertain = errors.New("nettoyage du runtime desktop non confirmé")

// DesktopOptions configures the headless runtime used by desktop frontends.
// It deliberately does not inherit the deprecated pane flags from the CLI.
type DesktopOptions struct {
	ConfigPath  string
	InitialSize terminal.Size
	Diagnostics io.Writer
}

// DesktopPlan is an immutable, Go-only preflight result. Preparing a plan may
// read configuration and resolve executables, but it never opens audit files,
// terminal backends or child processes.
type DesktopPlan struct {
	configuration config.Result
	configPath    string
	resolution    agentResolution
	selection     backendResolution
	policyEngine  *policy.Engine
	registry      *adapters.Registry
	dependencies  backendDependencies
	initialSize   terminal.Size
	diagnostics   io.Writer
}

// DesktopSession is display-safe startup metadata. Shell bodies, environment
// values and prompt matches never cross this boundary.
type DesktopSession struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Command string `json:"command"`
	Backend string `json:"backend"`
	Adapter string `json:"adapter"`
	Shell   bool   `json:"shell"`
}

// DesktopMetadata contains non-sensitive run settings suitable for a GUI.
type DesktopMetadata struct {
	RunID          string `json:"runID"`
	ConfigPath     string `json:"configPath"`
	ConfigRevision string `json:"-"`
	Backend        string `json:"backend"`
	PolicyAction   string `json:"policyAction"`
	PolicyDryRun   bool   `json:"policyDryRun"`
	AuditEnabled   bool   `json:"auditEnabled"`
	AuditMode      string `json:"auditMode"`
	AuditPath      string `json:"auditPath,omitempty"`
	Configuration  bool   `json:"configurationCreated"`
}

// DesktopRuntime owns one complete Relayer run without assuming a terminal
// UI. Consumers remain responsible for reducing Events and for never exposing
// free-form sensitive event fields.
type DesktopRuntime struct {
	ctx    context.Context
	cancel context.CancelFunc

	router        *backendRouter
	events        chan session.Event
	policyEngine  *policy.Engine
	auditor       *audit.Recorder
	configuration config.Result
	configPath    string
	sessions      []DesktopSession
	infos         []session.Info
	startupLogs   []string
	runID         string
	strictStop    bool
	strictStopped bool

	closeMu   sync.Mutex
	quiesceMu sync.Mutex
	closed    bool
	closeErr  error
}

// PrepareDesktopRuntime performs every validation that can safely happen
// before an existing desktop run is stopped.
func PrepareDesktopRuntime(options DesktopOptions) (*DesktopPlan, error) {
	configPath := strings.TrimSpace(options.ConfigPath)
	if configPath == "" {
		configPath = config.DefaultPath
	}
	diagnostics := options.Diagnostics
	if diagnostics == nil {
		diagnostics = io.Discard
	}

	configuration, err := config.LoadOrCreate(configPath)
	if err != nil {
		return nil, err
	}

	workingDirectory, err := os.Getwd()
	if err != nil {
		return nil, fmt.Errorf("lecture du dossier courant: %w", err)
	}
	resolution, err := resolveAgentPlans(configuration, optionsFromDesktop(), workingDirectory)
	if err != nil {
		return nil, err
	}
	policyEngine, err := policy.New(configuration.Policies)
	if err != nil {
		return nil, fmt.Errorf("initialisation des politiques: %w", err)
	}
	if err := validatePolicyAgentIDs(policyEngine.Config(), resolution.Specs); err != nil {
		return nil, err
	}
	registry, err := adapters.NewRegistry(configuration.Patterns)
	if err != nil {
		return nil, fmt.Errorf("initialisation des adaptateurs: %w", err)
	}
	resolution.Specs, err = resolveAgentAdapters(resolution.Specs, registry)
	if err != nil {
		return nil, err
	}

	dependencies := productionBackendDependencies()
	backendSelection, err := resolveAgentBackends(
		context.Background(), resolution.Specs, dependencies.lookup, dependencies.probeTmux)
	if err != nil {
		return nil, err
	}
	resolution.Specs = backendSelection.Specs
	resolution.Warnings = append(resolution.Warnings, backendSelection.Warnings...)
	size := options.InitialSize.Normalize()
	if options.InitialSize.Columns <= 0 {
		size.Columns = defaultDesktopColumns
	}
	if options.InitialSize.Rows <= 0 {
		size.Rows = defaultDesktopRows
	}
	return &DesktopPlan{
		configuration: configuration,
		configPath:    configPath,
		resolution:    resolution,
		selection:     backendSelection,
		policyEngine:  policyEngine,
		registry:      registry,
		dependencies:  dependencies,
		initialSize:   size,
		diagnostics:   diagnostics,
	}, nil
}

// StartDesktopRuntime starts an immutable preflight plan under the externally
// reserved identity shared by GUI events, tmux ownership and audit records.
func StartDesktopRuntime(parent context.Context, plan *DesktopPlan, runID string) (_ *DesktopRuntime, returnErr error) {
	if plan == nil {
		return nil, errors.New("plan desktop nil")
	}
	if !validDesktopRunID(runID) {
		return nil, errors.New("run_id desktop invalide")
	}
	if parent == nil {
		parent = context.Background()
	}
	ctx, cancel := context.WithCancel(parent)
	runtime := &DesktopRuntime{
		ctx:           ctx,
		cancel:        cancel,
		configuration: plan.configuration,
		configPath:    plan.configPath,
		policyEngine:  plan.policyEngine,
		runID:         runID,
	}
	for _, warning := range plan.resolution.Warnings {
		_, _ = fmt.Fprintln(plan.diagnostics, warning)
	}

	auditor, err := initializeAuditForRun(plan.configuration.Audit, plan.dependencies, runID)
	if err != nil {
		cancel()
		return nil, fmt.Errorf("initialisation du journal d'audit: %w", err)
	}
	runtime.auditor = auditor
	cleanup := true
	defer func() {
		if !cleanup {
			return
		}
		if abortErr := runtime.abortInitialization(); abortErr != nil {
			returnErr = errors.Join(returnErr, ErrCleanupUncertain, abortErr)
		}
	}()

	if err := auditor.Record(audit.Entry{
		Kind:       audit.KindRunStarted,
		DecisionBy: audit.DecisionBySystem,
		Outcome:    audit.OutcomeStarted,
	}); err != nil {
		return nil, fmt.Errorf("écriture du démarrage du run dans l'audit: %w", err)
	}

	events := make(chan session.Event, defaultEventCapacity)
	router, err := buildBackendRouterForRun(
		ctx,
		events,
		plan.registry,
		defaultRingCapacity,
		plan.selection,
		plan.configuration.Sessions,
		plan.dependencies,
		runID,
	)
	if err != nil {
		_ = auditor.Record(audit.Entry{
			Kind:       audit.KindBackendError,
			DecisionBy: audit.DecisionBySystem,
			Outcome:    audit.OutcomeFailed,
			Reason:     "backend_initialization_failed",
		})
		return nil, err
	}
	runtime.router = router
	runtime.events = events

	for _, spec := range plan.resolution.Specs {
		info, startErr := router.Start(ctx, spec, plan.initialSize)
		if startErr != nil {
			_ = auditor.Record(audit.Entry{
				Kind:       audit.KindBackendError,
				AgentID:    spec.ID,
				Backend:    spec.Backend,
				Adapter:    spec.Adapter,
				DecisionBy: audit.DecisionBySystem,
				Outcome:    audit.OutcomeFailed,
				Reason:     "session_start_failed",
			})
			return nil, fmt.Errorf("démarrage de l'agent %q: %w", spec.ID, startErr)
		}
		runtime.infos = append(runtime.infos, info)
		runtime.sessions = append(runtime.sessions, DesktopSession{
			ID:      info.ID,
			Name:    info.Name,
			Command: desktopCommandLabel(spec.Command, spec.Shell),
			Backend: info.Backend,
			Adapter: info.Adapter,
			Shell:   info.Shell,
		})
		if err := auditor.Record(audit.Entry{
			Kind:       audit.KindSessionStarted,
			SessionID:  info.ID,
			AgentID:    spec.ID,
			Backend:    info.Backend,
			Adapter:    info.Adapter,
			DecisionBy: audit.DecisionBySystem,
			Outcome:    audit.OutcomeStarted,
		}); err != nil {
			return nil, fmt.Errorf("audit du démarrage de l'agent %q: %w", spec.ID, err)
		}
	}

	runtime.startupLogs = buildStartupLogs(plan.configuration, plan.resolution, runtime.infos, plan.configPath)
	if auditor.Enabled() {
		runtime.startupLogs = append(runtime.startupLogs, fmt.Sprintf(
			"Audit local: mode=%s, fichier=%s",
			plan.configuration.Audit.Mode,
			auditor.Path(),
		))
	} else {
		runtime.startupLogs = append(runtime.startupLogs, "Audit local désactivé")
	}

	cleanup = false
	return runtime, nil
}

// NewDesktopRuntime preserves the historical one-call API.
func NewDesktopRuntime(parent context.Context, options DesktopOptions) (*DesktopRuntime, error) {
	plan, err := PrepareDesktopRuntime(options)
	if err != nil {
		return nil, err
	}
	runID, err := newDesktopRunID()
	if err != nil {
		return nil, err
	}
	return StartDesktopRuntime(parent, plan, runID)
}

func newDesktopRunID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", errors.New("génération du run_id desktop impossible")
	}
	return hex.EncodeToString(data[:]), nil
}

func validDesktopRunID(value string) bool {
	value = strings.TrimSpace(value)
	if len(value) == 0 || len(value) > 128 {
		return false
	}
	for index, character := range value {
		if (character >= 'a' && character <= 'z') ||
			(character >= 'A' && character <= 'Z') ||
			(character >= '0' && character <= '9') ||
			(index > 0 && (character == '.' || character == '_' || character == '-')) {
			continue
		}
		return false
	}
	return true
}

// optionsFromDesktop preserves the configured agent list and deliberately
// leaves both deprecated CLI overrides unset.
func optionsFromDesktop() options { return options{} }

// desktopCommandLabel deliberately omits argv values and shell bodies. Tokens
// and other credentials are commonly passed as command-line arguments; those
// values must never cross the native/WebView boundary merely for decoration.
func desktopCommandLabel(command []string, shell string) string {
	if strings.TrimSpace(shell) != "" {
		return "[shell explicite]"
	}
	if len(command) == 0 {
		return "[commande argv]"
	}
	executable := filepath.Base(strings.TrimSpace(command[0]))
	if executable == "" || executable == "." || executable == string(filepath.Separator) {
		return "[commande argv]"
	}
	redacted := audit.Redact(executable)
	if strings.Contains(redacted, "[REDACTED]") {
		return "[commande argv]"
	}
	return redacted
}

func (r *DesktopRuntime) Events() <-chan session.Event {
	if r == nil {
		return nil
	}
	return r.events
}

func (r *DesktopRuntime) Sessions() []DesktopSession {
	if r == nil {
		return nil
	}
	return append([]DesktopSession(nil), r.sessions...)
}

func (r *DesktopRuntime) StartupLogs() []string {
	if r == nil {
		return nil
	}
	return append([]string(nil), r.startupLogs...)
}

func (r *DesktopRuntime) Metadata() DesktopMetadata {
	if r == nil {
		return DesktopMetadata{}
	}
	metadata := DesktopMetadata{
		RunID:          r.runID,
		ConfigPath:     r.configPath,
		ConfigRevision: r.configuration.Revision,
		Backend:        effectiveBackendLabel(r.infos),
		PolicyAction:   string(r.configuration.Policies.DefaultAction),
		PolicyDryRun:   r.configuration.Policies.DryRun,
		AuditEnabled:   r.auditor != nil && r.auditor.Enabled(),
		AuditMode:      string(r.configuration.Audit.Mode),
		Configuration:  r.configuration.Created,
	}
	if metadata.AuditEnabled {
		metadata.AuditPath = r.auditor.Path()
	}
	return metadata
}

func (r *DesktopRuntime) Output(sessionID string) (string, error) {
	if err := r.available(); err != nil {
		return "", err
	}
	return r.router.Output(sessionID)
}

func (r *DesktopRuntime) Snapshot(ctx context.Context, sessionID string) (terminal.Snapshot, error) {
	if err := r.available(); err != nil {
		return terminal.Snapshot{}, err
	}
	return r.router.Snapshot(ctx, sessionID)
}

func (r *DesktopRuntime) PendingEvent(ctx context.Context, sessionID string) (*adapters.Event, error) {
	if err := r.available(); err != nil {
		return nil, err
	}
	return r.router.PendingEvent(ctx, sessionID)
}

// SendLine submits ordinary single-line text through the backend's atomic
// processor boundary. It never falls back to raw Send or decision resolution.
func (r *DesktopRuntime) SendLine(ctx context.Context, sessionID, line string) error {
	if err := r.available(); err != nil {
		return err
	}
	return r.router.SendLine(ctx, sessionID, line)
}

func (r *DesktopRuntime) Evaluate(event adapters.Event) policy.Evaluation {
	if r == nil || r.policyEngine == nil {
		return policy.Evaluation{Action: policy.ActionAsk, ProposedAction: policy.ActionAsk, Reason: policy.ReasonNoEngine}
	}
	return r.policyEngine.Evaluate(event)
}

func (r *DesktopRuntime) ApplyDecision(
	ctx context.Context,
	sessionID string,
	event adapters.Event,
	decision adapters.Decision,
	manualInput string,
) error {
	if err := r.available(); err != nil {
		return err
	}
	return r.router.ApplyDecision(ctx, sessionID, event, decision, manualInput)
}

func (r *DesktopRuntime) Resize(ctx context.Context, sessionID string, size terminal.Size) error {
	if err := r.available(); err != nil {
		return err
	}
	return r.router.Resize(ctx, sessionID, size.Normalize())
}

func (r *DesktopRuntime) Stop(ctx context.Context, sessionID string) error {
	if err := r.available(); err != nil {
		return err
	}
	return r.router.Stop(ctx, sessionID)
}

// RecordAudit is a synchronous, fail-closed persistence boundary for desktop
// decisions. Callers must not perform a backend write when it returns an error.
func (r *DesktopRuntime) RecordAudit(entry audit.Entry) error {
	if r == nil || r.auditor == nil {
		return errors.New("journal d'audit indisponible")
	}
	return r.auditor.Record(entry)
}

// BeginShutdown stops backend I/O while deliberately keeping the audit
// recorder open. Desktop frontends use this phase to unblock and join all
// in-flight decision goroutines before Close writes the final run records.
func (r *DesktopRuntime) BeginShutdown(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.cancel()
	r.quiesceMu.Lock()
	defer r.quiesceMu.Unlock()
	if r.router == nil {
		return nil
	}
	return r.router.Close(ctx)
}

// BeginRestart is the stricter desktop transition used before another run is
// allowed to start. Unlike a normal application shutdown it explicitly stops
// every session, including tmux sessions configured to persist on exit. A
// non-nil result means cleanup is uncertain and callers must not start a
// replacement run.
func (r *DesktopRuntime) BeginRestart(ctx context.Context) error {
	if r == nil {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	r.closeMu.Lock()
	defer r.closeMu.Unlock()
	if r.closed {
		return terminal.ErrClosed
	}

	r.quiesceMu.Lock()
	defer r.quiesceMu.Unlock()
	r.strictStop = true
	stopErr := r.stopAllSessions(ctx)
	r.strictStopped = stopErr == nil
	if stopErr == nil && r.auditor != nil {
		for _, info := range r.infos {
			if err := r.auditor.Record(audit.Entry{
				Kind:       audit.KindSessionFinished,
				SessionID:  info.ID,
				AgentID:    info.ID,
				Backend:    info.Backend,
				Adapter:    info.Adapter,
				DecisionBy: audit.DecisionBySystem,
				Outcome:    audit.OutcomeFinished,
				Reason:     "restart_stop_confirmed",
			}); err != nil {
				stopErr = errors.Join(stopErr, fmt.Errorf("audit de l'arrêt strict de la session %q: %w", info.ID, err))
			}
		}
	}
	r.cancel()
	if r.router != nil {
		stopErr = errors.Join(stopErr, r.router.Close(ctx))
	}
	return stopErr
}

func (r *DesktopRuntime) stopAllSessions(ctx context.Context) error {
	if r == nil || r.router == nil {
		return nil
	}
	errorsByIndex := make([]error, len(r.infos))
	var group sync.WaitGroup
	for index, info := range r.infos {
		index, info := index, info
		group.Add(1)
		go func() {
			defer group.Done()
			errorsByIndex[index] = r.router.Stop(ctx, info.ID)
		}()
	}
	group.Wait()
	var result error
	for index, err := range errorsByIndex {
		if err != nil {
			result = errors.Join(result, fmt.Errorf("arrêt strict de la session %q: %w", r.infos[index].ID, err))
		}
	}
	return result
}

func (r *DesktopRuntime) available() error {
	if r == nil || r.router == nil {
		return terminal.ErrUnavailable
	}
	r.closeMu.Lock()
	closed := r.closed
	r.closeMu.Unlock()
	if closed || r.ctx.Err() != nil {
		return terminal.ErrClosed
	}
	return nil
}

// Close stops supervision, closes every owned backend and then closes audit.
// It is idempotent and preserves the first complete shutdown result.
func (r *DesktopRuntime) Close(ctx context.Context) error {
	if r == nil {
		return nil
	}
	r.closeMu.Lock()
	defer r.closeMu.Unlock()
	if r.closed {
		return r.closeErr
	}
	r.closed = true
	r.cancel()

	var result error
	for _, info := range r.infos {
		if err := r.auditor.Record(audit.Entry{
			Kind:       audit.KindSupervisionFinished,
			SessionID:  info.ID,
			AgentID:    info.ID,
			Backend:    info.Backend,
			Adapter:    info.Adapter,
			DecisionBy: audit.DecisionBySystem,
			Outcome:    audit.OutcomeFinished,
			Reason:     "supervision_ended",
		}); err != nil {
			result = errors.Join(result, fmt.Errorf("audit de la fin de supervision: %w", err))
		}
	}
	if r.router != nil {
		r.quiesceMu.Lock()
		closeErr := r.router.Close(ctx)
		r.quiesceMu.Unlock()
		if closeErr != nil {
			result = errors.Join(result, fmt.Errorf("fermeture des backends: %w", closeErr))
		}
		for _, info := range r.infos {
			closed, known := r.router.backendCloseStatus(info.Backend)
			policy := r.configuration.Sessions
			if r.strictStop && r.strictStopped {
				policy.PersistOnExit = false
			}
			if r.strictStop && !r.strictStopped {
				known = false
			}
			outcome, reason := auditCleanupResult(info, policy, closed, known)
			if err := r.auditor.Record(audit.Entry{
				Kind:       audit.KindSessionCleanup,
				SessionID:  info.ID,
				AgentID:    info.ID,
				Backend:    info.Backend,
				Adapter:    info.Adapter,
				DecisionBy: audit.DecisionBySystem,
				Outcome:    outcome,
				Reason:     reason,
			}); err != nil {
				result = errors.Join(result, fmt.Errorf("audit du nettoyage de session: %w", err))
			}
		}
	}
	outcome := audit.OutcomeSucceeded
	if result != nil {
		outcome = audit.OutcomeFailed
	}
	if err := r.auditor.Record(audit.Entry{
		Kind:       audit.KindRunFinished,
		DecisionBy: audit.DecisionBySystem,
		Outcome:    outcome,
	}); err != nil {
		result = errors.Join(result, fmt.Errorf("audit de la fin du run: %w", err))
	}
	if err := r.auditor.Close(); err != nil {
		result = errors.Join(result, fmt.Errorf("fermeture du journal d'audit: %w", err))
	}
	r.closeErr = result
	return result
}

func (r *DesktopRuntime) abortInitialization() error {
	if r == nil {
		return nil
	}
	var result error
	if r.auditor != nil {
		for _, info := range r.infos {
			if err := r.auditor.Record(audit.Entry{
				Kind:       audit.KindSupervisionFinished,
				SessionID:  info.ID,
				AgentID:    info.ID,
				Backend:    info.Backend,
				Adapter:    info.Adapter,
				DecisionBy: audit.DecisionBySystem,
				Outcome:    audit.OutcomeFailed,
				Reason:     "supervision_ended",
			}); err != nil {
				result = errors.Join(result, err)
			}
		}
	}
	if r.router != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
		stopErr := r.stopAllSessions(ctx)
		r.strictStop = true
		r.strictStopped = stopErr == nil
		result = errors.Join(result, stopErr)
		r.cancel()
		result = errors.Join(result, r.router.Close(ctx))
		cancel()
		if r.auditor != nil {
			for _, info := range r.infos {
				closed, known := r.router.backendCloseStatus(info.Backend)
				policy := r.configuration.Sessions
				if r.strictStopped {
					policy.PersistOnExit = false
				} else {
					known = false
				}
				outcome, reason := auditCleanupResult(info, policy, closed, known)
				if err := r.auditor.Record(audit.Entry{
					Kind:       audit.KindSessionCleanup,
					SessionID:  info.ID,
					AgentID:    info.ID,
					Backend:    info.Backend,
					Adapter:    info.Adapter,
					DecisionBy: audit.DecisionBySystem,
					Outcome:    outcome,
					Reason:     reason,
				}); err != nil {
					result = errors.Join(result, err)
				}
			}
		}
	}
	if r.router == nil {
		r.cancel()
	}
	if r.auditor != nil {
		if err := r.auditor.Record(audit.Entry{
			Kind:       audit.KindRunFinished,
			DecisionBy: audit.DecisionBySystem,
			Outcome:    audit.OutcomeFailed,
		}); err != nil {
			result = errors.Join(result, err)
		}
		if err := r.auditor.Close(); err != nil {
			result = errors.Join(result, err)
		}
	}
	return result
}
