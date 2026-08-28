package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/fixturecapture"
)

func TestRunCapturesWritesAndDryValidatesFixture(t *testing.T) {
	printfPath, err := exec.LookPath("printf")
	if err != nil {
		t.Skip("printf is unavailable")
	}
	path := filepath.Join(t.TempDir(), "private", "capture.json")
	var stdout, stderr bytes.Buffer
	exitCode := run(context.Background(), []string{
		"--output", path,
		"--tool", "fixture-cli",
		"--timeout", "2s",
		"--", printfPath, "safe fixture output",
	}, &stdout, &stderr)
	if exitCode != 0 {
		t.Fatalf("capture exit = %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("fixture mode = %o, want 600", info.Mode().Perm())
	}

	stdout.Reset()
	stderr.Reset()
	exitCode = run(context.Background(), []string{"--validate", path}, &stdout, &stderr)
	if exitCode != 0 || strings.TrimSpace(stdout.String()) != "fixture valid" || stderr.Len() != 0 {
		t.Fatalf("validate exit = %d, stdout %q, stderr %q", exitCode, stdout.String(), stderr.String())
	}
}

func TestRunRejectsUnsafeOrAmbiguousModesWithoutLaunching(t *testing.T) {
	tests := [][]string{
		{"--output", "capture.json", "--tool", "fixture-cli"},
		{"--validate", "fixture.json", "--tool", "fixture-cli"},
		{"--validate", "fixture.json", "--backend", "tmux"},
		{"--output", "capture.json", "--tool", "fixture-cli", "--", "tool", "token=fixture-secret"},
		{"--timeout", "token=fixture-secret", "--output", "capture.json", "--tool", "fixture-cli", "--", "tool"},
	}
	for _, arguments := range tests {
		var stdout, stderr bytes.Buffer
		if exitCode := run(context.Background(), arguments, &stdout, &stderr); exitCode == 0 {
			t.Fatalf("run(%#v) unexpectedly succeeded", arguments)
		}
		if strings.Contains(stderr.String(), "fixture-secret") {
			t.Fatalf("run(%#v) leaked sensitive argv: %q", arguments, stderr.String())
		}
	}
}

func TestSafeDiagnosticAnonymizesIdentityAndSuppressesCredential(t *testing.T) {
	anonymizer, err := fixturecapture.NewAnonymizer([]string{"/Users/tester"})
	if err != nil {
		t.Fatal(err)
	}
	identity := safeDiagnostic(anonymizer, &testError{"open /Users/tester/project for person@example.invalid"})
	if identity != "open [HOME]/project for [EMAIL]" {
		t.Fatalf("identity diagnostic = %q", identity)
	}
	secret := safeDiagnostic(anonymizer, &testError{"token=fixture-secret-value"})
	if strings.Contains(secret, "fixture-secret-value") || !strings.Contains(secret, "suppressed") {
		t.Fatalf("secret diagnostic = %q", secret)
	}
}

type testError struct{ value string }

func (err *testError) Error() string { return err.value }
