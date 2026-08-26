package app

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/Hocsman/Relayer/internal/tmuxbackend"
)

func TestResolveAgentAdaptersUsesGenericFallbackAndReturnsDefensiveCopies(t *testing.T) {
	registry, err := adapters.NewRegistry(adapters.DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	input := []agent.Spec{
		{
			ID: "explicit-generic", Name: "Explicit generic",
			Command: []string{"runner", "argument with spaces"},
			Env:     map[string]string{"MODEL": "example-value"},
			Adapter: adapters.GenericID,
			Backend: agent.BackendPTY,
		},
		{
			ID: "unimplemented-hint", Name: "Unimplemented executable hint",
			Command: []string{"claude", "synthetic-argument"},
			Env:     map[string]string{"EMPTY": ""},
			Adapter: "",
			Backend: agent.BackendPTY,
		},
	}
	resolved, err := resolveAgentAdapters(input, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(resolved) != len(input) || resolved[0].Adapter != adapters.GenericID || resolved[1].Adapter != adapters.GenericID {
		t.Fatalf("resolved adapters = %#v", resolved)
	}
	resolved[0].Command[0] = "mutated"
	resolved[0].Env["MODEL"] = "mutated"
	resolved[1].Command[1] = "mutated"
	resolved[1].Env["NEW"] = "mutated"
	if input[0].Command[0] != "runner" || input[0].Env["MODEL"] != "example-value" ||
		input[1].Command[1] != "synthetic-argument" || len(input[1].Env) != 1 || input[1].Adapter != "" {
		t.Fatalf("resolved specs alias input storage: input=%#v resolved=%#v", input, resolved)
	}
}

func TestResolveAgentAdaptersRejectsUnknownAndExperimentalPlaceholders(t *testing.T) {
	registry, err := adapters.NewRegistry(adapters.DefaultPatterns())
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		adapter string
		wantErr error
	}{
		{adapter: "not-registered", wantErr: adapters.ErrUnknownAdapter},
		{adapter: "claude", wantErr: adapters.ErrAdapterUnavailable},
		{adapter: "codex", wantErr: adapters.ErrAdapterUnavailable},
	} {
		t.Run(test.adapter, func(t *testing.T) {
			resolved, err := resolveAgentAdapters([]agent.Spec{{
				ID: "agent-" + test.adapter, Name: "Synthetic agent",
				Command: []string{"runner"}, Adapter: test.adapter, Backend: agent.BackendPTY,
			}}, registry)
			if resolved != nil || !errors.Is(err, test.wantErr) || !strings.Contains(err.Error(), "agent-"+test.adapter) {
				t.Fatalf("resolve %q = specs %#v error %v", test.adapter, resolved, err)
			}
		})
	}
}

func TestAdapterResolutionFailsBeforeAnyBackendFactory(t *testing.T) {
	for _, test := range []struct {
		adapter string
		wantErr error
	}{
		{adapter: "claude", wantErr: adapters.ErrAdapterUnavailable},
		{adapter: "codex", wantErr: adapters.ErrAdapterUnavailable},
		{adapter: "not-registered", wantErr: adapters.ErrUnknownAdapter},
	} {
		t.Run(test.adapter, func(t *testing.T) {
			configPath := writeAdapterPreflightConfig(t, test.adapter)
			factoryCalls := 0
			lookupCalls := 0
			err := run([]string{"--config", configPath}, io.Discard, backendDependencies{
				lookup: func(string) (string, error) {
					lookupCalls++
					return "", errors.New("lookup must not run")
				},
				newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
					factoryCalls++
					return nil, errors.New("PTY factory must not run")
				},
				newTmux: func(context.Context, chan<- session.Event, *adapters.Registry, int, tmuxbackend.Options) (terminal.Backend, error) {
					factoryCalls++
					return nil, errors.New("tmux factory must not run")
				},
			})
			if !errors.Is(err, test.wantErr) {
				t.Fatalf("run error = %v, want %v", err, test.wantErr)
			}
			if factoryCalls != 0 || lookupCalls != 0 {
				t.Fatalf("preflight calls = factories %d lookup %d, want zero", factoryCalls, lookupCalls)
			}
		})
	}
}

func writeAdapterPreflightConfig(t *testing.T, adapterID string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 1\n" +
		"backend: pty\n" +
		"agents:\n" +
		"  - id: adapter-preflight\n" +
		"    name: Adapter preflight\n" +
		"    command: [runner]\n" +
		"    adapter: " + adapterID + "\n" +
		"intercept_patterns:\n" +
		"  - pattern: continue\n" +
		"    description: Synthetic confirmation\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
