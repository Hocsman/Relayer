package tmuxbackend

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const (
	// HelperSubcommand is private protocol between Relayer and its tmux panes.
	HelperSubcommand = "__relayer_tmux_exec"
	maxSpecSize      = 1 << 20
)

// HelperMain handles the private tmux launch mode before public CLI parsing.
// Callers should exit with exitCode when handled is true.
func HelperMain(arguments []string, diagnostics io.Writer) (handled bool, exitCode int) {
	if len(arguments) == 0 || arguments[0] != HelperSubcommand {
		return false, 0
	}
	if diagnostics == nil {
		diagnostics = io.Discard
	}
	if len(arguments) != 3 {
		fmt.Fprintln(diagnostics, "lanceur tmux Relayer: arguments internes invalides")
		return true, 125
	}
	if err := executeLaunchSpec(arguments[1], arguments[2]); err != nil {
		fmt.Fprintf(diagnostics, "lanceur tmux Relayer: %v\n", err)
		return true, 125
	}
	// On supported systems executeLaunchSpec replaces the process. Reaching
	// this line is only possible for a test double or unsupported platform.
	return true, 0
}

func readLaunchSpec(path string) (launchSpec, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return launchSpec{}, fmt.Errorf("reading the private specification: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o077 != 0 {
		return launchSpec{}, errors.New("invalid permissions for the private specification")
	}
	if info.Size() > maxSpecSize {
		return launchSpec{}, errors.New("private specification too large")
	}
	file, err := os.Open(path)
	if err != nil {
		return launchSpec{}, fmt.Errorf("opening the private specification: %w", err)
	}
	defer file.Close()

	decoder := json.NewDecoder(io.LimitReader(file, maxSpecSize+1))
	decoder.DisallowUnknownFields()
	var spec launchSpec
	if err := decoder.Decode(&spec); err != nil {
		return launchSpec{}, fmt.Errorf("decoding the private specification: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return launchSpec{}, errors.New("extra data in the private specification")
	}
	return spec, nil
}
