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
	"sync"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/notify"
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

var (
	serveHandlerMu sync.RWMutex
	serveHandler   func(arguments []string, output io.Writer, diagnostics io.Writer) error
)

// RegisterServeHandler registers an external handler for the "serve" subcommand.
func RegisterServeHandler(handler func(arguments []string, output io.Writer, diagnostics io.Writer) error) {
	serveHandlerMu.Lock()
	defer serveHandlerMu.Unlock()
	serveHandler = handler
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
	// `doctor` is a bare subcommand, so `version` reads like one too. Without
	// it the word fell through to the terminal interface and failed by asking
	// for a TTY — which is what running the released binary showed, since a
	// pipeline or a CI step has no controlling terminal to give it.
	versionRequested := len(arguments) == 1 && arguments[0] == "version"
	for _, argument := range arguments {
		if argument == "--version" || argument == "-version" {
			versionRequested = true
			break
		}
	}
	if versionRequested {
		if len(arguments) != 1 {
			return errors.New("the --version option must be used alone")
		}
		if output == nil {
			output = io.Discard
		}
		if _, err := fmt.Fprintln(output, buildversion.String()); err != nil {
			return fmt.Errorf("writing the version: %w", err)
		}
		return nil
	}
	if len(arguments) > 0 && arguments[0] == "doctor" {
		return runDoctor(arguments[1:], output, diagnostics, preflightRun)
	}
	if len(arguments) > 0 && arguments[0] == "audit" {
		return runAudit(arguments[1:], output, diagnostics)
	}
	if len(arguments) > 0 && arguments[0] == "serve" {
		serveHandlerMu.RLock()
		handler := serveHandler
		serveHandlerMu.RUnlock()
		if handler != nil {
			return handler(arguments[1:], output, diagnostics)
		}
		return errors.New("serve subcommand is not registered in this build")
	}
	return run(arguments, diagnostics, dependencies)
}

// tuiRunEndBudget bounds each phase of the end of a terminal run: the
// backends' stop, then the runtime's close. The desktop gives each phase the
// same 12 s, and the gateway its runEndBudget. The command used to give the
// backends' close session.StopBudget and three seconds more, 10.35 s on
// Windows, so a stop that fit before still fits.
const tuiRunEndBudget = 12 * time.Second

func run(arguments []string, diagnostics io.Writer, dependencies backendDependencies) error {
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	options, err := parseOptions(arguments, diagnostics)
	if err != nil {
		return err
	}
	terminalSize := dependencies.terminalSize
	if terminalSize == nil {
		terminalSize = initialTerminalSize
	}
	initialWidth, initialHeight := terminalSize()

	// The terminal command runs on the runtime the desktop and the gateway
	// share, which owns the backends, the journal, telemetry and the lifecycle,
	// and writes every run-level entry. Each agent starts at the size of its own
	// pane in the first layout, so a fast agent's first screen is already drawn
	// for the pane it is shown in; an agent the lifecycle starts again later
	// starts at the whole terminal, as it always has, and Bubble Tea resizes it
	// to its pane. Recording stays off: the recorder reports on diagnostics,
	// which is the screen the interface draws on.
	plan, err := prepareRuntime(runtimeRequest{
		configPath:   options.configPath,
		diagnostics:  diagnostics,
		cli:          options,
		dependencies: dependencies,
		initialSize:  terminal.Size{Columns: initialWidth, Rows: initialHeight},
		sizeFor: func(index, count int) terminal.Size {
			columns, rows := tui.AgentViewportSize(initialWidth, initialHeight, count, index)
			return terminal.Size{Columns: columns, Rows: rows}
		},
		recording: false,
	})
	if err != nil {
		return err
	}
	// One run ID names the run in the journal and in the tmux sessions' names.
	runID, err := newDesktopRunID()
	if err != nil {
		return err
	}
	// The run's context is its own, never one a signal ends: the run ends in the
	// order below, whatever made the interface return.
	runCtx, cancelRun := context.WithCancel(context.Background())
	defer cancelRun()
	// A startup that fails part-way stops the agents it already started, those
	// tmux keeps on exit included, and ends the journal itself.
	rt, err := StartDesktopRuntime(runCtx, plan, runID)
	if err != nil {
		return err
	}

	panes := make([]tui.Pane, 0, len(rt.infos))
	for _, info := range rt.infos {
		panes = append(panes, tui.Pane{
			ID:      info.ID,
			Name:    info.Name,
			Command: paneDisplayCommand(info),
			Backend: info.Backend,
			Adapter: info.Adapter,
			Shell:   info.Shell,
		})
	}
	// The interface still supervises on its own: it evaluates the policy with a
	// tracker of its own, and journals every prompt, decision and delivery
	// straight to the run's journal. Only the run around it is the runtime's.
	application, runErr := tui.NewModelWithPolicyAndAudit(
		&tuiBackendAdapter{router: rt.router, lifecycle: rt.lifecycle},
		rt.events,
		panes,
		initialWidth,
		initialHeight,
		rt.startupLogs,
		rt.policyEngine,
		rt.auditor,
	)
	if runErr == nil {
		application.SetNotifier(notify.New(rt.configuration.Notifications, diagnostics))
		program := dependencies.newProgram(
			application,
			tea.WithAltScreen(),
			tea.WithMouseCellMotion(),
		)
		_, runErr = program.Run()
	}

	// The run ends as the desktop and the gateway end theirs, each phase
	// bounded: the backends stop while the journal is still open, the run's
	// context ends, and only then does the runtime close, which writes the run's
	// last entries and closes the journal. A run whose interface failed, or
	// whose backends did not all stop, ends failed in the journal.
	beginCtx, cancelBegin := context.WithTimeout(context.Background(), tuiRunEndBudget)
	beginErr := rt.BeginShutdown(beginCtx)
	cancelBegin()
	if beginErr != nil {
		beginErr = fmt.Errorf("shut down the backends: %w", beginErr)
	}
	cancelRun()
	closeCtx, cancelClose := context.WithTimeout(context.Background(), tuiRunEndBudget)
	closeErr := rt.closeWithCause(closeCtx, errors.Join(runErr, beginErr))
	cancelClose()
	return errors.Join(runErr, beginErr, closeErr)
}

// uiProgram is the part of a Bubble Tea program that run depends on. run only
// runs it. Send, the one way to hand a running program a message from another
// goroutine, is in the interface too, so that a stand-in has to take such
// messages as the real program does. Naming the interface lets a test replace
// the program and still take run through every entry the journal gets around
// it.
type uiProgram interface {
	Run() (tea.Model, error)
	Send(tea.Msg)
}

var _ uiProgram = (*tea.Program)(nil)

// newTeaProgram builds the program the command runs, a real Bubble Tea one.
// productionBackendDependencies hands it to run as newProgram.
func newTeaProgram(model tea.Model, options ...tea.ProgramOption) uiProgram {
	return tea.NewProgram(model, options...)
}

func initializeAudit(configuration audit.Config, dependencies backendDependencies) (*audit.Recorder, error) {
	if dependencies.newAudit != nil {
		recorder, err := dependencies.newAudit(configuration)
		if err != nil {
			return nil, err
		}
		if recorder == nil {
			return nil, errors.New("audit factory returned a nil recorder")
		}
		return recorder, nil
	}
	if configuration.Enabled && configuration.Mode != audit.ModeOff {
		return nil, errors.New("the audit factory is unavailable")
	}
	return audit.NewRecorder(configuration, nil, nil, nil)
}

func initializeAuditForRun(configuration audit.Config, dependencies backendDependencies, runID string) (*audit.Recorder, error) {
	if strings.TrimSpace(runID) == "" {
		return nil, errors.New("empty desktop runtime run_id")
	}
	if dependencies.newAuditForRun != nil {
		recorder, err := dependencies.newAuditForRun(configuration, runID)
		if err != nil {
			return nil, err
		}
		if recorder == nil {
			return nil, errors.New("desktop audit factory returned a nil recorder")
		}
		return recorder, nil
	}
	if dependencies.newAudit != nil {
		// Test dependencies predating externally reserved run IDs remain usable,
		// but production always supplies newAuditForRun.
		return initializeAudit(configuration, dependencies)
	}
	if configuration.Enabled && configuration.Mode != audit.ModeOff {
		return nil, errors.New("the desktop audit factory is unavailable")
	}
	return audit.Open(configuration, audit.WithRunID(runID))
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
					"policy %q: unknown agent_id %q",
					rule.Name,
					configuredID,
				)
			}
		}
	}
	return nil
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
		logs = append(logs, "Legacy configuration detected: the two demonstration agents stay active")
	}
	if configuration.Created {
		logs = append(logs, fmt.Sprintf("Default configuration created: %s", configPath))
	}
	if len(resolution.MockAgentNames) > 0 {
		logs = append(logs, "Simulation mode active: "+strings.Join(resolution.MockAgentNames, ", "))
	}
	for _, info := range infos {
		if info.Shell {
			logs = append(logs, fmt.Sprintf(
				"Explicit shell mode active for %s: metacharacters are interpreted",
				info.Name,
			))
		}
	}
	logs = append(logs,
		fmt.Sprintf("%d agent(s) started via %s", len(infos), effectiveBackendLabel(infos)),
		fmt.Sprintf("Active adapter(s): %s", effectiveAdapterLabel(infos)),
		fmt.Sprintf("%d patterns loaded from %s", len(configuration.Patterns), configPath),
		fmt.Sprintf(
			"Policies: default_action=%s, dry_run=%t, %d rule(s)",
			configuration.Policies.DefaultAction,
			configuration.Policies.DryRun,
			len(configuration.Policies.Rules),
		),
	)
	if strings.Contains(strings.ToLower(effectiveBackendLabel(infos)), agent.BackendTmux) {
		logs = append(logs, fmt.Sprintf(
			"tmux sessions: persist_on_exit=%t, cleanup_on_success=%t",
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
		return "UNKNOWN"
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
		return "UNKNOWN BACKEND"
	}
	return strings.Join(names, "/")
}

func paneDisplayCommand(info session.Info) string {
	if info.Shell {
		// The UI must make the interpreted mode unmistakable without echoing a
		// potentially sensitive script into the persistent supervisor history.
		return "[explicit shell]"
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
