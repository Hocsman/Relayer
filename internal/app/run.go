// Package app composes Relayer's configuration, PTY sessions, and terminal UI.
// Keeping this wiring separate lets both the canonical cmd/relayer entrypoint
// and the documented root compatibility entrypoint stay tiny.
package app

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/tui"
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
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	options, err := parseOptions(arguments, diagnostics)
	if err != nil {
		return err
	}

	configuration, err := config.Load(options.configPath)
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
	for _, warning := range resolution.Warnings {
		fmt.Fprintln(diagnostics, warning)
	}
	initialWidth, initialHeight := initialTerminalSize()

	events := make(chan session.Event, defaultEventCapacity)
	manager, err := session.NewManager(
		context.Background(),
		events,
		configuration.Patterns,
		defaultRingCapacity,
	)
	if err != nil {
		return err
	}
	defer manager.Close()

	panes, infos, err := startAgentSessions(
		manager,
		resolution.Specs,
		initialWidth,
		initialHeight,
	)
	if err != nil {
		return err
	}

	startupLogs := buildStartupLogs(configuration, resolution, infos, options.configPath)
	application, err := tui.NewModel(
		manager,
		events,
		panes,
		initialWidth,
		initialHeight,
		startupLogs,
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

type sessionStarter interface {
	Start(spec agent.Spec, columns, rows int) (session.Info, error)
	Close()
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
	panes := make([]tui.Pane, 0, len(specs))
	infos := make([]session.Info, 0, len(specs))
	for index, spec := range specs {
		columns, rows := tui.AgentViewportSize(
			initialWidth,
			initialHeight,
			len(specs),
			index,
		)
		info, startErr := owner.Start(spec, columns, rows)
		if startErr != nil {
			owner.Close()
			return nil, nil, fmt.Errorf("démarrage de l'agent %q: %w", spec.ID, startErr)
		}
		infos = append(infos, info)
		panes = append(panes, tui.Pane{
			ID:      info.ID,
			Name:    info.Name,
			Command: paneDisplayCommand(info),
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
		fmt.Sprintf("%d agent(s) PTY démarré(s)", len(infos)),
		fmt.Sprintf("%d patterns chargés depuis %s", len(configuration.Patterns), configPath),
	)
	return logs
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
