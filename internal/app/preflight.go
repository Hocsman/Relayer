package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"strings"

	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/preflight"
)

// PreflightOptions selects the existing configuration inspected by the
// read-only doctor shared by command-line and desktop frontends.
type PreflightOptions struct {
	ConfigPath string
}

type preflightDependencies struct {
	getwd   func() (string, error)
	options preflight.Options
}

// RunPreflight builds the effective Relayer plan and inspects it without
// creating a configuration, opening an audit sink, constructing a terminal
// backend or starting an agent. Expected operational failures are represented
// by static checks rather than raw errors so presentation layers cannot leak
// configuration paths or user-controlled values.
func RunPreflight(ctx context.Context, options PreflightOptions) (preflight.Report, error) {
	return runPreflight(ctx, options, preflightDependencies{getwd: os.Getwd})
}

func runPreflight(
	ctx context.Context,
	options PreflightOptions,
	dependencies preflightDependencies,
) (preflight.Report, error) {
	coreOptions := dependencies.options
	failure := func(kind preflight.FailureKind) (preflight.Report, error) {
		return preflight.FailureReport(kind, coreOptions), nil
	}
	if ctx == nil {
		return preflight.Report{}, preflight.ErrNilContext
	}
	if err := ctx.Err(); err != nil {
		return preflight.Report{}, err
	}

	configPath := strings.TrimSpace(options.ConfigPath)
	if configPath == "" {
		configPath = config.DefaultPath
	}
	configuration, err := config.LoadExisting(configPath)
	if err != nil {
		return failure(classifyPreflightConfigFailure(err))
	}

	getwd := dependencies.getwd
	if getwd == nil {
		getwd = os.Getwd
	}
	workingDirectory, err := getwd()
	if err != nil {
		return failure(preflight.FailureWorkingDirectory)
	}
	resolution, err := resolveAgentPlans(configuration, optionsFromDesktop(), workingDirectory)
	if err != nil {
		return failure(preflight.FailureAgentResolution)
	}

	report, err := preflight.Check(ctx, preflight.Input{
		Configuration: configuration,
		Specs:         resolution.Specs,
		DemoAgents:    len(resolution.MockAgentNames) > 0,
	}, coreOptions)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return preflight.Report{}, err
		}
		return failure(preflight.FailurePreflightInternal)
	}
	return report, nil
}

func classifyPreflightConfigFailure(err error) preflight.FailureKind {
	if !errors.Is(err, config.ErrExistingConfigRead) {
		return preflight.FailureConfigInvalid
	}
	if errors.Is(err, fs.ErrNotExist) {
		return preflight.FailureConfigMissing
	}
	return preflight.FailureConfigUnreadable
}
