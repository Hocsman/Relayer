# Security model

Relayer's goal is to improve human visibility and provide an approval handoff
for interactive CLI prompts. It does not make an agent safe, constrain what it
can do, or prove that a displayed prompt is authentic.

This document describes the current alpha threat model and residual risk. For
private vulnerability reporting, see [SECURITY.md](../SECURITY.md).

## Assets

Depending on the configured CLI, relevant assets can include:

- source repositories and other files available to the current user;
- local credentials, agent subscriptions, tokens, and environment variables;
- network access and services reachable by the agent;
- human decisions and manually entered secrets;
- tmux sessions intentionally left running;
- local audit records and their metadata;
- unrelated processes and tmux sessions owned by the same user.

## Trust boundaries

### Configuration

Relayer trusts the selected configuration to name programs that the user
intends to execute. Strict YAML validation prevents ambiguous schema input; it
does not establish that a command, working directory, shell script, regex, or
environment value is safe.

Configuration is executable control data. Anyone who can modify it can change
which program runs, select explicit shell execution, alter prompt detection,
change persistence, disable auditing, or change policy rules.

### Agent process

An agent is outside Relayer's trust boundary. It runs with the same operating-
system authority as Relayer, subject only to controls independently applied by
the user or platform. It may:

- read or modify accessible files;
- use inherited credentials and network access;
- spawn children or detach work;
- act before printing a prompt;
- omit, rewrite, flood, or spoof terminal output;
- echo human input into output or tmux scrollback.

Relayer observes terminal bytes; it does not mediate system calls.

### Terminal output and adapter

All terminal output is untrusted. Regex detection is a heuristic classification
of current normalized text, not an authenticated protocol. Output that looks
like a confirmation can cause a false positive. Quoted examples, history, or
unusual rendering can still cause false positives or false negatives despite
the generic adapter's filters.

Event IDs and signatures prevent known replay and cross-occurrence mistakes in
Relayer state. They do not cryptographically authenticate an agent or prompt.

### Human supervisor

The human is the final authority for `ask` events but can be misled by agent
output or incomplete context. Pane highlighting and session IDs reduce routing
mistakes; they do not prove what an action will do.

Sensitive TUI input is masked on screen. It exists in process memory and is
delivered to the selected terminal. The target can echo, retain, transmit, or
act on it. Never assume masking is end-to-end secrecy.

Ordinary operator lines are distinct from prompt decisions. Relayer accepts
only bounded printable UTF-8, never records the value or its length, and checks
atomically that no detected event is pending before writing. This does not
cover a prompt the target emitted but Relayer has not read yet, and it does not
turn ordinary input into a policy-controlled action.

### tmux server

tmux is a separate local process and trust domain under the same user. An old
server can outlive Relayer. Relayer snapshots its own launch environment rather
than trusting a stale server environment, but the same user or a compromised
tmux process can still inspect panes and sessions.

Native attach transfers interaction to the tmux client. During attach, input is
direct and does not pass through Relayer's adapter, policy, manual-delivery, or
audit decision path. Resynchronization after detach reduces stale state; it
cannot retroactively govern actions taken while attached.

### Audit storage

Audit files protect against accidental broad local readability through Unix
ownership, type, symlink, and permission checks. They are not signed,
encrypted, remote, append-only at the filesystem level, or protected from the
same user, root, administrators, malware, backups, and snapshots.

## Core controls

### Validate before launch

Versioned configuration uses strict types and fields and rejects YAML aliases,
merge keys, multiple documents, invalid regexes, invalid environment names,
invalid working directories, duplicate agent IDs, unsupported adapters, and
unavailable explicitly requested tmux before launch.

Audit initialization and `run_started` recording also happen before the first
agent starts. A partial multi-agent start is rolled back.

The optional `relayer doctor` command and GUI **Health** panel provide an
earlier, read-only readiness view. They use passive `PATH` lookup and metadata
inspection only: no configuration is created, no audit file is opened, no
backend is constructed, and no provider executable is invoked. Their report
contains only catalogue identifiers, agent ordinals, closed statuses, and
static remediation text; it excludes configured names and IDs, argv,
environment values, full paths, and raw errors. This is the boundary of the
preflight report and its GUI DTO, not a claim about the separate configuration
editor, which must carry the fields it allows the user to edit.

Preflight is advisory, not a security proof. Executable presence does not
authenticate its publisher, validate its version or login state, inspect its
future output, or establish that executing it is safe. The normal startup path
repeats authoritative validation and remains fail-closed independently of a
previous doctor result.

### Exact direct execution

`agents[].command` is an exact argument vector. Relayer does not join it into a
shell string. Arguments containing spaces, quotes, metacharacters, newlines, or
empty strings retain those values.

`agents[].shell` is a deliberate, separate opt-in to `/bin/sh -c`. This does not
sanitize or secure shell code. Prefer direct commands and treat shell
configuration as code review material.

Deprecated pane flags use a limited tokenizer and do not interpret shell
operators. They should not be used as a secret channel: command-line arguments
can be visible to other tools under the same user.

### Bounded untrusted state

Per-session rendered output is retained in a 256 KiB ring. The normalized
detection window is 16 KiB. Supervisor logs retain 200 lines. TUI agent count is
limited to eight, audit metadata and summaries are bounded, audit retention is
bounded, and external tmux close operations use deadlines.

These limits constrain Relayer-owned storage. They do not constrain the target
process, tmux's own history, the operating system, or external files.

### Event occurrence and delivery

Adapter events carry a unique occurrence ID and stable signature. The processor
deduplicates repeated live/snapshot observation of the same occurrence,
acknowledges resolved events, and permits a later identical occurrence to get a
new ID. The TUI keys pending and resolved state by session plus event identity.

Delivery is attempted only for the exact pending session. A stale event,
finished session, unsupported adapter encoding, audit failure, attach
uncertainty, or transport error does not become a successful allow. Relayer does
not retry an uncertain automatic delivery.

Direct `operator_input` uses a separate CAS: it can proceed only when the same
processor state has no actionable event. It never invokes the legacy empty-ID
decision path, never acknowledges a prompt, and is frozen after an ambiguous
transport failure rather than retried.

### Conservative policy invariants

Configuration can propose allow, ask, or deny. Independent invariants apply:

- credentials and sensitive events always require human input;
- automatic allow requires explicit low risk;
- high and unknown risk prevent auto-allow, but a configured deny may remain
  automatic for a valid non-sensitive confirmation;
- invalid, incomplete, and non-actionable events ask;
- dry-run never delivers the proposed automatic action;
- inability to encode the action falls back to ask.

The generic and Claude adapters support manual input only, so their policy
allow and deny proposals cannot be automatically encoded. Codex supports
verified command-approval allow/deny bytes and a selection-independent
directory-trust deny; the directory-trust allow remains human-only. Both
interactions carry unknown or high risk, so policy still blocks automatic
allow; a configured non-sensitive deny can be delivered. Any future
interaction must preserve the same gates.

### tmux ownership and private transport

Relayer names and marks its tmux sessions and retains their immutable tmux IDs.
Before destructive cleanup it verifies the owner marker; malformed or missing
identity does not authorize a kill. Relayer never kills the server or selects a
session merely by a broad prefix.

On Unix, the runtime directory is private and launch specifications and FIFOs
are mode `0600`. The helper removes the spec before releasing the target. Human
input reaches `load-buffer` over standard input, not an argument, then the named
temporary buffer is pasted and removed.

These measures reduce accidental leakage and wrong-session cleanup. They do
not protect against the same user, root, process inspection, a compromised tmux
server, or a target that reads its input.

### Audit minimization and failure gates

The audit model has no fields for raw terminal output, prompt matches, event
signatures, commands, shell scripts, working directories, environment values,
manual decision input, ordinary operator lines, encoded decision bytes, or raw
errors. Metadata is a closed allowlist. Detailed summaries are bounded and
centrally redacted. Sensitive events use a constant summary and omit derivative
IDs.

Audit initialization or the initial record failing aborts before backend
startup. A runtime audit failure prevents further policy or manual delivery in
the affected state and prevents new attach actions. This fail-closed behavior
does not roll back actions the agent already performed and does not make the log
tamper-evident.

## Residual risks

| Risk | Why it remains |
| --- | --- |
| Agent acts without asking | Relayer observes prompts; it does not intercept file or network operations. |
| Prompt spoofing | Terminal text is unauthenticated and controlled by the process. |
| Missed prompt | Regexes and text normalization cannot model every interactive UI. |
| Wrong human decision | The visible context may be incomplete, misleading, or scrolled away. |
| Secret exposure to target | The response must be delivered to the program, which can echo or store it. |
| Secret exposure to local authority | Same-user inspection, root, backups, memory, and tmux are outside protection. |
| Shell injection | Explicit `shell` deliberately delegates parsing to `/bin/sh`. |
| Persistent work after exit | tmux persistence intentionally permits owned processes to outlive supervision. |
| PTY write cancellation | An in-flight blocked PTY write is interrupted by session stop/close, not by its request context alone. |
| Audit disclosure or tampering | Redaction is heuristic and the local file is unsigned and unencrypted. |
| Shared audit rotation races | Separate Relayer processes do not coordinate one audit path. |
| Native attach bypass | Direct tmux input is outside policy and decision auditing. |
| Platform surprises | Native Windows is unsupported; WSL is unvalidated during alpha. |

## Safer operating practices

- Begin with the bundled mocks and disposable repositories.
- Run Relayer and agents under a dedicated, least-privileged operating-system
  account, container, VM, or sandbox when stronger isolation is needed.
- Limit filesystem and network access independently of Relayer.
- Prefer `command` to `shell` and review every argument.
- Keep secrets out of YAML and process arguments; use short-lived credentials
  and an external secret-management flow.
- Use `policies.default_action: ask` and `audit.mode: metadata` while learning a
  CLI's behavior.
- Make intercept regexes narrow and test them against synthetic positive and
  false-positive fixtures.
- Treat the first new adapter release as untrusted until its decision encoding
  is reviewed.
- Leave `persist_on_exit: false` unless detached work is intentional. List and
  inspect tmux sessions after abnormal shutdown.
- Use a distinct audit path for each concurrent Relayer process.
- Protect the configuration file and its parent directory with appropriate
  local permissions.
- Review agent changes and repository state independently after a run.

## Platform boundary

The implemented process, PTY, shell, and tmux paths are Unix-oriented. Linux
and macOS are the supported alpha targets. Native Windows is unsupported. WSL
has not been validated and carries no alpha support guarantee.
