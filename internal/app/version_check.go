package app

import (
	"context"
	"errors"
	"os"
	"os/exec"
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

	timeoutCtx, cancel := context.WithTimeout(ctx, 2500*time.Millisecond)
	defer cancel()

	cmd := exec.CommandContext(timeoutCtx, executable, "--version")
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

	out, err := cmd.CombinedOutput()
	parsed := adapters.ParseVersion(string(out))
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
