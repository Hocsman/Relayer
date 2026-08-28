//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package fixturecapture

import (
	"context"
	"errors"
	"fmt"
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
)

const childMarker = "__relayer_fixture_test_child"

func TestMain(m *testing.M) {
	if handled, exitCode := HelperMain(os.Args[1:], io.Discard); handled {
		os.Exit(exitCode)
	}
	os.Exit(m.Run())
}

func TestCaptureHelperProcess(t *testing.T) {
	index := -1
	for candidate, argument := range os.Args {
		if argument == childMarker {
			index = candidate
			break
		}
	}
	if index < 0 {
		return
	}
	arguments := os.Args[index+1:]
	if len(arguments) == 0 {
		os.Exit(90)
	}
	switch arguments[0] {
	case "emit":
		fmt.Fprint(os.Stdout, "fixture prompt? [y/N]\r\n")
	case "exit-seven":
		fmt.Fprint(os.Stdout, "planned failure\r\n")
		os.Exit(7)
	case "secret":
		fmt.Fprint(os.Stdout, "token=fixture-secret-value\r\n")
	case "identity":
		fmt.Fprint(os.Stdout, "/Users/fixture-person/project fixture@example.invalid\r\n")
	case "ansi":
		fmt.Fprint(os.Stdout, "safe \x1b[3")
		time.Sleep(5 * time.Millisecond)
		fmt.Fprint(os.Stdout, "1mcolored\x1b[0m prompt\r\n")
	case "environment":
		if os.Getenv("RELAYER_FIXTURE_TEST_TOKEN") == "" && os.Getenv("OPENAI_API_KEY") == "" {
			fmt.Fprint(os.Stdout, "sensitive_environment_absent\r\n")
		} else {
			fmt.Fprint(os.Stdout, "sensitive_environment_present\r\n")
		}
	case "tmpdir":
		temporary := os.Getenv("TMPDIR")
		if temporary == "" {
			os.Exit(95)
		}
		if err := os.WriteFile(arguments[1], []byte(temporary), 0o600); err != nil {
			os.Exit(96)
		}
		fmt.Fprint(os.Stdout, "private_tmpdir_configured\r\n")
	case "private-roots":
		values := make([]string, 0, 5)
		for _, name := range []string{"HOME", "TMPDIR", "XDG_CONFIG_HOME", "XDG_CACHE_HOME", "XDG_DATA_HOME"} {
			value := os.Getenv(name)
			info, err := os.Stat(value)
			if value == "" || err != nil || !info.IsDir() || info.Mode().Perm()&0o077 != 0 {
				os.Exit(101)
			}
			values = append(values, value)
		}
		if err := os.WriteFile(arguments[1], []byte(strings.Join(values, "\n")), 0o600); err != nil {
			os.Exit(102)
		}
		fmt.Fprint(os.Stdout, "private_home_and_xdg_configured\r\n")
	case "sabotage-runtime-permissions":
		runtimeDirectory := filepath.Dir(os.Getenv("HOME"))
		nested := filepath.Join(runtimeDirectory, "sabotage", "nested")
		if err := os.MkdirAll(nested, 0o700); err != nil {
			os.Exit(105)
		}
		if err := os.WriteFile(filepath.Join(nested, "retained"), []byte("safe"), 0o600); err != nil {
			os.Exit(106)
		}
		if err := os.WriteFile(arguments[1], []byte(runtimeDirectory), 0o600); err != nil {
			os.Exit(107)
		}
		fmt.Fprint(os.Stdout, "runtime permissions changed\r\n")
		if err := os.Chmod(nested, 0); err != nil {
			os.Exit(108)
		}
		if err := os.Chmod(runtimeDirectory, 0); err != nil {
			os.Exit(109)
		}
	case "flood":
		count, _ := strconv.Atoi(arguments[1])
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", count))
	case "barrier-flood-exit":
		if err := os.WriteFile(arguments[1], []byte("ready"), 0o600); err != nil {
			os.Exit(103)
		}
		deadline := time.Now().Add(2 * time.Second)
		for {
			if _, err := os.Stat(arguments[2]); err == nil {
				break
			}
			if !time.Now().Before(deadline) {
				os.Exit(104)
			}
			time.Sleep(time.Millisecond)
		}
		count, _ := strconv.Atoi(arguments[3])
		_, _ = io.WriteString(os.Stdout, strings.Repeat("x", count))
	case "tail":
		_, _ = io.WriteString(os.Stdout, strings.Repeat("t", 128*1024))
		_, _ = io.WriteString(os.Stdout, "END-OF-FIXTURE-TAIL")
	case "wait":
		time.Sleep(30 * time.Second)
	case "wait-ignore-hangup-and-term":
		signal.Ignore(syscall.SIGHUP, syscall.SIGTERM)
		if err := os.WriteFile(arguments[1], []byte("ready"), 0o600); err != nil {
			os.Exit(97)
		}
		time.Sleep(30 * time.Second)
	case "spawn":
		child := exec.Command(os.Args[0], "-test.run=^TestCaptureHelperProcess$", "--", childMarker, "wait")
		if err := child.Start(); err != nil {
			os.Exit(91)
		}
		if err := os.WriteFile(arguments[1], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			os.Exit(92)
		}
		time.Sleep(30 * time.Second)
	case "spawn-descendant-then-exit":
		child := exec.Command(os.Args[0], "-test.run=^TestCaptureHelperProcess$", "--", childMarker, "wait-ignore-hangup-and-term", arguments[2])
		child.Stdin = os.Stdin
		child.Stdout = os.Stdout
		child.Stderr = os.Stderr
		if err := child.Start(); err != nil {
			os.Exit(98)
		}
		if err := os.WriteFile(arguments[1], []byte(strconv.Itoa(child.Process.Pid)), 0o600); err != nil {
			_ = child.Process.Kill()
			os.Exit(99)
		}
		deadline := time.Now().Add(time.Second)
		for {
			if _, err := os.Stat(arguments[2]); err == nil {
				break
			}
			if !time.Now().Before(deadline) {
				_ = child.Process.Kill()
				os.Exit(100)
			}
			time.Sleep(5 * time.Millisecond)
		}
		fmt.Fprint(os.Stdout, "leader exited with a live descendant\r\n")
	default:
		os.Exit(93)
	}
	os.Exit(0)
}

func TestCapturePrivateHelperProcess(t *testing.T) {
	separator := -1
	for index, argument := range os.Args {
		if argument == "--" {
			separator = index
			break
		}
	}
	if separator < 0 || separator+1 >= len(os.Args) {
		return
	}
	handled, exitCode := HelperMain(os.Args[separator+1:], os.Stderr)
	if !handled {
		os.Exit(94)
	}
	os.Exit(exitCode)
}

func helperCommand(mode string, arguments ...string) []string {
	command := []string{os.Args[0], "-test.run=^TestCaptureHelperProcess$", "--", childMarker, mode}
	return append(command, arguments...)
}

func captureOptions(t *testing.T, backend Backend, mode string, arguments ...string) Options {
	t.Helper()
	return Options{
		Tool:       "fixture-cli",
		Adapter:    "generic",
		Backend:    backend,
		Command:    helperCommand(mode, arguments...),
		Timeout:    2 * time.Second,
		MaxBytes:   64 * 1024,
		Anonymizer: testAnonymizer(t),
	}
}

func fixtureOutput(fixture Fixture) string {
	var result strings.Builder
	for _, chunk := range fixture.Chunks {
		result.WriteString(chunk.Data)
	}
	return result.String()
}

func TestPTYCaptureExitCodeAnonymizationEnvironmentAndSecretRefusal(t *testing.T) {
	t.Setenv("RELAYER_FIXTURE_TEST_TOKEN", "parent-fixture-secret")
	t.Setenv("OPENAI_API_KEY", "sk-parent-fixture-secret")
	parentTemp := t.TempDir()
	t.Setenv("TMPDIR", parentTemp)
	parentHome := t.TempDir()
	t.Setenv("HOME", parentHome)

	fixture, err := Capture(context.Background(), captureOptions(t, BackendPTY, "exit-seven"))
	if err != nil {
		t.Fatal(err)
	}
	if fixture.Outcome != OutcomeExited || fixture.ExitCode == nil || *fixture.ExitCode != 7 || !strings.Contains(fixtureOutput(fixture), "planned failure") {
		t.Fatalf("exit fixture = %#v, output %q", fixture, fixtureOutput(fixture))
	}

	fixture, err = Capture(context.Background(), captureOptions(t, BackendPTY, "identity"))
	if err != nil {
		t.Fatal(err)
	}
	if output := fixtureOutput(fixture); !strings.Contains(output, "[HOME]/project") || !strings.Contains(output, "[EMAIL]") || strings.Contains(output, "fixture-person") {
		t.Fatalf("identity output was not anonymized: %q", output)
	}

	fixture, err = Capture(context.Background(), captureOptions(t, BackendPTY, "ansi"))
	if err != nil {
		t.Fatal(err)
	}
	if output := fixtureOutput(fixture); !strings.Contains(output, "safe colored prompt") || strings.Contains(output, "\x1b") {
		t.Fatalf("fragmented ANSI was not normalized: %q", output)
	}

	fixture, err = Capture(context.Background(), captureOptions(t, BackendPTY, "environment"))
	if err != nil {
		t.Fatal(err)
	}
	if output := fixtureOutput(fixture); !strings.Contains(output, "sensitive_environment_absent") || strings.Contains(output, "parent-fixture-secret") {
		t.Fatalf("child received or emitted a sensitive environment: %q", output)
	}
	tmpdirRecord := filepath.Join(t.TempDir(), "tmpdir.txt")
	fixture, err = Capture(context.Background(), captureOptions(t, BackendPTY, "tmpdir", tmpdirRecord))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fixtureOutput(fixture), "private_tmpdir_configured") || strings.Contains(fixtureOutput(fixture), parentTemp) {
		t.Fatalf("private TMPDIR result = %q", fixtureOutput(fixture))
	}
	privateTemp, err := os.ReadFile(tmpdirRecord)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(privateTemp)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("private child TMPDIR was not removed: %v", err)
	}
	privateRootsRecord := filepath.Join(t.TempDir(), "private-roots.txt")
	fixture, err = Capture(context.Background(), captureOptions(t, BackendPTY, "private-roots", privateRootsRecord))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(fixtureOutput(fixture), "private_home_and_xdg_configured") {
		t.Fatalf("private HOME/XDG result = %q", fixtureOutput(fixture))
	}
	assertPrivateRootsRemoved(t, privateRootsRecord, parentHome, parentTemp)

	if _, err := Capture(context.Background(), captureOptions(t, BackendPTY, "secret")); !errors.Is(err, ErrSensitiveContent) || strings.Contains(err.Error(), "fixture-secret-value") {
		t.Fatalf("secret capture error = %v", err)
	}
}

func TestPTYCaptureBoundsTimeoutAndKillsProcessGroup(t *testing.T) {
	options := captureOptions(t, BackendPTY, "flood", "12000")
	options.MaxBytes = 777
	fixture, err := Capture(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.Outcome != OutcomeOutputLimit || !fixture.Truncated || len(fixtureOutput(fixture)) != 777 {
		t.Fatalf("bounded fixture = %#v, output length %d", fixture, len(fixtureOutput(fixture)))
	}

	options = captureOptions(t, BackendPTY, "wait")
	options.Timeout = 50 * time.Millisecond
	started := time.Now()
	fixture, err = Capture(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.Outcome != OutcomeTimedOut || time.Since(started) > 3*time.Second {
		t.Fatalf("timeout fixture = %#v after %s", fixture, time.Since(started))
	}

	pidPath := filepath.Join(t.TempDir(), "child.pid")
	options = captureOptions(t, BackendPTY, "spawn", pidPath)
	options.Timeout = 100 * time.Millisecond
	if _, err := Capture(context.Background(), options); err != nil {
		t.Fatal(err)
	}
	payload, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(payload))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("descendant process %d survived capture timeout", pid)
	}
}

func TestPTYTruncationWinsFloodAndImmediateExitRace(t *testing.T) {
	type response struct {
		fixture Fixture
		err     error
	}
	for iteration := 0; iteration < 12; iteration++ {
		directory := t.TempDir()
		readyPath := filepath.Join(directory, "ready")
		releasePath := filepath.Join(directory, "release")
		options := captureOptions(t, BackendPTY, "barrier-flood-exit", readyPath, releasePath, "16384")
		options.MaxBytes = 37
		result := make(chan response, 1)
		go func() {
			fixture, err := Capture(context.Background(), options)
			result <- response{fixture: fixture, err: err}
		}()
		deadline := time.Now().Add(2 * time.Second)
		for {
			if _, err := os.Stat(readyPath); err == nil {
				break
			}
			if !time.Now().Before(deadline) {
				t.Fatal("barrier flood helper did not become ready")
			}
			time.Sleep(time.Millisecond)
		}
		if err := os.WriteFile(releasePath, []byte("release"), 0o600); err != nil {
			t.Fatal(err)
		}
		captured := <-result
		if captured.err != nil {
			t.Fatalf("iteration %d: %v", iteration, captured.err)
		}
		if captured.fixture.Outcome != OutcomeOutputLimit || !captured.fixture.Truncated || len(fixtureOutput(captured.fixture)) != options.MaxBytes {
			t.Fatalf("iteration %d returned %#v with %d bytes", iteration, captured.fixture, len(fixtureOutput(captured.fixture)))
		}
	}
}

func TestPTYCaptureLeaderExitKillsLingeringDescendant(t *testing.T) {
	directory := t.TempDir()
	pidPath := filepath.Join(directory, "descendant.pid")
	readyPath := filepath.Join(directory, "descendant.ready")
	options := captureOptions(t, BackendPTY, "spawn-descendant-then-exit", pidPath, readyPath)

	fixture, err := Capture(context.Background(), options)
	if err != nil {
		t.Fatal(err)
	}
	if fixture.Outcome != OutcomeExited || fixture.ExitCode == nil || *fixture.ExitCode != 0 {
		t.Fatalf("leader-exit fixture = %#v", fixture)
	}
	if !strings.Contains(fixtureOutput(fixture), "leader exited with a live descendant") {
		t.Fatalf("leader-exit output = %q", fixtureOutput(fixture))
	}
	payload, err := os.ReadFile(pidPath)
	if err != nil {
		t.Fatal(err)
	}
	pid, err := strconv.Atoi(string(payload))
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(2 * time.Second)
	for processExists(pid) && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(pid) {
		t.Fatalf("descendant process %d survived its PTY leader", pid)
	}
}

func TestPTYCaptureRestoresAndRemovesSabotagedRuntime(t *testing.T) {
	recordPath := filepath.Join(t.TempDir(), "runtime-root")
	fixture, err := Capture(context.Background(), captureOptions(t, BackendPTY, "sabotage-runtime-permissions", recordPath))
	assertSabotagedRuntimeWasRemoved(t, recordPath, fixture, err)
	if err == nil && (fixture.Outcome != OutcomeExited || fixture.ExitCode == nil || *fixture.ExitCode != 0) {
		t.Fatalf("permission-sabotage fixture = %#v", fixture)
	}
}

func TestPTYCaptureCancellationProducesNoArtifact(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	fixture, err := Capture(ctx, captureOptions(t, BackendPTY, "emit"))
	if !errors.Is(err, context.Canceled) || fixture.SchemaVersion != 0 || fixture.Chunks != nil {
		t.Fatalf("cancelled Capture() = %#v, %v", fixture, err)
	}
}

func TestPTYLaunchFailureDoesNotEchoExecutable(t *testing.T) {
	options := captureOptions(t, BackendPTY, "emit")
	options.Command = []string{"token=fixture-executable-secret"}
	fixture, err := Capture(context.Background(), options)
	if err == nil || fixture.SchemaVersion != 0 || strings.Contains(err.Error(), options.Command[0]) {
		t.Fatalf("failed PTY launch = %#v, %v", fixture, err)
	}
}

func TestTmuxControlBufferIsBounded(t *testing.T) {
	buffer := boundedBuffer{limit: 7}
	input := []byte(strings.Repeat("x", 128*1024))
	written, err := buffer.Write(input)
	if err != nil || written != len(input) || buffer.len() != 7 || !buffer.truncated {
		t.Fatalf("bounded control buffer wrote %d bytes: len=%d truncated=%t err=%v", written, buffer.len(), buffer.truncated, err)
	}
	input[0] = 'z'
	if string(buffer.bytes()) != "xxxxxxx" {
		t.Fatalf("bounded control buffer retained caller storage: %q", buffer.bytes())
	}
}

func TestPTYCaptureCancellationAndNormalExitCloseDescriptors(t *testing.T) {
	descriptorDirectory := "/proc/self/fd"
	if _, err := os.Stat(descriptorDirectory); err != nil {
		descriptorDirectory = "/dev/fd"
	}
	before, err := os.ReadDir(descriptorDirectory)
	if err != nil {
		t.Skip("process descriptor inventory is unavailable")
	}
	for iteration := 0; iteration < 12; iteration++ {
		if _, err := Capture(context.Background(), captureOptions(t, BackendPTY, "emit")); err != nil {
			t.Fatal(err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(30 * time.Millisecond)
		cancel()
	}()
	if _, err := Capture(ctx, captureOptions(t, BackendPTY, "wait")); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled capture error = %v", err)
	}
	after, err := os.ReadDir(descriptorDirectory)
	if err != nil {
		t.Fatal(err)
	}
	if len(after) > len(before)+2 {
		t.Fatalf("capture retained descriptors: before %d, after %d", len(before), len(after))
	}
}

func TestTmuxCaptureUsesPrivateServerAndLeavesForeignServerAlone(t *testing.T) {
	tmuxPath, err := exec.LookPath("tmux")
	if err != nil {
		t.Skip("tmux is not installed")
	}
	t.Setenv("RELAYER_FIXTURE_TEST_TOKEN", "parent-tmux-fixture-secret")
	t.Setenv("OPENAI_API_KEY", "sk-parent-tmux-fixture-secret")
	parentTemp := t.TempDir()
	t.Setenv("TMPDIR", parentTemp)
	parentHome := t.TempDir()
	t.Setenv("HOME", parentHome)
	foreignDirectory, err := os.MkdirTemp("/tmp", ".relayer-foreign-")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(foreignDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(foreignDirectory) })
	foreignSocket := filepath.Join(foreignDirectory, "foreign.sock")
	foreign := exec.Command(tmuxPath, "-f", "/dev/null", "-S", foreignSocket, "new-session", "-d", "-s", "foreign", "exec /bin/sleep 30")
	if err := createPrivateEnvironment(foreignDirectory); err != nil {
		t.Fatal(err)
	}
	foreign.Env = safeEnvironment(foreignDirectory)
	if output, err := foreign.CombinedOutput(); err != nil || len(output) != 0 {
		t.Skip("test environment does not permit a private tmux socket")
	}
	t.Cleanup(func() {
		command := exec.Command(tmuxPath, "-S", foreignSocket, "kill-server")
		command.Env = safeEnvironment(foreignDirectory)
		_ = command.Run()
	})

	options := captureOptions(t, BackendTmux, "emit")
	options.TmuxPath = tmuxPath
	helperPath, diagnosticPath := tmuxTestHelper(t)
	options.HelperPath = helperPath
	fixture, err := Capture(context.Background(), options)
	if err != nil {
		if diagnostic, readErr := os.ReadFile(diagnosticPath); readErr == nil {
			t.Logf("private helper diagnostic: %s", diagnostic)
		}
		t.Fatal(err)
	}
	if fixture.Backend != BackendTmux || fixture.Outcome != OutcomeExited || !strings.Contains(fixtureOutput(fixture), "fixture prompt") {
		t.Fatalf("tmux fixture = %#v, output %q", fixture, fixtureOutput(fixture))
	}
	exitOptions := captureOptions(t, BackendTmux, "exit-seven")
	exitOptions.TmuxPath = tmuxPath
	exitOptions.HelperPath = helperPath
	exited, err := Capture(context.Background(), exitOptions)
	if err != nil {
		t.Fatalf("tmux nonzero exit capture: %v", err)
	}
	if exited.Outcome != OutcomeExited || exited.ExitCode == nil || *exited.ExitCode != 7 {
		t.Fatalf("tmux nonzero exit fixture = %#v", exited)
	}
	environmentOptions := captureOptions(t, BackendTmux, "environment")
	environmentOptions.TmuxPath = tmuxPath
	environmentOptions.HelperPath = helperPath
	environmentFixture, err := Capture(context.Background(), environmentOptions)
	if err != nil {
		t.Fatalf("tmux environment capture: %v", err)
	}
	if output := fixtureOutput(environmentFixture); !strings.Contains(output, "sensitive_environment_absent") || strings.Contains(output, "parent-tmux-fixture-secret") {
		t.Fatalf("tmux child received or emitted a sensitive environment: %q", output)
	}
	tmpdirRecord := filepath.Join(t.TempDir(), "tmux-tmpdir.txt")
	tmpdirOptions := captureOptions(t, BackendTmux, "tmpdir", tmpdirRecord)
	tmpdirOptions.TmuxPath = tmuxPath
	tmpdirOptions.HelperPath = helperPath
	tmpdirFixture, err := Capture(context.Background(), tmpdirOptions)
	if err != nil {
		t.Fatalf("tmux TMPDIR capture: %v", err)
	}
	if output := fixtureOutput(tmpdirFixture); !strings.Contains(output, "private_tmpdir_configured") || strings.Contains(output, parentTemp) {
		t.Fatalf("tmux private TMPDIR output = %q", output)
	}
	privateTemp, err := os.ReadFile(tmpdirRecord)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(string(privateTemp)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("tmux private child TMPDIR was not removed: %v", err)
	}
	tmuxPrivateRootsRecord := filepath.Join(t.TempDir(), "tmux-private-roots.txt")
	privateRootsOptions := captureOptions(t, BackendTmux, "private-roots", tmuxPrivateRootsRecord)
	privateRootsOptions.TmuxPath = tmuxPath
	privateRootsOptions.HelperPath = helperPath
	privateRootsFixture, err := Capture(context.Background(), privateRootsOptions)
	if err != nil {
		t.Fatalf("tmux private HOME/XDG capture: %v", err)
	}
	if !strings.Contains(fixtureOutput(privateRootsFixture), "private_home_and_xdg_configured") {
		t.Fatalf("tmux private HOME/XDG result = %q", fixtureOutput(privateRootsFixture))
	}
	assertPrivateRootsRemoved(t, tmuxPrivateRootsRecord, parentHome, parentTemp)
	tailOptions := captureOptions(t, BackendTmux, "tail")
	tailOptions.TmuxPath = tmuxPath
	tailOptions.HelperPath = helperPath
	tailOptions.MaxBytes = 256 * 1024
	tail, err := Capture(context.Background(), tailOptions)
	if err != nil {
		t.Fatalf("tmux tail capture: %v", err)
	}
	if tail.Outcome != OutcomeExited || !strings.HasSuffix(fixtureOutput(tail), "END-OF-FIXTURE-TAIL") {
		t.Fatalf("tmux tail was not completely drained: outcome %q, output length %d", tail.Outcome, len(fixtureOutput(tail)))
	}
	timeoutOptions := captureOptions(t, BackendTmux, "wait")
	timeoutOptions.TmuxPath = tmuxPath
	timeoutOptions.HelperPath = helperPath
	timeoutOptions.Timeout = 80 * time.Millisecond
	timedOut, err := Capture(context.Background(), timeoutOptions)
	if err != nil {
		t.Fatalf("tmux timeout capture: %v", err)
	}
	if timedOut.Outcome != OutcomeTimedOut {
		t.Fatalf("tmux timeout outcome = %q", timedOut.Outcome)
	}
	tmuxChildPIDPath := filepath.Join(t.TempDir(), "tmux-child.pid")
	spawnOptions := captureOptions(t, BackendTmux, "spawn", tmuxChildPIDPath)
	spawnOptions.TmuxPath = tmuxPath
	spawnOptions.HelperPath = helperPath
	spawnOptions.Timeout = 100 * time.Millisecond
	if _, err := Capture(context.Background(), spawnOptions); err != nil {
		t.Fatalf("tmux pane process-group capture: %v", err)
	}
	tmuxChildPIDBytes, err := os.ReadFile(tmuxChildPIDPath)
	if err != nil {
		t.Fatal(err)
	}
	tmuxChildPID, err := strconv.Atoi(string(tmuxChildPIDBytes))
	if err != nil {
		t.Fatal(err)
	}
	childDeadline := time.Now().Add(2 * time.Second)
	for processExists(tmuxChildPID) && time.Now().Before(childDeadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if processExists(tmuxChildPID) {
		t.Fatalf("tmux descendant process %d survived capture timeout", tmuxChildPID)
	}
	boundedOptions := captureOptions(t, BackendTmux, "flood", "12000")
	boundedOptions.TmuxPath = tmuxPath
	boundedOptions.HelperPath = helperPath
	boundedOptions.MaxBytes = 333
	bounded, err := Capture(context.Background(), boundedOptions)
	if err != nil {
		t.Fatalf("bounded tmux capture: %v", err)
	}
	if bounded.Outcome != OutcomeOutputLimit || !bounded.Truncated || len(fixtureOutput(bounded)) != 333 {
		t.Fatalf("bounded tmux fixture = %#v, output length %d", bounded, len(fixtureOutput(bounded)))
	}
	cancelOptions := captureOptions(t, BackendTmux, "wait")
	cancelOptions.TmuxPath = tmuxPath
	cancelOptions.HelperPath = helperPath
	cancelCtx, cancelCapture := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancelCapture()
	}()
	if fixture, err := Capture(cancelCtx, cancelOptions); !errors.Is(err, context.Canceled) || fixture.SchemaVersion != 0 {
		t.Fatalf("cancelled tmux capture = %#v, %v", fixture, err)
	}
	launchOptions := captureOptions(t, BackendTmux, "emit")
	launchOptions.TmuxPath = tmuxPath
	launchOptions.HelperPath = helperPath
	launchOptions.Command = []string{"relayer-fixture-command-that-does-not-exist"}
	if fixture, err := Capture(context.Background(), launchOptions); err == nil || fixture.SchemaVersion != 0 || strings.Contains(err.Error(), launchOptions.Command[0]) {
		t.Fatalf("failed tmux launch = %#v, %v", fixture, err)
	}
	tmuxSabotageRecord := filepath.Join(t.TempDir(), "tmux-runtime-root")
	tmuxSabotageOptions := captureOptions(t, BackendTmux, "sabotage-runtime-permissions", tmuxSabotageRecord)
	tmuxSabotageOptions.TmuxPath = tmuxPath
	tmuxSabotageOptions.HelperPath = helperPath
	tmuxSabotageOptions.Timeout = 250 * time.Millisecond
	tmuxSabotageFixture, tmuxSabotageErr := Capture(context.Background(), tmuxSabotageOptions)
	assertSabotagedRuntimeWasRemoved(t, tmuxSabotageRecord, tmuxSabotageFixture, tmuxSabotageErr)
	probe := exec.Command(tmuxPath, "-S", foreignSocket, "has-session", "-t", "foreign")
	probe.Env = safeEnvironment(foreignDirectory)
	if err := probe.Run(); err != nil {
		t.Fatalf("private capture interfered with foreign tmux server: %v", err)
	}
}

func tmuxTestHelper(t *testing.T) (string, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "fixture-helper")
	diagnosticPath := path + ".err"
	script := "#!/bin/sh\nexec " + quotePOSIX(os.Args[0]) + " -test.run '^TestCapturePrivateHelperProcess$' -- \"$@\" 2>" + quotePOSIX(diagnosticPath) + "\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path, diagnosticPath
}

func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func assertSabotagedRuntimeWasRemoved(t *testing.T, recordPath string, fixture Fixture, captureErr error) {
	t.Helper()
	payload, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	runtimeDirectory := string(payload)
	if captureErr != nil {
		if fixture.SchemaVersion != 0 || strings.Contains(captureErr.Error(), runtimeDirectory) {
			t.Fatalf("permission sabotage did not fail closed: fixture %#v, error %v", fixture, captureErr)
		}
	}
	if _, err := os.Lstat(runtimeDirectory); !errors.Is(err, os.ErrNotExist) {
		_ = os.Chmod(runtimeDirectory, 0o700)
		_ = os.RemoveAll(runtimeDirectory)
		t.Fatalf("sabotaged runtime survived cleanup: %v", err)
	}
}

func assertPrivateRootsRemoved(t *testing.T, recordPath, parentHome, parentTemp string) {
	t.Helper()
	payload, err := os.ReadFile(recordPath)
	if err != nil {
		t.Fatal(err)
	}
	paths := strings.Split(string(payload), "\n")
	if len(paths) != 5 {
		t.Fatalf("private environment roots = %#v", paths)
	}
	wantNames := []string{"home", "tmp", "xdg-config", "xdg-cache", "xdg-data"}
	for index, path := range paths {
		if path == parentHome || path == parentTemp || filepath.Base(path) != wantNames[index] {
			t.Fatalf("private environment root %d = %q", index, path)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("private environment root %q was not removed: %v", path, err)
		}
	}
}

// TestTmuxFieldSeparatorIsPrintable mirrors the tmuxbackend guard of the same
// name. The capture tool builds a deliberately minimal child environment, so it
// is even more exposed to tmux rewriting an unprintable separator in format
// output than the runtime backend is.
func TestTmuxFieldSeparatorIsPrintable(t *testing.T) {
	if tmuxFieldSeparator == "" {
		t.Fatal("tmux field separator is empty")
	}
	for _, character := range tmuxFieldSeparator {
		if character < 0x21 || character > 0x7e {
			t.Fatalf("tmux field separator %q contains %q, which tmux may rewrite in format output",
				tmuxFieldSeparator, character)
		}
	}
	for _, value := range []string{"$0", "%0", "30666"} {
		if strings.Contains(value, tmuxFieldSeparator) {
			t.Fatalf("tmux field separator %q occurs inside field value %q", tmuxFieldSeparator, value)
		}
	}
}
