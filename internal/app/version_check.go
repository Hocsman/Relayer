package app

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
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
// versionProbeWaitDelay then bounds how long the wait for its output may
// outlive the process: a grandchild that inherited stdout — a daemon the tool
// spawned, a pager, a hung child — would otherwise keep the read open, and
// the run's start with it, long after the timeout killed the probe.
const (
	versionProbeTimeout   = 2500 * time.Millisecond
	versionProbeWaitDelay = 500 * time.Millisecond
	// versionProbeGlobalBudget bounds all of a run's probes together. They
	// run in parallel, so the budget is what one slow tool can delay a run's
	// start by, not the sum of the tools.
	versionProbeGlobalBudget = 4 * time.Second
	// versionProbeOutputBound caps what one probe reads. --version prints a
	// line; a binary that floods stdout must not fill memory before the
	// deadline arrives.
	versionProbeOutputBound = 64 * 1024
)

// ErrVersionProbeNotApplicable is what an inspector returns for an agent it
// must not probe. The caller displays nothing, rather than a warning about a
// version nobody looked at.
var ErrVersionProbeNotApplicable = errors.New("the version probe does not apply to this agent")

// boundedProbeBuffer keeps the first limit bytes a probe prints and discards
// the rest. A --version answer is one line; what a binary prints beyond the
// bound is not read, and cannot fill memory before the deadline arrives.
type boundedProbeBuffer struct {
	buffer bytes.Buffer
	limit  int
}

func (b *boundedProbeBuffer) Write(p []byte) (int, error) {
	written := len(p)
	if room := b.limit - b.buffer.Len(); room > 0 {
		if len(p) > room {
			p = p[:room]
		}
		if _, err := b.buffer.Write(p); err != nil {
			return 0, err
		}
	}
	// The full length is always reported: a short write would tell the copy
	// that the writer failed, and the probe's deadline decides, not this.
	return written, nil
}

func (b *boundedProbeBuffer) String() string {
	return b.buffer.String()
}

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
	cmd.WaitDelay = versionProbeWaitDelay
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
	// What is read is bounded: the version is on one of the first lines, and a
	// binary that floods stdout must not fill memory before its deadline.
	stdout := &boundedProbeBuffer{limit: versionProbeOutputBound}
	cmd.Stdout = stdout
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

// versionProbeRequest is one agent to inspect: its spec, the adapter the run
// resolved for it, whether the resolver substituted a mock, and the inspector
// to use.
type versionProbeRequest struct {
	spec            agent.Spec
	resolvedAdapter string
	simulated       bool
	inspector       VersionInspector
}

// probeAgentVersions inspects every requested agent in parallel, within one
// global budget. The budget is enforced on the join rather than left to the
// inspectors: one that ignores its context keeps running, and the run's start
// returns without it. A result that arrives too late is dropped, which reads
// as no information — the honest state for a version nobody observed.
func probeAgentVersions(ctx context.Context, requests []versionProbeRequest) []AgentVersionInfo {
	results := make([]AgentVersionInfo, len(requests))
	if len(requests) == 0 {
		return results
	}

	probeCtx, cancel := context.WithTimeout(ctx, versionProbeGlobalBudget)
	defer cancel()

	type probeResult struct {
		index int
		info  AgentVersionInfo
	}
	// Buffered to the number of requests, so a late result never blocks its
	// goroutine and never touches results after this function returned.
	found := make(chan probeResult, len(requests))
	var wg sync.WaitGroup
	for index, request := range requests {
		wg.Add(1)
		go func(index int, request versionProbeRequest) {
			defer wg.Done()
			info := CheckAgentVersion(probeCtx, request.spec, request.resolvedAdapter, request.simulated, request.inspector)
			select {
			case found <- probeResult{index: index, info: info}:
			case <-probeCtx.Done():
			}
		}(index, request)
	}

	for remaining := len(requests); remaining > 0; {
		select {
		case result := <-found:
			results[result.index] = result.info
			remaining--
		case <-probeCtx.Done():
			return results
		}
	}
	wg.Wait()
	return results
}
