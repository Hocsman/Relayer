//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tmuxbackend

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
)

const (
	helperExecTargetEnv = "RELAYER_TMUX_HELPER_EXEC_TARGET"
	helperExecReportEnv = "RELAYER_TMUX_HELPER_EXEC_REPORT"
	helperExecStaleEnv  = "RELAYER_TMUX_HELPER_EXEC_STALE"
)

type helperExecReport struct {
	Arguments   []string          `json:"arguments"`
	Environment map[string]string `json:"environment"`
	Cwd         string            `json:"cwd"`
}

// TestTmuxHelperExecTarget is exec'd by TestHelperMainExecPreservesTransportExactly.
// In the ordinary test process it is deliberately a no-op.
func TestTmuxHelperExecTarget(t *testing.T) {
	if os.Getenv(helperExecTargetEnv) != "1" {
		return
	}
	separator := argumentPosition(os.Args, "--")
	if separator < 0 {
		t.Fatal("target process received no argument separator")
	}
	workingDirectory, err := os.Getwd()
	if err != nil {
		t.Fatalf("target working directory: %v", err)
	}
	report := helperExecReport{
		Arguments:   append([]string(nil), os.Args[separator+1:]...),
		Environment: environmentMap(os.Environ()),
		Cwd:         workingDirectory,
	}
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatalf("encode target report: %v", err)
	}
	if err := os.WriteFile(os.Getenv(helperExecReportEnv), payload, 0o600); err != nil {
		t.Fatalf("write target report: %v", err)
	}
}

func TestHelperMainExecPreservesTransportExactly(t *testing.T) {
	testExecutable, err := os.Executable()
	if err != nil {
		t.Fatalf("test executable: %v", err)
	}
	root := t.TempDir()
	runtimeDirectory := filepath.Join(root, "runtime with spaces")
	workingDirectory := filepath.Join(root, "working directory with spaces")
	if err := os.Mkdir(runtimeDirectory, 0o700); err != nil {
		t.Fatalf("create runtime directory: %v", err)
	}
	if err := os.Mkdir(workingDirectory, 0o700); err != nil {
		t.Fatalf("create working directory: %v", err)
	}
	reportPath := filepath.Join(root, "helper report with spaces.json")

	// The launch snapshot must not contain this value. It is injected only
	// into the intermediate helper to simulate a stale long-lived tmux server.
	t.Setenv(helperExecStaleEnv, "temporarily-present-before-snapshot")
	if err := os.Unsetenv(helperExecStaleEnv); err != nil {
		t.Fatalf("remove stale value before snapshot: %v", err)
	}

	hostileArguments := []string{
		"spaces and tabs\t",
		"apostrophe's",
		`double "quotes"`,
		"semi;colon",
		"dollar$HOME",
		"back`tick`",
		"line one\nline two",
		"",
		filepath.Join(root, "path with spaces", "file name"),
	}
	command := []string{testExecutable, "-test.run=^TestTmuxHelperExecTarget$", "--"}
	command = append(command, hostileArguments...)
	files, err := createLaunchFiles(runtimeDirectory, "helper-exec-agent", agent.Spec{
		Command: command,
		Cwd:     workingDirectory,
		Env: map[string]string{
			helperExecTargetEnv:           "1",
			helperExecReportEnv:           reportPath,
			"PWD":                         workingDirectory,
			"RELAYER_TMUX_HELPER_SPECIAL": "spaces ' \" ;$` and\nnewline",
			"RELAYER_TMUX_HELPER_EMPTY":   "",
		},
	})
	if err != nil {
		t.Fatalf("create launch files: %v", err)
	}
	t.Cleanup(func() {
		files.close()
		files.remove()
	})

	writtenSpec, err := readLaunchSpec(files.specPath)
	if err != nil {
		t.Fatalf("read launch snapshot: %v", err)
	}
	if _, found := writtenSpec.Env[helperExecStaleEnv]; found {
		t.Fatal("stale helper variable unexpectedly entered launch snapshot")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	commandProcess := exec.CommandContext(
		ctx,
		testExecutable,
		"-test.run=^TestTmuxHelperProcess$",
		"--",
		HelperSubcommand,
		files.specPath,
		files.gatePath,
	)
	commandProcess.Env = append(
		withoutEnvironmentNames(os.Environ(), "TERM", "TMUX", "TMUX_PANE", helperExecStaleEnv),
		helperExecStaleEnv+"=stale-helper-only-value",
	)
	var diagnostics bytes.Buffer
	commandProcess.Stdout = &diagnostics
	commandProcess.Stderr = &diagnostics
	if err := commandProcess.Start(); err != nil {
		t.Fatalf("start helper subprocess: %v", err)
	}

	if err := waitForRemoval(ctx, files.specPath); err != nil {
		_ = files.release()
		cancel()
		_ = commandProcess.Wait()
		t.Fatalf("helper did not consume private spec: %v (%s)", err, diagnostics.String())
	}
	if err := files.release(); err != nil {
		cancel()
		_ = commandProcess.Wait()
		t.Fatalf("release helper gate: %v (%s)", err, diagnostics.String())
	}
	if err := files.waitForHandoff(ctx); err != nil {
		cancel()
		_ = commandProcess.Wait()
		t.Fatalf("confirm helper handoff: %v (%s)", err, diagnostics.String())
	}
	if err := commandProcess.Wait(); err != nil {
		t.Fatalf("helper/target subprocess: %v (%s)", err, diagnostics.String())
	}

	payload, err := os.ReadFile(reportPath)
	if err != nil {
		t.Fatalf("read target report: %v (%s)", err, diagnostics.String())
	}
	var report helperExecReport
	if err := json.Unmarshal(payload, &report); err != nil {
		t.Fatalf("decode target report: %v", err)
	}
	if !reflect.DeepEqual(report.Arguments, hostileArguments) {
		t.Fatalf("target argv = %#v, want %#v", report.Arguments, hostileArguments)
	}
	if report.Cwd != workingDirectory {
		t.Fatalf("target cwd = %q, want %q", report.Cwd, workingDirectory)
	}
	if !reflect.DeepEqual(report.Environment, writtenSpec.Env) {
		t.Fatal("target environment differs from exact launch snapshot")
	}
	if _, found := report.Environment[helperExecStaleEnv]; found {
		t.Fatal("stale helper variable survived syscall.Exec")
	}
	for _, dynamic := range []string{"TERM", "TMUX", "TMUX_PANE"} {
		if _, found := report.Environment[dynamic]; found {
			t.Fatalf("helper invented dynamic tmux variable %q outside tmux", dynamic)
		}
	}
}

func TestPrepareLaunchSpecOpensGateBeforeRemovingSecrets(t *testing.T) {
	runtimeDirectory := t.TempDir()
	files, err := createLaunchFiles(runtimeDirectory, "ordered-handshake", agent.Spec{
		Command: []string{"/usr/bin/true"},
		Env:     map[string]string{"PRIVATE_TOKEN": "must-disappear-before-release"},
	})
	if err != nil {
		t.Fatalf("create launch files: %v", err)
	}
	t.Cleanup(func() {
		files.close()
		files.remove()
	})

	openedWhileSpecPresent := false
	spec, gate, err := prepareLaunchSpec(files.specPath, files.gatePath, func(path string) (*os.File, error) {
		if _, statErr := os.Stat(files.specPath); statErr != nil {
			return nil, fmt.Errorf("spec removed before FIFO open: %w", statErr)
		}
		openedWhileSpecPresent = true
		return os.Open(path)
	})
	if err != nil {
		t.Fatalf("prepare launch: %v", err)
	}
	if !openedWhileSpecPresent {
		t.Fatal("FIFO opener was not called while the private spec existed")
	}
	if _, err := os.Stat(files.specPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private spec still exists after FIFO handshake: %v", err)
	}
	if spec.Env["PRIVATE_TOKEN"] != "must-disappear-before-release" {
		t.Fatal("prepared spec lost its in-memory environment snapshot")
	}
	if err := gate.Close(); err != nil {
		t.Fatalf("close prepared gate: %v", err)
	}
}

func argumentPosition(arguments []string, value string) int {
	for index, argument := range arguments {
		if argument == value {
			return index
		}
	}
	return -1
}

func environmentMap(environment []string) map[string]string {
	result := make(map[string]string, len(environment))
	for _, assignment := range environment {
		name, value, found := strings.Cut(assignment, "=")
		if found {
			result[name] = value
		}
	}
	return result
}

func withoutEnvironmentNames(environment []string, names ...string) []string {
	excluded := make(map[string]struct{}, len(names))
	for _, name := range names {
		excluded[name] = struct{}{}
	}
	result := make([]string, 0, len(environment))
	for _, assignment := range environment {
		name, _, found := strings.Cut(assignment, "=")
		if !found {
			continue
		}
		if _, skip := excluded[name]; !skip {
			result = append(result, assignment)
		}
	}
	return result
}

func waitForRemoval(ctx context.Context, path string) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}
