package preflight

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

// agentExecutableStatus mirrors the lookup semantics of the selected runtime
// backend without executing the configured command. User-controlled paths are
// passed only to the injected detector and never become report text or errors
// returned by this package.
func agentExecutableStatus(
	ctx context.Context,
	detector toolcatalog.Detector,
	spec agent.Spec,
	effectiveBackend string,
) toolcatalog.InstallStatus {
	executable := "/bin/sh"
	if len(spec.Command) > 0 {
		executable = spec.Command[0]
	}

	if strings.ContainsRune(executable, os.PathSeparator) {
		candidate, ok := absoluteExecutableCandidate(spec.Cwd, executable)
		if !ok {
			return toolcatalog.InstallUnknown
		}
		status, _ := detectAbsoluteCandidate(ctx, detector, candidate)
		return status
	}

	// exec.CommandContext resolves a bare PTY executable before Cmd.Env is
	// assigned, so agent-specific PATH overrides intentionally do not apply.
	if effectiveBackend != agent.BackendTmux {
		status, _ := detectStatus(ctx, detector, []string{executable})
		return status
	}

	return tmuxExecutableStatus(ctx, detector, spec, executable)
}

// tmuxExecutableStatus mirrors the helper's post-chdir exec.LookPath call. The
// helper receives the parent environment with spec.Env overlaid, so PATH from
// spec.Env wins while an absent override retains the parent PATH.
func tmuxExecutableStatus(
	ctx context.Context,
	detector toolcatalog.Detector,
	spec agent.Spec,
	executable string,
) toolcatalog.InstallStatus {
	pathValue := os.Getenv("PATH")
	if override, ok := spec.Env["PATH"]; ok {
		pathValue = override
	}
	if pathValue == "" {
		return toolcatalog.InstallNotInstalled
	}

	for _, directory := range filepath.SplitList(pathValue) {
		if err := ctx.Err(); err != nil {
			return toolcatalog.InstallUnknown
		}
		relativeDirectory := !filepath.IsAbs(directory)
		candidate, ok := absoluteExecutableCandidate(spec.Cwd, filepath.Join(directory, executable))
		if !ok {
			return toolcatalog.InstallUnknown
		}
		status, failed := detectAbsoluteCandidate(ctx, detector, candidate)
		if failed || status == toolcatalog.InstallUnknown {
			return toolcatalog.InstallUnknown
		}
		if status == toolcatalog.InstallNotInstalled {
			continue
		}
		if relativeDirectory {
			// exec.LookPath rejects an executable reached through a relative PATH
			// entry with exec.ErrDot. Keep the report fail-closed and generic.
			return toolcatalog.InstallUnknown
		}
		return toolcatalog.InstallInstalled
	}
	return toolcatalog.InstallNotInstalled
}

// detectAbsoluteCandidate preserves Detector injection while translating the
// ordinary "direct path does not exist" result into not_installed. The shared
// PathDetector treats that condition as a lookup error because its general
// candidate API normally searches PATH; contextual PATH traversal must be able
// to continue to the next directory instead.
func detectAbsoluteCandidate(
	ctx context.Context,
	detector toolcatalog.Detector,
	candidate string,
) (toolcatalog.InstallStatus, bool) {
	detection, err := detector.Detect(ctx, []string{candidate})
	if err != nil {
		if errors.Is(err, os.ErrNotExist) || errors.Is(err, exec.ErrNotFound) {
			return toolcatalog.InstallNotInstalled, false
		}
		return toolcatalog.InstallUnknown, true
	}
	if !validPassiveDetection(detection) {
		return toolcatalog.InstallUnknown, true
	}
	return detection.Status, false
}

func absoluteExecutableCandidate(workingDirectory, executable string) (string, bool) {
	if filepath.IsAbs(executable) {
		return filepath.Clean(executable), true
	}

	base := workingDirectory
	if base == "" {
		var err error
		base, err = os.Getwd()
		if err != nil {
			return "", false
		}
	} else if !filepath.IsAbs(base) {
		var err error
		base, err = filepath.Abs(base)
		if err != nil {
			return "", false
		}
	}
	return filepath.Clean(filepath.Join(base, executable)), true
}
