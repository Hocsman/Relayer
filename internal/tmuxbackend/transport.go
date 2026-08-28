package tmuxbackend

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/agent"
)

type launchSpec struct {
	Command []string          `json:"command,omitempty"`
	Shell   string            `json:"shell,omitempty"`
	Cwd     string            `json:"cwd,omitempty"`
	Env     map[string]string `json:"env,omitempty"`
}

type launchFiles struct {
	specPath   string
	gatePath   string
	outputPath string
	gate       *os.File
	output     *os.File
}

// quotePOSIX encodes exactly one shell word using single quotes. The only
// special case closes the quote, emits a literal apostrophe, then reopens it.
// It is used only for private helper paths in tmux's shell-command; command
// argv and environment values live in the protected JSON specification.
func quotePOSIX(value string) string {
	return "'" + strings.ReplaceAll(value, "'", `'"'"'`) + "'"
}

func createLaunchFiles(runtimeDirectory, tmuxName string, spec agent.Spec) (_ *launchFiles, err error) {
	prefix := filepath.Join(runtimeDirectory, tmuxName)
	files := &launchFiles{
		specPath:   prefix + ".json",
		gatePath:   prefix + ".gate",
		outputPath: prefix + ".output",
	}
	defer func() {
		if err != nil {
			files.close()
			files.remove()
		}
	}()

	if err = makeFIFO(files.gatePath, 0o600); err != nil {
		return nil, fmt.Errorf("création du signal privé tmux: %w", err)
	}
	if err = makeFIFO(files.outputPath, 0o600); err != nil {
		return nil, fmt.Errorf("création du flux privé tmux: %w", err)
	}
	// O_RDWR prevents FIFO open-order deadlocks. This process never reads the
	// gate nor writes the output descriptor, so bytes still have one consumer.
	files.gate, err = openFIFO(files.gatePath)
	if err != nil {
		return nil, fmt.Errorf("ouverture du signal privé tmux: %w", err)
	}
	files.output, err = openFIFO(files.outputPath)
	if err != nil {
		return nil, fmt.Errorf("ouverture du flux privé tmux: %w", err)
	}

	payload, err := json.Marshal(launchSpec{
		Command: append([]string(nil), spec.Command...),
		Shell:   spec.Shell,
		Cwd:     spec.Cwd,
		Env:     mergedLaunchEnvironment(spec.Env),
	})
	if err != nil {
		return nil, fmt.Errorf("sérialisation du lancement tmux: %w", err)
	}
	file, err := os.OpenFile(files.specPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, fmt.Errorf("création de la spécification tmux privée: %w", err)
	}
	if _, err = file.Write(payload); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("écriture de la spécification tmux privée: %w", err)
	}
	if err = file.Sync(); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("synchronisation de la spécification tmux privée: %w", err)
	}
	if err = file.Close(); err != nil {
		return nil, fmt.Errorf("fermeture de la spécification tmux privée: %w", err)
	}
	return files, nil
}

// mergedLaunchEnvironment snapshots the launching Relayer environment because
// a long-lived personal tmux server may otherwise provide stale PATH, HOME or
// credentials. tmux-owned terminal metadata remains dynamic unless the agent
// explicitly overrides it. The protected spec is unlinked by the helper as
// soon as it has been decoded, before the command gate is released.
func mergedLaunchEnvironment(overrides map[string]string) map[string]string {
	merged := make(map[string]string, len(os.Environ())+len(overrides))
	for _, assignment := range os.Environ() {
		name, value, found := strings.Cut(assignment, "=")
		if !found || name == "TERM" || name == "TMUX" || name == "TMUX_PANE" {
			continue
		}
		merged[name] = value
	}
	for name, value := range overrides {
		merged[name] = value
	}
	return merged
}

func helperCommand(helperPath, specPath, gatePath string) string {
	return strings.Join([]string{
		quotePOSIX(helperPath),
		quotePOSIX(HelperSubcommand),
		quotePOSIX(specPath),
		quotePOSIX(gatePath),
	}, " ")
}

func (files *launchFiles) release() error {
	if files == nil || files.gate == nil {
		return nil
	}
	if _, err := files.gate.Write([]byte("start\n")); err != nil {
		closeErr := files.gate.Close()
		files.gate = nil
		return errors.Join(err, closeErr)
	}
	// Keep this O_RDWR endpoint alive until the helper has opened its reader.
	// FIFO bytes may be queued before that point; closing here would let a
	// delayed helper block forever if no writer remained. The descriptor count
	// is bounded by the configured agent count and closeTransport closes it.
	return nil
}

// waitForHandoff waits until the helper has loaded the private launch spec,
// consumed the start signal and removed the gate. Close may remove the runtime
// files as soon as Start returns, so this confirmation prevents an immediate
// persistent-session shutdown from racing a helper that has not taken over yet.
func (files *launchFiles) waitForHandoff(ctx context.Context) error {
	if files == nil || files.gate == nil {
		return nil
	}
	if ctx == nil {
		return errors.New("contexte de prise en charge tmux nil")
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, err := os.Lstat(files.gatePath)
		switch {
		case errors.Is(err, os.ErrNotExist):
			closeErr := files.gate.Close()
			files.gate = nil
			if closeErr != nil {
				return fmt.Errorf("fermeture du signal après prise en charge tmux: %w", closeErr)
			}
			return nil
		case err != nil:
			return fmt.Errorf("confirmation de prise en charge tmux: %w", err)
		}

		select {
		case <-ctx.Done():
			return fmt.Errorf("confirmation de prise en charge tmux: %w", ctx.Err())
		case <-ticker.C:
		}
	}
}

func (files *launchFiles) close() {
	if files == nil {
		return
	}
	if files.gate != nil {
		_ = files.gate.Close()
		files.gate = nil
	}
	if files.output != nil {
		// Closing a FIFO descriptor from another goroutine does not reliably
		// interrupt a blocking Read on every Unix (notably Darwin). A private
		// NUL wake byte lets the reader observe its cancelled context; terminal
		// sanitization discards that byte from retained output.
		_, _ = files.output.Write([]byte{0})
		_ = files.output.Close()
		files.output = nil
	}
}

func (files *launchFiles) remove() {
	if files == nil {
		return
	}
	_ = os.Remove(files.specPath)
	_ = os.Remove(files.gatePath)
	_ = os.Remove(files.outputPath)
}

func pipeCommand(path string) string {
	return "umask 077; exec /bin/cat > " + quotePOSIX(path)
}
