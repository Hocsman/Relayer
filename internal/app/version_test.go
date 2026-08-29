package app

import (
	"bytes"
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/Hocsman/Relayer/internal/adapters"
	"github.com/Hocsman/Relayer/internal/audit"
	"github.com/Hocsman/Relayer/internal/session"
	"github.com/Hocsman/Relayer/internal/terminal"
	"github.com/Hocsman/Relayer/internal/tmuxbackend"
	buildversion "github.com/Hocsman/Relayer/internal/version"
)

func TestRunWithOutputPrintsStandaloneVersionBeforeDependencies(t *testing.T) {
	previousVersion, previousCommit := buildversion.Version, buildversion.Commit
	t.Cleanup(func() {
		buildversion.Version, buildversion.Commit = previousVersion, previousCommit
	})
	buildversion.Version = "0.1.0-alpha.2"
	buildversion.Commit = "release-commit"

	for _, flag := range []string{"--version", "-version"} {
		t.Run(flag, func(t *testing.T) {
			var output, diagnostics bytes.Buffer
			called := false
			dependencies := backendDependencies{
				lookup: func(string) (string, error) {
					called = true
					return "", errors.New("must not run")
				},
				newAudit: func(audit.Config) (*audit.Recorder, error) {
					called = true
					return nil, errors.New("must not run")
				},
				newPTY: func(context.Context, chan<- session.Event, *adapters.Registry, int) (terminal.Backend, error) {
					called = true
					return nil, errors.New("must not run")
				},
				newTmux: func(context.Context, chan<- session.Event, *adapters.Registry, int, tmuxbackend.Options) (terminal.Backend, error) {
					called = true
					return nil, errors.New("must not run")
				},
			}
			if err := runWithOutput([]string{flag}, &output, &diagnostics, dependencies); err != nil {
				t.Fatalf("runWithOutput: %v", err)
			}
			if got, want := output.String(), "relayer 0.1.0-alpha.2 (commit release-commit)\n"; got != want {
				t.Fatalf("stdout = %q, want %q", got, want)
			}
			if diagnostics.Len() != 0 || called {
				t.Fatalf("version touched diagnostics/dependencies: diagnostics=%q called=%t", diagnostics.String(), called)
			}
		})
	}
}

func TestRunWithOutputRejectsVersionCombinedWithOtherArguments(t *testing.T) {
	for _, arguments := range [][]string{
		{"--version", "--config", "missing.yaml"},
		{"--config", "missing.yaml", "-version"},
		{"--version", "-version"},
	} {
		var output, diagnostics bytes.Buffer
		err := runWithOutput(arguments, &output, &diagnostics, backendDependencies{})
		if err == nil || !strings.Contains(err.Error(), "must be used alone") {
			t.Fatalf("arguments %q error = %v", arguments, err)
		}
		if output.Len() != 0 || diagnostics.Len() != 0 {
			t.Fatalf("rejected version request wrote output=%q diagnostics=%q", output.String(), diagnostics.String())
		}
	}
}

type failingVersionWriter struct{ err error }

func (w failingVersionWriter) Write([]byte) (int, error) { return 0, w.err }

func TestRunWithOutputPropagatesVersionWriterFailure(t *testing.T) {
	want := errors.New("stdout unavailable")
	err := runWithOutput([]string{"--version"}, failingVersionWriter{err: want}, io.Discard, backendDependencies{})
	if !errors.Is(err, want) {
		t.Fatalf("runWithOutput error = %v, want writer error", err)
	}
}

// `doctor` is a bare subcommand, so an operator reasonably types `version` the
// same way. Before this it fell through to the terminal interface and failed by
// demanding a TTY, which a pipeline or a CI step does not have — a failure the
// released binary showed and a `go run` in a terminal never would.
func TestBareVersionSubcommandPrintsTheVersion(t *testing.T) {
	var output strings.Builder
	if err := RunWithOutput([]string{"version"}, &output, io.Discard); err != nil {
		t.Fatalf("relayer version: %v", err)
	}
	if !strings.Contains(output.String(), "relayer") {
		t.Fatalf("version output = %q", output.String())
	}
}
