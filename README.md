# 🔄 Relayer

[![Go Version](https://img.shields.io/github/go-mod/go-version/Hocsman/relayer)](https://golang.org/)

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

[![Build Status](https://github.com/Hocsman/relayer/actions/workflows/build.yml/badge.svg)](https://github.com/Hocsman/relayer/actions)

**Relayer** is a lightweight, human-in-the-loop terminal multiplexer built in Go. It allows you to orchestrate multiple interactive AI CLI tools side-by-side using your existing subscriptions—**with zero API costs.**

Stop paying per-token API fees to orchestrate AI agents. Run `claude` (Claude Pro) in one pane and a local `ollama` model in another. When an agent hits a confirmation prompt (like `Overwrite file? [Y/n]`), Relayer intercepts the `stdout`, pauses the automation, and hands the control back to you.

![Relayer Demo](docs/demo.gif) *(Note: Create a GIF using [vhs](https://github.com/charmbracelet/vhs) and place it here)*

## ✨ Features

- **Zero API Costs:** Uses standard CLI interfaces, meaning it leverages your flat-rate subscriptions (Claude Pro, Copilot) or local hardware (Ollama, Llama 3.2).
- **Human-in-the-Loop Interception:** Bounded terminal-output monitoring automatically detects interactive prompts (`[y/N]`, `password:`) and safely pauses the workflow for human input.
- **Deterministic Policies:** Ordered `allow` / `ask` / `deny` rules evaluate detected events with a safe `ask` fallback and an observable dry-run mode.
- **Optional Native tmux Sessions:** Keep the lightweight PTY backend, or run each agent in an isolated detached tmux session and attach to its full terminal on demand.
- **Beautiful TUI:** Powered by the Elm-inspired [Bubble Tea](https://github.com/charmbracelet/bubbletea) framework for a smooth, glitch-free multi-pane terminal experience.
- **Single Binary:** Written in Go. No Python environments, no heavy dependencies. Just download and run.

## 🚀 Quick Start

### Installation

Download the latest compiled binary for macOS or Linux from the [Releases page](https://github.com/Hocsman/Relayer/releases). The current PTY and process-group implementation targets Unix-like systems; native Windows support is not implemented yet.

Or build it from source:

```bash
git clone https://github.com/Hocsman/Relayer.git
cd Relayer
go build -o relayer ./cmd/relayer
```

### Basic Usage

Define your agents in `config.yaml`, then launch Relayer:

```bash
./relayer --config config.yaml
```

Relayer accepts between 1 and 8 configured agents. It displays up to four agents per page: one agent uses the full area, two are placed side-by-side, three use a 2+1 layout, and four use a 2×2 grid. Additional agents are shown on another page.

Terminal controls:

- `Ctrl+Left` / `Ctrl+Right`: move focus across agents and the supervisor, changing page when needed.
- `Ctrl+PageUp` / `Ctrl+PageDown`: move between agent pages.
- `Up` / `Down`, `PageUp` / `PageDown`, or the mouse wheel: scroll the focused pane's retained history.
- `Enter`: send a requested human response to the blocked agent.
- `Enter` on an idle tmux agent: suspend Relayer and open that agent's native interactive session.
- `Ctrl+B`, then `D`: detach from tmux and return to Relayer; output, status, pending prompts, and size are reconciled automatically.
- `Ctrl+C`: stop the sessions and quit.

The `--pane1` and `--pane2` flags are retained for compatibility but are deprecated. When provided, they override configured agents 1 and 2 respectively. Their values are parsed as direct argv—quotes and backslash escaping group arguments, but shell expansion is not performed:

```bash
./relayer --pane1 'claude --model sonnet' --pane2 'ollama run llama3.2'
```

An explicitly empty flag, such as `--pane1=`, selects the built-in mock for that position. If `agents` is empty, Relayer starts two mocks; omitted override flags leave the corresponding configured agent unchanged.

## ⚙️ How it Works (The Architecture)

Relayer exposes one neutral, context-aware terminal backend contract. The built-in PTY implementation keeps the original lightweight behavior. The optional tmux implementation creates one detached, Relayer-owned session per agent. Bubble Tea never constructs tmux commands, and the interception engine never imports tmux-specific code.

```text
config.yaml / CLI compatibility flags
                  │
                  ▼
        backend selector (pty/tmux/auto)
                  │
          ┌───────┴────────┐
          ▼                ▼
     PTY backend      tmux backend
     master fd        detached session + private FIFO
          └───────┬────────┘
                  ▼
               transient raw terminal bytes
                              │
                              ▼
             ANSI sanitizer / CR normalization
                    │                    │
                    ▼                    ▼
       bounded detection window   bounded render Ring Buffer
                    │                    │
                    ▼                    │
          per-session Adapter            │
                    │                    │
                    └──── Event ─────────┘
                              │
                              ▼
                  deterministic policy engine
                    allow / ask / deny
                              │
                              ▼
                  Bubble Tea TUI / supervisor
                              │ Enter on tmux agent
                              ▼
                  tea.ExecProcess(tmux attach-session)
                              │ Ctrl+B, D
                              ▼
                  snapshot + prompt + size resynchronization
```

Detached tmux output is streamed with `pipe-pane` into a private FIFO inside a `0700` runtime directory. Relayer reads that stream continuously—without an aggressive output polling loop—and retains only the configured in-memory Ring Buffer. A low-frequency status probe detects attachment state and process exit; after an interactive detach, only the current pane tail is captured to reconcile a prompt without duplicating scrollback.

Direct agent arguments are never concatenated into a tmux shell command. Relayer writes the exact argv, working directory, and merged environment to a temporary `0600` JSON specification. The generated tmux command contains only internally generated, POSIX-quoted helper paths. The helper decodes and unlinks the specification before the start gate is released, then replaces itself with the requested process. Explicit `shell:` configurations remain intentionally interpreted by `/bin/sh -c`.

The code is split into focused internal packages: `config` owns strict YAML loading, `agent` validates execution specifications, `buffer` bounds retained output, `adapters` turns normalized terminal text into backend-neutral events, `session` exclusively owns PTYs and process lifecycles, and `tui` renders typed session events through a narrow backend interface. `intercept` remains a compatibility facade for the original API; regex detection itself has a single owner, `GenericRegexAdapter`. Unix-specific shell and process-group behavior is isolated in `internal/platform` behind build tags. The root `main.go` remains a compatibility entrypoint; `cmd/relayer` is the canonical command.

Terminal bytes, normalized detection text, rendered viewport history, and audit metadata have separate lifetimes. Raw bytes are processed and discarded, the detection window and viewport history are bounded independently, and semantic events contain only the metadata needed to route a human decision. Sensitive prompt matches and manual input are never added to supervisor logs.

### Agent adapters

- `generic` is the only stable, implemented adapter. It preserves `intercept_patterns`, detects prompts split across chunks or ANSI sequences, classifies credential input, and assigns a stable ID to each occurrence so repeated tmux snapshots, resizes, and attach/resume cycles do not duplicate a prompt.
- `claude` and `codex` are experimental, unimplemented registry placeholders. Explicitly selecting either one fails before a terminal backend starts. When the adapter is omitted, their executable names currently fall back to `generic`; Relayer does not claim tool-specific support.
- Vendor-specific detection rules will only be added together with anonymized, verified terminal fixtures. The current synthetic generic fixtures live in `internal/adapters/testdata/generic`; the Claude and Codex fixture directories contain policy notes only, not fabricated transcripts.

Events describe observations and pending human actions. The policy engine evaluates only those detected occurrences; it cannot retroactively stop an operation that already ran. An automatic decision is delivered only through the adapter that produced the exact pending event. If that adapter cannot encode the decision reliably, Relayer falls back to `ask`.

## 🛠️ Configuration

Schema version 1 configures the terminal backend, session retention, agents, and interception patterns together:

```yaml
version: 1
backend: auto

sessions:
  persist_on_exit: false
  cleanup_on_success: true

policies:
  default_action: ask
  dry_run: false
  rules:
    - name: ask-overwrite
      match:
        event_types: [confirmation]
        text_regex: '(?i)overwrite'
      action: ask

agents:
  - id: claude-backend
    name: Claude Backend
    command:
      - claude
      - --append-system-prompt
      - "Work only in this repository"
    cwd: .
    adapter: generic
    backend: auto
    env:
      RELAYER_PROFILE: "backend"

  - id: local-reviewer
    name: Local Reviewer
    shell: 'echo "Starting local reviewer" && exec ollama run llama3.2'
    cwd: .
    adapter: generic
    backend: tmux
    env:
      OLLAMA_HOST: "http://127.0.0.1:11434"

intercept_patterns:
  - pattern: '(?i)overwrite.*\[y/n\]'
    description: "File overwrite confirmation"
  - pattern: '(?i)do you want to continue'
    description: "Generic CLI pause"
```

The supported policy fields are deliberately limited:

- `default_action`: `allow`, `ask`, or `deny`; omitted configurations safely default to `ask`.
- `dry_run`: boolean. Rules are evaluated and displayed, but every effective action becomes `ask` and no automatic encoding or delivery is attempted.
- `rules`: an ordered list. Every rule requires a unique, non-empty `name`, a non-empty `match`, and an `action`.
- `match.event_types`: one or more implemented actionable types: `confirmation` or `credential`.
- `match.text_regex`: a Go regular expression compiled at startup and applied only to the event summary and matched prompt fragment, not to unbounded terminal history.
- `match.agent_ids`: configured agent IDs, compared case-insensitively and validated before any agent starts.
- `match.risk_levels`: `low`, `unknown`, or `high`.
- `match.sensitive`: a boolean selector. Setting it to `false` never declassifies an event that the adapter marked sensitive or credential-bearing.

Matchers inside one rule use AND semantics; values inside a list use OR semantics. Rules retain YAML order and the first matching rule wins. With no match, `default_action` applies. Sensitive and `credential` events always become `ask`. Automatic `allow` additionally requires an explicit `low` risk event. Invalid configuration stops startup, and runtime uncertainty never becomes an implicit allow. If a transport reports an error after delivery may have begun, Relayer freezes that pane instead of sending a second response that could reach the following prompt.

The bundled `generic` adapter intentionally encodes manual input only. It never invents a `Y`, `N`, or tool-specific refusal, so its automatic `allow` and `deny` evaluations fall back to `ask`. The policy engine and delivery path are tested with explicit encoder adapters, but no Claude or Codex encoder is claimed.

Three valid policy examples are shown below. The `allow` and `deny` examples become automatic only when a future or custom tested adapter emits the matching event and explicitly supports that encoding:

```yaml
policies:
  default_action: ask
  dry_run: false
  rules:
    - name: allow-low-risk-reviewer-check
      match:
        event_types: [confirmation]
        agent_ids: [reviewer]
        risk_levels: [low]
        sensitive: false
        text_regex: '(?i)(go test|npm test|pytest)'
      action: allow

    - name: deny-high-risk-release-confirmation
      match:
        event_types: [confirmation]
        agent_ids: [release]
        risk_levels: [high]
        sensitive: false
      action: deny

    - name: always-ask-for-credentials
      match:
        event_types: [credential]
      action: ask
```

Do not put passwords, tokens, OTP values, response bytes, or other secrets in policy YAML. Rules describe event metadata only.

Each agent must define exactly one execution mode:

- `command` is an exact argv list. Relayer passes every element directly to the executable without an implicit shell, word splitting, variable or command expansion, globbing, pipes, or redirection. For example, an item containing spaces remains one argument.
- `shell` is the explicit alternative for scripts that require shell syntax. It is passed verbatim to the platform shell—currently `/bin/sh -c` on supported Unix systems—so metacharacters such as `&&`, pipes, redirections, variables, and substitutions are interpreted.

A relative `cwd` is resolved from the directory containing the selected configuration file. `env` is merged with Relayer's inherited environment without duplicate keys and agent values take precedence. PTY sessions default `TERM` to `xterm-256color`; tmux sessions preserve the fresh `TERM`, `TMUX`, and `TMUX_PANE` metadata supplied by tmux. Environment values, including sensitive credentials, are never written to Relayer's logs.

Backend selectors are available globally and per agent:

- `pty` preserves the original pseudo-terminal process manager.
- `tmux` requires the `tmux` executable and fails before starting any agent when it is unavailable.
- `auto` selects tmux when it is installed and otherwise falls back to PTY with a visible warning.

Mixed concrete backends are supported in one run. The interface and startup logs show the effective backend and adapter of every agent; `auto` is always resolved before startup. The optional `adapter` key defaults through the registry to the stable `generic` fallback. The names `claude` and `codex` are reserved experimental placeholders and cannot be selected as working adapters yet.

With `persist_on_exit: false`, shutdown destroys only sessions created and still owned by this Relayer run. With `persist_on_exit: true`, unfinished tmux sessions remain after Relayer exits. `cleanup_on_success: true` removes a tmux session after a confirmed zero exit code; failed sessions remain inspectable until the normal ownership policy applies. Relayer never calls `tmux kill-server`.

An empty `agents: []` list activates the two built-in mocks. Agent IDs must be unique, and a version 1 file may define at most eight agents.

Use an alternate file with `./relayer --config path/to/config.yaml`. If the selected file does not exist, Relayer creates a version 1 default without overwriting an existing user file. Legacy pattern-only files remain readable in both forms: a direct YAML list or an `intercept_patterns` wrapper; because they contain no agents, they use the two-mock fallback unless deprecated CLI overrides are supplied.

### Manual tmux smoke test

1. Install tmux and verify `tmux -V` succeeds.
2. Set `backend: tmux`, keep `agents: []`, then run `./relayer --config config.yaml`.
3. Confirm both mocks stream 20 lines and raise the overwrite interception.
4. Answer one prompt from the supervisor and confirm that agent completes.
5. Focus the other tmux pane and press `Enter`; interact with it directly, then press `Ctrl+B`, followed by `D`.
6. Confirm Relayer returns, restores the pane dimensions, refreshes output/status, and does not repeat an already answered prompt.
7. Repeat once with `persist_on_exit: true`; quit Relayer and inspect the remaining `relayer-*` session with `tmux list-sessions`. Remove that test session manually when finished.

### Known limitations

- The supported runtime targets are Linux, macOS, and WSL. Native Windows terminal execution is not implemented.
- Bubble Tea viewports show a sanitized, bounded output stream; they are not VT100 emulators. Full-screen TUIs are used through the tmux attachment path.
- tmux is an optional external dependency and must be installed when `backend: tmux` is selected.
- Relayer monitoring stops after the Relayer process exits, even when tmux sessions are intentionally persisted.

### Policy security model

Relayer can approve or refuse only a prompt that an adapter has detected while the underlying CLI is still waiting for input, and only when that adapter can encode the selected action. A `deny` sends an adapter-defined refusal; it does not kill the process. `ask` is the safe default and remains mandatory for sensitive input.

Relayer cannot block commands, file writes, network requests, or other effects that happened before a prompt was observed. Output-based rules can miss an event, and terminal output can be ambiguous or adversarial. Direct interaction during a native tmux attach is outside policy enforcement. Relayer is therefore not a sandbox, a system firewall, an authorization boundary, or a replacement for OS-level isolation and permissions.

Use `dry_run: true` to review proposed rule matches before enabling automation. The supervisor marks dry-run evaluations clearly and retains only bounded terminal output plus whitelisted policy metadata; it does not log prompt matches, arbitrary event metadata, encoded decision bytes, or manual secret input.

## 🤝 Contributing

Contributions are welcome! Whether it's adding new default regex patterns for popular AI tools, improving the Lipgloss UI, or writing tests.

1. Fork the Project
2. Create your Feature Branch (`git checkout -b feature/AmazingFeature`)
3. Commit your Changes (`git commit -m 'Add some AmazingFeature'`)
4. Push to the Branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request

## 📄 License

Distributed under the MIT License. See `LICENSE` for more information.
