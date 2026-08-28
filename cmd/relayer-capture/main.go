// Command relayer-capture records bounded, anonymized output-only PTY or tmux
// fixture artifacts. It never invokes an implicit shell or accepts environment
// values or manual input.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/Hocsman/Relayer/internal/fixturecapture"
)

func main() {
	if handled, exitCode := fixturecapture.HelperMain(os.Args[1:], os.Stderr); handled {
		os.Exit(exitCode)
	}
	ctx, cancel := commandContext()
	defer cancel()
	os.Exit(run(ctx, os.Args[1:], os.Stdout, os.Stderr))
}

func run(ctx context.Context, arguments []string, stdout, stderr io.Writer) int {
	if stdout == nil {
		stdout = io.Discard
	}
	if stderr == nil {
		stderr = io.Discard
	}
	flags := flag.NewFlagSet("relayer-capture", flag.ContinueOnError)
	// flag's default parse diagnostics echo invalid values. Capture flags may
	// be supplied from scripts, so suppress those values and print only the
	// static usage text below.
	flags.SetOutput(io.Discard)
	var (
		validatePath string
		outputPath   string
		tool         string
		adapter      string
		backend      string
		cwd          string
		tmuxPath     string
		timeout      time.Duration
		maxBytes     int
	)
	flags.StringVar(&validatePath, "validate", "", "validate one existing fixture without launching a process")
	flags.StringVar(&outputPath, "output", "", "destination JSON fixture path")
	flags.StringVar(&tool, "tool", "", "non-sensitive fixture tool identifier")
	flags.StringVar(&adapter, "adapter", "generic", "adapter identifier represented by the fixture")
	flags.StringVar(&backend, "backend", string(fixturecapture.BackendPTY), "capture backend: pty or tmux")
	flags.StringVar(&cwd, "cwd", "", "child working directory (never persisted)")
	flags.StringVar(&tmuxPath, "tmux-path", "", "optional tmux executable path (never persisted)")
	flags.DurationVar(&timeout, "timeout", fixturecapture.DefaultTimeout, "capture timeout")
	flags.IntVar(&maxBytes, "max-bytes", fixturecapture.DefaultMaxBytes, "maximum captured output bytes")
	flags.Usage = func() {
		fmt.Fprintln(stderr, "usage: relayer-capture [flags] -- executable [literal argv ...]")
		fmt.Fprintln(stderr, "       relayer-capture --validate fixture.json")
		flags.SetOutput(stderr)
		flags.PrintDefaults()
		flags.SetOutput(io.Discard)
	}
	if err := flags.Parse(arguments); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	anonymizer, err := fixturecapture.NewDefaultAnonymizer()
	if err != nil {
		fmt.Fprintln(stderr, "relayer-capture: secure anonymizer unavailable")
		return 1
	}
	if strings.TrimSpace(validatePath) != "" {
		captureFlagUsed := false
		flags.Visit(func(current *flag.Flag) {
			if current.Name != "validate" {
				captureFlagUsed = true
			}
		})
		if captureFlagUsed || strings.TrimSpace(outputPath) != "" || strings.TrimSpace(tool) != "" || len(flags.Args()) != 0 {
			fmt.Fprintln(stderr, "relayer-capture: --validate cannot be combined with capture arguments")
			return 2
		}
		if _, err := fixturecapture.ReadFile(validatePath, anonymizer); err != nil {
			fmt.Fprintf(stderr, "relayer-capture: validation failed: %s\n", safeDiagnostic(anonymizer, err))
			return 1
		}
		fmt.Fprintln(stdout, "fixture valid")
		return 0
	}
	if strings.TrimSpace(outputPath) == "" || strings.TrimSpace(tool) == "" || len(flags.Args()) == 0 {
		flags.Usage()
		return 2
	}
	fixture, err := fixturecapture.Capture(ctx, fixturecapture.Options{
		Tool:       tool,
		Adapter:    adapter,
		Backend:    fixturecapture.Backend(backend),
		Command:    append([]string(nil), flags.Args()...),
		Cwd:        cwd,
		TmuxPath:   tmuxPath,
		Timeout:    timeout,
		MaxBytes:   maxBytes,
		Anonymizer: anonymizer,
	})
	if err != nil {
		fmt.Fprintf(stderr, "relayer-capture: capture failed: %s\n", safeDiagnostic(anonymizer, err))
		return 1
	}
	if err := fixturecapture.WriteFile(outputPath, fixture, anonymizer); err != nil {
		fmt.Fprintf(stderr, "relayer-capture: write failed: %s\n", safeDiagnostic(anonymizer, err))
		return 1
	}
	fmt.Fprintf(stdout, "fixture written (%s)\n", fixture.Outcome)
	return 0
}

func safeDiagnostic(anonymizer *fixturecapture.Anonymizer, err error) string {
	if err == nil {
		return "operation failed"
	}
	safe, sanitizeErr := anonymizer.Anonymize([]byte(err.Error()))
	if sanitizeErr != nil {
		return "operation failed safely; sensitive diagnostics were suppressed"
	}
	return string(safe)
}
