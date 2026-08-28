//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package main

import (
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/fixturecapture"
)

const (
	signalRunnerMarker = "__relayer_capture_signal_runner"
	signalTargetMarker = "__relayer_capture_signal_target"
	signalChildMarker  = "__relayer_capture_signal_child"
)

type signalCaptureRecord struct {
	TargetPID     int      `json:"target_pid"`
	ChildPID      int      `json:"child_pid"`
	TmuxServerPID int      `json:"tmux_server_pid,omitempty"`
	PrivateRoots  []string `json:"private_roots"`
}

func TestMain(m *testing.M) {
	if handled, exitCode := fixturecapture.HelperMain(os.Args[1:], io.Discard); handled {
		os.Exit(exitCode)
	}
	os.Exit(m.Run())
}

func TestCLIProcessForSignal(t *testing.T) {
	index := argumentIndex(signalRunnerMarker)
	if index < 0 {
		return
	}
	ctx, cancel := commandContext()
	defer cancel()
	os.Exit(run(ctx, os.Args[index+1:], io.Discard, io.Discard))
}

func TestCapturedProcessForSignal(t *testing.T) {
	index := argumentIndex(signalTargetMarker)
	if index < 0 {
		return
	}
	if len(os.Args) != index+3 {
		os.Exit(90)
	}
	signal.Ignore(syscall.SIGHUP, syscall.SIGTERM)
	recordPath := os.Args[index+1]
	readyPath := os.Args[index+2]
	child := exec.Command(os.Args[0], "-test.run=^TestLingeringCaptureChildForSignal$", "--", signalChildMarker, readyPath)
	child.Stdin = os.Stdin
	child.Stdout = os.Stdout
	child.Stderr = os.Stderr
	if err := child.Start(); err != nil {
		os.Exit(91)
	}
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(readyPath); err == nil {
			break
		}
		if !time.Now().Before(deadline) {
			_ = child.Process.Kill()
			os.Exit(92)
		}
		time.Sleep(10 * time.Millisecond)
	}
	record := signalCaptureRecord{
		TargetPID: os.Getpid(),
		ChildPID:  child.Process.Pid,
		PrivateRoots: []string{
			os.Getenv("HOME"),
			os.Getenv("TMPDIR"),
			os.Getenv("XDG_CONFIG_HOME"),
			os.Getenv("XDG_CACHE_HOME"),
			os.Getenv("XDG_DATA_HOME"),
		},
	}
	parts := strings.Split(os.Getenv("TMUX"), ",")
	if len(parts) >= 2 {
		record.TmuxServerPID, _ = strconv.Atoi(parts[1])
	}
	payload, err := json.Marshal(record)
	if err != nil || os.WriteFile(recordPath, payload, 0o600) != nil {
		_ = child.Process.Kill()
		os.Exit(93)
	}
	_, _ = io.WriteString(os.Stdout, "capture target ready\r\n")
	for {
		time.Sleep(time.Hour)
	}
}

func TestLingeringCaptureChildForSignal(t *testing.T) {
	index := argumentIndex(signalChildMarker)
	if index < 0 {
		return
	}
	if len(os.Args) != index+2 {
		os.Exit(94)
	}
	signal.Ignore(syscall.SIGHUP, syscall.SIGTERM)
	if err := os.WriteFile(os.Args[index+1], []byte("ready"), 0o600); err != nil {
		os.Exit(95)
	}
	for {
		time.Sleep(time.Hour)
	}
}

func TestSIGTERMCleansCaptureWithoutPublishingArtifact(t *testing.T) {
	t.Setenv("HOME", t.TempDir())
	backends := []string{"pty"}
	if tmuxPath, ok := privateTmuxAvailable(t); ok {
		backends = append(backends, "tmux="+tmuxPath)
	}
	for _, backendSpec := range backends {
		backend, tmuxPath, _ := strings.Cut(backendSpec, "=")
		t.Run(backend, func(t *testing.T) {
			testSIGTERMCleanup(t, backend, tmuxPath)
		})
	}
}

func testSIGTERMCleanup(t *testing.T, backend, tmuxPath string) {
	t.Helper()
	directory := t.TempDir()
	artifactPath := filepath.Join(directory, "must-not-exist.json")
	recordPath := filepath.Join(directory, "capture-record.json")
	readyPath := filepath.Join(directory, "child-ready")
	targetArgv := []string{
		os.Args[0], "-test.run=^TestCapturedProcessForSignal$", "--",
		signalTargetMarker, recordPath, readyPath,
	}
	arguments := []string{
		"-test.run=^TestCLIProcessForSignal$", "--", signalRunnerMarker,
		"--output", artifactPath,
		"--tool", "fixture-cli",
		"--adapter", "generic",
		"--backend", backend,
		"--timeout", "30s",
	}
	if tmuxPath != "" {
		arguments = append(arguments, "--tmux-path", tmuxPath)
	}
	arguments = append(arguments, "--")
	arguments = append(arguments, targetArgv...)
	runner := exec.Command(os.Args[0], arguments...)
	if err := runner.Start(); err != nil {
		t.Fatal(err)
	}
	waited := make(chan error, 1)
	go func() { waited <- runner.Wait() }()
	finished := false
	defer func() {
		if !finished {
			_ = runner.Process.Kill()
			<-waited
		}
	}()

	record := waitForSignalRecord(t, recordPath, waited)
	if err := runner.Process.Signal(syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-waited:
		finished = true
		if err == nil {
			t.Fatal("SIGTERM capture runner unexpectedly exited successfully")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SIGTERM capture runner did not finish cleanup")
	}

	for _, pid := range []int{record.TargetPID, record.ChildPID, record.TmuxServerPID} {
		if pid > 1 && !waitForProcessGone(pid, 3*time.Second) {
			t.Fatalf("capture process %d survived SIGTERM cleanup", pid)
		}
	}
	for _, path := range record.PrivateRoots {
		if path == "" {
			t.Fatal("captured process did not receive all private roots")
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private capture root %q survived SIGTERM cleanup: %v", path, err)
		}
	}
	if _, err := os.Stat(artifactPath); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("SIGTERM published an artifact: %v", err)
	}
}

func waitForSignalRecord(t *testing.T, path string, waited <-chan error) signalCaptureRecord {
	t.Helper()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		payload, err := os.ReadFile(path)
		if err == nil {
			var record signalCaptureRecord
			if err := json.Unmarshal(payload, &record); err != nil {
				t.Fatal(err)
			}
			return record
		}
		select {
		case err := <-waited:
			t.Fatalf("capture runner exited before target was ready: %v", err)
		default:
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("capture target did not become ready")
	return signalCaptureRecord{}
}

func privateTmuxAvailable(t *testing.T) (string, bool) {
	t.Helper()
	path, err := exec.LookPath("tmux")
	if err != nil {
		return "", false
	}
	directory, err := os.MkdirTemp("/tmp", ".relayer-signal-tmux-")
	if err != nil {
		return "", false
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		_ = os.RemoveAll(directory)
		return "", false
	}
	t.Cleanup(func() { _ = os.RemoveAll(directory) })
	for _, name := range []string{"home", "tmp"} {
		if err := os.Mkdir(filepath.Join(directory, name), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	socket := filepath.Join(directory, "probe.sock")
	environment := []string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + filepath.Join(directory, "home"),
		"TMPDIR=" + filepath.Join(directory, "tmp"),
		"TERM=xterm-256color",
	}
	command := exec.Command(path, "-f", "/dev/null", "-S", socket, "new-session", "-d", "-s", "probe", "exec /bin/sleep 5")
	command.Env = environment
	output, err := command.CombinedOutput()
	if err != nil || len(output) != 0 {
		return "", false
	}
	cleanup := exec.Command(path, "-S", socket, "kill-server")
	cleanup.Env = environment
	_ = cleanup.Run()
	return path, true
}

func waitForProcessGone(pid int, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		err := syscall.Kill(pid, 0)
		if err != nil && !errors.Is(err, syscall.EPERM) {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func argumentIndex(marker string) int {
	for index, argument := range os.Args {
		if argument == marker {
			return index
		}
	}
	return -1
}
