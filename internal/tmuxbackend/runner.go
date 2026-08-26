package tmuxbackend

import (
	"bytes"
	"context"
	"fmt"
	"os/exec"

	"github.com/Hocsman/Relayer/internal/terminal"
)

// CommandSpec is an argv-safe process invocation. Args never pass through a
// shell and Stdin keeps sensitive answers out of argv and process listings.
type CommandSpec struct {
	Path  string
	Args  []string
	Stdin []byte
}

// CommandRunner makes tmux behavior deterministic in unit tests. Command is
// separate from Run because tea.ExecProcess needs the unstarted *exec.Cmd.
type CommandRunner interface {
	LookPath(file string) (string, error)
	Run(context.Context, CommandSpec) ([]byte, error)
	Command(context.Context, CommandSpec) *exec.Cmd
}

type execRunner struct{}

func (execRunner) LookPath(file string) (string, error) {
	return exec.LookPath(file)
}

func (execRunner) Run(ctx context.Context, spec CommandSpec) ([]byte, error) {
	command := exec.CommandContext(ctx, spec.Path, spec.Args...)
	if spec.Stdin != nil {
		command.Stdin = bytes.NewReader(spec.Stdin)
	}
	return command.CombinedOutput()
}

func (execRunner) Command(ctx context.Context, spec CommandSpec) *exec.Cmd {
	return exec.CommandContext(ctx, spec.Path, spec.Args...)
}

// ResolveBinary locates tmux and returns a recognizable error when an explicit
// tmux backend cannot be satisfied. It is also used by backend:auto selection.
func ResolveBinary(runner CommandRunner, configuredPath string) (string, error) {
	if runner == nil {
		runner = execRunner{}
	}
	candidate := configuredPath
	if candidate == "" {
		candidate = "tmux"
	}
	resolved, err := runner.LookPath(candidate)
	if err != nil {
		return "", fmt.Errorf("%w: %w: %s", terminal.ErrUnavailable, ErrTmuxNotFound, candidate)
	}
	return resolved, nil
}

func (m *Manager) run(ctx context.Context, args ...string) ([]byte, error) {
	bounded, cancel := boundedCommandContext(ctx)
	defer cancel()
	output, err := m.runner.Run(bounded, CommandSpec{Path: m.tmuxPath, Args: args})
	if err != nil {
		return output, &CommandError{Operation: commandOperation(args), Err: err}
	}
	return output, nil
}

func (m *Manager) runInput(ctx context.Context, input []byte, args ...string) ([]byte, error) {
	bounded, cancel := boundedCommandContext(ctx)
	defer cancel()
	output, err := m.runner.Run(bounded, CommandSpec{
		Path:  m.tmuxPath,
		Args:  args,
		Stdin: append([]byte(nil), input...),
	})
	if err != nil {
		return output, &CommandError{Operation: commandOperation(args), Err: err}
	}
	return output, nil
}

func boundedCommandContext(ctx context.Context) (context.Context, context.CancelFunc) {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithTimeout(ctx, commandTimeout)
}

// CommandError identifies the failed tmux operation without echoing argv,
// which may contain paths or other private metadata.
type CommandError struct {
	Operation string
	Err       error
}

func (e *CommandError) Error() string {
	return fmt.Sprintf("tmux %s: %v", e.Operation, e.Err)
}

func (e *CommandError) Unwrap() error { return e.Err }

func commandOperation(args []string) string {
	if len(args) == 0 || args[0] == "" {
		return "command"
	}
	return args[0]
}
