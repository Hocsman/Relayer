// Package app composes Relayer's configuration, PTY sessions, and terminal UI.
// Keeping this wiring separate lets both the canonical cmd/relayer entrypoint
// and the documented root compatibility entrypoint stay tiny.
package app

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

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

type options struct {
	pane1      string
	pane2      string
	configPath string
}

func parseOptions(arguments []string, diagnostics io.Writer) (options, error) {
	flags := flag.NewFlagSet("relayer", flag.ContinueOnError)
	flags.SetOutput(diagnostics)
	pane1 := flags.String("pane1", "", "commande shell du premier agent (mock si omise)")
	pane2 := flags.String("pane2", "", "commande shell du second agent (mock si omise)")
	configPath := flags.String("config", config.DefaultPath, "fichier YAML des patterns d'interception")
	if err := flags.Parse(arguments); err != nil {
		return options{}, err
	}
	return options{pane1: *pane1, pane2: *pane2, configPath: *configPath}, nil
}

// Run parses the public CLI, initializes the process owner and starts Bubble
// Tea. It never returns while sessions are still owned by the manager.
func Run(arguments []string, diagnostics io.Writer) error {
	options, err := parseOptions(arguments, diagnostics)
	if err != nil {
		return err
	}
	pane1Command, pane1UsesMock := resolvePaneCommand(options.pane1)
	pane2Command, pane2UsesMock := resolvePaneCommand(options.pane2)

	configuration, err := config.Load(options.configPath)
	if err != nil {
		return err
	}
	initialLayout := initialTerminalLayout()

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

	first, err := manager.Start(
		"Agent A (Claude)",
		pane1Command,
		initialLayout.AgentViewportWidths[0],
		initialLayout.AgentViewportHeight,
	)
	if err != nil {
		return err
	}
	second, err := manager.Start(
		"Agent B (Local)",
		pane2Command,
		initialLayout.AgentViewportWidths[1],
		initialLayout.AgentViewportHeight,
	)
	if err != nil {
		return err
	}

	startupLogs := make([]string, 0, 3)
	mockAgents := make([]string, 0, 2)
	if pane1UsesMock {
		mockAgents = append(mockAgents, "Agent A")
	}
	if pane2UsesMock {
		mockAgents = append(mockAgents, "Agent B")
	}
	if len(mockAgents) > 0 {
		startupLogs = append(startupLogs, "Mode simulation actif: "+strings.Join(mockAgents, ", "))
	}
	if configuration.Created {
		startupLogs = append(startupLogs, fmt.Sprintf("Configuration par défaut créée: %s", options.configPath))
	}
	startupLogs = append(startupLogs, fmt.Sprintf(
		"%d patterns chargés depuis %s",
		len(configuration.Patterns),
		options.configPath,
	))

	application := tui.NewModel(
		manager,
		events,
		[2]tui.Pane{
			{ID: first.ID, Name: first.Name, Command: first.Command},
			{ID: second.ID, Name: second.Name, Command: second.Command},
		},
		initialLayout.Width,
		initialLayout.Height,
		startupLogs,
	)
	program := tea.NewProgram(
		application,
		tea.WithAltScreen(),
		tea.WithMouseCellMotion(),
	)
	_, err = program.Run()
	return err
}

// initialTerminalLayout avoids starting fast-producing CLIs with an arbitrary
// PTY size. Bubble Tea remains authoritative for all subsequent resizes.
func initialTerminalLayout() tui.Geometry {
	const fallbackWidth = 80
	const fallbackHeight = 24

	for _, terminal := range []*os.File{os.Stdout, os.Stdin, os.Stderr} {
		rows, columns, err := pty.Getsize(terminal)
		if err == nil && rows > 0 && columns > 0 {
			return tui.CalculateLayout(columns, rows)
		}
	}
	return tui.CalculateLayout(fallbackWidth, fallbackHeight)
}
