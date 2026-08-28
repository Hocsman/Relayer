//go:build darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package tmuxbackend

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

func executeLaunchSpec(specPath, gatePath string) error {
	spec, gate, err := prepareLaunchSpec(specPath, gatePath, os.Open)
	if err != nil {
		return err
	}
	_, readErr := bufio.NewReader(gate).ReadString('\n')
	closeErr := gate.Close()
	removeErr := os.Remove(gatePath)
	if readErr != nil {
		return errors.Join(
			fmt.Errorf("waiting for the start signal: %w", readErr),
			wrapGateCloseError(closeErr),
			wrapGateRemoveError(removeErr),
		)
	}
	if closeErr != nil {
		return errors.Join(
			fmt.Errorf("closing the start signal: %w", closeErr),
			wrapGateRemoveError(removeErr),
		)
	}
	if err := wrapGateRemoveError(removeErr); err != nil {
		return err
	}

	arguments := append([]string(nil), spec.Command...)
	if spec.Shell != "" {
		arguments = []string{"/bin/sh", "-c", spec.Shell}
	}
	if len(arguments) == 0 || strings.TrimSpace(arguments[0]) == "" {
		return errors.New("empty tmux command")
	}
	if spec.Cwd != "" {
		if err := os.Chdir(spec.Cwd); err != nil {
			return fmt.Errorf("working directory tmux: %w", err)
		}
	}
	dynamicTmuxEnvironment := make(map[string]string, 3)
	for _, name := range []string{"TERM", "TMUX", "TMUX_PANE"} {
		if value, found := os.LookupEnv(name); found {
			dynamicTmuxEnvironment[name] = value
		}
	}
	// A personal tmux server may be days old. Rebuild the environment from the
	// exact Relayer snapshot instead of overlaying stale server values, while
	// preserving tmux's fresh terminal metadata unless explicitly overridden.
	os.Clearenv()
	for name, value := range spec.Env {
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("variable d'environnement tmux %q: %w", name, err)
		}
	}
	for name, value := range dynamicTmuxEnvironment {
		if _, overridden := spec.Env[name]; overridden {
			continue
		}
		if err := os.Setenv(name, value); err != nil {
			return fmt.Errorf("tmux environment metadata %q: %w", name, err)
		}
	}

	executable := arguments[0]
	if !strings.ContainsRune(executable, os.PathSeparator) {
		executable, err = exec.LookPath(executable)
		if err != nil {
			return fmt.Errorf("agent executable not found: %w", err)
		}
	}
	return syscall.Exec(executable, arguments, os.Environ())
}

// prepareLaunchSpec opens and verifies the FIFO before unlinking the private
// specification, so disappearance never advertises readiness before the helper
// owns a reader. The parent also retains its O_RDWR endpoint after queuing the
// start signal, making the handshake independent of process scheduling. The
// decoded specification remains only in this short-lived process memory.
func prepareLaunchSpec(
	specPath string,
	gatePath string,
	openGate func(string) (*os.File, error),
) (launchSpec, *os.File, error) {
	spec, err := readLaunchSpec(specPath)
	if err != nil {
		return launchSpec{}, nil, err
	}
	gateInfo, err := os.Lstat(gatePath)
	if err != nil {
		return launchSpec{}, nil, fmt.Errorf("inspecting the start signal: %w", err)
	}
	if !validLaunchGate(gateInfo) {
		return launchSpec{}, nil, errors.New("invalid permissions for the start signal")
	}
	gate, err := openGate(gatePath)
	if err != nil {
		return launchSpec{}, nil, fmt.Errorf("opening the start signal: %w", err)
	}
	openedInfo, statErr := gate.Stat()
	if statErr != nil || !validLaunchGate(openedInfo) || !os.SameFile(gateInfo, openedInfo) {
		closeErr := gate.Close()
		if statErr != nil {
			return launchSpec{}, nil, errors.Join(
				fmt.Errorf("validating the opened start signal: %w", statErr),
				wrapGateCloseError(closeErr),
			)
		}
		return launchSpec{}, nil, errors.Join(
			errors.New("start signal replaced or invalid"),
			wrapGateCloseError(closeErr),
		)
	}
	if err := os.Remove(specPath); err != nil {
		closeErr := gate.Close()
		return launchSpec{}, nil, errors.Join(
			fmt.Errorf("removing the private specification: %w", err),
			wrapGateCloseError(closeErr),
		)
	}
	return spec, gate, nil
}

func validLaunchGate(info os.FileInfo) bool {
	return info != nil && info.Mode()&os.ModeNamedPipe != 0 && info.Mode().Perm()&0o077 == 0
}

func wrapGateCloseError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("closing the start signal: %w", err)
}

func wrapGateRemoveError(err error) error {
	if err == nil || errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return fmt.Errorf("removing the start signal: %w", err)
}
