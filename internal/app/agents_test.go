package app

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/session"
)

type fakeSessionStarter struct {
	failAt     int
	startCalls []agent.Spec
	closeCalls int
}

func (f *fakeSessionStarter) Start(spec agent.Spec, _, _ int) (session.Info, error) {
	callIndex := len(f.startCalls)
	f.startCalls = append(f.startCalls, spec)
	if callIndex == f.failAt {
		return session.Info{}, errors.New("planned startup failure")
	}
	return session.Info{ID: spec.ID, Name: spec.Name, DisplayCommand: spec.Command[0]}, nil
}

func (f *fakeSessionStarter) Close() { f.closeCalls++ }

func TestResolveAgentPlansPreservesConfiguredAgentsFromOneToEight(t *testing.T) {
	for count := 1; count <= 8; count++ {
		t.Run(fmt.Sprintf("%d agents", count), func(t *testing.T) {
			configured := configuredAgentSpecs(count)
			resolution, err := resolveAgentPlans(config.Result{
				Backend: agent.BackendPTY,
				Agents:  configured,
			}, options{}, t.TempDir())
			if err != nil {
				t.Fatalf("resolveAgentPlans: %v", err)
			}
			if len(resolution.Specs) != count {
				t.Fatalf("got %d agents, want %d", len(resolution.Specs), count)
			}
			for index := range resolution.Specs {
				if resolution.Specs[index].ID != configured[index].ID ||
					!reflect.DeepEqual(resolution.Specs[index].Command, configured[index].Command) {
					t.Fatalf("agent %d changed: %#v", index, resolution.Specs[index])
				}
			}
			if len(resolution.MockAgentNames) != 0 || len(resolution.Warnings) != 0 {
				t.Fatalf("unexpected resolution metadata: %#v", resolution)
			}
		})
	}
}

func TestResolveAgentPlansAppliesOverridesAcrossEveryMultiAgentCardinality(t *testing.T) {
	for count := 2; count <= 8; count++ {
		t.Run(fmt.Sprintf("%d agents", count), func(t *testing.T) {
			configured := configuredAgentSpecs(count)
			resolution, err := resolveAgentPlans(config.Result{
				Backend: agent.BackendPTY,
				Agents:  configured,
			}, options{
				pane1Set: true,
				pane1:    `first "argument one"`,
				pane2Set: true,
				pane2:    `second "argument two"`,
			}, t.TempDir())
			if err != nil {
				t.Fatalf("resolveAgentPlans: %v", err)
			}

			if got, want := resolution.Specs[0].Command, []string{"first", "argument one"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("first command = %#v, want %#v", got, want)
			}
			if got, want := resolution.Specs[1].Command, []string{"second", "argument two"}; !reflect.DeepEqual(got, want) {
				t.Fatalf("second command = %#v, want %#v", got, want)
			}
			for index := 2; index < count; index++ {
				if !reflect.DeepEqual(resolution.Specs[index], configured[index]) {
					t.Fatalf("tail agent %d changed: got %#v, want %#v", index, resolution.Specs[index], configured[index])
				}
			}
		})
	}
}

func TestResolveAgentPlansAppliesOnlyRequestedLegacyOverrides(t *testing.T) {
	configured := configuredAgentSpecs(4)
	resolution, err := resolveAgentPlans(config.Result{
		Backend: agent.BackendPTY,
		Agents:  configured,
	}, options{
		pane1Set: true,
		pane1:    `claude --model "sonnet latest" 'literal|pipe'`,
		pane2Set: true,
		pane2:    `ollama run qwen3-coder`,
	}, t.TempDir())
	if err != nil {
		t.Fatalf("resolveAgentPlans: %v", err)
	}

	wantFirst := []string{"claude", "--model", "sonnet latest", "literal|pipe"}
	wantSecond := []string{"ollama", "run", "qwen3-coder"}
	if !reflect.DeepEqual(resolution.Specs[0].Command, wantFirst) {
		t.Fatalf("first command = %#v, want %#v", resolution.Specs[0].Command, wantFirst)
	}
	if !reflect.DeepEqual(resolution.Specs[1].Command, wantSecond) {
		t.Fatalf("second command = %#v, want %#v", resolution.Specs[1].Command, wantSecond)
	}
	for index := 2; index < len(configured); index++ {
		if !reflect.DeepEqual(resolution.Specs[index].Command, configured[index].Command) {
			t.Fatalf("tail agent %d changed: %#v", index, resolution.Specs[index])
		}
	}
	if resolution.Specs[0].Shell != "" || resolution.Specs[1].Shell != "" {
		t.Fatal("legacy overrides unexpectedly selected shell mode")
	}
	if len(resolution.Warnings) != 4 ||
		!strings.Contains(strings.Join(resolution.Warnings, "\n"), "obsolète") ||
		!strings.Contains(strings.Join(resolution.Warnings, "\n"), "sans interprétation par un shell") {
		t.Fatalf("deprecation/direct-mode warnings = %#v", resolution.Warnings)
	}
}

func TestResolveAgentPlansExplicitBlankOverrideSelectsMock(t *testing.T) {
	configured := configuredAgentSpecs(2)
	resolution, err := resolveAgentPlans(config.Result{
		Backend: agent.BackendPTY,
		Agents:  configured,
	}, options{pane1Set: true, pane1: " \t "}, t.TempDir())
	if err != nil {
		t.Fatalf("resolveAgentPlans: %v", err)
	}
	if !reflect.DeepEqual(resolution.Specs[0].Command, mockCommand()) {
		t.Fatalf("blank override command = %#v", resolution.Specs[0].Command)
	}
	if !reflect.DeepEqual(resolution.Specs[1].Command, configured[1].Command) {
		t.Fatalf("second configured agent changed: %#v", resolution.Specs[1])
	}
	if !reflect.DeepEqual(resolution.MockAgentNames, []string{configured[0].Name}) {
		t.Fatalf("mock names = %#v", resolution.MockAgentNames)
	}
}

func TestResolveAgentPlansLeavesOmittedShellUntouchedAndExplicitOverrideClearsIt(t *testing.T) {
	configured := agent.Spec{
		ID:      "shell-agent",
		Name:    "Shell agent",
		Shell:   `printf '%s' "$HOME;$(literal)"`,
		Adapter: agent.AdapterGeneric,
		Backend: agent.BackendPTY,
	}
	omitted, err := resolveAgentPlans(config.Result{
		Backend: agent.BackendPTY,
		Agents:  []agent.Spec{configured},
	}, options{}, t.TempDir())
	if err != nil {
		t.Fatalf("resolve omitted override: %v", err)
	}
	if omitted.Specs[0].Shell != configured.Shell || omitted.Specs[0].Command != nil {
		t.Fatalf("omitted flag changed shell spec: %#v", omitted.Specs[0])
	}

	overridden, err := resolveAgentPlans(config.Result{
		Backend: agent.BackendPTY,
		Agents:  []agent.Spec{configured},
	}, options{pane1Set: true, pane1: `runner '$HOME;$(literal)'`}, t.TempDir())
	if err != nil {
		t.Fatalf("resolve explicit override: %v", err)
	}
	if overridden.Specs[0].Shell != "" {
		t.Fatalf("explicit direct override retained shell: %#v", overridden.Specs[0])
	}
	if got, want := overridden.Specs[0].Command, []string{"runner", "$HOME;$(literal)"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("direct command = %#v, want %#v", got, want)
	}
}

func TestResolveAgentPlansRejectsPane2WhenOnlyOneAgentExists(t *testing.T) {
	_, err := resolveAgentPlans(config.Result{
		Backend: agent.BackendPTY,
		Agents:  configuredAgentSpecs(1),
	}, options{pane2Set: true, pane2: "runner"}, t.TempDir())
	if err == nil || !strings.Contains(err.Error(), "--pane2") || !strings.Contains(err.Error(), "1 agent") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveAgentPlansFallsBackToTwoDirectMockCommands(t *testing.T) {
	workingDirectory := t.TempDir()
	resolution, err := resolveAgentPlans(config.Result{Backend: agent.BackendPTY}, options{}, workingDirectory)
	if err != nil {
		t.Fatalf("resolveAgentPlans: %v", err)
	}
	if len(resolution.Specs) != 2 || len(resolution.MockAgentNames) != 2 {
		t.Fatalf("fallback resolution = %#v", resolution)
	}
	for _, spec := range resolution.Specs {
		if !reflect.DeepEqual(spec.Command, mockCommand()) || spec.Shell != "" {
			t.Fatalf("mock is not a direct argv command: %#v", spec)
		}
		if spec.Cwd != workingDirectory {
			t.Fatalf("mock cwd = %q, want %q", spec.Cwd, workingDirectory)
		}
	}
}

func TestResolveAgentPlansPreservesConfiguredSpecialArgumentsExactly(t *testing.T) {
	want := []string{
		"runner",
		"argument with spaces",
		"apostrophe's value",
		"$HOME",
		"$(touch should-not-run)",
		"a|b;c&d*e",
		"",
	}
	resolution, err := resolveAgentPlans(config.Result{
		Backend: agent.BackendPTY,
		Agents: []agent.Spec{{
			ID:      "exact",
			Name:    "Exact arguments",
			Command: append([]string(nil), want...),
		}},
	}, options{}, t.TempDir())
	if err != nil {
		t.Fatalf("resolveAgentPlans: %v", err)
	}
	if !reflect.DeepEqual(resolution.Specs[0].Command, want) {
		t.Fatalf("arguments = %#v, want %#v", resolution.Specs[0].Command, want)
	}
}

func TestStartupLogsIdentifyShellModeWithoutEchoingCommandsOrSecrets(t *testing.T) {
	secret := "super-secret-token-value"
	script := "printf '%s' " + secret
	logs := buildStartupLogs(
		config.Result{Version: 1, Backend: agent.BackendPTY},
		agentResolution{Specs: []agent.Spec{{
			ID:      "secret-env",
			Name:    "Secret environment",
			Command: []string{"runner"},
			Env:     map[string]string{"OPENAI_API_KEY": secret},
		}}},
		[]session.Info{{
			ID:             "shell-agent",
			Name:           "Shell Agent",
			DisplayCommand: script,
			Shell:          true,
		}},
		"config.yaml",
	)
	rendered := strings.Join(logs, "\n")
	if !strings.Contains(rendered, "Mode shell explicite actif pour Shell Agent") {
		t.Fatalf("shell mode is not identified in logs: %q", rendered)
	}
	if strings.Contains(rendered, script) || strings.Contains(rendered, secret) {
		t.Fatalf("shell script or secret leaked into logs: %q", rendered)
	}
	if got := paneDisplayCommand(session.Info{DisplayCommand: script, Shell: true}); got != "[shell explicite]" {
		t.Fatalf("shell pane label = %q", got)
	}
}

func TestStartupLogsDistinguishLegacyFallbackWithoutLeakingCommands(t *testing.T) {
	resolution := agentResolution{
		Specs:          defaultAgentSpecs(t.TempDir(), agent.BackendPTY),
		MockAgentNames: []string{"Agent A (Claude)", "Agent B (Local)"},
	}
	legacy := strings.Join(buildStartupLogs(
		config.Result{Legacy: true, Backend: agent.BackendPTY},
		resolution,
		nil,
		"legacy.yaml",
	), "\n")
	versioned := strings.Join(buildStartupLogs(
		config.Result{Version: 1, Backend: agent.BackendPTY},
		resolution,
		nil,
		"config.yaml",
	), "\n")
	if !strings.Contains(legacy, "Configuration historique détectée") {
		t.Fatalf("legacy fallback is not identified: %q", legacy)
	}
	if strings.Contains(versioned, "Configuration historique détectée") {
		t.Fatalf("versioned fallback was labeled legacy: %q", versioned)
	}
	if strings.Contains(legacy, defaultMockScript) || strings.Contains(versioned, defaultMockScript) {
		t.Fatal("mock command leaked into startup logs")
	}
}

func TestStartAgentSessionsClosesOwnerImmediatelyOnPartialFailure(t *testing.T) {
	owner := &fakeSessionStarter{failAt: 1}
	panes, infos, err := startAgentSessions(owner, configuredAgentSpecs(4), 120, 40)
	if err == nil || !strings.Contains(err.Error(), `agent-2`) {
		t.Fatalf("startAgentSessions error = %v", err)
	}
	if panes != nil || infos != nil {
		t.Fatalf("partial startup escaped metadata: panes=%#v infos=%#v", panes, infos)
	}
	if owner.closeCalls != 1 {
		t.Fatalf("Close calls = %d, want 1", owner.closeCalls)
	}
	if len(owner.startCalls) != 2 || owner.startCalls[0].ID != "agent-1" || owner.startCalls[1].ID != "agent-2" {
		t.Fatalf("Start calls = %#v", owner.startCalls)
	}
}

func configuredAgentSpecs(count int) []agent.Spec {
	specs := make([]agent.Spec, count)
	for index := range specs {
		specs[index] = agent.Spec{
			ID:      fmt.Sprintf("agent-%d", index+1),
			Name:    fmt.Sprintf("Agent %d", index+1),
			Command: []string{"runner", fmt.Sprintf("argument-%d", index+1)},
			Adapter: agent.AdapterGeneric,
			Backend: agent.BackendPTY,
		}
	}
	return specs
}
