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
| Windows | Configuration only; agent execution is refused | No native backend until ConPTY is implemented and tested |

Windows support must not be inferred from Wails' ability to create a Windows
window. Relayer's current process and PTY implementations are Unix-specific.
The Windows build can inspect and save agent profiles, but deliberately refuses
to start or restart them. Native execution requires a real ConPTY backend and
platform tests.

The terminal TUI remains available on supported Unix systems:

```bash
go build -o relayer ./cmd/relayer
./relayer
```

## Build prerequisites

The desktop module currently pins:

- Go 1.25.11 or newer;
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

`wails doctor` checks the Wails build environment. It is distinct from
Relayer's **Santé du système** report, which checks one saved Relayer
configuration without starting a run.

Build the application locally with:

```bash
cd cmd/relayer-gui
wails build
```

On Linux distributions providing WebKitGTK 4.1, including the Ubuntu 24.04 CI
image, use the same build tag exercised by CI:

```bash
wails build -tags webkit2_41
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
mock agents when a run is explicitly started. Opening the GUI alone never
launches them.

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

## Read-only health report

Open **Santé** in the top bar, or **Vérifier l’installation** from the idle
workspace. The panel delegates to the same `internal/app.RunPreflight` facade
as `relayer doctor`; the WebView does not implement its own checks.

The report shows:

- supported platform and configuration/audit metadata;
- passive installation state for the fixed tool catalogue;
- each effective agent by ordinal, including demo/configured source, direct or
  shell mode, executable availability, effective adapter and maturity, and
  concrete backend or unavailable state;
- static checks and remediation for policies, adapters, backend selection,
  audit storage, and configuration readiness.

Opening or refreshing this panel never creates a missing configuration, opens
the audit journal, constructs a backend, or starts a process. Native errors are
replaced by a fixed fail-closed message. The preflight report and its bridge DTO
contain no command, environment value, configured agent identity, full path, or
raw error. This boundary is specific to preflight; the separate agent-settings
workflow necessarily carries the editable configuration fields it displays. A
missing configuration therefore appears as a blocker; create it explicitly
through the agent configuration workflow or a reviewed normal startup.

The panel is not an authentication or compatibility test. It does not execute
provider `--version`, contact a service, validate a login, or certify a binary.
Warnings do not prevent startup, while a blocker means the configuration is not
ready on the inspected host. See [doctor](doctor.md) for the shared contract.

## Choosing local agents

Open **Agents** in the desktop top bar to prepare between one and eight launch
profiles. The built-in catalogue currently provides:

| Profile | Default executable | Detection and decisions |
| --- | --- | --- |
| Claude Code | `claude` | Experimental Claude 2.1.59 rules plus generic fallback; manual decisions only. |
| Codex CLI | `codex` | Experimental Codex 0.148.0-alpha.21 rules plus generic fallback; command allow/deny and directory deny bytes verified. |
| MiMo Code | `mimo` | Generic regex adapter only. |
| Ollama / DeepSeek | `ollama run` plus an explicit model argument | Generic regex adapter only; no model is inferred and no DeepSeek protocol is claimed. |
| Custom CLI | Explicit argv required | Generic regex adapter only. |

The installation badge is a passive `PATH` lookup. It does not execute the
program, validate its publisher, inspect authentication, discover models, or
claim vendor-specific prompt support. An absolute executable path can be used
when the desktop process cannot see the shell's `PATH`.

Each profile controls its display name, stable agent ID, working directory,
backend (`auto`, `pty`, or `tmux`), and exact argument vector. No shell parses
that vector. The exact argv, including a model identifier entered for
`ollama run`, is persisted in the local YAML. The picker has no fields for
environment variables, API keys, passwords, or provider credentials. Configure
authentication through the chosen CLI's own supported flow, and never place a
credential in argv.

DeepSeek is generally a provider or model rather than one canonical local
executable. The combined Ollama / DeepSeek catalogue entry requires the exact
`run` subcommand and a model identifier supplied by the user. Relayer does not
invent a `deepseek` executable, choose a model, or imply a DeepSeek-specific
adapter.

For confidentiality, argv already stored in YAML never crosses into the
WebView. The picker shows only a fixed known executable label (or “commande
personnalisée”) and an argument count. Select **Remplacer la commande** to
enter a complete new argv. Profiles using `shell`, environment overrides, or
unknown advanced adapters remain read-only and are preserved server-side.
Profiles whose historical identifiers cannot be represented by the stricter
form are also preserved read-only. A legacy configuration document must first
be migrated to `version: 1`; the GUI never rewrites it implicitly.

Saving uses an opaque revision token, a per-file lock, and atomic replacement.
Concurrent Relayer writers cannot both publish from the same revision; an
external editor that does not honor Relayer's lock is still best-effort. The
lock wait is bounded, and a stale or post-commit-uncertain save reloads the
authoritative file before another attempt. The Go bridge repeats the
cardinality, length, identifier, adapter, required-argv-prefix, and conservative
secret-shaped-argument validation rather than trusting the WebView. Secret
detection is a heuristic defense in depth, not a guarantee that arbitrary
credentials will be recognized.

## Starting, stopping, and restarting

The desktop control plane always opens in `idle`. It loads configuration for
editing but starts no audit recorder, PTY, tmux session, or agent process until
you explicitly request a run.

The **Agents** panel separates persistence from activation:

- **Enregistrer** atomically updates the YAML only. It never mutates an active
  run.
- From `idle`, **Enregistrer et démarrer** saves, validates, preflights, and
  starts the candidate configuration.
- From `running`, **Enregistrer et redémarrer** performs the same save and
  preflight, asks for confirmation, and replaces the active run. If the YAML is
  already saved, the action is labelled **Redémarrer les agents**.
- **Arrêter le run** returns the application to an editable `idle` state without
  closing the window.

Each successful start receives a new opaque `runID`. Snapshots, supervision
events, status/error events, decisions, resizes, and session-stop requests all
carry that ID. Both Go and the WebView reject data for any run other than the
currently active one, so a late callback from a stopped generation cannot
modify or answer a replacement session that reused the same agent ID.

A restart is a fail-closed transaction:

1. Relayer captures the exact previous YAML bytes and file mode, publishes the
   candidate under the configuration revision lock, and preflights the complete
   candidate before touching the active run.
2. It closes admission for new decisions and session mutations, then begins a
   strict stop of every PTY and every Relayer-owned tmux session. Starting the
   stop first is necessary to unblock a PTY write that may be waiting below
   Go's context boundary.
3. It waits for every already-admitted operation and its terminal audit outcome,
   closes the old run audit, and only then starts the candidate under a new
   `runID`.
4. If candidate startup fails after the old run stopped, Relayer restores the
   exact previous YAML with compare-and-swap and attempts to launch the previous
   immutable plan as another fresh run. The UI reports the `rolled_back`
   outcome; it never presents the failed candidate as active.

The explicit GUI stop/restart path intentionally overrides
`sessions.persist_on_exit`: a tmux session cannot remain alive while a
replacement run with the same agent identity starts. This exception applies to
**Arrêter le run** and GUI restart. An ordinary application shutdown continues
to honor the configured tmux persistence policy, and Relayer never kills the
tmux server.

If the candidate fails during preflight, the active run remains untouched and
Relayer attempts a compare-and-swap restore of the prior YAML. A concurrent
Relayer writer is serialized by the revision lock. If an external edit is
visible at the revision check or immediately after candidate publication,
Relayer preserves it and leaves the existing run active with visible
configuration drift. A non-cooperating editor can still race in the narrow
check-to-rename window because portable filesystems do not offer an atomic
content compare-and-swap. Avoid editing the YAML concurrently with a GUI
save/restart. If the first start from `idle` fails, the YAML is restored and
the GUI returns to `idle`. Once the old run has stopped, any uncertain strict
cleanup, YAML restoration, or rollback startup puts the GUI in `failed` and
starts no replacement process. Close Relayer and inspect local sessions before
retrying from an uncertain cleanup state.

Windows follows the same configuration-save rules, but the start/restart
transaction is refused before any agent execution until ConPTY support exists.

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

## Ordinary operator input

Each running, detached agent card provides a one-line composer for an initial
instruction or other ordinary application text. This is not raw keystroke or
VT passthrough. The Go core accepts at most 4096 UTF-8 bytes, rejects every
control character, and appends exactly one carriage return.

The WebView field is uncontrolled and cleared synchronously before awaiting
the native call. The compose path never copies the line or its length into
React state, bridge status/error events, or the JSONL audit; the audit records
only static `operator_input` attempt and outcome metadata. The target program
can still echo the line into its terminal output, which is intentionally
rendered in the bounded output snapshot.

Input is disabled when the session is attached, exited, waiting for a detected
event, already delivering another mutation, or frozen after uncertainty. The
backend repeats the authoritative check under the same processor lock as event
detection. If a prompt is already pending, zero line bytes are written and the
supervisor event takes priority. A prompt emitted by the CLI but not yet read
by Relayer cannot be protected by that check, so ordinary input remains a
direct human action rather than a policy approval.

If the transport reports an ambiguous failure, the card is frozen and Relayer
does not retry the line. Stop or restart the session instead of sending the
same text again.

## Human decisions and sensitive input

Only a semantic event detected by the Go core can open a supervisor request.
The GUI submits the active run ID, session ID, and exact event occurrence ID
with the manual value. Sensitive fields use a masked, uncontrolled input that
is cleared before and after delivery and is never added to frontend logs or
notifications.

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
