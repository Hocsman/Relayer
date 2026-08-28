//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package session

import (
	"context"
	"fmt"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
)

func TestManagerDirectCommandPreservesArgumentsLiterally(t *testing.T) {
	printfPath, err := exec.LookPath("printf")
	if err != nil {
		t.Skipf("printf is required: %v", err)
	}

	arguments := []string{
		"value with spaces",
		"apostrophe's value",
		"$(printf command-substitution)",
		"; printf shell-injection",
		"*.does-not-expand",
	}
	command := append([]string{printfPath, "%s\n"}, arguments...)
	manager, events := newTestManager(t, 128)
	info, err := manager.Start(agent.Spec{
		ID:      "literal-arguments",
		Name:    "literal arguments",
		Command: command,
	}, 200, 20)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	if info.ID != "literal-arguments" || info.Name != "literal arguments" || info.Shell {
		t.Fatalf("direct command info = %#v", info)
	}
	quoted := make([]string, len(command))
	for index, argument := range command {
		quoted[index] = strconv.Quote(argument)
	}
	if want := strings.Join(quoted, " "); info.DisplayCommand != want {
		t.Fatalf("DisplayCommand = %q, want %q", info.DisplayCommand, want)
	}

	output := waitForOutputAndExit(t, manager, events, info.ID, arguments...)
	for _, argument := range arguments {
		if !strings.Contains(output, argument) {
			t.Fatalf("literal argument %q missing from output %q", argument, output)
		}
	}
}

func TestManagerAppliesCwdAndMergedEnvironment(t *testing.T) {
	t.Setenv("RELAYER_INHERITED", "inherited value")
	t.Setenv("RELAYER_OVERRIDE", "parent value")
	workingDirectory := t.TempDir()
	physicalWorkingDirectory, err := filepath.EvalSymlinks(workingDirectory)
	if err != nil {
		t.Fatalf("resolving temporary directory: %v", err)
	}

	manager, events := newTestManager(t, 128)
	info, err := manager.Start(agent.Spec{
		ID:   "cwd-environment",
		Name: "cwd and environment",
		Command: []string{"/bin/sh", "-c",
			`printf 'cwd='; pwd; printf 'inherited=%s\noverride=%s\nterm=%s\n' "$RELAYER_INHERITED" "$RELAYER_OVERRIDE" "$TERM"`,
		},
		Cwd: workingDirectory,
		Env: map[string]string{
			"RELAYER_OVERRIDE": "spec value",
			"TERM":             "relayer-test-term",
		},
	}, 160, 20)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}
	output := waitForOutputAndExit(
		t,
		manager,
		events,
		info.ID,
		"cwd="+physicalWorkingDirectory,
		"inherited=inherited value",
		"override=spec value",
		"term=relayer-test-term",
	)
	if !strings.Contains(output, "cwd="+physicalWorkingDirectory) {
		t.Fatalf("command did not run in requested cwd: %q", output)
	}
}

func TestManagerCopiesMutableSpecBeforeStarting(t *testing.T) {
	manager, events := newTestManager(t, 128)
	spec := agent.Spec{
		ID:      "immutable-spec",
		Name:    "immutable spec",
		Command: []string{"/bin/sh", "-c", `sleep 0.05; printf 'argument=%s\nenv=%s\n' "$1" "$RELAYER_COPY_TEST"`, "relayer", "original argument"},
		Env:     map[string]string{"RELAYER_COPY_TEST": "original env"},
	}
	info, err := manager.Start(spec, 120, 20)
	if err != nil {
		t.Fatalf("Start returned an error: %v", err)
	}

	spec.ID = "mutated-id"
	spec.Name = "mutated name"
	spec.Command[4] = "mutated argument"
	spec.Env["RELAYER_COPY_TEST"] = "mutated env"

	output := waitForOutputAndExit(
		t,
		manager,
		events,
		info.ID,
		"argument=original argument",
		"env=original env",
	)
	if strings.Contains(output, "mutated") {
		t.Fatalf("process observed a post-Start spec mutation: %q", output)
	}
	if info.ID != "immutable-spec" || info.Name != "immutable spec" {
		t.Fatalf("Info was not derived from the normalized copy: %#v", info)
	}
}

func TestManagerRejectsDuplicateStableID(t *testing.T) {
	manager, _ := newTestManager(t, 64)
	first := agent.Spec{
		ID:      "stable-id",
		Name:    "first",
		Command: []string{"/bin/sh", "-c", "while :; do sleep 30; done"},
	}
	if _, err := manager.Start(first, 40, 10); err != nil {
		t.Fatalf("starting first session: %v", err)
	}
	second := agent.Spec{ID: "STABLE-ID", Name: "second", Command: []string{"true"}}
	if _, err := manager.Start(second, 40, 10); err == nil || !strings.Contains(err.Error(), "already started") {
		t.Fatalf("duplicate ID returned %v", err)
	}
}

func TestManagerClosesEightSessions(t *testing.T) {
	manager, _ := newTestManager(t, 512)
	doneChannels := make([]<-chan struct{}, 0, 8)
	for index := 0; index < 8; index++ {
		id := fmt.Sprintf("agent-%d", index+1)
		info, err := manager.Start(agent.Spec{
			ID:      id,
			Name:    id,
			Command: []string{"/bin/sh", "-c", "while :; do sleep 30; done"},
		}, 40, 10)
		if err != nil {
			t.Fatalf("starting %s: %v", id, err)
		}
		done, err := manager.Done(info.ID)
		if err != nil {
			t.Fatalf("Done(%q): %v", info.ID, err)
		}
		doneChannels = append(doneChannels, done)
	}

	started := time.Now()
	manager.Close()
	if elapsed := time.Since(started); elapsed > 5*time.Second {
		t.Fatalf("closing eight sessions took %v", elapsed)
	}
	for index, done := range doneChannels {
		select {
		case <-done:
		default:
			t.Fatalf("session %d is not done after Close", index+1)
		}
	}
}

func TestMergedEnvironmentIsUniqueAndHonorsDefaultsAndOverrides(t *testing.T) {
	t.Setenv("RELAYER_ENV_INHERITED", "parent")
	t.Setenv("TERM", "parent-term")

	assertEnvironment := func(environment []string, wantTerm string) {
		t.Helper()
		seen := make(map[string]string, len(environment))
		for index, assignment := range environment {
			name, value, found := strings.Cut(assignment, "=")
			if !found {
				t.Fatalf("malformed environment entry at index %d", index)
			}
			if _, duplicate := seen[name]; duplicate {
				t.Fatalf("duplicate environment key %q", name)
			}
			seen[name] = value
		}
		if seen["RELAYER_ENV_INHERITED"] != "parent" {
			t.Fatal("inherited environment is missing")
		}
		if seen["TERM"] != wantTerm {
			t.Fatalf("TERM = %q, want %q", seen["TERM"], wantTerm)
		}
	}

	assertEnvironment(mergedEnvironment(nil), "xterm-256color")
	assertEnvironment(mergedEnvironment(map[string]string{"TERM": "agent-term"}), "agent-term")
}

func newTestManager(t *testing.T, eventCapacity int) (*Manager, chan Event) {
	t.Helper()
	events := make(chan Event, eventCapacity)
	manager, err := NewManager(context.Background(), events, integrationPatterns, 64*1024)
	if err != nil {
		t.Fatalf("NewManager returned an error: %v", err)
	}
	t.Cleanup(manager.Close)
	return manager, events
}

func waitForOutputAndExit(
	t *testing.T,
	manager *Manager,
	events <-chan Event,
	sessionID string,
	wanted ...string,
) string {
	t.Helper()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	exited := false
	latest := ""
	for {
		select {
		case event := <-events:
			switch event := event.(type) {
			case OutputAvailable:
				if event.SessionID == sessionID {
					latest, _ = manager.Output(sessionID)
				}
			case AdapterEvent:
				if event.Event.SessionID == sessionID && event.Event.Type == adapters.EventProcessExit {
					_, waitErr, _, resultErr := manager.Result(sessionID)
					if resultErr != nil || waitErr != nil {
						t.Fatalf("session %q exit result = (%v, %v); output: %q", sessionID, waitErr, resultErr, latest)
					}
					exited = true
					latest, _ = manager.Output(sessionID)
				}
			case Error:
				if event.SessionID == sessionID {
					t.Fatalf("session %q emitted PTY error: %v", sessionID, event.Err)
				}
			}
			if exited && containsAll(latest, wanted) {
				return latest
			}
		case <-deadline.C:
			t.Fatalf("timed out waiting for session %q; output: %q", sessionID, latest)
		}
	}
}

func containsAll(value string, wanted []string) bool {
	for _, fragment := range wanted {
		if !strings.Contains(value, fragment) {
			return false
		}
	}
	return true
}
