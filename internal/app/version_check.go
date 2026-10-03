package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
)

// AgentVersionInfo stores the inspection result for an agent process.
type AgentVersionInfo struct {
	InstalledVersion string
	Unverified       bool
	Reason           string
}

// VersionInspector executes a version check for a given agent spec.
// It returns the detected raw version or an error if inspection failed.
type VersionInspector func(ctx context.Context, spec agent.Spec) (string, error)

// DefaultVersionInspector runs "<cmd> --version" with a timeout to detect the installed version.
func DefaultVersionInspector(ctx context.Context, spec agent.Spec) (string, error) {
	executable := ""
	if len(spec.Command) > 0 {
		executable = spec.Command[0]
	} else {
		switch spec.Adapter {
		case adapters.AiderID:
			executable = "aider"
		case adapters.ClaudeID:
			executable = "claude"
		case adapters.GooseID:
			executable = "goose"
		case adapters.OpenInterpreterID:
			executable = "interpreter"
		case adapters.CodexID:
			executable = "codex"
		}
	}
	executable = strings.TrimSpace(executable)
	if executable == "" {
		return "", errors.New("no executable found for agent")
	}

	var cmd *exec.Cmd
	timeoutCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()

	// Check for shell wrapper like `cmd.exe /c <tool>` or `sh -c <tool>`
	baseName := strings.ToLower(filepath.Base(executable))
	if len(spec.Command) >= 3 && (baseName == "cmd.exe" || baseName == "cmd") && strings.EqualFold(spec.Command[1], "/c") {
		subCmd := strings.Join(spec.Command[2:], " ") + " --version"
		cmd = exec.CommandContext(timeoutCtx, spec.Command[0], spec.Command[1], subCmd)
	} else if len(spec.Command) >= 3 && (baseName == "sh" || baseName == "bash") && spec.Command[1] == "-c" {
		subCmd := strings.Join(spec.Command[2:], " ") + " --version"
		cmd = exec.CommandContext(timeoutCtx, spec.Command[0], spec.Command[1], subCmd)
	} else {
		cmd = exec.CommandContext(timeoutCtx, executable, "--version")
	}
	if spec.Cwd != "" {
		cmd.Dir = spec.Cwd
	}
	if len(spec.Env) > 0 {
		envMap := make(map[string]string)
		for _, e := range os.Environ() {
			parts := strings.SplitN(e, "=", 2)
			if len(parts) == 2 {
				envMap[parts[0]] = parts[1]
			}
		}
		for k, v := range spec.Env {
			envMap[k] = v
		}
		envList := make([]string, 0, len(envMap))
		for k, v := range envMap {
			envList = append(envList, k+"="+v)
		}
		cmd.Env = envList
	}

	// Only stdout is parsed, and only stderr is discarded: CombinedOutput let
	// a banner, a launcher's own version or a shell rc greeting become "the
	// agent's version", which every client then saw as the installed version.
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = nil
	err := cmd.Run()
	parsed := adapters.ParseVersion(stdout.String(), adapters.VendorExecutable(spec.Adapter))
	if parsed != "" {
		return parsed, nil
	}
	if err != nil {
		return "", err
	}
	return "", errors.New("could not parse version from output")
}

// CheckAgentVersion inspects and evaluates the version for an agent specification.
// If simulated is true or adapter is not a vendor adapter, no warning is generated.
func CheckAgentVersion(ctx context.Context, spec agent.Spec, simulated bool, inspector VersionInspector) AgentVersionInfo {
	if simulated || !adapters.IsVendorAdapter(spec.Adapter) {
		return AgentVersionInfo{}
	}

	if inspector == nil {
		inspector = DefaultVersionInspector
	}

	version, err := inspector(ctx, spec)
	if err != nil && version == "" {
		unverified, reason := adapters.CheckVersion(spec.Adapter, "")
		return AgentVersionInfo{
			InstalledVersion: "",
			Unverified:       unverified,
			Reason:           reason,
		}
	}

	unverified, reason := adapters.CheckVersion(spec.Adapter, version)
	return AgentVersionInfo{
		InstalledVersion: version,
		Unverified:       unverified,
		Reason:           reason,
	}
}
