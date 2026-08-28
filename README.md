# Relayer

**A local human-approval and supervision layer for AI CLI agents.**

[![Go Version](https://img.shields.io/github/go-mod/go-version/Hocsman/Relayer)](https://go.dev/)
[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Build](https://github.com/Hocsman/Relayer/actions/workflows/build.yml/badge.svg)](https://github.com/Hocsman/Relayer/actions/workflows/build.yml)

> [!WARNING]
> Relayer is alpha software. Configuration, adapter, backend, and audit APIs may
> change without compatibility guarantees. Use it with disposable work first,
> review every proposed action, and keep independent backups.

Relayer starts one to eight interactive command-line agents in local terminal
sessions, shows their output in a Bubble Tea TUI or an optional alpha Wails
desktop GUI, and brings detected confirmation, permission, or credential prompts to a human
supervisor. It can use directly owned PTYs or detached, Relayer-owned tmux
sessions.

No direct per-token API integration is required. Relayer works with your existing CLI tools, subscriptions and local models.

Relayer does not provide, proxy, or alter access to any AI service. The CLI
tools you launch retain their own authentication, billing, usage limits, terms,
and network behavior.

## What works today

- One to eight agents, with up to four visible per page.
- Exact argument-vector commands, or explicitly requested `/bin/sh -c` shell
  commands on supported Unix systems.
- PTY, tmux, automatic tmux-to-PTY selection, and mixed concrete backends.
- A bounded terminal-output view and bounded streaming prompt detection.
- A stable, product-neutral `generic` regex adapter.
- Experimental, fixture-backed Claude Code and Codex CLI adapters with the
  stable generic detector retained as fallback.
- First-match approval policies with conservative handling of credentials,
  sensitive events, high or unknown risk, and dry runs.
- Optional local JSONL audit records with rotation, restrictive Unix
  permissions, bounded fields, and mandatory redaction.
- Two deterministic Bash mock agents when `agents: []` is configured.
- An optional source-built Wails desktop GUI for macOS and Linux; the TUI
  remains fully available. Its local agent picker can prepare one to eight
  Claude Code, Codex CLI, MiMo Code, Ollama / DeepSeek, or custom CLI
  launch profiles, then start, stop, or generation-safely restart them without
  closing the application.

Relayer is not a sandbox, a policy enforcement boundary, a terminal emulator,
or a substitute for reviewing an agent's work. See the
[security model](docs/security-model.md) before using it on valuable data.

## Platform status

| Platform | Alpha status | Notes |
| --- | --- | --- |
| Linux | Supported (CI); GUI alpha | PTY backend; tmux backend when tmux is installed. |
| macOS | Supported (CI); GUI alpha | PTY backend; tmux backend when tmux is installed. |
| Windows, native | Configuration only | The GUI can edit profiles but refuses agent execution until a tested ConPTY backend exists. |
| WSL | Not validated | No support guarantee during alpha. |

## Prerequisites

- Go 1.25.8 or newer to build from source. The patch-level minimum keeps
  release binaries on a standard library version covered by the vulnerability
  gate.
- A UTF-8 interactive terminal.
- Bash for the bundled mock agents and the reproducible demo.
- tmux only when selecting `tmux` or when you want `auto` to choose it.
- The agent CLIs you configure, installed and authenticated independently.

## Install

### Build from source

```bash
git clone https://github.com/Hocsman/Relayer.git
cd Relayer
go build -o relayer ./cmd/relayer
./relayer --version
```

The root entry point remains available for compatibility:

```bash
go build -o relayer main.go
```

Development builds report `relayer dev (commit unknown)` unless build metadata
is injected.

### Desktop GUI (alpha)

The optional GUI uses Wails v2.14.0 and is currently a source build for macOS
and Linux. It does not provide functional Windows agent execution, an
installer, or a published desktop release.

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.14.0
cd cmd/relayer-gui
wails doctor
wails dev       # development window
wails build     # local development artifact below build/bin/
```

By default the GUI loads
`os.UserConfigDir()/relayer/config.yaml`; set `RELAYER_CONFIG` to use another
path. Applications opened from Finder may not inherit the Homebrew paths used
by an interactive shell, so use absolute executable paths or launch the app
with an explicit `PATH` when required.

Agent panels display bounded, ANSI-stripped text snapshots, not a full VT/ANSI
terminal. The Bubble Tea TUI and its native tmux attach workflow are preserved.
Use **Agents** in the top bar to configure exact argv, working directory, and
backend. Existing argv values are never sent to the WebView: replacing a
command requires re-entering its complete vector. Newly entered argv, including
an explicit Ollama model identifier, is persisted in the local YAML; Relayer
never infers a model, and secret-shaped-argument filtering remains heuristic.
The GUI opens idle and does
not launch a process until you choose **Enregistrer et démarrer**. While a run
is active, **Enregistrer** changes only the YAML; **Enregistrer et redémarrer**
applies it through a guarded lifecycle transaction. Historical configuration
shapes are shown read-only until migrated to `version: 1`.

Each desktop run has a new opaque `runID`. Session mutations and emitted events
carry that identity, so a delayed result from a stopped generation cannot act
on a replacement run. Before a GUI stop or restart, Relayer stops admitting
new decisions and begins a strict PTY/tmux stop so blocked I/O can return. It
then drains every already-admitted mutation and its terminal audit outcome
before closing the run. This explicit lifecycle stop overrides
`sessions.persist_on_exit`; ordinary application shutdown keeps the configured
persistence behavior.

A restart publishes and preflights the candidate configuration before stopping
the active run. If candidate startup then fails, Relayer atomically restores
the exact previous YAML when the candidate revision is still current, then
attempts to launch the previous immutable plan as a fresh run. A concurrent
Relayer writer is protected by the revision lock. An editor that does not use
that lock remains best-effort because portable filesystems do not provide an
atomic compare-and-swap for file contents. If cleanup or restoration is
uncertain, the GUI enters a failed, fail-closed state and does not start
another run.

See the [desktop GUI guide](docs/gui.md) for prerequisites, configuration,
build commands, platform status, and rendering limitations.

### Releases

There is no published release at the time of writing. After an authorized tag
has been published, release archives and checksums may appear on the
[Releases page](https://github.com/Hocsman/Relayer/releases); until then, that
page may be empty. Do not treat an unreviewed third-party binary as an official
Relayer release.

Once an alpha release such as `v0.1.0-alpha` actually exists, select a published
`OS` (`linux` or `darwin`) and `ARCH` (`amd64` or `arm64`), then download and
verify the matching archive:

```bash
VERSION=0.1.0-alpha
OS=linux
ARCH=amd64
ARCHIVE="relayer_${VERSION}_${OS}_${ARCH}.tar.gz"
BASE_URL="https://github.com/Hocsman/Relayer/releases/download/v${VERSION}"

curl -fLO "${BASE_URL}/${ARCHIVE}"
curl -fLO "${BASE_URL}/relayer_${VERSION}_checksums.txt"
grep "  ${ARCHIVE}$" "relayer_${VERSION}_checksums.txt" | sha256sum -c -
tar -xzf "${ARCHIVE}"
"./relayer_${VERSION}_${OS}_${ARCH}/relayer" --version
```

On macOS, replace the verification command with:

```bash
grep "  ${ARCHIVE}$" "relayer_${VERSION}_checksums.txt" | shasum -a 256 -c -
```

Compare the reported version with the authorized tag before placing the binary
on your `PATH`. A checksum downloaded from the same release detects corruption;
it is not a cryptographic signature or independent publisher authentication.

## Quick start with safe mocks

On first launch, Relayer creates `config.yaml` without overwriting an existing
file. The generated `agents: []` activates two synthetic Bash agents:

```bash
./relayer
```

Each mock prints 20 progress lines, asks `Overwrite file? [Y/n]`, waits for a
human answer, and displays that answer. It does not call Claude, Codex, Ollama,
or another remote service.

Use another configuration path with:

```bash
./relayer --config ./examples/local.yaml
```

The old `--pane1` and `--pane2` flags still override the first two configured
agents, but they are deprecated. Their values are tokenized into an argument
vector; shell operators, variable expansion, globbing, pipes, substitutions,
and redirections are not interpreted.

## TUI controls

| Key or input | Action |
| --- | --- |
| `Ctrl+Left`, `Ctrl+Right` | Move focus between agents and the supervisor. |
| `Ctrl+PageUp`, `Ctrl+PageDown` | Move between pages of agents. |
| `Up`, `Down`, `PageUp`, `PageDown` | Scroll the focused viewport. |
| Mouse wheel | Scroll the viewport under the pointer. |
| Left click | Select an agent or the supervisor. |
| `Enter` on a pending prompt | Send the supervisor input to that agent. |
| `Enter` on an idle tmux agent | Attach the native tmux client. |
| `Ctrl+B`, then `d` | Default tmux detach sequence; custom tmux bindings may differ. |
| `Ctrl+C` | Stop supervision and begin backend shutdown. |

When a prompt is pending, Relayer highlights the pane and focuses the
supervisor. Credential and sensitive inputs are masked in the TUI. Masking does
not prevent the target program from echoing the value into its own terminal or
tmux scrollback.

The in-TUI viewport is a bounded text view, not a full VT emulator. Use native
tmux attach for full-screen interactive applications.

## Configuration

Version 1 configuration is strict YAML: unknown fields, aliases, merge keys,
multiple documents, and incorrect scalar types are rejected before any backend
starts. The following example shows every top-level section:

```yaml
version: 1
backend: auto # pty, tmux, or auto

sessions:
  persist_on_exit: false
  cleanup_on_success: true

policies:
  default_action: ask
  dry_run: false
  rules:
    - name: ask-reviewer-confirmations
      match:
        event_types: [confirmation]
        agent_ids: [reviewer]
        risk_levels: [unknown]
        sensitive: false
        text_regex: '(?i)continue'
      action: ask

audit:
  enabled: true
  mode: metadata # off, metadata, or detailed
  path: ""       # empty selects the private per-user default
  max_file_size_mb: 10
  max_files: 5

agents:
  - id: builder
    name: Builder
    command: ["claude"]
    cwd: .
    env:
      RELAYER_ROLE: builder
    adapter: generic
    backend: pty

  - id: reviewer
    name: Local reviewer
    command: ["ollama", "run", "llama3.2"]
    adapter: generic
    backend: tmux

  - id: scripted
    name: Explicit shell example
    shell: 'printf "ready\\n"; exec ./local-agent'
    adapter: generic
    backend: auto

intercept_patterns:
  - pattern: '(?i)overwrite.*\[y/n\]'
    description: overwrite confirmation
  - pattern: '(?im)password:[[:space:]]*$'
    description: credential prompt
```

Important configuration behavior:

- `command` is an exact argument vector and does not invoke a shell. Prefer it.
- `shell` is mutually exclusive with `command` and explicitly invokes
  `/bin/sh -c` on supported Unix systems. Treat shell text as code.
- Relative `cwd` and audit paths are resolved from the configuration file's
  directory. An agent working directory must already exist.
- Agent environment entries override the inherited process environment. Avoid
  putting credentials in YAML: generated configuration files use mode `0644`.
- A blank per-agent backend inherits the global backend. `auto` chooses tmux
  when its executable is found and otherwise falls back to PTY with a visible
  warning. An explicit unavailable `tmux` backend is an error before startup.
- `persist_on_exit` concerns detached tmux sessions during ordinary application
  shutdown. PTY sessions remain owned by the Relayer process. An explicit GUI
  **Arrêter le run** or restart strictly stops both PTY and owned tmux sessions,
  regardless of this setting. Relayer never kills the tmux server.
- `cleanup_on_success` removes a successful Relayer-owned tmux session even
  when persistence is enabled.
- `agents: []` means the two mocks; otherwise one to eight agents are accepted.

See [configuration](docs/configuration.md) for validation, inheritance,
backends, deprecated flags, policies, and legacy pattern-only files.

## Prompt detection and decisions

The generic adapter strips ANSI sequences, handles fragmented output and
carriage-return rewrites, and tests the active prompt line against ordered
regular expressions. It suppresses common quotation, code-fence, table,
history, and old-log shapes to reduce false positives. Regex interception is
still heuristic: it can miss a prompt or be tricked by output that resembles
one.

Policies use first-match order. Match fields are combined with AND, while
values inside one list use OR. Conservative invariants always win:

- credentials and sensitive events require a human;
- automatic `allow` requires explicit `low` risk; `unknown` or `high` risk
  cannot be auto-allowed;
- a matched `deny` may be automatic for an otherwise valid, non-sensitive
  confirmation, including at `unknown` or `high` risk;
- invalid, incomplete, or non-actionable events ask;
- dry-run mode records the proposal but asks instead of delivering it;
- if an adapter cannot encode an automatic decision, Relayer asks instead.

The current generic adapter encodes manual supervisor input only. Consequently,
an `allow` or `deny` policy evaluated against a generic prompt falls back to a
human ask. `deny` means an adapter-defined refusal, not process termination.

Three adapters are implemented: stable `generic`, plus version-specific
experimental `claude` and `codex`. Claude Code coverage is limited to the
workspace-trust and detected-environment-key prompts observed with 2.1.59;
Codex coverage is limited to directory trust and command approval observed
with `codex-cli 0.148.0-alpha.21`. Every other prompt still uses the configured
`intercept_patterns` fallback. See [adapters](docs/adapters.md) for the exact
decision bytes and non-claims.

## Audit log

Newly generated configuration enables local `metadata` auditing. Configurations
created before the audit block existed and legacy pattern-only configurations
remain disabled for compatibility.

The audit is JSONL and records Relayer lifecycle, event, policy, delivery,
attach, and cleanup metadata. It never has fields for raw terminal output,
commands, environment values, manual input, encoded decision bytes, or raw
errors. Detailed summaries are bounded and redacted. Sensitive events use a
constant summary and omit derivative event IDs.

On Unix, the dedicated audit directory and files are checked for restrictive
ownership, type, and permissions. Writes are synchronized line by line, and
files rotate within configured bounds. Audit failure is fail-closed for startup
and further decision delivery, but the audit is not signed and redaction is not
a data-loss-prevention guarantee.

See [audit logging](docs/audit.md) for the schema, default path, retention,
failure behavior, and confidentiality limits.

## Architecture and security

Relayer separates configuration and validation, adapter event processing,
policy evaluation, audit recording, terminal backends, and the TUI. Sessions
communicate through typed events; terminal output, prompt windows, supervisor
logs, and queues are bounded. Startup validates all plans and initializes the
audit before launching an agent, and partial startup is rolled back.

The tmux backend creates one marked session per agent and checks immutable
ownership metadata before cleanup. Runtime launch files and FIFOs are private,
but a process still runs with the current user's authority. Native tmux attach
temporarily leaves the TUI and is outside policy interception until Relayer
resynchronizes after detach.

Read [architecture](docs/architecture.md), the [security model](docs/security-model.md),
and [SECURITY.md](SECURITY.md) before using Relayer with untrusted commands or
sensitive repositories.

## Limits worth knowing

- An agent may act before emitting a detectable prompt.
- Prompt-like output can spoof the supervisor; a real prompt can evade regexes.
- Generic and Claude cannot automate allow/deny delivery; Codex automation is
  limited to the exact fixture-backed interactions documented above.
- Terminal rendering is intentionally bounded and not a complete emulator.
- tmux persistence can intentionally leave processes running after Relayer
  exits; inspect them with `tmux list-sessions`.
- Cancellation of an already blocked PTY input write relies on session
  `Stop`/`Close` closing the PTY descriptor; a request context alone cannot yet
  interrupt that in-flight Unix `write`.
- Separate Relayer processes do not coordinate rotation of one shared audit
  path.
- Configuration files and command-line arguments are not secret stores.
- WSL has not been validated, and native Windows agent execution is
  unsupported.

See [troubleshooting](docs/troubleshooting.md) for startup, tmux, prompt,
rendering, persistence, and audit diagnostics.

## Development and contribution

```bash
go test -race ./...
go vet ./...
go build ./cmd/relayer
```

Contributions are welcome, especially product-neutral prompt fixtures, backend
lifecycle tests, accessibility improvements, and documentation that narrows
ambiguous security claims. Read [CONTRIBUTING.md](CONTRIBUTING.md) first. Report
security issues using the private process in [SECURITY.md](SECURITY.md), not a
public issue containing secrets.

The reproducible [`docs/demo.tape`](docs/demo.tape) exercises only bundled mocks
and tmux; it does not reference a pre-rendered image or vendor transcript.

## License

Relayer is distributed under the [MIT License](LICENSE).
