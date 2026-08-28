package toolcatalog

import (
	"context"
	"errors"
	"os/exec"
	"reflect"
	"strings"
	"testing"
)

type detectorFunc func(context.Context, []string) (Detection, error)

func (function detectorFunc) Detect(ctx context.Context, candidates []string) (Detection, error) {
	return function(ctx, candidates)
}

func TestDetectUsesProfileCandidatesOrExactOverride(t *testing.T) {
	tests := []struct {
		name       string
		profile    ProfileID
		override   string
		candidates []string
	}{
		{name: "claude", profile: ClaudeCode, candidates: []string{"claude"}},
		{name: "codex", profile: CodexCLI, candidates: []string{"codex"}},
		{name: "mimo", profile: MimoCode, candidates: []string{"mimo"}},
		{name: "ollama", profile: Ollama, candidates: []string{"ollama"}},
		{name: "override", profile: ClaudeCode, override: "/opt/claude-preview", candidates: []string{"/opt/claude-preview"}},
		{name: "custom", profile: Custom, override: "local-cli", candidates: []string{"local-cli"}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			detector := detectorFunc(func(_ context.Context, candidates []string) (Detection, error) {
				if !reflect.DeepEqual(candidates, test.candidates) {
					t.Fatalf("candidates = %#v, want %#v", candidates, test.candidates)
				}
				candidates[0] = "mutated by detector"
				return Detection{
					Status: InstallInstalled, Executable: test.candidates[0], Path: "/resolved/tool",
				}, nil
			})
			result, err := Detect(context.Background(), test.profile, test.override, detector)
			if err != nil {
				t.Fatal(err)
			}
			if result.Status != InstallInstalled || result.Path != "/resolved/tool" {
				t.Fatalf("Detect() = %#v", result)
			}
		})
	}

	claude, _ := Lookup(ClaudeCode)
	if !reflect.DeepEqual(claude.Executables, []string{"claude"}) {
		t.Fatalf("detector mutated catalogue storage: %#v", claude)
	}
}

func TestDetectCustomWithoutExecutableIsUnknownWithoutDetector(t *testing.T) {
	result, err := Detect(context.Background(), Custom, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	if result != (Detection{Status: InstallUnknown}) {
		t.Fatalf("Detect() = %#v", result)
	}
}

func TestDetectRejectsInvalidInputAndDetectorResults(t *testing.T) {
	validDetector := detectorFunc(func(context.Context, []string) (Detection, error) {
		return Detection{Status: InstallNotInstalled}, nil
	})
	if _, err := Detect(context.Background(), "missing", "", validDetector); err == nil || !strings.Contains(err.Error(), "unknown tool profile") {
		t.Fatalf("unknown profile error = %v", err)
	}
	if _, err := Detect(nil, ClaudeCode, "", validDetector); err == nil || !strings.Contains(err.Error(), "context") {
		t.Fatalf("nil context error = %v", err)
	}
	if _, err := Detect(context.Background(), ClaudeCode, "bad\x00tool", validDetector); err == nil || !strings.Contains(err.Error(), "NUL") {
		t.Fatalf("NUL override error = %v", err)
	}
	if _, err := Detect(context.Background(), ClaudeCode, "", nil); err == nil || !strings.Contains(err.Error(), "detector") {
		t.Fatalf("nil detector error = %v", err)
	}

	tests := []Detection{
		{},
		{Status: "invalid"},
		{Status: InstallUnknown, Path: "/unexpected"},
		{Status: InstallNotInstalled, Executable: "unexpected"},
		{Status: InstallInstalled, Executable: "tool"},
		{Status: InstallInstalled, Path: "/tool"},
		{Status: InstallInstalled, Executable: "tool", Path: "/tool", VersionKnown: true},
		{Status: InstallInstalled, Executable: "tool", Path: "/tool", Version: "1.0"},
		{Status: InstallInstalled, Executable: "bad\x00tool", Path: "/tool"},
		{Status: InstallInstalled, Executable: "tool", Path: "/bad\x00path"},
		{Status: InstallInstalled, Executable: "tool", Path: "/tool", Version: "bad\x00version", VersionKnown: true},
	}
	for _, invalid := range tests {
		_, err := Detect(context.Background(), ClaudeCode, "", detectorFunc(
			func(context.Context, []string) (Detection, error) { return invalid, nil },
		))
		if err == nil {
			t.Fatalf("invalid detection %#v accepted", invalid)
		}
	}

	want := errors.New("detector failed")
	_, err := Detect(context.Background(), ClaudeCode, "", detectorFunc(
		func(context.Context, []string) (Detection, error) { return Detection{}, want },
	))
	if !errors.Is(err, want) {
		t.Fatalf("detector error = %v, want wrapping %v", err, want)
	}
}

func TestPathDetectorFindsInOrderAndUsesInjectedVersionMetadata(t *testing.T) {
	var lookedUp []string
	var versionPath string
	detector := PathDetector{
		LookPath: func(candidate string) (string, error) {
			lookedUp = append(lookedUp, candidate)
			if candidate == "first" {
				return "", exec.ErrNotFound
			}
			return "/safe/bin/second", nil
		},
		VersionLookup: func(path string) (string, bool) {
			versionPath = path
			return "  2.4.1  ", true
		},
	}

	result, err := detector.Detect(context.Background(), []string{"first", "second", "third"})
	if err != nil {
		t.Fatal(err)
	}
	want := Detection{
		Status: InstallInstalled, Executable: "second", Path: "/safe/bin/second",
		Version: "2.4.1", VersionKnown: true,
	}
	if !reflect.DeepEqual(result, want) {
		t.Fatalf("Detect() = %#v, want %#v", result, want)
	}
	if !reflect.DeepEqual(lookedUp, []string{"first", "second"}) || versionPath != "/safe/bin/second" {
		t.Fatalf("lookups = %#v, version path = %q", lookedUp, versionPath)
	}
}

func TestPathDetectorReportsNotInstalledAndDoesNotInventVersion(t *testing.T) {
	versionCalls := 0
	detector := PathDetector{
		LookPath: func(string) (string, error) { return "", exec.ErrNotFound },
		VersionLookup: func(string) (string, bool) {
			versionCalls++
			return "should-not-be-used", true
		},
	}
	result, err := detector.Detect(context.Background(), []string{"one", "two"})
	if err != nil {
		t.Fatal(err)
	}
	if result != (Detection{Status: InstallNotInstalled}) || versionCalls != 0 {
		t.Fatalf("Detect() = %#v, version calls = %d", result, versionCalls)
	}

	result, err = (PathDetector{
		LookPath: func(string) (string, error) { return "/bin/tool", nil },
	}).Detect(context.Background(), []string{"tool"})
	if err != nil {
		t.Fatal(err)
	}
	if result.VersionKnown || result.Version != "" {
		t.Fatalf("version invented without metadata source: %#v", result)
	}
}

func TestPathDetectorHandlesContextAndLookupFailures(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	lookupCalls := 0
	_, err := (PathDetector{LookPath: func(string) (string, error) {
		lookupCalls++
		return "/bin/tool", nil
	}}).Detect(ctx, []string{"tool"})
	if !errors.Is(err, context.Canceled) || lookupCalls != 0 {
		t.Fatalf("cancelled detection error = %v, lookup calls = %d", err, lookupCalls)
	}

	want := errors.New("lookup unavailable")
	_, err = (PathDetector{LookPath: func(string) (string, error) { return "", want }}).
		Detect(context.Background(), []string{"tool"})
	if !errors.Is(err, want) {
		t.Fatalf("lookup error = %v, want wrapping %v", err, want)
	}

	_, err = (PathDetector{LookPath: func(string) (string, error) { return "", exec.ErrDot }}).
		Detect(context.Background(), []string{"tool"})
	if !errors.Is(err, exec.ErrDot) {
		t.Fatalf("exec.ErrDot = %v", err)
	}
}

func TestPathDetectorRejectsMalformedCandidatesAndResults(t *testing.T) {
	tests := []struct {
		name       string
		candidates []string
		lookup     LookPathFunc
		part       string
	}{
		{name: "blank", candidates: []string{" "}, lookup: func(string) (string, error) { return "/tool", nil }, part: "blank"},
		{name: "nul", candidates: []string{"bad\x00tool"}, lookup: func(string) (string, error) { return "/tool", nil }, part: "NUL"},
		{name: "empty result", candidates: []string{"tool"}, lookup: func(string) (string, error) { return "", nil }, part: "empty path"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := (PathDetector{LookPath: test.lookup}).Detect(context.Background(), test.candidates)
			if err == nil || !strings.Contains(err.Error(), test.part) {
				t.Fatalf("Detect() error = %v, want containing %q", err, test.part)
			}
		})
	}

	result, err := (PathDetector{}).Detect(context.Background(), nil)
	if err != nil || result != (Detection{Status: InstallUnknown}) {
		t.Fatalf("empty candidates = %#v, %v", result, err)
	}
}
