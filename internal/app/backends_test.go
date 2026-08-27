package app

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/Hocsman/Relayer/internal/tmuxbackend"
)

func TestResolveAgentBackendsPTYDoesNotProbeTmux(t *testing.T) {
	lookupCalls := 0
	resolution, err := resolveAgentBackends(
		backendTestSpecs(agent.BackendPTY, agent.BackendPTY),
		func(name string) (string, error) {
			lookupCalls++
			return "", errors.New("lookup must not be called for PTY")
		},
	)
	if err != nil {
		t.Fatalf("resolveAgentBackends: %v", err)
	}
	if lookupCalls != 0 {
		t.Fatalf("tmux lookup calls = %d, want zero", lookupCalls)
	}
	if !resolution.NeedsPTY || resolution.NeedsTmux || resolution.TmuxPath != "" || resolution.UsedAuto || resolution.AutoFallback {
		t.Fatalf("PTY resolution metadata = %#v", resolution)
	}
	for _, spec := range resolution.Specs {
		if spec.Backend != agent.BackendPTY {
			t.Fatalf("PTY spec was rewritten to %q", spec.Backend)
		}
	}
}

func TestResolveAgentBackendsExplicitTmuxFailsClearlyWhenUnavailable(t *testing.T) {
	lookupCalls := 0
	resolution, err := resolveAgentBackends(
		backendTestSpecs(agent.BackendTmux),
		func(name string) (string, error) {
			lookupCalls++
			if name != "tmux" {
				t.Fatalf("lookup name = %q, want tmux", name)
			}
			return "", errors.New("not installed")
		},
	)
	if err == nil {
		t.Fatalf("resolveAgentBackends returned %#v without an error", resolution)
	}
	if lookupCalls != 1 {
		t.Fatalf("tmux lookup calls = %d, want one", lookupCalls)
	}
	for _, text := range []string{"tmux", "introuvable", "agent-1"} {
		if !strings.Contains(err.Error(), text) {
			t.Fatalf("error %q does not contain %q", err, text)
		}
	}
}

func TestResolveAgentBackendsAutoSelectsTmuxWhenAvailable(t *testing.T) {
	lookupCalls := 0
	resolution, err := resolveAgentBackends(
		backendTestSpecs(agent.BackendAuto, agent.BackendAuto),
		func(string) (string, error) {
			lookupCalls++
			return "/opt/relayer-test/tmux", nil
		},
	)
	if err != nil {
		t.Fatalf("resolveAgentBackends: %v", err)
	}
	if lookupCalls != 1 {
		t.Fatalf("tmux lookup calls = %d, want one shared lookup", lookupCalls)
	}
	if resolution.NeedsPTY || !resolution.NeedsTmux || !resolution.UsedAuto || resolution.AutoFallback {
		t.Fatalf("auto/tmux metadata = %#v", resolution)
	}
	if resolution.TmuxPath != "/opt/relayer-test/tmux" {
		t.Fatalf("tmux path = %q", resolution.TmuxPath)
	}
	for _, spec := range resolution.Specs {
		if spec.Backend != agent.BackendTmux {
			t.Fatalf("auto spec was canonicalized to %q, want tmux", spec.Backend)
		}
	}
	if got := strings.Join(resolution.Warnings, "\n"); !strings.Contains(got, "tmux détecté") {
		t.Fatalf("auto selection is not visible in warnings: %q", got)
	}
}

func TestResolveAgentBackendsAutoFallsBackToPTYWhenTmuxIsUnavailable(t *testing.T) {
	lookupCalls := 0
	resolution, err := resolveAgentBackends(
		backendTestSpecs(agent.BackendAuto, agent.BackendAuto),
		func(string) (string, error) {
			lookupCalls++
			return "", errors.New("tmux missing")
		},
	)
	if err != nil {
		t.Fatalf("resolveAgentBackends: %v", err)
	}
	if lookupCalls != 1 {
		t.Fatalf("tmux lookup calls = %d, want one shared lookup", lookupCalls)
	}
	if !resolution.NeedsPTY || resolution.NeedsTmux || !resolution.UsedAuto || !resolution.AutoFallback || resolution.TmuxPath != "" {
		t.Fatalf("auto/PTY metadata = %#v", resolution)
	}
	for _, spec := range resolution.Specs {
		if spec.Backend != agent.BackendPTY {
			t.Fatalf("auto spec was canonicalized to %q, want PTY", spec.Backend)
		}
	}
	if got := strings.Join(resolution.Warnings, "\n"); !strings.Contains(got, "repli explicite sur PTY") {
		t.Fatalf("auto fallback is not visible in warnings: %q", got)
	}
}

func TestResolveAgentBackendsSupportsMixedConcreteBackendsAndLooksUpOnce(t *testing.T) {
	lookupCalls := 0
	resolution, err := resolveAgentBackends(
		backendTestSpecs(agent.BackendPTY, agent.BackendTmux, agent.BackendAuto, agent.BackendPTY),
		func(string) (string, error) {
			lookupCalls++
			return "/usr/local/bin/tmux", nil
		},
	)
	if err != nil {
		t.Fatalf("resolveAgentBackends: %v", err)
	}
	if lookupCalls != 1 {
		t.Fatalf("tmux lookup calls = %d, want one", lookupCalls)
	}
	if !resolution.NeedsPTY || !resolution.NeedsTmux {
		t.Fatalf("mixed resolution did not request both managers: %#v", resolution)
	}
	wantBackends := []string{agent.BackendPTY, agent.BackendTmux, agent.BackendTmux, agent.BackendPTY}
	gotBackends := make([]string, len(resolution.Specs))
	for index, spec := range resolution.Specs {
		gotBackends[index] = spec.Backend
	}
	if !reflect.DeepEqual(gotBackends, wantBackends) {
		t.Fatalf("resolved backends = %#v, want %#v", gotBackends, wantBackends)
	}
}

func TestResolveAgentBackendsReturnsDefensiveCopies(t *testing.T) {
	input := backendTestSpecs(agent.BackendAuto)
	input[0].Command = []string{"runner", "original argument"}
	input[0].Env = map[string]string{"MODEL": "original env"}

	resolution, err := resolveAgentBackends(input, func(string) (string, error) {
		return "", errors.New("tmux missing")
	})
	if err != nil {
		t.Fatalf("resolveAgentBackends: %v", err)
	}
	if input[0].Backend != agent.BackendAuto {
		t.Fatalf("input backend mutated to %q", input[0].Backend)
	}
	resolution.Specs[0].Command[1] = "mutated result"
	resolution.Specs[0].Env["MODEL"] = "mutated result"
	if input[0].Command[1] != "original argument" || input[0].Env["MODEL"] != "original env" {
		t.Fatalf("resolved specs alias input storage: input=%#v result=%#v", input, resolution.Specs)
	}
}

func TestBuildBackendRouterPTYOnlyNeverConstructsTmux(t *testing.T) {
	pty := newRouterFakeBackend(agent.BackendPTY)
	ptyCalls := 0
	router, err := buildBackendRouter(
		context.Background(),
		make(chan session.Event, 1),
		mustBackendTestRegistry(t),
		4096,
		backendResolution{NeedsPTY: true},
		config.SessionPolicy{PersistOnExit: true, CleanupOnSuccess: false},
		backendDependencies{
			newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
				ptyCalls++
				return pty, nil
			},
			newTmux: func(context.Context, chan<- session.Event, *adapters.Registry, int, tmuxbackend.Options) (terminal.Backend, error) {
				t.Fatal("tmux factory called for a PTY-only selection")
				return nil, nil
			},
		},
	)
	if err != nil {
		t.Fatalf("buildBackendRouter: %v", err)
	}
	if ptyCalls != 1 || router.Name() != agent.BackendPTY {
		t.Fatalf("PTY factory calls = %d, router name = %q", ptyCalls, router.Name())
	}
	if err := router.Close(context.Background()); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

func TestBuildBackendRouterPassesTmuxPathPolicyAndCaptureLimit(t *testing.T) {
	tmux := newRouterFakeBackend(agent.BackendTmux)
	var captured tmuxbackend.Options
	router, err := buildBackendRouterForRun(
		context.Background(),
		make(chan session.Event, 1),
		mustBackendTestRegistry(t),
		8192,
		backendResolution{NeedsTmux: true, TmuxPath: "/resolved/bin/tmux"},
		config.SessionPolicy{PersistOnExit: true, CleanupOnSuccess: false},
		backendDependencies{
			newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
				t.Fatal("PTY factory called for a tmux-only selection")
				return nil, nil
			},
			newTmux: func(_ context.Context, _ chan<- session.Event, _ *adapters.Registry, capacity int, options tmuxbackend.Options) (terminal.Backend, error) {
				if capacity != 8192 {
					t.Fatalf("tmux ring capacity = %d, want 8192", capacity)
				}
				captured = options
				return tmux, nil
			},
		},
		"desktop-run-17",
	)
	if err != nil {
		t.Fatalf("buildBackendRouter: %v", err)
	}
	defer router.Close(context.Background())
	if captured.TmuxPath != "/resolved/bin/tmux" || captured.RunID != "desktop-run-17" || !captured.PersistOnExit || captured.CleanupOnSuccess || captured.CaptureLimit != 8192 {
		t.Fatalf("tmux options = %#v", captured)
	}
}

func TestBuildBackendRouterRollsBackPTYWhenTmuxConstructionFails(t *testing.T) {
	pty := newRouterFakeBackend(agent.BackendPTY)
	tmuxFailure := errors.New("planned tmux construction failure")
	router, err := buildBackendRouter(
		context.Background(),
		make(chan session.Event, 1),
		mustBackendTestRegistry(t),
		1024,
		backendResolution{NeedsPTY: true, NeedsTmux: true, TmuxPath: "/fake/tmux"},
		config.SessionPolicy{},
		backendDependencies{
			newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
				return pty, nil
			},
			newTmux: func(context.Context, chan<- session.Event, *adapters.Registry, int, tmuxbackend.Options) (terminal.Backend, error) {
				return nil, tmuxFailure
			},
		},
	)
	if router != nil || err == nil || !errors.Is(err, tmuxFailure) {
		t.Fatalf("build result = router %#v error %v", router, err)
	}
	pty.mu.Lock()
	closeCalls := pty.closeCalls
	pty.mu.Unlock()
	if closeCalls != 1 {
		t.Fatalf("PTY rollback Close calls = %d, want one", closeCalls)
	}
}

func TestStartupLogsExposeEffectiveMixedBackendAndTmuxPolicyWithoutSecrets(t *testing.T) {
	secret := "startup-log-secret"
	logs := buildStartupLogs(
		config.Result{
			Version:  1,
			Backend:  agent.BackendAuto,
			Sessions: config.SessionPolicy{PersistOnExit: true, CleanupOnSuccess: false},
		},
		agentResolution{
			Warnings: []string{"Backend auto: tmux détecté et sélectionné."},
			Specs: []agent.Spec{{
				ID: "secret", Name: "Secret", Command: []string{"runner"}, Env: map[string]string{"TOKEN": secret},
			}},
		},
		[]session.Info{
			{ID: "left", Name: "Left", Backend: agent.BackendPTY},
			{ID: "right", Name: "Right", Backend: agent.BackendTmux},
		},
		"config.yaml",
	)
	rendered := strings.Join(logs, "\n")
	for _, text := range []string{
		"Backend auto: tmux détecté et sélectionné.",
		"2 agent(s) démarré(s) via PTY/TMUX",
		"persist_on_exit=true, cleanup_on_success=false",
	} {
		if !strings.Contains(rendered, text) {
			t.Fatalf("startup logs do not contain %q:\n%s", text, rendered)
		}
	}
	if strings.Contains(rendered, secret) {
		t.Fatalf("environment secret leaked in startup logs: %q", rendered)
	}
}

func TestRunExplicitTmuxAbsenceFailsBeforeAnyBackendConstruction(t *testing.T) {
	path := writeAppBackendConfig(t, agent.BackendTmux)
	constructed := 0
	var diagnostics strings.Builder
	err := run([]string{"--config", path}, &diagnostics, backendDependencies{
		lookup: func(string) (string, error) { return "", errors.New("tmux is absent") },
		newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
			constructed++
			return newRouterFakeBackend(agent.BackendPTY), nil
		},
		newTmux: func(context.Context, chan<- session.Event, *adapters.Registry, int, tmuxbackend.Options) (terminal.Backend, error) {
			constructed++
			return newRouterFakeBackend(agent.BackendTmux), nil
		},
	})
	if err == nil || !strings.Contains(err.Error(), "tmux") || !strings.Contains(err.Error(), "introuvable") {
		t.Fatalf("run error = %v", err)
	}
	if constructed != 0 {
		t.Fatalf("constructed %d backend(s) after explicit tmux preflight failure", constructed)
	}
}

func TestRunAutoAbsenceSelectsPTYAndReportsFallbackBeforeStartup(t *testing.T) {
	path := writeAppBackendConfig(t, agent.BackendAuto)
	startFailure := errors.New("stop before Bubble Tea")
	pty := newRouterFakeBackend(agent.BackendPTY)
	pty.startErr = startFailure
	tmuxConstructed := false
	var diagnostics strings.Builder
	err := run([]string{"--config", path}, &diagnostics, backendDependencies{
		lookup: func(string) (string, error) { return "", errors.New("tmux is absent") },
		newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
			return pty, nil
		},
		newTmux: func(context.Context, chan<- session.Event, *adapters.Registry, int, tmuxbackend.Options) (terminal.Backend, error) {
			tmuxConstructed = true
			return newRouterFakeBackend(agent.BackendTmux), nil
		},
	})
	if err == nil || !errors.Is(err, startFailure) {
		t.Fatalf("run error = %v", err)
	}
	if tmuxConstructed {
		t.Fatal("tmux backend was constructed after auto fallback")
	}
	if !strings.Contains(diagnostics.String(), "repli explicite sur PTY") {
		t.Fatalf("fallback is not visible in diagnostics: %q", diagnostics.String())
	}
	pty.mu.Lock()
	starts := append([]routerStartCall(nil), pty.starts...)
	pty.mu.Unlock()
	if len(starts) != 1 || starts[0].spec.Backend != agent.BackendPTY {
		t.Fatalf("effective PTY starts = %#v", starts)
	}
}

func TestRunAutoAvailabilitySelectsTmuxAndPassesSessionPolicy(t *testing.T) {
	path := writeAppBackendConfigWithSessions(t, agent.BackendAuto, true, false)
	startFailure := errors.New("stop before Bubble Tea")
	tmux := newRouterFakeBackend(agent.BackendTmux)
	tmux.startErr = startFailure
	ptyConstructed := false
	var captured tmuxbackend.Options
	var diagnostics strings.Builder
	err := run([]string{"--config", path}, &diagnostics, backendDependencies{
		lookup: func(string) (string, error) { return "/resolved/tmux", nil },
		newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
			ptyConstructed = true
			return newRouterFakeBackend(agent.BackendPTY), nil
		},
		newTmux: func(_ context.Context, _ chan<- session.Event, _ *adapters.Registry, _ int, options tmuxbackend.Options) (terminal.Backend, error) {
			captured = options
			return tmux, nil
		},
	})
	if err == nil || !errors.Is(err, startFailure) {
		t.Fatalf("run error = %v", err)
	}
	if ptyConstructed {
		t.Fatal("PTY backend was constructed when auto selected tmux")
	}
	if captured.TmuxPath != "/resolved/tmux" || !captured.PersistOnExit || captured.CleanupOnSuccess {
		t.Fatalf("effective tmux options = %#v", captured)
	}
	if !strings.Contains(diagnostics.String(), "tmux détecté et sélectionné") {
		t.Fatalf("auto tmux selection is not visible in diagnostics: %q", diagnostics.String())
	}
	tmux.mu.Lock()
	starts := append([]routerStartCall(nil), tmux.starts...)
	tmux.mu.Unlock()
	if len(starts) != 1 || starts[0].spec.Backend != agent.BackendTmux {
		t.Fatalf("effective tmux starts = %#v", starts)
	}
}

func writeAppBackendConfig(t *testing.T, backend string) string {
	t.Helper()
	return writeAppBackendConfigWithSessions(t, backend, false, true)
}

func writeAppBackendConfigWithSessions(t *testing.T, backend string, persistOnExit, cleanupOnSuccess bool) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.yaml")
	content := "version: 1\n" +
		"backend: " + backend + "\n" +
		"sessions:\n" +
		"  persist_on_exit: " + boolText(persistOnExit) + "\n" +
		"  cleanup_on_success: " + boolText(cleanupOnSuccess) + "\n" +
		"agents:\n" +
		"  - id: configured-agent\n" +
		"    name: Configured Agent\n" +
		"    command: [runner]\n" +
		"intercept_patterns:\n" +
		"  - pattern: continue\n" +
		"    description: Continue\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatalf("write app backend config: %v", err)
	}
	return path
}

func boolText(value bool) string {
	if value {
		return "true"
	}
	return "false"
}

func backendTestSpecs(backends ...string) []agent.Spec {
	result := make([]agent.Spec, len(backends))
	for index, backend := range backends {
		result[index] = agent.Spec{
			ID:      "agent-" + string(rune('1'+index)),
			Name:    "Agent " + string(rune('1'+index)),
			Command: []string{"runner"},
			Adapter: agent.AdapterGeneric,
			Backend: backend,
		}
	}
	return result
}

func mustBackendTestRegistry(t *testing.T) *adapters.Registry {
	t.Helper()
	registry, err := adapters.NewRegistry([]adapters.Pattern{{
		Name: "gate", Description: "gate", Expression: "continue",
	}})
	if err != nil {
		t.Fatal(err)
	}
	return registry
}
