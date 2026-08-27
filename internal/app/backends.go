package app

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/ptybackend"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/Hocsman/Relayer/internal/tmuxbackend"
)

type executableLookup func(string) (string, error)

type backendDependencies struct {
	lookup         executableLookup
	newAudit       func(audit.Config) (*audit.Recorder, error)
	newAuditForRun func(audit.Config, string) (*audit.Recorder, error)
	newPTY         func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error)
	newTmux        func(context.Context, chan<- session.Event, *adapters.Registry, int, tmuxbackend.Options) (terminal.Backend, error)
}

func productionBackendDependencies() backendDependencies {
	return backendDependencies{
		lookup: exec.LookPath,
		newAudit: func(configuration audit.Config) (*audit.Recorder, error) {
			return audit.Open(configuration)
		},
		newAuditForRun: func(configuration audit.Config, runID string) (*audit.Recorder, error) {
			return audit.Open(configuration, audit.WithRunID(runID))
		},
		newPTY: func(
			ctx context.Context,
			events chan<- session.Event,
			registry *adapters.Registry,
			ringCapacity int,
		) (terminal.Backend, error) {
			return ptybackend.NewWithRegistry(ctx, events, registry, ringCapacity)
		},
		newTmux: func(
			ctx context.Context,
			events chan<- session.Event,
			registry *adapters.Registry,
			ringCapacity int,
			options tmuxbackend.Options,
		) (terminal.Backend, error) {
			return tmuxbackend.NewManagerWithRegistry(ctx, events, registry, ringCapacity, options)
		},
	}
}

// resolveAgentAdapters makes every runtime spec concrete before any terminal
// backend is constructed. Explicit experimental placeholders fail clearly;
// executable-name detection only selects implementations present in Registry.
func resolveAgentAdapters(specs []agent.Spec, registry *adapters.Registry) ([]agent.Spec, error) {
	resolved := cloneAgentSpecs(specs)
	for index := range resolved {
		spec := &resolved[index]
		executable := ""
		if len(spec.Command) > 0 {
			executable = spec.Command[0]
		}
		adapter, _, err := registry.Resolve(spec.Adapter, executable)
		if err != nil {
			return nil, fmt.Errorf("adaptateur de l'agent %q: %w", spec.ID, err)
		}
		spec.Adapter = adapter.ID()
	}
	return resolved, nil
}

type backendResolution struct {
	Specs        []agent.Spec
	TmuxPath     string
	NeedsPTY     bool
	NeedsTmux    bool
	UsedAuto     bool
	AutoFallback bool
	Warnings     []string
}

// resolveAgentBackends replaces every auto selector with a concrete backend
// before any process starts. An explicitly requested tmux backend fails early
// when the executable is absent; auto records a visible PTY fallback instead.
func resolveAgentBackends(specs []agent.Spec, lookup executableLookup) (backendResolution, error) {
	if lookup == nil {
		lookup = exec.LookPath
	}
	result := backendResolution{Specs: cloneAgentSpecs(specs)}
	var (
		lookedUp  bool
		tmuxPath  string
		lookupErr error
	)
	resolveTmux := func() (string, error) {
		if !lookedUp {
			lookedUp = true
			tmuxPath, lookupErr = lookup("tmux")
		}
		return tmuxPath, lookupErr
	}

	for index := range result.Specs {
		spec := &result.Specs[index]
		switch spec.Backend {
		case agent.BackendPTY:
			result.NeedsPTY = true
		case agent.BackendTmux:
			path, err := resolveTmux()
			if err != nil {
				return backendResolution{}, fmt.Errorf(
					"backend tmux demandé pour l'agent %q, mais le binaire tmux est introuvable: %w",
					spec.ID,
					fmt.Errorf("%w: %v", tmuxbackend.ErrTmuxNotFound, err),
				)
			}
			result.TmuxPath = path
			result.NeedsTmux = true
		case agent.BackendAuto:
			result.UsedAuto = true
			path, err := resolveTmux()
			if err == nil {
				spec.Backend = agent.BackendTmux
				result.TmuxPath = path
				result.NeedsTmux = true
				continue
			}
			spec.Backend = agent.BackendPTY
			result.NeedsPTY = true
			result.AutoFallback = true
		default:
			return backendResolution{}, fmt.Errorf("backend non résolu %q pour l'agent %q", spec.Backend, spec.ID)
		}
	}

	if result.UsedAuto {
		if result.AutoFallback {
			result.Warnings = append(result.Warnings,
				"Backend auto: tmux est indisponible, repli explicite sur PTY.",
			)
		} else {
			result.Warnings = append(result.Warnings,
				"Backend auto: tmux détecté et sélectionné.",
			)
		}
	}
	return result, nil
}

func cloneAgentSpecs(specs []agent.Spec) []agent.Spec {
	cloned := make([]agent.Spec, len(specs))
	for index, spec := range specs {
		cloned[index] = spec
		cloned[index].Command = append([]string(nil), spec.Command...)
		if spec.Env != nil {
			cloned[index].Env = make(map[string]string, len(spec.Env))
			for name, value := range spec.Env {
				cloned[index].Env[name] = value
			}
		}
	}
	return cloned
}

func buildBackendRouter(
	parent context.Context,
	events chan<- session.Event,
	registry *adapters.Registry,
	ringCapacity int,
	selection backendResolution,
	policy config.SessionPolicy,
	dependencies backendDependencies,
) (*backendRouter, error) {
	return buildBackendRouterForRun(parent, events, registry, ringCapacity, selection, policy, dependencies, "")
}

func buildBackendRouterForRun(
	parent context.Context,
	events chan<- session.Event,
	registry *adapters.Registry,
	ringCapacity int,
	selection backendResolution,
	policy config.SessionPolicy,
	dependencies backendDependencies,
	runID string,
) (*backendRouter, error) {
	defaults := productionBackendDependencies()
	if dependencies.newPTY == nil {
		dependencies.newPTY = defaults.newPTY
	}
	if dependencies.newTmux == nil {
		dependencies.newTmux = defaults.newTmux
	}

	created := make([]terminal.Backend, 0, 2)
	rollback := func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		for _, backend := range created {
			_ = backend.Close(ctx)
		}
	}
	if selection.NeedsPTY {
		backend, err := dependencies.newPTY(parent, events, registry, ringCapacity)
		if err != nil {
			return nil, fmt.Errorf("initialisation du backend PTY: %w", err)
		}
		created = append(created, backend)
	}
	if selection.NeedsTmux {
		backend, err := dependencies.newTmux(parent, events, registry, ringCapacity, tmuxbackend.Options{
			TmuxPath:         selection.TmuxPath,
			RunID:            runID,
			PersistOnExit:    policy.PersistOnExit,
			CleanupOnSuccess: policy.CleanupOnSuccess,
			CaptureLimit:     ringCapacity,
		})
		if err != nil {
			rollback()
			return nil, fmt.Errorf("initialisation du backend tmux: %w", err)
		}
		created = append(created, backend)
	}
	if len(created) == 0 {
		return nil, errors.New("la sélection ne contient aucun backend concret")
	}
	router, err := newBackendRouter(parent, created...)
	if err != nil {
		rollback()
		return nil, err
	}
	router.adapters = registry
	return router, nil
}
