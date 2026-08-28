# Troubleshooting

Relayer diagnostics and most TUI status messages are currently in French. This
page groups failures by lifecycle stage so that a startup validation error is
not confused with a terminal or target-program problem.

## Establish the build and configuration

Check the binary first:

```bash
./relayer --version
./relayer -h
go version
```

A source development build normally prints:

```text
relayer dev (commit unknown)
```

Then use an explicit configuration path while diagnosing:

```bash
./relayer --config /absolute/path/to/config.yaml
```

Relative agent working directories and relative audit paths are resolved from
the configuration file's directory, not necessarily the shell's current
directory.

## The configuration file was created unexpectedly

If the selected path does not exist, Relayer creates a complete version 1 file
atomically. It does not overwrite an existing file. The default path is
`config.yaml` in the current directory.

The generated `agents: []` intentionally starts two Bash mocks. Add one to eight
agent entries to launch real configured commands. If the file appeared in the
wrong directory, stop the mocks with `Ctrl+C` and relaunch with `--config`.

Configuration creation uses mode `0644` on Unix. Do not add secrets to that
file; restrict its permissions yourself if its non-secret contents still
require local confidentiality.

## YAML is rejected

Version 1 is strict. Common causes are:

- a required `version`, `backend`, `agents`, or `intercept_patterns` field is
  absent;
- an unknown field or misspelling is present;
- `true`, `false`, or an integer was quoted and became a string;
- a list or map has the wrong YAML node type;
- `command` and `shell` are both present, or neither is usable;
- a policy matcher list is present but empty;
- a regex does not compile;
- a YAML alias, merge key, or second `---` document is present;
- an agent working directory does not exist;
- two agent IDs differ only by letter case;
- an explicit adapter is unknown or unavailable.

Compare against [configuration.md](configuration.md). Relayer rejects these
conditions before backend construction; changing terminal settings will not
fix them.

## A command works in my shell but not in Relayer

`agents[].command` is not a shell string. Each YAML list item is one exact
argument:

```yaml
command: ["./agent", "--message", "hello world"]
```

This does not work as a pipeline because `|` would be an ordinary argument:

```yaml
command: ["producer", "|", "consumer"]
```

If shell parsing is truly required, opt in explicitly:

```yaml
shell: 'producer | exec consumer'
```

Shell mode uses `/bin/sh`, not necessarily Bash. Bash-specific syntax belongs
in `command: ["bash", "-c", "..."]` or in an executable script with a reviewed
shebang. Avoid shell mode for untrusted strings.

For deprecated `--pane1` and `--pane2`, quotes only group arguments. Operators,
variables, globs, substitutions, pipes, and redirects are deliberately not
interpreted.

## The executable is not found

Relayer inherits its own `PATH`. GUI-launched terminals, IDEs, services, and
login shells can have different environments. Check from the same shell:

```bash
command -v your-agent
```

Use an absolute executable path in `command[0]` if needed. Authentication and
installation of third-party agent CLIs are outside Relayer.

## Bash mock startup fails

The two bundled mocks require `bash`. Install Bash or replace `agents: []` with
configured commands. A shell named only `/bin/sh` is not enough for the mock's
Bash loop syntax.

## tmux was requested but not found

An explicit `backend: tmux` is fail-fast. Verify:

```bash
command -v tmux
tmux -V
```

Install tmux or select `pty`. `auto` is the only selector that falls back to PTY
when tmux cannot be located; it prints the effective choice.

If global `auto` unexpectedly chooses tmux, set either the global or per-agent
backend to `pty`. If one agent chooses tmux while another uses PTY, mixed
backend routing is expected.

## Native attach does not start

Attach is available only for an idle focused tmux agent. It is not started when:

- a human prompt is pending, because Enter submits that prompt;
- the focused pane uses PTY;
- an attach is already active;
- audit state has failed or frozen attach admission;
- the session already exited;
- the tmux client or session cannot be resolved.

Move focus to the agent with `Ctrl+Left` or `Ctrl+Right`, then press Enter. To
detach with the default tmux prefix, press `Ctrl+B`, release it, then press `d`.
If your tmux configuration changes the prefix or detach binding, use that
binding instead.

Relayer freezes decisions for a pane when post-detach resynchronization fails.
That is intentional: it will not guess whether a prompt changed while direct
attach bypassed interception. Inspect tmux and restart supervision rather than
forcing a stale response.

## Terminal output looks incomplete or corrupted

The TUI stores bounded text and is not a complete VT emulator. Alternate screen
buffers, complex cursor addressing, interactive editors, and full-screen CLI
interfaces may not render faithfully. Use native tmux attach when available.

Resize the terminal to at least 30 columns by 10 rows. Relayer recalculates
viewports and sends pane dimensions to backends; a target may still cache its
own layout.

ANSI escape sequences split across reads and carriage-return progress lines are
handled by the processor, but unusual control protocols remain outside scope.
Capture a small synthetic reproducer for a bug report rather than a real private
transcript.

## New output does not jump to the bottom

This is expected after keyboard or mouse scrolling. Relayer preserves the
viewport while the user is reading history. Scroll to the bottom to resume
automatic following.

## A prompt was not detected

Check that:

- the prompt is visible as the active current line, not only old history;
- at least one `intercept_patterns` expression matches normalized text;
- the prompt is not presented inside quotes, a Markdown fence/table, or a line
  beginning `log:`, `previous:`, or `historique:`;
- the target is not relying entirely on unsupported cursor-addressed UI;
- another event for the same session is not already pending.

Patterns use Go regular expressions. Make them narrow enough to avoid matching
documentation but broad enough for whitespace and capitalization differences.
Use synthetic fixtures to exercise chunk splits and ANSI escapes.

After native tmux detach, Relayer reconciles a bounded recent snapshot. Prompts
outside that snapshot cannot be recovered from arbitrary history.

## Text that is not a prompt was detected

Terminal output is unauthenticated. The generic adapter filters common quoted,
fenced, table, and old-log forms but cannot identify intent. Tighten the regex,
keep the policy default at `ask`, and do not automatically trust a highlighted
pane. Review surrounding output and repository state before answering.

## A policy says allow or deny, but Relayer asks

This can be correct for several reasons:

- credentials and sensitive events always ask;
- automatic allow requires explicit `low` risk;
- the event is invalid, incomplete, non-actionable, or no longer pending;
- `policies.dry_run` is true;
- audit delivery is frozen after a write failure;
- the adapter cannot encode the proposed action.

The generic and Claude adapters can encode manual input only. Their allow and
deny proposals therefore fall back to a human ask. Codex automatic encoding is
limited to command-approval allow/deny and the selection-independent
directory-trust deny documented in [adapters](adapters.md). This is not a
policy parser failure. A configured deny is not a request to terminate the
process.

Unknown or high risk specifically prevents automatic allow. It does not by
itself prevent a valid non-sensitive deny proposal, but generic encoding still
cannot deliver that proposal automatically.

## The supervisor input is masked

Credential, sensitive, and high-risk prompts use masked input. The target still
receives the actual value and may echo it. If a prompt was incorrectly marked
sensitive, inspect the pattern expression and description for credential words.
Do not weaken a genuinely sensitive pattern merely to display input.

## Agents remain after `Ctrl+C`

PTY agents are process-owned and should be closed during shutdown. tmux agents
may remain intentionally when:

```yaml
sessions:
  persist_on_exit: true
```

Inspect tmux without broad deletion:

```bash
tmux list-sessions
```

Relayer never calls `kill-server` and refuses to kill a session without its
ownership marker. If shutdown could not prove cleanup, diagnostics and audit
records use conservative unknown/incomplete outcomes rather than claiming the
session is gone.

`cleanup_on_success: true` removes a successful owned session even when
persistence is enabled. Unsuccessful or still-running sessions can persist.

## An audit file is missing

Check all of the following:

- `audit.enabled` is true;
- `audit.mode` is `metadata` or `detailed`, not `off`;
- the configuration actually contains an audit block;
- the default path is under the current user's configuration directory;
- a relative configured path is being resolved from the config directory.

Legacy files and older version 1 files without an audit block are disabled for
compatibility. Newly generated files enable metadata mode.

The effective empty-path locations are usually:

- Linux: `$XDG_CONFIG_HOME/relayer/audit/audit.jsonl` or
  `~/.config/relayer/audit/audit.jsonl`;
- macOS: `~/Library/Application Support/relayer/audit/audit.jsonl`.

See [audit.md](audit.md) for rotation names and schema.

## Audit initialization is rejected

Relayer refuses unsafe audit targets. On Unix, verify that the dedicated audit
directory is owned by the current UID with mode `0700` and files are regular,
owned by the current UID, and mode `0600`. A symlink used as the dedicated
audit directory, active file, or rotated generation is rejected. An unsafe
ancestor directory is not automatically repaired.

Do not point multiple Relayer processes at the same audit path. Their recorders
do not coordinate rotation.

## Audit writes fail during a run

Recorder write failure becomes sticky. Relayer reports an audit failure and
freezes new decisions and attaches instead of continuing with an uncertain
record. Free disk space and repair the storage issue, then restart Relayer; do
not assume an in-flight response was safely delivered or retry it blindly.

A final partial JSONL line from an interrupted append can be recovered by the
file sink on the next open. Complete earlier lines remain independently
decodable.

## `Ctrl+C` appears slow

Shutdown stops admitting work, closes concrete backends, and synchronizes the
audit. tmux operations are externally bounded but may wait briefly for their
shared deadline. Repeatedly killing the terminal can bypass cleanup, especially
for PTY children.

If the process remains stuck, gather a synthetic reproduction and process/tmux
state without credentials. Do not attach raw private terminal output to a public
issue.

## Native Windows or WSL

Native Windows is unsupported: the implemented PTY, process-group, shell, FIFO,
and tmux paths are Unix-specific. WSL has not been validated and has no support
guarantee during alpha. A successful build or one local run in WSL is not proof
of supported lifecycle and cleanup behavior.

## Asking for help

For non-sensitive bugs, include the development/release version, operating
system, backend, sanitized configuration shape, and the smallest synthetic
reproduction. Never publish credentials, environment values, real transcripts,
source code, audit files, or personal paths.

Use the private process in [SECURITY.md](../SECURITY.md) when the behavior can
expose secrets, affect unowned sessions, bypass an audit/policy gate, or leave
unexpected processes running.
