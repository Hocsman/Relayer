//go:build aix || darwin || dragonfly || freebsd || illumos || linux || netbsd || openbsd || solaris

package fixturecapture

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"time"

	"github.com/Hocsman/Relayer/internal/platform"
	"github.com/creack/pty"
)

func capturePTY(ctx context.Context, options Options) (result captureResult, resultErr error) {
	runtimeDirectory, err := os.MkdirTemp(shortSystemTemp(), ".relayer-fixture-pty-")
	if err != nil {
		return captureResult{}, errors.New("create private PTY capture runtime")
	}
	if err := os.Chmod(runtimeDirectory, 0o700); err != nil {
		return captureResult{}, errors.Join(errors.New("restrict private PTY capture runtime"), cleanupPrivateRuntime(runtimeDirectory))
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
	command := exec.Command(options.Command[0], options.Command[1:]...)
	command.Dir = options.Cwd
	command.Env = safeEnvironment(runtimeDirectory)
	terminal, err := pty.StartWithSize(command, &pty.Winsize{Cols: 100, Rows: 30})
	if err != nil {
		return captureResult{}, errors.New("start output-only PTY capture")
	}
	collector := newBoundedCollector(terminal, terminal, options.MaxBytes)
	defer collector.stop()
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()

	timer := time.NewTimer(options.Timeout)
	defer timer.Stop()

	var collected *collectorResult
	for {
		select {
		case result := <-collector.complete:
			collected = &result
			if result.err != nil {
				waitErr := terminatePTYCommand(command, waited)
				zeroBytes(result.data)
				return captureResult{}, errors.Join(fmt.Errorf("read PTY capture: %w", result.err), waitErr)
			}
			if result.truncated {
				if err := terminatePTYCommand(command, waited); err != nil {
					zeroBytes(result.data)
					return captureResult{}, err
				}
				collector.stop()
				return captureResult{raw: result.data, outcome: OutcomeOutputLimit, truncated: true}, nil
			}
		case waitErr := <-waited:
			// command.Wait only proves that the PTY leader exited. A descendant can
			// still keep the process group (and the PTY slave) alive, so do not
			// publish an exited fixture until that group has disappeared.
			if err := terminatePTYDescendantsAfterLeaderExit(command); err != nil {
				collector.stop()
				if collected == nil {
					result := <-collector.complete
					collected = &result
				}
				zeroBytes(collected.data)
				return captureResult{}, err
			}
			var drainErr error
			collected, drainErr = drainPTYCollector(collector, collected)
			if collected.err != nil {
				zeroBytes(collected.data)
				return captureResult{}, fmt.Errorf("read PTY capture: %w", collected.err)
			}
			if collected.truncated {
				return captureResult{raw: collected.data, outcome: OutcomeOutputLimit, truncated: true}, nil
			}
			if drainErr != nil {
				zeroBytes(collected.data)
				return captureResult{}, drainErr
			}
			code, err := processExitCode(waitErr)
			if err != nil {
				zeroBytes(collected.data)
				return captureResult{}, err
			}
			return captureResult{raw: collected.data, outcome: OutcomeExited, exitCode: &code}, nil
		case <-timer.C:
			if err := terminatePTYCommand(command, waited); err != nil {
				collector.stop()
				if collected == nil {
					result := <-collector.complete
					collected = &result
				}
				zeroBytes(collected.data)
				return captureResult{}, err
			}
			var drainErr error
			collected, drainErr = drainPTYCollector(collector, collected)
			if collected.err != nil {
				zeroBytes(collected.data)
				return captureResult{}, fmt.Errorf("read PTY capture: %w", collected.err)
			}
			if collected.truncated {
				return captureResult{raw: collected.data, outcome: OutcomeOutputLimit, truncated: true}, nil
			}
			if drainErr != nil {
				zeroBytes(collected.data)
				return captureResult{}, drainErr
			}
			return captureResult{raw: collected.data, outcome: OutcomeTimedOut}, nil
		case <-ctx.Done():
			stopErr := terminatePTYCommand(command, waited)
			collector.stop()
			if collected == nil {
				result := <-collector.complete
				collected = &result
			}
			zeroBytes(collected.data)
			return captureResult{}, errors.Join(ctx.Err(), stopErr)
		}
	}
}

func drainPTYCollector(collector *boundedCollector, collected *collectorResult) (*collectorResult, error) {
	if collected != nil {
		return collected, nil
	}
	select {
	case result := <-collector.complete:
		return &result, nil
	case <-time.After(2 * time.Second):
		collector.stop()
		result := <-collector.complete
		return &result, errors.New("PTY output drain completion was not confirmed")
	}
}

func terminatePTYCommand(command *exec.Cmd, waited <-chan error) error {
	platform.TerminateProcessGroup(command)
	leaderExited := false
	if waitForPTYShutdown(command, waited, &leaderExited, 250*time.Millisecond) {
		return nil
	}
	platform.KillProcessGroup(command)
	if waitForPTYShutdown(command, waited, &leaderExited, 2*time.Second) {
		return nil
	}
	if platform.ProcessGroupExists(command) {
		return errors.New("PTY capture process group disappearance was not confirmed")
	}
	return errors.New("PTY capture leader did not exit after SIGKILL")
}

func terminatePTYDescendantsAfterLeaderExit(command *exec.Cmd) error {
	if !platform.ProcessGroupExists(command) {
		return nil
	}
	platform.TerminateProcessGroup(command)
	if waitForPTYProcessGroupExit(command, 250*time.Millisecond) {
		return nil
	}
	platform.KillProcessGroup(command)
	if waitForPTYProcessGroupExit(command, 2*time.Second) {
		return nil
	}
	return errors.New("PTY capture descendant process group disappearance was not confirmed")
}

func waitForPTYShutdown(command *exec.Cmd, waited <-chan error, leaderExited *bool, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for {
		if !*leaderExited {
			select {
			case <-waited:
				*leaderExited = true
			default:
			}
		}
		if *leaderExited && !platform.ProcessGroupExists(command) {
			return true
		}
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func waitForPTYProcessGroupExit(command *exec.Cmd, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for platform.ProcessGroupExists(command) {
		if !time.Now().Before(deadline) {
			return false
		}
		time.Sleep(10 * time.Millisecond)
	}
	return true
}

func processExitCode(waitErr error) (int, error) {
	if waitErr == nil {
		return 0, nil
	}
	var exitError *exec.ExitError
	if errors.As(waitErr, &exitError) {
		return exitError.ExitCode(), nil
	}
	return 0, fmt.Errorf("wait for captured process: %w", waitErr)
}

func isTerminalCloseError(err error) bool { return platform.IsPTYCloseError(err) }
