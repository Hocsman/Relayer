package preflight

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/agent"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/config"
	"github.com/Hocsman/Relayer/internal/policy"
	"github.com/Hocsman/Relayer/internal/toolcatalog"
)

type detectorFunc func(context.Context, []string) (toolcatalog.Detection, error)

func (function detectorFunc) Detect(ctx context.Context, candidates []string) (toolcatalog.Detection, error) {
	return function(ctx, candidates)
}

func TestCheckReadyReportIsDeterministicAndSafe(t *testing.T) {
	input := validInput()
	options := testOptions(detectorWithInstalled("runner"))

	first, err := Check(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := Check(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("reports are not deterministic:\nfirst  %#v\nsecond %#v", first, second)
	}
	if !first.Ready() || first.HasBlockers() || first.Status != StatusReady {
		t.Fatalf("ready report status = %q", first.Status)
	}
	if first.SchemaVersion != CurrentSchemaVersion || first.Platform != (PlatformInfo{OS: "darwin", Arch: "arm64", Supported: true}) {
		t.Fatalf("report header = schema %d platform %#v", first.SchemaVersion, first.Platform)
	}
	if len(first.Agents) != 1 || first.Agents[0] != (AgentInfo{
		Ordinal: 1, Source: AgentConfigured, Command: CommandDirect,
		Installation: toolcatalog.InstallInstalled, Adapter: adapters.GenericID,
		AdapterMaturity: adapters.StatusStable, Backend: agent.BackendPTY,
	}) {
		t.Fatalf("agent report = %#v", first.Agents)
	}
	if got := first.Audit; got.Enabled || got.Location != AuditLocationDisabled || got.Mode != audit.ModeOff {
		t.Fatalf("audit report = %#v", got)
	}
	if len(first.Tools) != len(toolcatalog.Descriptors()) {
		t.Fatalf("tool count = %d", len(first.Tools))
	}
	for _, check := range first.Checks {
		if check.Status != CheckPass {
			t.Fatalf("unexpected non-pass check: %#v", check)
		}
	}

	payload, err := json.Marshal(first)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbiddenField := range []string{"config_path", "executable_path", "argv", "environment", "agent_id", "agent_name"} {
		if strings.Contains(string(payload), forbiddenField) {
			t.Fatalf("report contains forbidden field %q: %s", forbiddenField, payload)
		}
	}
}

func TestCheckResolvesExperimentalAdapterAndAutoBackend(t *testing.T) {
	input := validInput()
	input.Configuration.Backend = agent.BackendAuto
	input.Specs[0].Command = []string{"claude", "--safe"}
	input.Specs[0].Adapter = ""
	input.Specs[0].Backend = agent.BackendAuto
	report, err := Check(context.Background(), input, testOptions(detectorWithInstalled("claude")))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusWarning || report.HasBlockers() || report.Ready() {
		t.Fatalf("auto/experimental status = %q", report.Status)
	}
	want := AgentInfo{
		Ordinal: 1, Source: AgentConfigured, Command: CommandDirect,
		Installation: toolcatalog.InstallInstalled, Adapter: adapters.ClaudeID,
		AdapterMaturity: adapters.StatusExperimental, Backend: agent.BackendPTY,
	}
	if !reflect.DeepEqual(report.Agents, []AgentInfo{want}) {
		t.Fatalf("agent = %#v, want %#v", report.Agents, want)
	}
	if check := findCheck(t, report, "agent.1.adapter"); check.Status != CheckWarning || check.Summary != summaryAdapterExperimental {
		t.Fatalf("adapter check = %#v", check)
	}
	if check := findCheck(t, report, "agent.1.backend"); check.Status != CheckWarning || check.Summary != summaryBackendAutoFallback {
		t.Fatalf("backend check = %#v", check)
	}
}

func TestCheckExplicitGenericOverridesClaudeExecutableHint(t *testing.T) {
	input := validInput()
	input.Specs[0].Command = []string{"claude"}
	input.Specs[0].Adapter = adapters.GenericID
	report, err := Check(context.Background(), input, testOptions(detectorWithInstalled("claude")))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusReady || len(report.Agents) != 1 ||
		report.Agents[0].Adapter != adapters.GenericID ||
		report.Agents[0].AdapterMaturity != adapters.StatusStable {
		t.Fatalf("explicit generic report = %#v", report)
	}
	if check := findCheck(t, report, "agent.1.adapter"); check.Status != CheckPass {
		t.Fatalf("explicit generic adapter check = %#v", check)
	}
}

func TestCheckExplicitTmuxAndMissingExecutableBlock(t *testing.T) {
	input := validInput()
	input.Configuration.Backend = agent.BackendTmux
	input.Specs[0].Backend = agent.BackendTmux
	report, err := Check(context.Background(), input, testOptions(detectorWithInstalled()))
	if err != nil {
		t.Fatal(err)
	}
	if !report.HasBlockers() || report.Status != StatusBlocked {
		t.Fatalf("blocked report status = %q", report.Status)
	}
	if check := findCheck(t, report, "agent.1.executable"); check.Status != CheckBlock {
		t.Fatalf("executable check = %#v", check)
	}
	if check := findCheck(t, report, "agent.1.backend"); check.Status != CheckBlock {
		t.Fatalf("tmux check = %#v", check)
	}
	if report.Agents[0].Backend != "" {
		t.Fatalf("unavailable backend reported as effective: %#v", report.Agents[0])
	}
}

func TestCheckInstalledTmuxIsEffective(t *testing.T) {
	input := validInput()
	input.Configuration.Backend = agent.BackendAuto
	input.Specs[0].Backend = agent.BackendAuto
	detector := detectorFunc(func(ctx context.Context, candidates []string) (toolcatalog.Detection, error) {
		if len(candidates) == 1 && candidates[0] == "tmux" {
			return installed("tmux"), nil
		}
		for _, candidate := range candidates {
			if filepath.Base(candidate) == "runner" {
				return installed(candidate), nil
			}
		}
		return detectorWithInstalled().Detect(ctx, candidates)
	})
	report, err := Check(context.Background(), input, testOptions(detector))
	if err != nil {
		t.Fatal(err)
	}
	if report.Status != StatusReady || report.Agents[0].Backend != agent.BackendTmux {
		t.Fatalf("installed tmux report = %#v", report)
	}
}

func TestCheckDirectRelativeExecutableUsesAgentWorkingDirectory(t *testing.T) {
	workingDirectory := t.TempDir()
	executable := filepath.Join(workingDirectory, "relative-runner")
	writeTestExecutable(t, executable)

	input := validInput()
	input.Specs[0].Command = []string{"./relative-runner"}
	input.Specs[0].Cwd = workingDirectory
	input.Configuration.Agents = cloneSpecs(input.Specs)

	passive := toolcatalog.PathDetector{}
	var observed string
	detector := detectorFunc(func(ctx context.Context, candidates []string) (toolcatalog.Detection, error) {
		result, err := passive.Detect(ctx, candidates)
		if len(candidates) == 1 && candidates[0] == executable {
			observed = candidates[0]
		}
		if len(candidates) > 0 {
			candidates[0] = "mutated"
		}
		return result, err
	})
	report, err := Check(context.Background(), input, testOptions(detector))
	if err != nil {
		t.Fatal(err)
	}
	if observed != executable {
		t.Fatalf("contextual candidate = %q, want absolute cwd candidate", observed)
	}
	if report.Agents[0].Installation != toolcatalog.InstallInstalled ||
		findCheck(t, report, "agent.1.executable").Status != CheckPass {
		t.Fatalf("relative executable report = %#v", report)
	}
	if input.Specs[0].Command[0] != "./relative-runner" {
		t.Fatalf("detector mutation reached caller command: %#v", input.Specs[0].Command)
	}
	payload, _ := json.Marshal(report)
	if strings.Contains(string(payload), workingDirectory) || strings.Contains(string(payload), executable) {
		t.Fatalf("report leaked contextual path: %s", payload)
	}
}

func TestCheckTmuxExecutableUsesEffectivePath(t *testing.T) {
	for _, test := range []struct {
		name          string
		overrideAgent bool
	}{
		{name: "agent override", overrideAgent: true},
		{name: "parent fallback"},
	} {
		t.Run(test.name, func(t *testing.T) {
			workingDirectory := t.TempDir()
			binaryDirectory := t.TempDir()
			executable := filepath.Join(binaryDirectory, "tmux-runner")
			writeTestExecutable(t, executable)
			if test.overrideAgent {
				t.Setenv("PATH", t.TempDir())
			} else {
				t.Setenv("PATH", binaryDirectory)
			}

			input := validInput()
			input.Configuration.Backend = agent.BackendTmux
			input.Specs[0].Backend = agent.BackendTmux
			input.Specs[0].Command = []string{"tmux-runner"}
			input.Specs[0].Cwd = workingDirectory
			if test.overrideAgent {
				input.Specs[0].Env = map[string]string{"PATH": binaryDirectory}
			}
			input.Configuration.Agents = cloneSpecs(input.Specs)

			passive := toolcatalog.PathDetector{}
			var observed string
			detector := detectorFunc(func(ctx context.Context, candidates []string) (toolcatalog.Detection, error) {
				if len(candidates) == 1 && candidates[0] == "tmux" {
					return installed("tmux"), nil
				}
				result, err := passive.Detect(ctx, candidates)
				if len(candidates) == 1 && candidates[0] == executable {
					observed = candidates[0]
				}
				if len(candidates) > 0 {
					candidates[0] = "mutated"
				}
				return result, err
			})
			report, err := Check(context.Background(), input, testOptions(detector))
			if err != nil {
				t.Fatal(err)
			}
			if observed != executable || report.Status != StatusReady ||
				report.Agents[0].Installation != toolcatalog.InstallInstalled ||
				report.Agents[0].Backend != agent.BackendTmux {
				t.Fatalf("tmux contextual report = %#v, candidate = %q", report, observed)
			}
			if test.overrideAgent && input.Specs[0].Env["PATH"] != binaryDirectory {
				t.Fatalf("detector mutation reached caller environment: %#v", input.Specs[0].Env)
			}
			payload, _ := json.Marshal(report)
			if strings.Contains(string(payload), workingDirectory) || strings.Contains(string(payload), binaryDirectory) {
				t.Fatalf("report leaked cwd or PATH: %s", payload)
			}
		})
	}
}

func TestCheckAutoFallbackUsesPTYProcessPathSemantics(t *testing.T) {
	const executable = "auto-pty-runner"
	privatePath := filepath.Join(t.TempDir(), "private-agent-path")
	input := validInput()
	input.Configuration.Backend = agent.BackendAuto
	input.Specs[0].Backend = agent.BackendAuto
	input.Specs[0].Command = []string{executable}
	input.Specs[0].Env = map[string]string{"PATH": privatePath}
	input.Configuration.Agents = cloneSpecs(input.Specs)

	var observed []string
	detector := detectorFunc(func(_ context.Context, candidates []string) (toolcatalog.Detection, error) {
		for _, candidate := range candidates {
			if candidate == "tmux" {
				return toolcatalog.Detection{Status: toolcatalog.InstallNotInstalled}, nil
			}
			if candidate == executable {
				observed = append(observed, candidate)
				return installed(candidate), nil
			}
		}
		return toolcatalog.Detection{Status: toolcatalog.InstallNotInstalled}, nil
	})
	report, err := Check(context.Background(), input, testOptions(detector))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(observed, []string{executable}) ||
		report.Agents[0].Installation != toolcatalog.InstallInstalled ||
		report.Agents[0].Backend != agent.BackendPTY || report.Status != StatusWarning {
		t.Fatalf("auto PTY fallback report = %#v, candidates = %#v", report, observed)
	}
	payload, _ := json.Marshal(report)
	if strings.Contains(string(payload), privatePath) {
		t.Fatalf("report leaked ignored agent PATH: %s", payload)
	}
}

func TestCheckTmuxPathOrderEmptyEntriesAndErrorsRemainSafe(t *testing.T) {
	workingDirectory := t.TempDir()
	absoluteDirectory := t.TempDir()
	writeTestExecutable(t, filepath.Join(workingDirectory, "relative-hit"))
	writeTestExecutable(t, filepath.Join(absoluteDirectory, "relative-hit"))
	writeTestExecutable(t, filepath.Join(absoluteDirectory, "later-hit"))

	newInput := func(command, pathValue string) Input {
		input := validInput()
		input.Configuration.Backend = agent.BackendTmux
		input.Specs[0].Backend = agent.BackendTmux
		input.Specs[0].Command = []string{command}
		input.Specs[0].Cwd = workingDirectory
		input.Specs[0].Env = map[string]string{"PATH": pathValue}
		input.Configuration.Agents = cloneSpecs(input.Specs)
		return input
	}
	passive := toolcatalog.PathDetector{}
	detector := detectorFunc(func(ctx context.Context, candidates []string) (toolcatalog.Detection, error) {
		if len(candidates) == 1 && candidates[0] == "tmux" {
			return installed("tmux"), nil
		}
		return passive.Detect(ctx, candidates)
	})

	relativeFirst := "." + string(os.PathListSeparator) + absoluteDirectory
	report, err := Check(context.Background(), newInput("relative-hit", relativeFirst), testOptions(detector))
	if err != nil {
		t.Fatal(err)
	}
	if report.Agents[0].Installation != toolcatalog.InstallUnknown ||
		findCheck(t, report, "agent.1.executable").Summary != summaryAgentExecutableUnknown {
		t.Fatalf("relative PATH hit must mirror ErrDot: %#v", report)
	}

	missingRelativeFirst := "missing" + string(os.PathListSeparator) + absoluteDirectory
	report, err = Check(context.Background(), newInput("later-hit", missingRelativeFirst), testOptions(detector))
	if err != nil {
		t.Fatal(err)
	}
	if report.Agents[0].Installation != toolcatalog.InstallInstalled {
		t.Fatalf("PATH order skipped later absolute hit: %#v", report)
	}

	report, err = Check(context.Background(), newInput("absent", ""), testOptions(detector))
	if err != nil {
		t.Fatal(err)
	}
	if report.Agents[0].Installation != toolcatalog.InstallNotInstalled {
		t.Fatalf("empty tmux PATH = %#v", report.Agents[0])
	}

	const secret = "TMUX_PATH_FAILURE_SECRET"
	secretDirectory := filepath.Join(workingDirectory, secret)
	errorDetector := detectorFunc(func(_ context.Context, candidates []string) (toolcatalog.Detection, error) {
		if len(candidates) == 1 && candidates[0] == "tmux" {
			return installed("tmux"), nil
		}
		if len(candidates) > 0 && strings.Contains(candidates[0], secret) {
			privateError := errors.New("private lookup failure: " + candidates[0])
			candidates[0] = "mutated"
			return toolcatalog.Detection{}, privateError
		}
		return toolcatalog.Detection{Status: toolcatalog.InstallNotInstalled}, nil
	})
	failedInput := newInput("secret-runner", secretDirectory)
	report, err = Check(context.Background(), failedInput, testOptions(errorDetector))
	if err != nil {
		t.Fatal(err)
	}
	payload, _ := json.Marshal(report)
	if report.Agents[0].Installation != toolcatalog.InstallUnknown ||
		strings.Contains(string(payload), secret) || strings.Contains(string(payload), secretDirectory) {
		t.Fatalf("contextual detector error leaked into report: %s", payload)
	}
	if failedInput.Specs[0].Env["PATH"] != secretDirectory {
		t.Fatalf("detector mutation reached caller environment: %#v", failedInput.Specs[0].Env)
	}
}

func TestCheckBlocksUnsupportedPlatformInvalidPolicyAndAdapter(t *testing.T) {
	input := validInput()
	input.Specs[0].Adapter = "not-registered"
	input.Configuration.Policies.Rules = []policy.Rule{{
		Name: "bad reference", Action: policy.ActionAsk,
		Match: policy.Match{AgentIDs: []string{"absent"}},
	}}
	options := testOptions(detectorWithInstalled("runner"))
	options.GOOS = "windows"
	options.GOARCH = "amd64"
	report, err := Check(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Platform.Supported || report.Status != StatusBlocked {
		t.Fatalf("unsupported platform report = %#v", report.Platform)
	}
	for _, id := range []string{"platform.execution", "policy.agent_references", "agent.1.adapter"} {
		if check := findCheck(t, report, id); check.Status != CheckBlock {
			t.Fatalf("%s = %#v", id, check)
		}
	}
}

func TestCheckDoesNotClaimUntestedBSDPlatformSupport(t *testing.T) {
	input := validInput()
	options := testOptions(detectorWithInstalled("runner"))
	options.GOOS = "freebsd"
	options.GOARCH = "amd64"
	report, err := Check(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	if report.Platform.Supported || !report.HasBlockers() {
		t.Fatalf("FreeBSD support was overstated: %#v", report.Platform)
	}
	if check := findCheck(t, report, "platform.execution"); check.Status != CheckBlock {
		t.Fatalf("FreeBSD platform check = %#v", check)
	}
}

func TestCheckCancellationStopsPassiveDetection(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	// The public boundary intentionally verifies a genuinely cancelled context.
	_, err := Check(ctx, validInput(), testOptions(detectorFunc(func(context.Context, []string) (toolcatalog.Detection, error) {
		calls++
		return toolcatalog.Detection{}, nil
	})))
	if !errors.Is(err, context.Canceled) || calls != 0 {
		t.Fatalf("cancelled check = %v, detector calls = %d", err, calls)
	}

	ctx, cancel = context.WithCancel(context.Background())
	calls = 0
	_, err = Check(ctx, validInput(), testOptions(detectorFunc(func(context.Context, []string) (toolcatalog.Detection, error) {
		calls++
		cancel()
		return toolcatalog.Detection{}, context.Canceled
	})))
	if !errors.Is(err, context.Canceled) || calls != 1 {
		t.Fatalf("mid-check cancellation = %v, detector calls = %d", err, calls)
	}

	//lint:ignore SA1012 The public boundary must reject a genuinely nil context.
	if _, err := Check(nil, validInput(), testOptions(detectorWithInstalled("runner"))); !errors.Is(err, ErrNilContext) {
		t.Fatalf("nil context error = %v", err)
	}
}

func TestCheckReportNeverContainsCallerSecretsOrRawErrors(t *testing.T) {
	const secret = "ULTRA_PRIVATE_9xQ"
	input := validInput()
	input.Specs[0] = agent.Spec{
		ID: "agent-" + secret, Name: "name-" + secret,
		Command: []string{"/private/" + secret + "/runner", "--token=" + secret},
		Env:     map[string]string{"API_TOKEN": secret}, Adapter: "adapter-" + strings.ToLower(secret), Backend: agent.BackendPTY,
	}
	input.Configuration.Agents = cloneSpecs(input.Specs)
	input.Configuration.Patterns = []adapters.Pattern{{
		Name: secret, Description: "token " + secret, Expression: "(?i)" + secret,
	}}
	input.Configuration.Policies.Rules = []policy.Rule{{
		Name: secret, Action: policy.ActionAsk,
		Match: policy.Match{AgentIDs: []string{"agent-" + secret}, TextRegex: secret},
	}}
	input.Configuration.Audit = audit.DefaultConfig()
	input.Configuration.Audit.Path = "/private/" + secret + "/audit.jsonl"

	options := testOptions(detectorFunc(func(_ context.Context, candidates []string) (toolcatalog.Detection, error) {
		if len(candidates) > 0 && strings.Contains(candidates[0], secret) {
			return toolcatalog.Detection{
				Status: toolcatalog.InstallInstalled, Executable: candidates[0], Path: "/resolved/" + secret,
			}, nil
		}
		return toolcatalog.Detection{}, errors.New("raw dependency failure " + secret)
	}))
	options.ResolveAuditPath = func(configured string) (string, error) {
		if !strings.Contains(configured, secret) {
			t.Fatal("audit resolver did not receive configured path")
		}
		return filepath.Join(string(filepath.Separator), "safe", "audit.jsonl"), nil
	}
	options.Lstat = func(string) (fs.FileInfo, error) { return nil, fs.ErrNotExist }

	report, err := Check(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	payload, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), secret) || strings.Contains(string(payload), "raw dependency failure") ||
		strings.Contains(string(payload), "API_TOKEN") || strings.Contains(string(payload), "--token") {
		t.Fatalf("safe report leaked caller data: %s", payload)
	}

	input.Configuration.Audit.Mode = audit.Mode(secret)
	report, err = Check(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	payload, _ = json.Marshal(report)
	if strings.Contains(string(payload), secret) || report.Audit.Mode != audit.ModeOff || report.Audit.Enabled {
		t.Fatalf("invalid audit mode leaked: %s", payload)
	}
}

func TestCheckCopiesInputDetectorCandidatesAndReports(t *testing.T) {
	input := validInput()
	originalCommand := append([]string(nil), input.Specs[0].Command...)
	detector := detectorFunc(func(_ context.Context, candidates []string) (toolcatalog.Detection, error) {
		candidate := candidates[0]
		candidates[0] = "mutated"
		if candidate == "runner" {
			return installed(candidate), nil
		}
		return toolcatalog.Detection{Status: toolcatalog.InstallNotInstalled}, nil
	})
	report, err := Check(context.Background(), input, testOptions(detector))
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(input.Specs[0].Command, originalCommand) {
		t.Fatalf("detector mutated input command: %#v", input.Specs[0].Command)
	}
	clone := report.Clone()
	clone.Tools[0].ProfileID = "mutated"
	clone.Agents[0].Adapter = "mutated"
	clone.Checks[0].Summary = "mutated"
	if report.Tools[0].ProfileID == "mutated" || report.Agents[0].Adapter == "mutated" || report.Checks[0].Summary == "mutated" {
		t.Fatalf("Report.Clone retained shared storage: %#v", report)
	}
}

func TestCheckAuditIsReadOnlyAndValidatesExistingPaths(t *testing.T) {
	input := validInput()
	input.Configuration.Audit = audit.DefaultConfig()
	missingPath := filepath.Join(t.TempDir(), "not-created", "audit.jsonl")
	options := testOptions(detectorWithInstalled("runner"))
	options.ResolveAuditPath = func(string) (string, error) { return missingPath, nil }
	options.OwnerCheck = func(fs.FileInfo) OwnerStatus { return OwnerCurrent }
	report, err := Check(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	if check := findCheck(t, report, "audit.path"); check.Status != CheckPass || check.Summary != summaryAuditPathReady {
		t.Fatalf("missing audit path check = %#v", check)
	}
	if _, err := os.Lstat(filepath.Dir(missingPath)); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("preflight mutated missing audit directory: %v", err)
	}

	path := filepath.Join(string(filepath.Separator), "safe", "audit.jsonl")
	directory := filepath.Dir(path)
	options.ResolveAuditPath = func(string) (string, error) { return path, nil }
	options.ReadDir = func(string) ([]fs.DirEntry, error) { return nil, nil }
	options.Lstat = statTable(map[string]statResult{
		directory: {info: fakeFileInfo{name: "safe", mode: os.ModeDir | 0o755}},
		path:      {err: fs.ErrNotExist},
	})
	report, err = Check(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	if check := findCheck(t, report, "audit.path"); check.Status != CheckBlock {
		t.Fatalf("public audit directory check = %#v", check)
	}

	options.Lstat = statTable(map[string]statResult{
		directory: {info: fakeFileInfo{name: "safe", mode: os.ModeDir | 0o500}},
		path:      {err: fs.ErrNotExist},
	})
	report, err = Check(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	if check := findCheck(t, report, "audit.path"); check.Status != CheckBlock || check.Summary != summaryAuditPathUnsafe {
		t.Fatalf("non-writable audit directory check = %#v", check)
	}

	options.Lstat = statTable(map[string]statResult{
		directory: {info: fakeFileInfo{name: "safe", mode: os.ModeDir | 0o700}},
		path:      {info: fakeFileInfo{name: "audit.jsonl", mode: 0o644}},
	})
	report, err = Check(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	if check := findCheck(t, report, "audit.path"); check.Status != CheckWarning || check.Summary != summaryAuditPathHarden {
		t.Fatalf("permissive audit file check = %#v", check)
	}

	options.Lstat = statTable(map[string]statResult{
		directory: {info: fakeFileInfo{name: "safe", mode: os.ModeDir | 0o700}},
		path:      {info: fakeFileInfo{name: "audit.jsonl", mode: 0o400}},
	})
	report, err = Check(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	if check := findCheck(t, report, "audit.path"); check.Status != CheckBlock || check.Summary != summaryAuditPathUnsafe {
		t.Fatalf("read-only active audit file check = %#v", check)
	}

	options.Lstat = statTable(map[string]statResult{
		directory: {info: fakeFileInfo{name: "safe", mode: os.ModeDir | 0o700}},
		path:      {info: fakeFileInfo{name: "audit.jsonl", mode: os.ModeSymlink | 0o777}},
	})
	report, err = Check(context.Background(), input, options)
	if err != nil {
		t.Fatal(err)
	}
	if check := findCheck(t, report, "audit.path"); check.Status != CheckBlock {
		t.Fatalf("symlink audit path check = %#v", check)
	}
}

func TestCheckAuditInspectsRecognizedGenerationsWithoutMutation(t *testing.T) {
	input := validInput()
	input.Configuration.Audit = audit.DefaultConfig()
	path := filepath.Join(string(filepath.Separator), "private-audit", "audit.jsonl")
	directory := filepath.Dir(path)
	baseOptions := testOptions(detectorWithInstalled("runner"))
	baseOptions.ResolveAuditPath = func(string) (string, error) { return path, nil }
	baseOptions.OwnerCheck = func(fs.FileInfo) OwnerStatus { return OwnerCurrent }
	directoryInfo := fakeFileInfo{name: "private-audit", mode: os.ModeDir | 0o700}

	t.Run("private generations", func(t *testing.T) {
		statCalls := make(map[string]int)
		options := baseOptions
		options.ReadDir = func(requested string) ([]fs.DirEntry, error) {
			if requested != directory {
				t.Fatalf("ReadDir path = %q", requested)
			}
			return []fs.DirEntry{
				fs.FileInfoToDirEntry(fakeFileInfo{name: "audit.jsonl.1", mode: 0o600}),
				fs.FileInfoToDirEntry(fakeFileInfo{name: "audit.jsonl.999", mode: 0o600}),
				fs.FileInfoToDirEntry(fakeFileInfo{name: "audit.jsonl.01", mode: 0o600}),
				fs.FileInfoToDirEntry(fakeFileInfo{name: "unrelated", mode: os.ModeSymlink | 0o777}),
			}, nil
		}
		options.Lstat = func(requested string) (fs.FileInfo, error) {
			statCalls[requested]++
			switch requested {
			case directory:
				return directoryInfo, nil
			case path:
				return nil, fs.ErrNotExist
			case path + ".1":
				return fakeFileInfo{name: "audit.jsonl.1", mode: 0o600}, nil
			case path + ".999":
				return fakeFileInfo{name: "audit.jsonl.999", mode: 0o600}, nil
			default:
				t.Fatalf("unexpected Lstat path %q", requested)
				return nil, fs.ErrNotExist
			}
		}
		report, err := Check(context.Background(), input, options)
		if err != nil {
			t.Fatal(err)
		}
		if check := findCheck(t, report, "audit.generations"); check.Status != CheckPass || check.Summary != summaryAuditGenerationsReady {
			t.Fatalf("private generation check = %#v", check)
		}
		if statCalls[path+".1"] != 1 || statCalls[path+".999"] != 1 || len(statCalls) != 4 {
			t.Fatalf("generation Lstat calls = %#v", statCalls)
		}
	})

	t.Run("symlink generation", func(t *testing.T) {
		options := baseOptions
		options.ReadDir = func(string) ([]fs.DirEntry, error) {
			return []fs.DirEntry{fs.FileInfoToDirEntry(fakeFileInfo{name: "audit.jsonl.2", mode: os.ModeSymlink | 0o777})}, nil
		}
		options.Lstat = statTable(map[string]statResult{
			directory:   {info: directoryInfo},
			path + ".2": {info: fakeFileInfo{name: "audit.jsonl.2", mode: os.ModeSymlink | 0o777}},
			path:        {err: fs.ErrNotExist},
		})
		report, err := Check(context.Background(), input, options)
		if err != nil {
			t.Fatal(err)
		}
		if check := findCheck(t, report, "audit.generations"); check.Status != CheckBlock || check.Summary != summaryAuditGenerationsUnsafe {
			t.Fatalf("symlink generation check = %#v", check)
		}
		if !report.HasBlockers() {
			t.Fatalf("symlink generation did not block: %#v", report)
		}
	})

	t.Run("read-only owned generation is repairable", func(t *testing.T) {
		options := baseOptions
		options.ReadDir = func(string) ([]fs.DirEntry, error) {
			return []fs.DirEntry{fs.FileInfoToDirEntry(fakeFileInfo{name: "audit.jsonl.4", mode: 0o400})}, nil
		}
		options.Lstat = statTable(map[string]statResult{
			directory:   {info: directoryInfo},
			path + ".4": {info: fakeFileInfo{name: "audit.jsonl.4", mode: 0o400}},
			path:        {err: fs.ErrNotExist},
		})
		report, err := Check(context.Background(), input, options)
		if err != nil {
			t.Fatal(err)
		}
		if check := findCheck(t, report, "audit.generations"); check.Status != CheckPass || check.Summary != summaryAuditGenerationsReady {
			t.Fatalf("repairable read-only generation check = %#v", check)
		}
	})

	t.Run("read directory failure", func(t *testing.T) {
		const secret = "read-dir-secret-error"
		options := baseOptions
		options.ReadDir = func(string) ([]fs.DirEntry, error) { return nil, errors.New(secret) }
		options.Lstat = statTable(map[string]statResult{directory: {info: directoryInfo}})
		report, err := Check(context.Background(), input, options)
		if err != nil {
			t.Fatal(err)
		}
		check := findCheck(t, report, "audit.generations")
		if check.Status != CheckBlock || check.Summary != summaryAuditGenerationsUnread {
			t.Fatalf("ReadDir failure check = %#v", check)
		}
		payload, marshalErr := json.Marshal(report)
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if strings.Contains(string(payload), secret) {
			t.Fatalf("ReadDir error leaked into report: %s", payload)
		}
	})

	t.Run("permissive generation", func(t *testing.T) {
		options := baseOptions
		options.ReadDir = func(string) ([]fs.DirEntry, error) {
			return []fs.DirEntry{fs.FileInfoToDirEntry(fakeFileInfo{name: "audit.jsonl.3", mode: 0o644})}, nil
		}
		options.Lstat = statTable(map[string]statResult{
			directory:   {info: directoryInfo},
			path + ".3": {info: fakeFileInfo{name: "audit.jsonl.3", mode: 0o644}},
			path:        {err: fs.ErrNotExist},
		})
		report, err := Check(context.Background(), input, options)
		if err != nil {
			t.Fatal(err)
		}
		if check := findCheck(t, report, "audit.generations"); check.Status != CheckWarning || check.Summary != summaryAuditGenerationsHarden {
			t.Fatalf("permissive generation check = %#v", check)
		}
		if report.Status != StatusWarning {
			t.Fatalf("permissive generation status = %q", report.Status)
		}
	})
}

func TestDisabledAuditDoesNotResolveOrInspectAPath(t *testing.T) {
	input := validInput()
	calls := 0
	options := testOptions(detectorWithInstalled("runner"))
	options.ResolveAuditPath = func(string) (string, error) {
		calls++
		return "", errors.New("must not run")
	}
	options.Lstat = func(string) (fs.FileInfo, error) {
		calls++
		return nil, errors.New("must not run")
	}
	options.ReadDir = func(string) ([]fs.DirEntry, error) {
		calls++
		return nil, errors.New("must not run")
	}
	if _, err := Check(context.Background(), input, options); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("disabled audit performed %d filesystem operation(s)", calls)
	}
}

func TestFailureReportUsesClosedStaticVocabulary(t *testing.T) {
	const secret = "failure-secret-value"
	options := Options{GOOS: secret, GOARCH: secret}
	for _, kind := range []FailureKind{
		FailureConfigMissing, FailureConfigInvalid, FailureConfigUnreadable,
		FailureWorkingDirectory, FailureAgentResolution, FailurePolicyResolution,
		FailureAdapterResolution, FailurePreflightInternal, FailureKind(secret),
	} {
		report := FailureReport(kind, options)
		if report.Status != StatusBlocked || !report.HasBlockers() || len(report.Checks) != 1 || report.Checks[0].Status != CheckBlock {
			t.Fatalf("failure report for %q = %#v", kind, report)
		}
		payload, _ := json.Marshal(report)
		if strings.Contains(string(payload), secret) || report.Platform.OS != "unknown" || report.Platform.Arch != "unknown" {
			t.Fatalf("failure report leaked caller value: %s", payload)
		}
	}
}

func TestConcurrentChecksAreIndependent(t *testing.T) {
	input := validInput()
	options := testOptions(detectorWithInstalled("runner"))
	const workers = 32
	var group sync.WaitGroup
	errorsFound := make(chan error, workers)
	for index := 0; index < workers; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			report, err := Check(context.Background(), input, options)
			if err != nil {
				errorsFound <- err
				return
			}
			if !report.Ready() || len(report.Agents) != 1 {
				errorsFound <- errors.New("incoherent concurrent report")
			}
		}()
	}
	group.Wait()
	close(errorsFound)
	for err := range errorsFound {
		t.Error(err)
	}
}

func validInput() Input {
	auditConfiguration := audit.DefaultConfig()
	auditConfiguration.Enabled = false
	auditConfiguration.Mode = audit.ModeOff
	spec := agent.Spec{
		ID: "primary", Name: "Primary", Command: []string{"runner"},
		Adapter: agent.AdapterGeneric, Backend: agent.BackendPTY,
	}
	return Input{
		Configuration: config.Result{
			Version: config.CurrentVersion, Backend: agent.BackendPTY,
			Agents: []agent.Spec{spec}, Patterns: adapters.DefaultPatterns(),
			Policies: policy.DefaultConfig(), Audit: auditConfiguration,
		},
		Specs: []agent.Spec{spec},
	}
}

func testOptions(detector toolcatalog.Detector) Options {
	return Options{
		Detector: detector, GOOS: "darwin", GOARCH: "arm64",
		OwnerCheck: func(fs.FileInfo) OwnerStatus { return OwnerCurrent },
	}
}

func detectorWithInstalled(names ...string) toolcatalog.Detector {
	available := make(map[string]struct{}, len(names))
	for _, name := range names {
		available[name] = struct{}{}
	}
	return detectorFunc(func(ctx context.Context, candidates []string) (toolcatalog.Detection, error) {
		if err := ctx.Err(); err != nil {
			return toolcatalog.Detection{}, err
		}
		for _, candidate := range candidates {
			if _, ok := available[candidate]; ok {
				return installed(candidate), nil
			}
		}
		return toolcatalog.Detection{Status: toolcatalog.InstallNotInstalled}, nil
	})
}

func installed(executable string) toolcatalog.Detection {
	return toolcatalog.Detection{
		Status: toolcatalog.InstallInstalled, Executable: executable, Path: "/resolved/tool",
	}
}

func writeTestExecutable(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o700); err != nil {
		t.Fatal(err)
	}
}

func findCheck(t *testing.T, report Report, id string) CheckResult {
	t.Helper()
	for _, check := range report.Checks {
		if check.ID == id {
			return check
		}
	}
	t.Fatalf("check %q not found in %#v", id, report.Checks)
	return CheckResult{}
}

type fakeFileInfo struct {
	name string
	mode fs.FileMode
}

func (info fakeFileInfo) Name() string      { return info.name }
func (fakeFileInfo) Size() int64            { return 0 }
func (info fakeFileInfo) Mode() fs.FileMode { return info.mode }
func (fakeFileInfo) ModTime() time.Time     { return time.Time{} }
func (info fakeFileInfo) IsDir() bool       { return info.mode.IsDir() }
func (fakeFileInfo) Sys() any               { return nil }

type statResult struct {
	info fs.FileInfo
	err  error
}

func statTable(results map[string]statResult) LstatFunc {
	return func(path string) (fs.FileInfo, error) {
		result, ok := results[path]
		if !ok {
			return nil, fs.ErrNotExist
		}
		return result.info, result.err
	}
}
