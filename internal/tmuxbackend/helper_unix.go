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
	spec, err := readLaunchSpec(specPath)
	if err != nil {
		return err
	}
	// Remove secrets before waiting and before the user process starts. The
	// decoded values now live only in this short-lived helper's memory.
	if err := os.Remove(specPath); err != nil {
		return fmt.Errorf("suppression de la spécification privée: %w", err)
	}

	gateInfo, err := os.Lstat(gatePath)
	if err != nil {
		return fmt.Errorf("inspection du signal de démarrage: %w", err)
	}
	if gateInfo.Mode()&os.ModeNamedPipe == 0 || gateInfo.Mode().Perm()&0o077 != 0 {
		return errors.New("permissions invalides pour le signal de démarrage")
	}
	gate, err := os.Open(gatePath)
	if err != nil {
		return fmt.Errorf("ouverture du signal de démarrage: %w", err)
	}
	_, readErr := bufio.NewReader(gate).ReadString('\n')
	closeErr := gate.Close()
	_ = os.Remove(gatePath)
	if readErr != nil {
		return fmt.Errorf("attente du signal de démarrage: %w", readErr)
	}
	if closeErr != nil {
		return fmt.Errorf("fermeture du signal de démarrage: %w", closeErr)
	}

	arguments := append([]string(nil), spec.Command...)
	if spec.Shell != "" {
		arguments = []string{"/bin/sh", "-c", spec.Shell}
	}
	if len(arguments) == 0 || strings.TrimSpace(arguments[0]) == "" {
		return errors.New("commande tmux vide")
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
			return fmt.Errorf("métadonnée d'environnement tmux %q: %w", name, err)
		}
	}

	executable := arguments[0]
	if !strings.ContainsRune(executable, os.PathSeparator) {
		executable, err = exec.LookPath(executable)
		if err != nil {
			return fmt.Errorf("exécutable agent introuvable: %w", err)
		}
	}
	return syscall.Exec(executable, arguments, os.Environ())
}
