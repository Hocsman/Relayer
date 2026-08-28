package tmuxbackend

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

type probeRunner struct {
	output map[string]string
	fail   map[string]error
	calls  []CommandSpec
}

func newProbeRunner(newSessionOutput string) *probeRunner {
	return &probeRunner{
		output: map[string]string{"new-session": newSessionOutput},
		fail:   map[string]error{},
	}
}

func (r *probeRunner) LookPath(file string) (string, error) { return file, nil }

func (r *probeRunner) Run(_ context.Context, spec CommandSpec) ([]byte, error) {
	r.calls = append(r.calls, spec)
	operation := commandOperation(spec.Args)
	if err := r.fail[operation]; err != nil {
		return nil, err
	}
	return []byte(r.output[operation]), nil
}

func (r *probeRunner) Command(_ context.Context, spec CommandSpec) *exec.Cmd {
	return exec.Command(spec.Path, spec.Args...)
}

func (r *probeRunner) operations() []string {
	operations := make([]string, 0, len(r.calls))
	for _, call := range r.calls {
		operations = append(operations, commandOperation(call.Args))
	}
	return operations
}

func TestProbeAcceptsAParsableIdentity(t *testing.T) {
	runner := newProbeRunner("$0" + tmuxFieldSeparator + "@0" + tmuxFieldSeparator + "%0\n")
	if err := Probe(context.Background(), runner, "/usr/bin/tmux"); err != nil {
		t.Fatalf("Probe = %v, want nil", err)
	}

	operations := runner.operations()
	if len(operations) != 2 || operations[0] != "new-session" || operations[1] != "kill-session" {
		t.Fatalf("probe operations = %v, want new-session then kill-session", operations)
	}
	for _, call := range runner.calls {
		if commandOperation(call.Args) == "kill-server" {
			t.Fatal("probe called kill-server")
		}
		if argumentIndex(call.Args, "-S") < 0 {
			t.Fatalf("probe command %v did not target a private socket", call.Args)
		}
	}
	// The probe must exercise the production format, not a simplified one.
	format := runner.calls[0].Args[argumentIndex(runner.calls[0].Args, "-F")+1]
	if format != tmuxFormat("#{session_id}", "#{window_id}", "#{pane_id}") {
		t.Fatalf("probe format = %q, want the runtime identity format", format)
	}
}

// This is the regression the probe exists for: tmux 3.7 rewrites an unprintable
// separator in rendered format output, so a tmux that is present and starts a
// session can still be unable to serve the protocol. Startup must reject it here
// rather than at the first session start.
func TestProbeRejectsSanitizedFormatOutput(t *testing.T) {
	runner := newProbeRunner("$0_@0_%0\n")
	err := Probe(context.Background(), runner, "/usr/bin/tmux")
	if !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("Probe = %v, want ErrProbeFailed", err)
	}
	// A failed probe still removes its own private session.
	operations := runner.operations()
	if len(operations) == 0 || operations[len(operations)-1] != "kill-session" {
		t.Fatalf("probe operations = %v, want a kill-session cleanup", operations)
	}
}

func TestProbeRejectsAFailedSessionStart(t *testing.T) {
	runner := newProbeRunner("")
	runner.fail["new-session"] = errors.New("tmux: /home/someone/.tmux.conf:3: unknown command")
	err := Probe(context.Background(), runner, "/usr/bin/tmux")
	if !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("Probe = %v, want ErrProbeFailed", err)
	}
	// tmux diagnostics can carry paths and environment detail, and this error
	// reaches startup diagnostics and the operator.
	if strings.Contains(err.Error(), "/home/someone") || strings.Contains(err.Error(), ".tmux.conf") {
		t.Fatalf("probe error leaked a tmux diagnostic: %v", err)
	}
}

func TestProbeRejectsABlankPath(t *testing.T) {
	runner := newProbeRunner("")
	if err := Probe(context.Background(), runner, "   "); !errors.Is(err, ErrProbeFailed) {
		t.Fatalf("Probe = %v, want ErrProbeFailed", err)
	}
	if len(runner.calls) != 0 {
		t.Fatalf("probe ran %d commands for a blank path", len(runner.calls))
	}
}

func TestCommandOperationSkipsGlobalFlags(t *testing.T) {
	for _, test := range []struct {
		args []string
		want string
	}{
		{args: []string{"new-session", "-d"}, want: "new-session"},
		{args: []string{"-S", "/tmp/x.sock", "kill-session", "-t", "x"}, want: "kill-session"},
		{args: []string{"-f", "/dev/null", "-S", "/tmp/x.sock", "list-panes"}, want: "list-panes"},
		{args: []string{"-2", "-u", "display-message", "-p"}, want: "display-message"},
		{args: nil, want: "command"},
		{args: []string{"-S", "/tmp/x.sock"}, want: "command"},
	} {
		if got := commandOperation(test.args); got != test.want {
			t.Fatalf("commandOperation(%v) = %q, want %q", test.args, got, test.want)
		}
	}
}
