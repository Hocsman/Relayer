package tmuxbackend

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// ErrProbeFailed reports a tmux executable that was found but cannot serve
// Relayer's machine-readable protocol.
var ErrProbeFailed = errors.New("tmux inutilisable")

const probeSessionPrefix = "relayer-backend-probe-"

// probeCommand keeps the probe pane alive for the lifetime of the check. The
// identity is printed synchronously by new-session, so the command only has to
// avoid racing the teardown.
const probeCommand = "sleep 5"

// Probe reports whether a resolved tmux executable can actually run a Relayer
// session. Finding the binary on PATH is not sufficient evidence: tmux
// sanitizes unprintable bytes while rendering a format, so a tmux whose
// responses cannot be parsed makes every session start fail with an opaque
// identity error long after startup reported success.
//
// The check is self-contained and never touches the user's tmux server. It
// creates one short-lived session on a private socket inside a 0700 temporary
// directory, reads its identity through the same format the runtime uses, then
// kills that session by name. The private server exits with its last session,
// so the probe never calls kill-server.
//
// The user's tmux configuration is deliberately loaded: a configuration that
// breaks format rendering would break the real backend too, and the probe
// exists to observe exactly that.
func Probe(ctx context.Context, runner CommandRunner, tmuxPath string) error {
	if runner == nil {
		runner = execRunner{}
	}
	if strings.TrimSpace(tmuxPath) == "" {
		return fmt.Errorf("%w: empty tmux path", ErrProbeFailed)
	}
	if ctx == nil {
		ctx = context.Background()
	}

	root, err := os.MkdirTemp("", "relayer-tmux-probe-")
	if err != nil {
		return fmt.Errorf("%w: probe directory unavailable", ErrProbeFailed)
	}
	defer func() { _ = os.RemoveAll(root) }()
	if err := os.Chmod(root, 0o700); err != nil {
		return fmt.Errorf("%w: probe directory not restricted", ErrProbeFailed)
	}

	suffix, err := newRunID()
	if err != nil {
		return fmt.Errorf("%w: probe identifier unavailable", ErrProbeFailed)
	}
	socketPath := filepath.Join(root, "probe.sock")
	sessionName := probeSessionPrefix + suffix

	defer func() {
		// Best effort, and deliberately detached from ctx so a cancelled or
		// timed-out probe still removes its own private session.
		stopCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), commandTimeout)
		defer cancel()
		_, _ = runner.Run(stopCtx, CommandSpec{
			Path: tmuxPath,
			Args: []string{"-S", socketPath, "kill-session", "-t", sessionName},
		})
	}()

	startCtx, cancel := context.WithTimeout(ctx, commandTimeout)
	defer cancel()
	output, err := runner.Run(startCtx, CommandSpec{
		Path: tmuxPath,
		Args: []string{
			"-S", socketPath,
			"new-session", "-d", "-P", "-F",
			tmuxFormat("#{session_id}", "#{window_id}", "#{pane_id}"),
			"-s", sessionName, "-x", "80", "-y", "24", probeCommand,
		},
	})
	if err != nil {
		// tmux diagnostics can carry paths and environment detail, so the
		// reason stays static exactly like CommandError.
		return fmt.Errorf("%w: probe session did not start", ErrProbeFailed)
	}
	if _, err := parseIdentity(string(output)); err != nil {
		return fmt.Errorf("%w: unreadable format response (%w)", ErrProbeFailed, err)
	}
	return nil
}
