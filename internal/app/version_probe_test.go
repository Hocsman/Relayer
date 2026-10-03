package app

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
)

// The version-probe tests run real executables: a copy of this test binary
// named after a vendor tool, and small scripts that stand in for one. The
// probe runs "<binary> --version", which a test binary rejects as an unknown
// flag, so the helper answers from its environment in TestMain, before
// testing parses anything.
const (
	versionHelperEnv       = "RELAYER_VERSION_HELPER"
	versionHelperMarkerEnv = "RELAYER_VERSION_HELPER_MARKER"
)

func TestMain(m *testing.M) {
	if mode := os.Getenv(versionHelperEnv); mode != "" {
		os.Exit(runVersionHelper(mode))
	}
	os.Exit(m.Run())
}

func runVersionHelper(mode string) int {
	switch mode {
	case "version":
		// What a wrapped tool really prints: the launcher's own version on
		// stderr, a banner on stdout, and the product's line last.
		_, _ = os.Stderr.WriteString("npm warn node v99.0.0\n")
		_, _ = os.Stdout.WriteString("helper banner 9.9.9\n")
		_, _ = os.Stdout.WriteString("claude 2.1.285 (Claude Code)\n")
	case "marker":
		if path := os.Getenv(versionHelperMarkerEnv); path != "" {
			if err := os.WriteFile(path, []byte("probed\n"), 0o600); err != nil {
				return 1
			}
		}
		_, _ = os.Stdout.WriteString("claude 2.1.285 (Claude Code)\n")
	case "agent":
		_, _ = os.Stdout.WriteString("agent ready\n")
		// Stay alive until the runtime closes the terminal.
		buffer := make([]byte, 1)
		_, _ = os.Stdin.Read(buffer)
	default:
		return 2
	}
	return 0
}

// versionHelperExecutable copies this test binary into directory under the
// vendor tool's name, so the probe's eligibility check sees the executable it
// accepts.
func versionHelperExecutable(t *testing.T, directory, name string) string {
	t.Helper()
	source, err := os.Executable()
	if err != nil {
		t.Fatalf("os.Executable: %v", err)
	}
	raw, err := os.ReadFile(source)
	if err != nil {
		t.Fatalf("read the test binary: %v", err)
	}
	target := filepath.Join(directory, name)
	if runtime.GOOS == "windows" {
		target += ".exe"
	}
	if err := os.WriteFile(target, raw, 0o755); err != nil {
		t.Fatalf("write the helper executable: %v", err)
	}
	return target
}

// writeFakeVendorExecutable writes a script that records having run, and
// answers --version the way the vendor tool would.
func writeFakeVendorExecutable(t *testing.T, directory, name, marker string) string {
	t.Helper()
	if runtime.GOOS == "windows" {
		path := filepath.Join(directory, name+".cmd")
		script := "@echo off\r\necho probed> \"" + marker + "\"\r\necho claude 2.1.285 (Claude Code)\r\n"
		if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
			t.Fatalf("write the fake executable: %v", err)
		}
		return path
	}
	path := filepath.Join(directory, name)
	script := "#!/bin/sh\necho probed > '" + marker + "'\necho 'claude 2.1.285 (Claude Code)'\n"
	if err := os.WriteFile(path, []byte(script), 0o755); err != nil {
		t.Fatalf("write the fake executable: %v", err)
	}
	return path
}

func TestProbeExecutableEligibility(t *testing.T) {
	tests := []struct {
		name    string
		command []string
		adapter string
		want    bool
	}{
		{name: "the vendor executable", command: []string{"claude"}, adapter: adapters.ClaudeID, want: true},
		{name: "an absolute path", command: []string{filepath.Join("usr", "local", "bin", "claude")}, adapter: adapters.ClaudeID, want: true},
		{name: "a windows suffix", command: []string{"claude.exe"}, adapter: adapters.ClaudeID, want: true},
		{name: "an upper-case windows suffix", command: []string{"CLAUDE.EXE"}, adapter: adapters.ClaudeID, want: true},
		{name: "the vendor executable with arguments", command: []string{"claude", "--token", "fixture"}, adapter: adapters.ClaudeID, want: true},
		{name: "a shell", command: []string{"sh", "-c", "claude"}, adapter: adapters.ClaudeID, want: false},
		{name: "a windows shell", command: []string{"cmd.exe", "/c", "claude"}, adapter: adapters.ClaudeID, want: false},
		{name: "a node launcher", command: []string{"node", "claude.js"}, adapter: adapters.ClaudeID, want: false},
		{name: "an npx launcher", command: []string{"npx", "claude"}, adapter: adapters.ClaudeID, want: false},
		{name: "a container launcher", command: []string{"docker", "run", "claude"}, adapter: adapters.ClaudeID, want: false},
		{name: "another vendor's executable", command: []string{"aider"}, adapter: adapters.ClaudeID, want: false},
		{name: "no argv at all", command: nil, adapter: adapters.ClaudeID, want: false},
		{name: "an empty argv[0]", command: []string{"  "}, adapter: adapters.ClaudeID, want: false},
		{name: "no vendor executable for the adapter", command: []string{"claude"}, adapter: adapters.GenericID, want: false},
		{name: "no adapter at all", command: []string{"claude"}, adapter: "", want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			spec := agent.Spec{ID: "probe", Command: test.command}
			if _, got := probeExecutable(spec, test.adapter); got != test.want {
				t.Fatalf("probeExecutable(%#v, %q) eligible = %t, want %t", test.command, test.adapter, got, test.want)
			}
		})
	}
}

// The probe used to rejoin a shell wrapper's configured command line and run
// it again with --version appended, so a script's side effects happened twice
// per start. It now runs nothing for a wrapper: not the script, not a probe.
func TestTheVersionProbeNeverReplaysAConfiguredCommandLine(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "replayed")

	var spec agent.Spec
	if runtime.GOOS == "windows" {
		if _, err := exec.LookPath("cmd.exe"); err != nil {
			t.Skipf("no cmd.exe: %v", err)
		}
		spec = agent.Spec{
			ID:      "wrapped",
			Adapter: adapters.ClaudeID,
			Command: []string{"cmd.exe", "/c", `echo replayed> "` + marker + `"`},
		}
	} else {
		shell, err := exec.LookPath("sh")
		if err != nil {
			t.Skipf("no sh: %v", err)
		}
		spec = agent.Spec{
			ID:      "wrapped",
			Adapter: adapters.ClaudeID,
			Command: []string{shell, "-c", "printf replayed > '" + marker + "'"},
		}
	}

	if _, err := DefaultVersionInspector(context.Background(), spec); err == nil {
		t.Fatal("the probe ran a shell wrapper instead of refusing it")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatalf("the probe replayed the configured command line: %s exists", marker)
	}
	if info := CheckAgentVersion(context.Background(), spec, adapters.ClaudeID, false, nil); info != (AgentVersionInfo{}) {
		t.Fatalf("a wrapped agent reported version information: %+v", info)
	}
}

// A launcher's --version is the launcher's version, not the agent's: npx
// reports npm's, node reports node's. The probe runs no launcher at all, so a
// wrapped installation shows no version and makes no claim.
func TestTheVersionProbeRunsNoLauncher(t *testing.T) {
	for _, command := range [][]string{
		{"node", "claude.js"},
		{"npx", "claude"},
		{"python", "-m", "claude"},
		{"docker", "run", "claude"},
	} {
		called := false
		spec := agent.Spec{ID: "launched", Adapter: adapters.ClaudeID, Command: command}
		info := CheckAgentVersion(context.Background(), spec, adapters.ClaudeID, false,
			func(context.Context, agent.Spec) (string, error) {
				called = true
				return "22.4.1", nil
			})
		if called {
			t.Errorf("%v: the probe ran a launcher", command)
		}
		if info != (AgentVersionInfo{}) {
			t.Errorf("%v: a launched agent reported version information: %+v", command, info)
		}
	}
}

// A shell-mode agent has no argv, and the probe used to fall back to whatever
// "claude" the PATH resolved — a different program than the one configured,
// run with the run's environment.
func TestTheVersionProbeNeverSearchesThePathForAShellAgent(t *testing.T) {
	directory := t.TempDir()
	marker := filepath.Join(directory, "path-probed")
	writeFakeVendorExecutable(t, directory, "claude", marker)
	t.Setenv("PATH", directory+string(os.PathListSeparator)+os.Getenv("PATH"))

	spec := agent.Spec{
		ID:      "shell-agent",
		Adapter: adapters.ClaudeID,
		Shell:   "exec claude",
	}
	if _, err := DefaultVersionInspector(context.Background(), spec); err == nil {
		t.Fatal("the probe ran for an agent with no argv")
	}
	if _, err := os.Stat(marker); err == nil {
		t.Fatal("the probe ran a PATH executable instead of the configured shell agent")
	}
	if info := CheckAgentVersion(context.Background(), spec, adapters.ClaudeID, false, nil); info != (AgentVersionInfo{}) {
		t.Fatalf("a shell-mode agent reported version information: %+v", info)
	}
}

// "command: [claude]" with no configured adapter resolves to the Claude
// adapter, and the probe used to test the configured field: that agent got no
// warning whatever its version. The check now takes the resolved adapter, and
// only the resolved one decides.
func TestCheckAgentVersionUsesTheResolvedAdapter(t *testing.T) {
	recording := func(version string, calls *int) VersionInspector {
		return func(context.Context, agent.Spec) (string, error) {
			*calls++
			return version, nil
		}
	}

	calls := 0
	spec := agent.Spec{ID: "resolved", Command: []string{"claude"}}
	info := CheckAgentVersion(context.Background(), spec, adapters.ClaudeID, false, recording("3.0.0", &calls))
	if calls != 1 {
		t.Fatalf("the inspector ran %d times, want once for the resolved vendor adapter", calls)
	}
	if info.InstalledVersion != "3.0.0" || !info.Unverified {
		t.Fatalf("a resolved Claude agent at 3.0.0 = %+v, want an unverified 3.0.0", info)
	}

	// The converse: a configured vendor adapter the run resolved away — the
	// registry fell back to generic — probes nothing and warns about nothing.
	calls = 0
	configured := agent.Spec{ID: "fell-back", Adapter: adapters.ClaudeID, Command: []string{"claude"}}
	info = CheckAgentVersion(context.Background(), configured, adapters.GenericID, false, recording("3.0.0", &calls))
	if calls != 0 {
		t.Fatal("the probe ran for an agent whose resolved adapter is generic")
	}
	if info != (AgentVersionInfo{}) {
		t.Fatalf("a generic agent reported version information: %+v", info)
	}
}

// The real inspector, against a real executable: no agent, no terminal, no
// network. The helper prints a launcher's version on stderr and a banner on
// stdout before the product's line; the probe reads the product's line alone.
func TestTheRealProbeReadsTheAgentsOwnVersion(t *testing.T) {
	directory := t.TempDir()
	executable := versionHelperExecutable(t, directory, "claude")

	spec := agent.Spec{
		ID:      "real-probe",
		Adapter: adapters.ClaudeID,
		Command: []string{executable},
		Env:     map[string]string{versionHelperEnv: "version"},
	}
	version, err := DefaultVersionInspector(context.Background(), spec)
	if err != nil {
		t.Fatalf("DefaultVersionInspector: %v", err)
	}
	if version != "2.1.285" {
		t.Fatalf("the probe read %q, want the product line's 2.1.285", version)
	}

	info := CheckAgentVersion(context.Background(), spec, adapters.ClaudeID, false, nil)
	if info.InstalledVersion != "2.1.285" || info.Unverified || info.Reason != "" {
		t.Fatalf("a verified version reported %+v", info)
	}
}
