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

// versionProbeTimeout bounds one probe of an agent's own executable.
const versionProbeTimeout = 2500 * time.Millisecond

// ErrVersionProbeNotApplicable is what an inspector returns for an agent it
// must not probe. The caller displays nothing, rather than a warning about a
// version nobody looked at.
var ErrVersionProbeNotApplicable = errors.New("the version probe does not apply to this agent")

// probeExecutable returns the executable a version probe may run: the agent's
// own argv[0], and only when its base name — without a Windows .exe suffix —
// is exactly the adapter's vendor executable.
//
// Nothing else is ever executed. The probe used to rejoin a shell wrapper's
// command line (sh -c, cmd /c) and run it again with --version appended: the
// configured script's side effects happened twice, which a marker file proved
// by being written once per start. A launcher (npx, node, python, docker)
// reported its own version as the agent's, and a shell-mode agent — no argv at
// all — probed whatever "claude" the PATH happened to resolve.
func probeExecutable(spec agent.Spec, adapterID string) (string, bool) {
	want := adapters.VendorExecutable(adapterID)
	if want == "" || len(spec.Command) == 0 {
		return "", false
	}
	executable := strings.TrimSpace(spec.Command[0])
	if executable == "" {
		return "", false
	}
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(executable)), ".exe")
	if base != want {
		return "", false
	}
	return executable, true
}

// DefaultVersionInspector runs "<argv[0]> --version" — the adapter's own
// executable, with none of the configured arguments — and reads the version
// from stdout.
func DefaultVersionInspector(ctx context.Context, spec agent.Spec) (string, error) {
	executable, ok := probeExecutable(spec, spec.Adapter)
	if !ok {
		return "", ErrVersionProbeNotApplicable
	}

	timeoutCtx, cancel := context.WithTimeout(ctx, versionProbeTimeout)
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

	// Only stdout is parsed, and stderr is discarded: CombinedOutput let a
	// banner, a launcher's own version or a shell rc greeting become "the
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

// CheckAgentVersion inspects and evaluates the version of one agent.
//
// resolvedAdapter is the adapter the run resolved, not the one the
// configuration named: "command: [claude]" with no adapter resolves to the
// Claude adapter, and testing spec.Adapter left that agent without a warning.
// A simulated agent, a non-vendor adapter, and an agent whose argv[0] is not
// the adapter's own executable produce no information at all: nothing is run,
// and nothing is displayed.
func CheckAgentVersion(ctx context.Context, spec agent.Spec, resolvedAdapter string, simulated bool, inspector VersionInspector) AgentVersionInfo {
	if simulated || !adapters.IsVendorAdapter(resolvedAdapter) {
		return AgentVersionInfo{}
	}
	if _, ok := probeExecutable(spec, resolvedAdapter); !ok {
		return AgentVersionInfo{}
	}

	if inspector == nil {
		inspector = DefaultVersionInspector
	}

	// The inspector sees the resolved adapter, so its own eligibility check
	// and its parse anchor agree with this one.
	probeSpec := spec
	probeSpec.Adapter = resolvedAdapter

	version, err := inspector(ctx, probeSpec)
	if errors.Is(err, ErrVersionProbeNotApplicable) {
		return AgentVersionInfo{}
	}
	if err != nil && version == "" {
		unverified, reason := adapters.CheckVersion(resolvedAdapter, "")
		return AgentVersionInfo{
			InstalledVersion: "",
			Unverified:       unverified,
			Reason:           reason,
		}
	}

	unverified, reason := adapters.CheckVersion(resolvedAdapter, version)
	return AgentVersionInfo{
		InstalledVersion: version,
		Unverified:       unverified,
		Reason:           reason,
	}
}
