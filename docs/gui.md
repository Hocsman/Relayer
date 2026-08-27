# Desktop GUI (alpha)

Relayer includes an optional Wails desktop interface alongside the existing
Bubble Tea TUI. The GUI is an alpha source build: it is not a replacement for
the TUI, and the project does not currently publish a desktop release,
installer, Developer ID-signed application, or notarized package.

## Platform status

| Platform | GUI status | Terminal backend |
| --- | --- | --- |
| macOS | Alpha, build and run from source | Unix PTY; tmux when installed and visible on `PATH` |
| Linux | Alpha, build and run from source | Unix PTY; tmux when installed and visible on `PATH` |
| Windows | UI scaffolding only; agent execution is refused | No native backend until ConPTY is implemented and tested |

Windows support must not be inferred from Wails' ability to create a Windows
window. Relayer's current process and PTY implementations are Unix-specific.
The Windows build deliberately refuses to launch agents; native support
requires a real ConPTY backend and platform tests.

The terminal TUI remains available on supported Unix systems:

```bash
go build -o relayer ./cmd/relayer
./relayer
```

## Build prerequisites

The desktop module currently pins:

- Go 1.25.8 or newer;
- Wails v2.14.0;
- Node.js 20.19 or newer, or Node.js 22.12 or newer;
- npm and the native WebView/build dependencies required by Wails.

Install the matching Wails CLI and inspect the host dependencies before the
first build:

```bash
go install github.com/wailsapp/wails/v2/cmd/wails@v2.14.0
wails doctor
```

On Linux, `wails doctor` reports the WebKitGTK and compiler packages needed by
the current distribution. On macOS, install the Xcode command-line tools when
the doctor reports that they are missing.

## Development and build

The GUI is a separate Go module so desktop-only dependencies do not become
dependencies of the terminal binary:

```bash
cd cmd/relayer-gui
wails dev
```

`wails dev` installs/builds the frontend through the commands declared in
`wails.json` and starts the native development window. The frontend demo is
never selected as an automatic fallback by a packaged build.

Build the application locally with:

```bash
cd cmd/relayer-gui
wails build
```

Wails writes local build artifacts below `cmd/relayer-gui/build/bin/`. On
macOS, Wails may apply a local ad-hoc/self-signature; this is not Developer ID
signing or notarization. No installer or release artifact is produced or
published by this workflow.

The frontend can be checked independently:

```bash
cd cmd/relayer-gui/frontend
npm ci
npm test
npm run build
```

For frontend-only development, the simulated dashboard requires an explicit
development command:

```bash
npm run dev:demo
```

Simulation is restricted to Vite development mode. If the Wails bridge is
missing in a production build, the GUI displays an error instead of silently
running mock data.

## Configuration location

The GUI does not use a `config.yaml` beside the application bundle by default.
It loads:

```text
os.UserConfigDir()/relayer/config.yaml
```

Typical locations are:

- macOS: `~/Library/Application Support/relayer/config.yaml`;
- Linux: `${XDG_CONFIG_HOME}/relayer/config.yaml`, or
  `~/.config/relayer/config.yaml` when `XDG_CONFIG_HOME` is unset.

When the file is missing, the existing strict configuration loader creates the
default configuration with `agents: []`, which activates the two deterministic
mock agents.

Override the path with `RELAYER_CONFIG`. Prefer an absolute path:

```bash
RELAYER_CONFIG="$PWD/config.yaml" wails dev
```

For a built macOS application launched from a terminal:

```bash
RELAYER_CONFIG="$PWD/config.yaml" \
  ./build/bin/Relayer.app/Contents/MacOS/Relayer
```

The same version 1 YAML schema is shared by the GUI and TUI. See
[configuration](configuration.md). Do not put passwords, tokens, or other
secrets in that file.

## Executable discovery and `PATH`

Relayer does not rewrite `PATH`. An application opened from Finder does not
generally receive the same shell initialization as a terminal session. On
macOS, Homebrew commonly installs commands in `/opt/homebrew/bin` on Apple
Silicon and `/usr/local/bin` on Intel. Consequently, `claude`, `ollama`, `tmux`,
or another configured executable may work in a shell but be invisible to an
application opened from Finder.

Use one of these approaches:

1. configure an absolute executable path in the agent's `command` vector;
2. launch the application executable from a terminal with the required `PATH`;
3. configure the desktop environment that starts Relayer with the required
   executable directories.

For example:

```bash
PATH="/opt/homebrew/bin:/usr/local/bin:/usr/bin:/bin" \
  ./build/bin/Relayer.app/Contents/MacOS/Relayer
```

Linux desktop launchers may have the same difference from an interactive
shell. An explicit command path is the least ambiguous option. An explicit
`tmux` backend still requires the `tmux` executable itself to be discoverable
on the GUI process's `PATH`.

## Rendering and interaction limits

The agent panels render complete, bounded text snapshots supplied by the Go
core. Output is ANSI-stripped and carriage returns are normalized before it
reaches the GUI. Each update replaces the previous snapshot; the viewer keeps
auto-follow enabled until the user scrolls upward.

For display safety, the GUI shows only the executable basename for an argv
agent and masks every argument; explicit shell bodies are never sent to the
WebView.

This is deliberately **not** a VT/ANSI terminal emulator:

- ANSI colors and cursor-control sequences are not rendered;
- full-screen terminal applications are not faithfully reproduced;
- the GUI does not expose arbitrary live keystroke passthrough;
- output older than the bounded core buffer is discarded.

The viewer still reports its measured rows and columns to the Go runtime so
the underlying PTY or tmux pane can be resized. A future VT renderer requires a
separate, bounded raw-terminal stream; the current snapshot contract must not
be presented as one.

Use the TUI and its native tmux attach path when a full interactive terminal is
required.

## Human decisions and sensitive input

Only a semantic event detected by the Go core can open a supervisor request.
The GUI submits the session ID and exact event occurrence ID with the manual
value. Sensitive fields use a masked, uncontrolled input that is cleared before
and after delivery and is never added to frontend logs or notifications.

If delivery is uncertain, the GUI disables further input for that occurrence
and asks the user to stop or resynchronize the session. It does not guess that
the decision failed and does not send a second value automatically.

Masking in Relayer does not prevent a target CLI from echoing a secret into its
own output, files, or tmux history. The GUI is a supervision interface, not a
sandbox, secret manager, or operating-system enforcement boundary. Read the
[security model](security-model.md) before supervising untrusted commands.

## Current non-goals

The alpha GUI currently provides no:

- Windows agent execution;
- installer, auto-updater, Developer ID signing, or notarization;
- published desktop release;
- remote audit or synchronization service;
- guarantee of complete prompt detection;
- VT/ANSI emulation.
