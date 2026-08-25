# 🔄 Relayer

[![Go Version](https://img.shields.io/github/go-mod/go-version/Hocsman/relayer)](https://golang.org/)

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](https://opensource.org/licenses/MIT)

[![Build Status](https://github.com/Hocsman/relayer/actions/workflows/build.yml/badge.svg)](https://github.com/Hocsman/relayer/actions)

**Relayer** is a lightweight, human-in-the-loop terminal multiplexer built in Go. It allows you to orchestrate multiple interactive AI CLI tools side-by-side using your existing subscriptions—**with zero API costs.**

Stop paying per-token API fees to orchestrate AI agents. Run `claude` (Claude Pro) in one pane and a local `ollama` model in another. When an agent hits a confirmation prompt (like `Overwrite file? [Y/n]`), Relayer intercepts the `stdout`, pauses the automation, and hands the control back to you.

![Relayer Demo](docs/demo.gif) *(Note: Create a GIF using [vhs](https://github.com/charmbracelet/vhs) and place it here)*

## ✨ Features

- **Zero API Costs:** Uses standard CLI interfaces, meaning it leverages your flat-rate subscriptions (Claude Pro, Copilot) or local hardware (Ollama, Llama 3.2).
- **Human-in-the-Loop Interception:** PTY-based stdout monitoring automatically detects interactive prompts (`[y/N]`, `password:`) and safely pauses the workflow for human input.
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
- `Ctrl+C`: stop the sessions and quit.

The `--pane1` and `--pane2` flags are retained for compatibility but are deprecated. When provided, they override configured agents 1 and 2 respectively. Their values are parsed as direct argv—quotes and backslash escaping group arguments, but shell expansion is not performed:

```bash
./relayer --pane1 'claude --model sonnet' --pane2 'ollama run llama3.2'
```

An explicitly empty flag, such as `--pane1=`, selects the built-in mock for that position. If `agents` is empty, Relayer starts two mocks; omitted override flags leave the corresponding configured agent unchanged.

## ⚙️ How it Works (The Architecture)

Relayer allocates pseudo-terminals (PTY) for each CLI tool, tricking them into believing they are attached to a real interactive terminal. A background Go engine continuously strips ANSI codes and parses the output using regex patterns. When a blocking pattern is matched, it triggers an event in the Bubble Tea UI, highlighting the pane and awaiting your manual keystroke to relay back to the agent.

The code is split into focused internal packages: `config` owns strict YAML loading, `agent` validates execution specifications, `buffer` bounds retained output, `intercept` detects prompts independently of Bubble Tea, `session` exclusively owns PTYs and process lifecycles, and `tui` renders typed session events through a narrow backend interface. Unix-specific shell and process-group behavior is isolated in `internal/platform` behind build tags. The root `main.go` remains a compatibility entrypoint; `cmd/relayer` is the canonical command.

## 🛠️ Configuration

Schema version 1 configures the PTY backend, agents, and interception patterns together:

```yaml
version: 1
backend: pty

agents:
  - id: claude-backend
    name: Claude Backend
    command:
      - claude
      - --append-system-prompt
      - "Work only in this repository"
    cwd: .
    adapter: generic
    backend: pty
    env:
      RELAYER_PROFILE: "backend"

  - id: local-reviewer
    name: Local Reviewer
    shell: 'echo "Starting local reviewer" && exec ollama run llama3.2'
    cwd: .
    adapter: generic
    backend: pty
    env:
      OLLAMA_HOST: "http://127.0.0.1:11434"

intercept_patterns:
  - pattern: '(?i)overwrite.*\[y/n\]'
    description: "File overwrite confirmation"
  - pattern: '(?i)do you want to continue'
    description: "Generic CLI pause"
```

Each agent must define exactly one execution mode:

- `command` is an exact argv list. Relayer passes every element directly to the executable without an implicit shell, word splitting, variable or command expansion, globbing, pipes, or redirection. For example, an item containing spaces remains one argument.
- `shell` is the explicit alternative for scripts that require shell syntax. It is passed verbatim to the platform shell—currently `/bin/sh -c` on supported Unix systems—so metacharacters such as `&&`, pipes, redirections, variables, and substitutions are interpreted.

A relative `cwd` is resolved from the directory containing the selected configuration file. `env` is merged with Relayer's inherited environment without duplicate keys; agent values take precedence, and `TERM` defaults to `xterm-256color`. Environment values, including sensitive credentials, are never written to Relayer's logs.

The current implementation supports only the `pty` backend and the `generic` adapter, both globally and per agent. An empty `agents: []` list activates the two built-in mocks. Agent IDs must be unique, and a version 1 file may define at most eight agents.

Use an alternate file with `./relayer --config path/to/config.yaml`. If the selected file does not exist, Relayer creates a version 1 default without overwriting an existing user file. Legacy pattern-only files remain readable in both forms: a direct YAML list or an `intercept_patterns` wrapper; because they contain no agents, they use the two-mock fallback unless deprecated CLI overrides are supplied.

## 🤝 Contributing

Contributions are welcome! Whether it's adding new default regex patterns for popular AI tools, improving the Lipgloss UI, or writing tests.

1. Fork the Project
2. Create your Feature Branch (`git checkout -b feature/AmazingFeature`)
3. Commit your Changes (`git commit -m 'Add some AmazingFeature'`)
4. Push to the Branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request

## 📄 License

Distributed under the MIT License. See `LICENSE` for more information.
