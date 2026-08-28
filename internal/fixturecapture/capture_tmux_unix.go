//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package fixturecapture

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

const (
	helperSubcommand = "__relayer_fixture_capture_exec"
	maxLaunchSpec    = 1024 * 1024
	maxLaunchStatus  = 128
	maxTmuxControl   = 64 * 1024
	tmuxControlLimit = 5 * time.Second
	tmuxWaitDelay    = 250 * time.Millisecond
	helperGateLimit  = 10 * time.Second
)

type captureLaunchSpec struct {
	Command []string `json:"command"`
	Cwd     string   `json:"cwd,omitempty"`
}

type captureLaunchStatus struct {
	ExitCode     *int `json:"exit_code,omitempty"`
	LaunchFailed bool `json:"launch_failed,omitempty"`
}

type tmuxIdentity struct {
	sessionID string
	paneID    string
	panePID   int
}

type boundedBuffer struct {
	data      bytes.Buffer
	limit     int
	truncated bool
}

func (buffer *boundedBuffer) Write(value []byte) (int, error) {
	remaining := buffer.limit - buffer.data.Len()
	if remaining > 0 {
		_, _ = buffer.data.Write(value[:min(remaining, len(value))])
	}
	if len(value) > remaining {
		buffer.truncated = true
	}
	return len(value), nil
}

func (buffer *boundedBuffer) bytes() []byte { return bytes.Clone(buffer.data.Bytes()) }

func (buffer *boundedBuffer) string() string { return buffer.data.String() }

func (buffer *boundedBuffer) len() int { return buffer.data.Len() }

func captureTmux(ctx context.Context, options Options) (result captureResult, resultErr error) {
	tmuxPath := strings.TrimSpace(options.TmuxPath)
	if tmuxPath == "" {
		resolved, err := exec.LookPath("tmux")
		if err != nil {
			return captureResult{}, fmt.Errorf("tmux capture unavailable: %w", err)
		}
		tmuxPath = resolved
	}
	// tmux uses Unix-domain sockets, whose path limit is notably short on
	// macOS. A short system temp root keeps the randomized private socket under
	// that limit even when TMPDIR is a long per-user path.
	runtimeDirectory, err := os.MkdirTemp(shortSystemTemp(), ".relayer-fixture-")
	if err != nil {
		return captureResult{}, fmt.Errorf("create private tmux runtime: %w", err)
	}
	if err := os.Chmod(runtimeDirectory, 0o700); err != nil {
		return captureResult{}, errors.Join(errors.New("restrict private tmux runtime"), cleanupPrivateRuntime(runtimeDirectory))
	}
	if err := createPrivateEnvironment(runtimeDirectory); err != nil {
		return captureResult{}, errors.Join(err, cleanupPrivateRuntime(runtimeDirectory))
	}
	defer func() {
		if cleanupErr := cleanupPrivateRuntime(runtimeDirectory); cleanupErr != nil {
			zeroBytes(result.raw)
			result = captureResult{}
			resultErr = errors.Join(resultErr, cleanupErr)
		}
	}()

	socketPath := filepath.Join(runtimeDirectory, "s")
	specPath := filepath.Join(runtimeDirectory, "launch.json")
	statusPath := specPath + ".status"
	releasePath := filepath.Join(runtimeDirectory, "launch.release")
	outputPath := filepath.Join(runtimeDirectory, "terminal.output")
	donePath := filepath.Join(runtimeDirectory, "terminal.done")
	helperPath := strings.TrimSpace(options.HelperPath)
	if helperPath == "" {
		helperPath, err = os.Executable()
		if err != nil {
			return captureResult{}, errors.New("resolve fixture capture helper executable")
		}
	}
	serverPresent := false
	identity := tmuxIdentity{}
	defer func() {
		if serverPresent {
			restorePrivateRuntimePermissions(runtimeDirectory)
			cleanupCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			cleanupErr := stopPrivateTmux(cleanupCtx, tmuxPath, socketPath, identity, identity.panePID > 1)
			cancel()
			if cleanupErr != nil {
				zeroBytes(result.raw)
				result = captureResult{}
				resultErr = errors.Join(resultErr, cleanupErr)
			}
		}
	}()
	_, err = runTmux(ctx, tmuxPath, socketPath, true,
		"new-session", "-d",
		"-s", "relayer-fixture-capture", "-x", "100", "-y", "30", "exec /bin/sleep 30",
	)
	if err != nil {
		if _, statErr := os.Lstat(socketPath); statErr == nil {
			serverPresent = true
		}
		return captureResult{}, err
	}
	serverPresent = true
	identityOutput, err := runTmux(ctx, tmuxPath, socketPath, false,
		"list-panes", "-a", "-F", "#{session_id}\t#{pane_id}\t#{pane_pid}",
	)
	if err != nil {
		return captureResult{}, err
	}
	identity, err = parseTmuxIdentity(identityOutput)
	if err != nil {
		return captureResult{}, err
	}
	if err := writeLaunchSpec(specPath, captureLaunchSpec{Command: options.Command, Cwd: options.Cwd}); err != nil {
		return captureResult{}, err
	}
	if err := createPrivateOutputFile(outputPath); err != nil {
		return captureResult{}, err
	}
	helperCommand := "exec " + quotePOSIX(helperPath) + " " + quotePOSIX(helperSubcommand) + " " + quotePOSIX(specPath) + " " + quotePOSIX(releasePath)
	pipeCommand := "umask 077; if /usr/bin/head -c " + strconv.Itoa(options.MaxBytes+1) + " > " + quotePOSIX(outputPath) + "; then : > " + quotePOSIX(donePath) + "; fi"
	if _, err := runTmux(ctx, tmuxPath, socketPath, false, "pipe-pane", "-o", "-t", identity.paneID, pipeCommand); err != nil {
		return captureResult{}, err
	}
	if _, err := runTmux(ctx, tmuxPath, socketPath, false, "respawn-pane", "-k", "-t", identity.paneID, helperCommand); err != nil {
		return captureResult{}, err
	}
	identityOutput, err = runTmux(ctx, tmuxPath, socketPath, false,
		"list-panes", "-a", "-F", "#{session_id}\t#{pane_id}\t#{pane_pid}",
	)
	if err != nil {
		return captureResult{}, err
	}
	identity, err = parseTmuxIdentity(identityOutput)
	if err != nil {
		return captureResult{}, err
	}
	if err := createPrivateSignal(releasePath); err != nil {
		return captureResult{}, err
	}

	timer := time.NewTimer(options.Timeout)
	defer timer.Stop()
	poller := time.NewTicker(20 * time.Millisecond)
	defer poller.Stop()
	for {
		select {
		case <-poller.C:
			acknowledged, err := tmuxSinkAcknowledged(donePath)
			if err != nil {
				return captureResult{}, err
			}
			if !acknowledged {
				continue
			}
			raw, truncated, err := readPrivateOutput(outputPath, options.MaxBytes)
			if err != nil {
				return captureResult{}, err
			}
			if truncated {
				stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				err = stopPrivateTmux(stopCtx, tmuxPath, socketPath, identity, true)
				cancel()
				serverPresent = err != nil
				if err != nil {
					zeroBytes(raw)
					return captureResult{}, err
				}
				return captureResult{raw: raw, outcome: OutcomeOutputLimit, truncated: true}, nil
			}
			status, err := readCaptureStatus(statusPath)
			if err != nil {
				zeroBytes(raw)
				return captureResult{}, err
			}
			if status.LaunchFailed {
				zeroBytes(raw)
				return captureResult{}, errors.New("captured command could not be launched")
			}
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err = stopPrivateTmux(stopCtx, tmuxPath, socketPath, identity, true)
			cancel()
			serverPresent = err != nil
			if err != nil {
				zeroBytes(raw)
				return captureResult{}, err
			}
			return captureResult{raw: raw, outcome: OutcomeExited, exitCode: status.ExitCode}, nil
		case <-timer.C:
			drainCtx, drainCancel := context.WithTimeout(context.Background(), 3*time.Second)
			drainErr := finishTmuxSink(drainCtx, tmuxPath, socketPath, identity.paneID, donePath)
			drainCancel()
			if drainErr != nil {
				return captureResult{}, drainErr
			}
			raw, truncated, readErr := readPrivateOutput(outputPath, options.MaxBytes)
			if readErr != nil {
				return captureResult{}, readErr
			}
			status, statusErr := readCaptureStatusIfPresent(statusPath)
			if statusErr != nil {
				zeroBytes(raw)
				return captureResult{}, statusErr
			}
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			err := stopPrivateTmux(stopCtx, tmuxPath, socketPath, identity, true)
			cancel()
			serverPresent = err != nil
			if err != nil {
				zeroBytes(raw)
				return captureResult{}, err
			}
			if truncated {
				return captureResult{raw: raw, outcome: OutcomeOutputLimit, truncated: true}, nil
			}
			if status.LaunchFailed {
				zeroBytes(raw)
				return captureResult{}, errors.New("captured command could not be launched")
			}
			if status.ExitCode != nil {
				return captureResult{raw: raw, outcome: OutcomeExited, exitCode: status.ExitCode}, nil
			}
			return captureResult{raw: raw, outcome: OutcomeTimedOut}, nil
		case <-ctx.Done():
			drainCtx, drainCancel := context.WithTimeout(context.Background(), 3*time.Second)
			drainErr := finishTmuxSink(drainCtx, tmuxPath, socketPath, identity.paneID, donePath)
			drainCancel()
			stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			stopErr := stopPrivateTmux(stopCtx, tmuxPath, socketPath, identity, true)
			cancel()
			serverPresent = stopErr != nil
			return captureResult{}, errors.Join(ctx.Err(), drainErr, stopErr)
		}
	}
}

func runTmux(ctx context.Context, path, socket string, initial bool, arguments ...string) ([]byte, error) {
	global := []string{"-S", socket}
	if initial {
		global = []string{"-f", "/dev/null", "-S", socket}
	}
	commandCtx, cancel := context.WithTimeout(ctx, tmuxControlLimit)
	defer cancel()
	command := exec.CommandContext(commandCtx, path, append(global, arguments...)...)
	command.WaitDelay = tmuxWaitDelay
	command.Env = safeEnvironment(filepath.Dir(socket))
	stdout := boundedBuffer{limit: maxTmuxControl}
	stderr := boundedBuffer{limit: maxTmuxControl}
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	if err != nil || stderr.len() > 0 || stdout.truncated || stderr.truncated {
		operation := "command"
		if len(arguments) > 0 {
			operation = arguments[0]
		}
		// Deliberately omit stderr, socket paths, and argv from diagnostics.
		failure := fmt.Errorf("private tmux %s failed", operation)
		if ctxErr := commandCtx.Err(); ctxErr != nil {
			return nil, errors.Join(failure, ctxErr)
		}
		return nil, failure
	}
	return stdout.bytes(), nil
}

func parseTmuxIdentity(output []byte) (tmuxIdentity, error) {
	line := strings.TrimSuffix(string(output), "\n")
	if strings.ContainsAny(line, "\r\n") {
		return tmuxIdentity{}, errors.New("private tmux returned multiple identities")
	}
	fields := strings.Split(line, "\t")
	if len(fields) != 3 || !numericTmuxID(fields[0], '$') || !numericTmuxID(fields[1], '%') {
		return tmuxIdentity{}, errors.New("private tmux returned an invalid identity")
	}
	pid, err := strconv.Atoi(fields[2])
	if err != nil || pid < 2 {
		return tmuxIdentity{}, errors.New("private tmux returned an invalid pane process")
	}
	return tmuxIdentity{sessionID: fields[0], paneID: fields[1], panePID: pid}, nil
}

func numericTmuxID(value string, prefix byte) bool {
	if len(value) < 2 || value[0] != prefix {
		return false
	}
	_, err := strconv.ParseUint(value[1:], 10, 64)
	return err == nil
}

func stopPrivateTmux(ctx context.Context, path, socket string, identity tmuxIdentity, terminateTree bool) error {
	if terminateTree && identity.panePID > 1 {
		if tmuxProcessGroupExists(identity.panePID) {
			_ = syscall.Kill(-identity.panePID, syscall.SIGTERM)
			graceDeadline := time.Now().Add(50 * time.Millisecond)
			for tmuxProcessGroupExists(identity.panePID) && time.Now().Before(graceDeadline) {
				select {
				case <-ctx.Done():
					return errors.Join(errors.New("private tmux pane process-group termination was interrupted"), ctx.Err())
				case <-time.After(10 * time.Millisecond):
				}
			}
			if tmuxProcessGroupExists(identity.panePID) {
				_ = syscall.Kill(-identity.panePID, syscall.SIGKILL)
				_ = syscall.Kill(identity.panePID, syscall.SIGKILL)
			}
		}
	}
	_, _ = runTmux(ctx, path, socket, false, "kill-server")
	for {
		gone, probeErr := privateTmuxGone(ctx, path, socket)
		if probeErr != nil {
			return probeErr
		}
		if gone {
			break
		}
		select {
		case <-ctx.Done():
			return errors.Join(errors.New("private tmux server disappearance was not confirmed"), ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
	_ = os.Remove(socket)
	if terminateTree && identity.panePID > 1 {
		for tmuxProcessGroupExists(identity.panePID) {
			select {
			case <-ctx.Done():
				return errors.Join(errors.New("private tmux pane process group disappearance was not confirmed"), ctx.Err())
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
	return nil
}

func tmuxProcessGroupExists(pid int) bool {
	err := syscall.Kill(-pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

func privateTmuxGone(ctx context.Context, path, socket string) (bool, error) {
	command := exec.CommandContext(ctx, path, "-S", socket, "has-session")
	command.WaitDelay = tmuxWaitDelay
	command.Env = safeEnvironment(filepath.Dir(socket))
	stderr := boundedBuffer{limit: maxTmuxControl}
	command.Stderr = &stderr
	err := command.Run()
	if err == nil && stderr.len() == 0 && !stderr.truncated {
		return false, nil
	}
	if ctxErr := ctx.Err(); ctxErr != nil {
		return false, errors.Join(errors.New("private tmux server probe was interrupted"), ctxErr)
	}
	if stderr.truncated {
		return false, errors.New("private tmux server probe returned an oversized diagnostic")
	}
	diagnostic := strings.ToLower(strings.TrimSpace(stderr.string()))
	if strings.Contains(diagnostic, "\n") {
		return false, errors.New("private tmux server probe returned an invalid diagnostic")
	}
	if strings.HasPrefix(diagnostic, "no server running on ") ||
		(strings.HasPrefix(diagnostic, "error connecting to ") &&
			(strings.HasSuffix(diagnostic, "(connection refused)") || strings.HasSuffix(diagnostic, "(no such file or directory)"))) {
		return true, nil
	}
	return false, errors.New("private tmux server disappearance could not be verified")
}

func createPrivateOutputFile(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create private tmux output file")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return errors.New("restrict private tmux output file")
	}
	if err := file.Close(); err != nil {
		return errors.New("close private tmux output file")
	}
	return nil
}

func createPrivateSignal(path string) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create private fixture launch signal")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return errors.New("restrict private fixture launch signal")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("sync private fixture launch signal")
	}
	if err := file.Close(); err != nil {
		return errors.New("close private fixture launch signal")
	}
	return nil
}

func tmuxSinkAcknowledged(path string) (bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() != 0 {
		return false, errors.New("private tmux output acknowledgement is invalid")
	}
	return true, nil
}

func waitTmuxSink(ctx context.Context, donePath string) error {
	for {
		acknowledged, err := tmuxSinkAcknowledged(donePath)
		if err != nil {
			return err
		}
		if acknowledged {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.Join(errors.New("private tmux output acknowledgement was not received"), ctx.Err())
		case <-time.After(10 * time.Millisecond):
		}
	}
}

func finishTmuxSink(ctx context.Context, tmuxPath, socketPath, paneID, donePath string) error {
	acknowledged, err := tmuxSinkAcknowledged(donePath)
	if err != nil || acknowledged {
		return err
	}
	if _, err := runTmux(ctx, tmuxPath, socketPath, false, "pipe-pane", "-t", paneID); err != nil {
		// A concurrent command exit or output-limit completion may have already
		// closed the pipe. The sink acknowledgement, rather than the tmux
		// diagnostic, is authoritative for a complete drain.
		if waitErr := waitTmuxSink(ctx, donePath); waitErr != nil {
			return errors.Join(err, waitErr)
		}
		return nil
	}
	return waitTmuxSink(ctx, donePath)
}

func readCaptureStatus(path string) (captureLaunchStatus, error) {
	status, present, err := readCaptureStatusFile(path)
	if err != nil {
		return captureLaunchStatus{}, err
	}
	if !present {
		return captureLaunchStatus{}, errors.New("private fixture launch status is missing")
	}
	return status, nil
}

func readCaptureStatusIfPresent(path string) (captureLaunchStatus, error) {
	status, _, err := readCaptureStatusFile(path)
	return status, err
}

func readCaptureStatusFile(path string) (captureLaunchStatus, bool, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return captureLaunchStatus{}, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxLaunchStatus {
		return captureLaunchStatus{}, false, errors.New("private fixture launch status is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return captureLaunchStatus{}, false, errors.New("open private fixture launch status")
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxLaunchStatus+1))
	decoder.DisallowUnknownFields()
	var status captureLaunchStatus
	if err := decoder.Decode(&status); err != nil {
		return captureLaunchStatus{}, false, errors.New("decode private fixture launch status")
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return captureLaunchStatus{}, false, errors.New("invalid trailing private launch status")
	}
	if (status.ExitCode == nil) == !status.LaunchFailed {
		return captureLaunchStatus{}, false, errors.New("private fixture launch status is inconsistent")
	}
	if status.ExitCode != nil && (*status.ExitCode < -1 || *status.ExitCode > 255) {
		return captureLaunchStatus{}, false, errors.New("private fixture exit status is invalid")
	}
	return status, true, nil
}

func readPrivateOutput(path string, limit int) ([]byte, bool, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return nil, false, errors.New("private tmux output file is invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, false, errors.New("open private tmux output file")
	}
	defer file.Close()
	output, err := io.ReadAll(io.LimitReader(file, int64(limit)+2))
	if err != nil {
		return nil, false, errors.New("read private tmux output file")
	}
	truncated := len(output) > limit
	if truncated {
		output = output[:limit]
	}
	return bytes.Clone(output), truncated, nil
}

func writeLaunchSpec(path string, spec captureLaunchSpec) error {
	payload, err := json.Marshal(spec)
	if err != nil {
		return errors.New("encode private fixture launch specification")
	}
	if len(payload) > maxLaunchSpec {
		return errors.New("private fixture launch specification is too large")
	}
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create private fixture launch specification")
	}
	if err := file.Chmod(0o600); err != nil {
		_ = file.Close()
		return errors.New("restrict private fixture launch specification")
	}
	if _, err := file.Write(payload); err != nil {
		_ = file.Close()
		return errors.New("write private fixture launch specification")
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		return errors.New("sync private fixture launch specification")
	}
	if err := file.Close(); err != nil {
		return errors.New("close private fixture launch specification")
	}
	return nil
}

func quotePOSIX(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func shortSystemTemp() string {
	for _, candidate := range []string{"/private/tmp", "/tmp"} {
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	return os.TempDir()
}

// HelperMain handles only the private tmux launch protocol. Public commands
// must call it before flag parsing. Diagnostics intentionally contain no argv,
// paths, environment values, or captured output.
func HelperMain(arguments []string, diagnostics io.Writer) (handled bool, exitCode int) {
	if len(arguments) == 0 || arguments[0] != helperSubcommand {
		return false, 0
	}
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	if len(arguments) != 3 {
		fmt.Fprintln(diagnostics, "relayer fixture helper: invalid private invocation")
		return true, 125
	}
	code, err := executeCaptureSpec(arguments[1], arguments[2])
	if err != nil {
		fmt.Fprintln(diagnostics, "relayer fixture helper: private launch failed")
		return true, 125
	}
	if code < 0 || code > 125 {
		return true, 125
	}
	return true, code
}

func executeCaptureSpec(specPath, releasePath string) (int, error) {
	spec, err := readCaptureSpec(specPath)
	if err != nil {
		return 125, err
	}
	if err := os.Remove(specPath); err != nil {
		return 125, err
	}
	if err := waitForPrivateSignal(releasePath); err != nil {
		_ = writeCaptureStatus(specPath+".status", captureLaunchStatus{LaunchFailed: true})
		return 125, err
	}
	if spec.Cwd != "" {
		if err := os.Chdir(spec.Cwd); err != nil {
			_ = writeCaptureStatus(specPath+".status", captureLaunchStatus{LaunchFailed: true})
			return 125, err
		}
	}
	environment := safeEnvironment(filepath.Dir(specPath))
	for _, name := range []string{"TMUX", "TMUX_PANE"} {
		if value, exists := os.LookupEnv(name); exists {
			environment = append(environment, name+"="+value)
		}
	}
	executable := spec.Command[0]
	if !strings.ContainsRune(executable, os.PathSeparator) {
		resolved, err := exec.LookPath(executable)
		if err != nil {
			_ = writeCaptureStatus(specPath+".status", captureLaunchStatus{LaunchFailed: true})
			return 125, err
		}
		executable = resolved
	}
	command := exec.Command(executable, spec.Command[1:]...)
	command.Args = append([]string(nil), spec.Command...)
	command.Env = environment
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	runErr := command.Run()
	status := captureLaunchStatus{}
	code := 0
	if runErr != nil {
		var exitError *exec.ExitError
		if !errors.As(runErr, &exitError) {
			status.LaunchFailed = true
			if err := writeCaptureStatus(specPath+".status", status); err != nil {
				return 125, err
			}
			return 125, runErr
		}
		code = exitError.ExitCode()
	}
	status.ExitCode = &code
	if err := writeCaptureStatus(specPath+".status", status); err != nil {
		return 125, err
	}
	return code, nil
}

func waitForPrivateSignal(path string) error {
	deadline := time.Now().Add(helperGateLimit)
	for {
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			if !time.Now().Before(deadline) {
				return errors.New("private fixture launch signal was not received")
			}
			time.Sleep(10 * time.Millisecond)
			continue
		}
		if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() != 0 {
			return errors.New("private fixture launch signal is invalid")
		}
		if err := os.Remove(path); err != nil {
			return errors.New("consume private fixture launch signal")
		}
		return nil
	}
}

func writeCaptureStatus(path string, status captureLaunchStatus) error {
	payload, err := json.Marshal(status)
	if err != nil || len(payload) > maxLaunchStatus {
		return errors.New("encode private fixture launch status")
	}
	temporaryPath := path + ".tmp"
	file, err := os.OpenFile(temporaryPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return errors.New("create private fixture launch status")
	}
	committed := false
	defer func() {
		_ = file.Close()
		if !committed {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := file.Chmod(0o600); err != nil {
		return errors.New("restrict private fixture launch status")
	}
	if _, err := file.Write(payload); err != nil {
		return errors.New("write private fixture launch status")
	}
	if err := file.Sync(); err != nil {
		return errors.New("sync private fixture launch status")
	}
	if err := file.Close(); err != nil {
		return errors.New("close private fixture launch status")
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return errors.New("commit private fixture launch status")
	}
	committed = true
	return nil
}

func readCaptureSpec(path string) (captureLaunchSpec, error) {
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 || info.Size() > maxLaunchSpec {
		return captureLaunchSpec{}, errors.New("invalid private launch specification")
	}
	file, err := os.Open(path)
	if err != nil {
		return captureLaunchSpec{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, maxLaunchSpec+1))
	decoder.DisallowUnknownFields()
	var spec captureLaunchSpec
	if err := decoder.Decode(&spec); err != nil {
		return captureLaunchSpec{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return captureLaunchSpec{}, errors.New("invalid trailing private launch data")
	}
	if len(spec.Command) == 0 || strings.TrimSpace(spec.Command[0]) == "" {
		return captureLaunchSpec{}, errors.New("empty private launch argv")
	}
	for _, argument := range spec.Command {
		if strings.IndexByte(argument, 0) >= 0 {
			return captureLaunchSpec{}, errors.New("invalid private launch argv")
		}
	}
	return spec, nil
}
