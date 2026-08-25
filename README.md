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

Launch Relayer by specifying the CLI commands you want to run in parallel. For example, assigning backend tasks to Claude Code and relying on a local Llama 3.2 instance for QA:

```bash
./relayer --pane1 "claude" --pane2 "ollama run llama3.2"
```

Use `Ctrl+Left` and `Ctrl+Right` to switch focus between the supervisor input and the agent panes.

## ⚙️ How it Works (The Architecture)

Relayer allocates pseudo-terminals (PTY) for each CLI tool, tricking them into believing they are attached to a real interactive terminal. A background Go engine continuously strips ANSI codes and parses the output using regex patterns. When a blocking pattern is matched, it triggers an event in the Bubble Tea UI, highlighting the pane and awaiting your manual keystroke to relay back to the agent.

The code is split into focused internal packages: `config` owns strict YAML loading, `buffer` bounds retained output, `intercept` detects prompts independently of Bubble Tea, `session` exclusively owns PTYs and process lifecycles, and `tui` renders typed session events through a narrow backend interface. Unix-specific process-group behavior is isolated in `internal/platform` behind build tags. The root `main.go` remains a compatibility entrypoint; `cmd/relayer` is the canonical command.

## 🛠️ Configuration

You can customize interception patterns in the `config.yaml` file to support new CLI tools:

```yaml
intercept_patterns:

- pattern: '(?i)overwrite.*\[y/n\]'

  description: "File overwrite confirmation"
- pattern: '(?i)do you want to continue'

  description: "Generic CLI pause"
```

Use an alternate file with `./relayer --config path/to/config.yaml`. If the selected file does not exist, Relayer creates it with the default patterns without overwriting an existing user file.

## 🤝 Contributing

Contributions are welcome! Whether it's adding new default regex patterns for popular AI tools, improving the Lipgloss UI, or writing tests.

1. Fork the Project
2. Create your Feature Branch (`git checkout -b feature/AmazingFeature`)
3. Commit your Changes (`git commit -m 'Add some AmazingFeature'`)
4. Push to the Branch (`git push origin feature/AmazingFeature`)
5. Open a Pull Request

## 📄 License

Distributed under the MIT License. See `LICENSE` for more information.
