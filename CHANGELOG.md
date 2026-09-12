# Changelog

All notable user-visible changes are documented here. This file follows the structure of [Keep a Changelog](https://keepachangelog.com/en/1.1.0/).

## [Unreleased]

## [0.4.0] - 2026-09-12

Minor release introducing granular per-agent process lifecycle controls, a comprehensive typography and UI visual overhaul, and cross-platform CI hardening.

### Added

- **Granular Per-Agent Lifecycle (Stop, Start, Restart)**:
  - Added individual `Stop`, `Restart`, and `Start` controls on each agent terminal card in the Desktop GUI, allowing operators to terminate, reboot in place, or restart an agent without interrupting other running processes.
  - Implemented TUI hot keys `x` (Stop active agent) and `r` (Restart active agent) with two-press safety confirmation and timeout disarm.
  - Introduced `internal/app.agentLifecycle` with fail-closed process ownership, `terminal.SessionRemover` backend release verification, and human operator attribution across the entire cryptographic audit trail.
  - Automated E2E Playwright test (`cmd/relayer-gui/frontend/e2e/supervisor.spec.ts`) validating dynamic agent creation from the catalog, form validation, and per-agent lifecycle operations.
  - Unit and integration tests in Go (`profiles_test.go`, `agent_lifecycle_test.go`) and Vitest (`agentProfiles.test.ts`, `relayerState.test.ts`, `AgentCard.test.tsx`) covering all 8 catalog templates, adapter round-trips, and lifecycle states.

### Changed

- **Visual Comfort & Typography Overhaul**:
  - Scaled up small text and table font sizes across the Desktop GUI (Settings editor, Catalog, Audit Trail tables, Webhooks inventory, Preflight checks, and modal cards) from 7.5–9px to 11–13.5px for increased readability on high-DPI displays.
  - Scaled up typography across the Observability & Metrics panel (KPI cards, Donut chart center and legend, histogram values and labels, security guardrails, and telemetry exporters) from 7.5–10px to 10–24px.
  - Optimized latency histogram bar widths, gaps, and compact interval labels to prevent any text clipping or label collision.
  - Added dedicated catalog logo initials and visual color schemes for Aider (`A`), Goose (`G`), Open Interpreter (`I`), and Ollama (`O`).

### Fixed

- **Cross-Platform Audit Permissions & CI Hardening**:
  - Scoped Unix-specific file permission assertions (`syscall.Stat_t`) under `//go:build unix` build constraints to ensure seamless cross-compilation on Windows.
  - Resolved staticcheck unused parameter warnings and standardized code formatting across internal supervisor packages.

## [0.3.1] - 2026-09-12

Patch release addressing GUI agent creation crashes and adapter lockout behavior.

### Fixed

- **GUI Agent Catalog Creation Crash (`nextProfileID`)**:
  - Resolved an uncaught `TypeError: Cannot read properties of undefined (reading 'trim')` crash when adding Aider, Goose CLI, or Open Interpreter profiles via the GUI catalog (`+`).
  - Added full preset mapping and defensive fallbacks in `nextProfileID` to guarantee valid, non-empty IDs for all built-in and third-party CLIs.
  - Added null-coalescing guards in `validateAgentProfiles` to prevent component unmounting on malformed or incomplete profile inputs.
- **Agent Profile Lockout (`editableProfileAdapter`)**:
  - Registered `toolcatalog.GooseCLI` (`goose`) and `toolcatalog.OpenInterpreter` (`interpreter`) in `editableProfileAdapter` and `safeExecutableLabel`.
  - Prevented saved Goose and Open Interpreter profiles from being erroneously flagged as locked `advanced_adapter`, ensuring they remain fully editable within the visual settings editor.

## [0.3.0] - 2026-09-11

General Availability (GA) release of Relayer, concluding the Alpha phase with enterprise telemetry, multi-channel alerting, visual configuration hot-reload, terminal comfort, and strict TUI/GUI parities.

### Added

- **Visual Settings Editor (Desktop GUI)**:
  - Interactive tabbed configuration: `🤖 Agents`, `🛡️ Security & Guardrails`, and `🔔 Notifications & Webhooks`.
  - Zero-downtime hot-reload: security policies, guardrails, and webhook endpoints are updated without restarting or interrupting active agent processes.
- **Terminal Search (`Ctrl+F`)**:
  - Integrated `@xterm/addon-search` in Desktop GUI agent viewports with match counter, match highlighting, and circular navigation (`Enter` / `Shift+Enter`).
- **Rapid Arbitration Shortcuts**:
  - `Alt+1..8`: Direct focus switch and instant arbitration dialog opening.
  - `Ctrl+Enter`: Quick approval (`Allow`) or direct line send.
  - `Esc`: Quick refusal (`Deny`).
- **TUI Metrics Overlay (`m` / `M`)**:
  - Bubble Tea full-screen overlay displaying session uptime, decision counts, guardrail blocks, and operator reaction latency statistics.
- **Enterprise Observability & Grafana Stack**:
  - Official Grafana dashboard template (`telemetry/grafana/dashboards/relayer-dashboard.json`).
  - Pre-provisioned `docker-compose.telemetry.yml` with Prometheus and Grafana.
- **Multi-Channel Alerts**:
  - Native OS notifications on Windows (Toast), macOS (osascript), and Linux (`notify-send`).
  - Webhooks for Slack (`blocks`), Discord (`embeds`), and generic JSON with adaptive rate limiting.

### Changed

- Promoted Relayer to General Availability (GA) status across CLI and Desktop GUI packages.
- Enhanced Desktop GUI typography with enlarged, comfortable font sizes in tables, badges, and inventory cards.



First pre-release of the 0.3.x cycle: multi-platform desktop distribution, Goose & Open-Interpreter agent adapters, contextual path-based policies, automated Playwright E2E testing, PTY stress endurance, and native OpenTelemetry & Prometheus telemetry export.

### Added

- Multi-platform Desktop GUI release workflow (`.github/workflows/release.yml`):
  - Automated Wails compilation matrix across Windows (x64 standalone .exe), macOS (Universal binary for Intel & Apple Silicon), and Linux (WebKitGTK-based distribution).
  - Cryptographic release signing via Sigstore Cosign keyless signatures (`.sig`, `.pem`) and SHA-256 checksums with build provenance attestations.

- New AI Agent Adapters (`internal/adapters`):
  - Block/Goose CLI adapter (`goose`): intercepts tool execution approvals, file editing confirmations, shell execution requests, and prompt withdrawals.
  - Open Interpreter adapter (`interpreter`): recognizes code execution prompts (Python/shell/JS), package installation requests, and interactive user inputs.
  - Full integration into `relayer doctor` inventory and Desktop GUI agent settings catalog with `StatusExperimental` maturity classification.

- Advanced Policy Engine & Contextual Rules (`internal/policy`):
  - Path-based contextual security rules (`sensitive_paths`, `allowed_paths`): prevents unauthorized access or alteration of critical project and system targets (`.env`, `~/.ssh`, keys).
  - Conditional auto-approval: safely auto-approves recognized read-only commands (`git status`, `npm test`, `cargo check`) while strictly requiring human arbitration for writes, installations, or deletions.
  - Built-in policy profiles: `strict` (zero automated allowances), `developer` (convenient read-only defaults), and `permissive` (low friction for sandboxed execution).

- Playwright E2E Automated Test Suite & PTY Stress Endurance:
  - Full desktop supervisor lifecycle E2E tests (`cmd/relayer-gui/frontend/e2e/supervisor.spec.ts`) validating agent startup, dual terminal streaming, sensitive event interception, interactive modal decisions, and compliance journal inspection.
  - PTY/ConPTY high-throughput endurance and concurrency stress tests (`internal/ptybackend/stress_test.go`) demonstrating 0 MB memory leak over 30,000+ ANSI lines and strictly bounded $\mathcal{O}(1)$ ring buffer memory consumption.

- Native OpenTelemetry & Prometheus Telemetry Engine (`internal/telemetry`):
  - Zero-leakage metric collection via `audit.EntryObserver`: metrics are derived solely from already-sanitized audit records, strictly preventing prompt, argument, or token leaks in metric labels.
  - Embedded Prometheus HTTP exporter (default `:9090/metrics`) serving standard v0.0.4 text format with graceful shutdown.
  - Periodic OpenTelemetry OTLP/HTTP JSON v1 metrics exporter (`/v1/metrics`) with configurable export intervals and authorization headers.
  - Comprehensive operational metrics: active sessions, pending events, total runs, detected events, withdrawn events, decisions applied, guardrail violations, and human reaction latency histograms (`relayer_decision_duration_seconds`).
  - Preflight checks (`telemetry.valid` under `ScopeTelemetry`), doctor command integration, and Desktop GUI metadata exposure.

- Desktop GUI Audit & Compliance Panel: added dedicated Audit modal panel accessible
  from the top bar. The panel presents chronological journal inspection, cryptographic
  and sequence continuity verification via `audit.VerifyJournal`, human vs policy
  decision metrics, an interactive filtered entries table with display-safe metadata
  inspection, and instant report exports to JSON and CSV formats.

### Fixed

- A question the agent takes back stops being asked. Detection was reconciled
  with the rendered screen, but an occurrence already awaiting a decision never
  was: nothing compared it to what the agent still had on screen. So a request
  the agent withdrew by itself — a timeout, a cancellation, an ESC typed in the
  attached view — stayed on offer indefinitely, and the operator's answer was
  delivered into a terminal that had gone back to its prompt: a `y` typed into a
  shell. Every write now reconciles the pending occurrence with the visible
  grid. One that has left it is withdrawn, the pane unblocks, the action queue
  drops it, and the audit records that the supervision gate opened without a
  decision — distinguished from the resync that could already withdraw one. A
  decision arriving after the withdrawal is refused rather than delivered.

  Absence of the question's text is never the evidence, because it is absent for
  reasons that have nothing to do with the agent giving up on it: halfway
  through a repaint larger than one read, after scrolling out of view, or when
  the grid stops joining two wrapped rows. Replacement is the evidence. The
  question must have been seen on the visible grid under this occurrence's
  identity, nothing may have left the grid since, and the row that carried it
  must now hold content that is neither blank, nor the question, nor the same
  line serialised differently. Everything that cannot be proved keeps the
  occurrence pending, which is what happened before; an agent that only appends
  is untouched.

## [0.2.0] - 2026-09-11

Second minor release: native Windows support, interactive desktop terminal emulator, audit CLI, Aider adapter, decision alerts, policy engine guardrails, and virtual screen detection.

### Added

- Native Windows ConPTY backend (`github.com/charmbracelet/x/conpty`). Relayer runs
  interactively on Windows without WSL or remote daemons, driving CLI agents through the
  Windows Pseudo Console API. Process groups, window resizing, input routing, and output
  capture are fully supported.

- Interactive xterm.js terminal emulator in the desktop interface. The plain-text console
  in the Svelte GUI is replaced by `@xterm/xterm` and `@xterm/addon-fit`, providing full
  terminal emulation with faithful ANSI color rendering, cursor positioning, progress
  bars, and alternate screen buffers.

- `relayer audit` CLI inspection tool. Dedicated subcommands inspect and verify Relayer's
  append-only JSONL audit trails:
  - `relayer audit inspect` for formatted, human-readable session playback and event filtering.
  - `relayer audit stats` for aggregated decision metrics and agent breakdown.
  - `relayer audit verify` for cryptographic Ed25519 signature verification across the audit chain.

- Native Aider coding assistant adapter (`internal/adapters/aider.go`). Dedicated pattern
  recognition and response encoding for the Aider AI pair programmer CLI, supporting
  tool execution confirmations, file modification prompts, and command execution approvals.

- Desktop notifications and terminal BEL alerts (`internal/notify`). When an agent prompts
  for human decision, Relayer emits a terminal BEL (`\a`) audio cue and sends a native OS
  desktop notification via `beeep` to alert operators working in background windows. Gracefully
  falls back or silences in headless and quiet environments.

- Policy engine guardrails and rate-limiting (`internal/policy`). Added consecutive
  auto-decision threshold (`max_consecutive_auto_decisions`) forcing human review after
  repeated automated allowances, sliding-window rate limiting (`max_actions_per_minute`)
  to prevent runaway loops, and high-risk destructive command inspection (blocking destructive
  shell commands such as `rm -rf`, `mkfs`, `dd`, and exfiltration patterns).

- Virtual terminal screen model (`internal/screen`). Terminal byte streams are parsed
  through a total escape sequence parser (CSI, OSC, DCS, SOS, PM, APC) into a 2D virtual
  character grid. Prompt detection now operates on the rendered screen rather than raw byte
  chunks, eliminating chunk boundary fragmentation and repaint artifacts.

### Fixed

- Detection on rendered screens: prompt detection evaluates the actual visual grid state,
  eliminating chunk-boundary misses when questions are split across multiple read system
  calls or surrounded by pinned status footers and frames (#29).

- ANSI DoS protection: terminal screen parser strictly bounds escape sequence parameter
  lengths and grid memory allocations to prevent denial-of-service from unbounded or
  malicious byte sequences.

- Question identity by full line: questions are keyed by their complete text line rather
  than partial ambiguous fragments (#30).

- Escape-only writes trigger detection: terminal updates consisting solely of cursor
  motion or styling escape codes without printable text correctly trigger screen
  re-evaluation (#31).

- Code-fence parity tracking: orphaned or unmatched markdown code fences in agent outputs
  no longer confuse multi-line prompt block detection (#32).

- Codex adapter repaint idempotence: answered prompts are preserved across screen repaints,
  preventing duplicate human decision requests when cursor or spinner updates occur (#33).

- Answered question memory retention across partial read chunks (#35) and tmux snapshot
  reconciliations (`ReconcileSnapshot`) (#37).

- Detection burst bounding: detection is constrained to the visible viewport grid,
  preventing ghost prompts from past scrollback lines (#36).

- ECH (Erase Character) and DCH (Delete Character) escape sequences mark screen regions as
  repaints (#38, #44).

- Multi-line question continuation boundary expanded to 16 lines to support large diffs
  and command preview displays (#39).

- A question the operator answered stops suppressing a different one. The memory
  that keeps an answered question from being asked again while it is still
  painted was tied to a row located after the fact, by searching the grid for the
  matched text — which returns the last row carrying it. With a fragment like
  `[y/n]` that is routinely another question, or a line detection had explicitly
  excluded as a candidate, such as one prefixed `log:`. The entry then watched a
  line that never changes, so it never expired and the re-asked question was
  swallowed for good: the operator was never asked and the agent waited forever.
  The row now comes from the render itself, at the moment the question is
  detected, and travels with the occurrence.

- An answered question is no longer re-asked after a scroll region moves. Rows
  were named by an index plus a count of what had scrolled away, and that count
  stood still under a scroll region whose top is the first row, and on the
  alternate screen. Rows that had never moved stopped recognising themselves, the
  memory was released, and the question came back — so the operator answered a
  second time and the keystroke reached an agent that had already acted on the
  first. A row is now named by an identity carried on the row itself, which
  follows the content when scrolling, insertion or deletion move it.

- A frame the agent moves while the operator is deciding no longer re-asks the
  question. The row is taken when the question is detected and the answer comes
  many frames later; an agent that erases and repaints its frame at a different
  height moves the question onto another row without ever taking it down. The
  row of the occurrence awaiting a decision is now kept current, so the memory
  written when the operator answers describes where the question actually is.

- Abandoning a question no longer records it as answered. A snapshot that comes
  back empty, a snapshot with nothing detectable on it, and a process that exited
  all wrote into that memory, leaving an entry describing a screen the caller had
  just declared stale. Nobody decided anything on those paths.

## [0.1.1-alpha] - 2026-08-29

Alpha still: configuration, adapter, backend and audit APIs may change without
compatibility guarantees.

### Fixed

- A prompt is found wherever the read boundary fell. Detection kept a match only
  when it touched the last non-empty line of the accumulated window, so whether
  an agent was supervised depended on where the operating system happened to end
  a read: a question written together with the frame, option list or footer
  beneath it was missed, and the identical bytes split across two reads were
  caught. Measured against this repository's own captured Codex screen — a
  question wrapping over three lines above a choice list and a key hint — the
  generic adapter found nothing in any chunking. Any CLI that paints a prompt
  that way, which is most of them, was supervised by the default adapter in name
  only.
- Codex keeps recording while a prompt is pending. The adapter returned before
  writing the chunk into its detection window, so a second prompt arriving while
  a human was deciding was never in the text at all: nothing could recover it,
  and the request vanished with the agent still waiting on it.
- The desktop interface no longer weights the permissive answer. **Allow** was
  the loudest control in the application — a filled gradient with a glow — while
  **Deny** was a low-opacity tint. In a tool whose purpose is to make a person
  stop and choose, the eye picked the permissive one. Both carry the same weight
  now, and the filled treatment is reserved for the single action a screen is
  asking for.
- The decision modal can scroll. It had `overflow: hidden`, no maximum height,
  and its only scroll rule sat behind a media query the shipped window can never
  reach, so a tall prompt was clipped and took its own controls with it.
- Text contrast meets WCAG. The faint tier measured 2.99:1 against the surfaces
  it actually composites onto and failed at every one of its text call sites;
  all three text tiers were re-spaced to 13.7 / 7.7 / 5.1, measured on the
  rendered page rather than computed on paper.
- Focus indicators are visible again. `outline: none` sat in a base rule rather
  than inside `:focus`, removing the ring in every state, and the settings fields
  replaced it with a shadow at 0.07 alpha. The three dialogs that had no Escape
  handler and no focus trap now share one, which also recovers focus when a
  control is disabled underneath it mid-decision.
- The top bar no longer reports a queue before a run exists, the agent
  catalogue no longer cuts its descriptions mid-word, and a session that has
  exited no longer claims to be under supervision.

### Added

- `internal/screen` renders a terminal byte stream into the grid a person
  actually sees, with a deliberately total escape parser: every CSI, OSC, DCS,
  SOS, PM and APC sequence is recognised and consumed. Detection is not wired to
  it yet — that needs a terminal size the adapters package cannot currently
  reach — so the two repaint cases it fixes are kept in the suite, running and
  skipped, naming what has to land.
- The desktop interface answers with the adapter's own encoding. **Allow** and
  **Deny** appear for the exact occurrences an adapter has verified bytes for,
  probed per event rather than assumed per adapter, and the prompt carries the
  end of the agent's own output so the decision is not made on a one-line
  summary.
- Substituted demo agents are marked as such on screen, and the startup facts
  that used to go only to standard error — which a windowed application does not
  have — reach the supervisor panel.
- The README shows the desktop interface. It animated the terminal one and
  described the other without ever showing it.

### Changed

- The whole interface is English, terminal and desktop, including the
  configuration file Relayer writes for a new install. French remains only where
  it is data rather than prose: the sensitivity patterns matched against what an
  agent prints, which exist to catch French-speaking CLIs.
- staticcheck's ST1005 applies to the desktop module. Its errors were carrying
  the sentence shown to the operator; they are ordinary Go errors now, and the
  interface phrases them where they are displayed.
- `golang.org/x/sys` moves to v0.44.0, clearing the last advisory govulncheck
  reported — unreachable from Relayer's code and on a platform the release does
  not build, but not worth shipping.

## [0.1.0-alpha] - 2026-08-28

First tagged release. Alpha: configuration, adapter, backend and audit APIs may
still change without compatibility guarantees.

### Added

- The supervisor title shows how many agents are waiting once more than one is.
  Only the agent being answered was named and only four are visible per page, so
  a queue building up behind it — on another page — was invisible.

- Release artifacts are signed and attested. The checksum file is signed with
  keyless cosign, so the release workflow's own identity is bound into a
  short-lived certificate and recorded in the public transparency log; build
  provenance is attested separately, and each archive ships an SBOM. A checksum
  published beside the binaries only proves the download was not corrupted, and
  anyone able to write to the release could replace both — which matters here,
  because a tampered `relayer` can approve everything and still write a
  plausible audit log. The README documents `cosign verify-blob`, including the
  identity flags without which any valid signature would pass.
- CI now builds the release archives on every change instead of only validating
  the configuration schema, so the first real run of the release pipeline is no
  longer the publishing one.

- `F2` and `F3` answer a pending prompt with an explicit allow or deny in the
  terminal interface. The adapters already encoded both and the audit already
  modelled a decision made by a person, but no operator surface could ask for
  either, so every human answer went through the free-text field and was
  recorded as an ask — `decision=deny, by=human` was impossible to produce. An
  adapter that cannot represent the answer leaves the prompt pending instead of
  having terminal bytes invented for it; today only the Codex adapter encodes
  them.

- `intercept_patterns` entries accept an optional `sensitive: true`, which masks
  the operator field and forces a human decision. Sensitivity was inferred from
  the pattern text alone, so a prompt worded outside that word list was entered
  unmasked with no way to correct it. The inference also now recognizes
  one-time-code, 2FA/MFA, recovery-code, private-key and seed-phrase wording in
  English and French. Declaring the field can only escalate: `sensitive: false`
  never downgrades an inferred secret.
- `examples/local.yaml`, a worked configuration showing real agents, mixed
  backends, a policy rule and a sensitive pattern. The README referenced this
  path but the file did not exist.

- A Bubble Tea supervisor for one to eight local interactive agents, with
  paging, viewport navigation, mouse scrolling, prompt focus, and bounded logs.
- PTY sessions with process-group ownership, resize propagation, input routing,
  output retention, and shutdown cleanup.
- Relayer-owned tmux sessions with private launch transport, streamed output,
  native attach/resync, explicit persistence policy, and ownership-verified
  cleanup.
- A product-neutral generic regex adapter with ANSI streaming, carriage-return
  handling, active-prompt filtering, occurrence IDs, snapshot reconciliation,
  and bounded state.
- Experimental, version-specific Claude Code and Codex CLI adapters backed by
  anonymized terminal fixtures, with the generic regex adapter retained as a
  compatibility fallback.
- A bounded output-only PTY/tmux fixture capture command with strict JSON,
  centralized secret refusal, private tmux isolation, and offline validation.
- A bounded, single-line operator input path for the TUI and desktop GUI that
  never acknowledges a pending event and never records the submitted text.
- A shared read-only doctor/preflight report for the CLI and desktop GUI, with
  passive executable discovery, effective adapter/backend reporting, static
  remediation, and no configuration, audit, backend, or process side effects.
- A combined Ollama / DeepSeek launch profile that requires the exact `run`
  subcommand and a user-selected model instead of inventing either.
- Strict version 1 YAML for agents, backends, session lifecycle, prompt
  patterns, policies, and audit recording.
- Conservative first-match policy evaluation with allow, ask, deny, dry-run,
  sensitive-event, credential, and risk safeguards.
- A local versioned JSONL audit recorder with metadata and detailed modes,
  bounded redaction, rotation, private Unix storage, lifecycle records, and
  fail-closed delivery behavior.
- Deterministic Bash mock agents and a VHS tape that does not depend on a
  vendor CLI or captured transcript.
- Development build version output through `--version`.

### Changed

- The minimum build toolchain is Go 1.25.13. The desktop module reached two
  vulnerable standard library paths through its Wails dependencies that the root
  module never calls — `crypto/x509`, and `encoding/asn1` under the Linux
  WebKit build tag — so the previous 1.25.8 pin left reachable vulnerabilities
  in the one module CI did not scan.

- Go error strings are in English. Roughly 40% of them were French, so an
  operator got a mix of two languages and could not search for the message they
  hit — a genuine support cost: `identifiants immuables tmux invalides` returns
  nothing anywhere. Format verbs, wrapping and the deliberate vagueness of
  messages that must not leak paths or terminal output are unchanged. The
  desktop application keeps French for now: its interface and its messages are
  one unit, and mixing them would be worse than either.

- The minimum build toolchain is Go 1.25.8 so release binaries include the
  standard-library security fixes enforced by `govulncheck`.
- `--pane1` and `--pane2` remain available as deprecated direct-command
  overrides; versioned YAML is the primary interface.
- Empty version 1 agent lists retain the historical two-mock quick start.
- Legacy direct pattern lists and `intercept_patterns` wrappers remain readable,
  with conservative defaults and auditing disabled for compatibility.

### Fixed

- Two tests reported a genuine failure as a skip. Starting the foreign tmux
  session in the fixture-capture test, and inspecting processes after a rollback
  in the app integration test, both ran an operation and skipped on any error —
  so a regression in the code under test looked like an absent capability. That
  is structurally how the tmux 3.7 format break stayed hidden behind a green
  suite. Both now fail with the underlying error, and `CONTRIBUTING.md` already
  stated the rule they violated.

- An operator on an unsupported platform was told only that agent execution is
  unavailable, never why. The build carried the explanation — a ConPTY backend
  must be implemented and tested first — in a function nothing called, which is
  what a `staticcheck` run on this module reported first.

- A half-composed direct instruction was discarded silently whenever a prompt
  took the shared input field. Preempting it is correct — a supervision request
  outranks a note to an agent — but nothing was logged, so the operator's
  sentence vanished under them with no explanation. The discard is now
  announced; the text itself is still never recorded.

- Pressing Enter on an empty supervisor field answered the prompt. The generic
  adapter encodes a manual decision as the typed text plus a carriage return, so
  an empty submission delivered a bare carriage return — whatever the prompt
  treats as its default, frequently the permissive one — and it was recorded as
  a human decision. Because the field takes focus by itself when a prompt
  arrives, a reflex keystroke could answer a prompt the operator had not read.
  An empty or whitespace-only manual decision is now refused in the core, so
  every interface is covered, and the terminal interface says so instead of
  doing nothing. The desktop interface already refused to submit an empty field.

- Withdrawing a pending occurrence left no trace. Snapshot reconciliation drops
  a prompt the replayed screen no longer shows — legitimate, since an operator
  attached to tmux may have answered it directly — but the pane simply stopped
  being blocked and the journal could not tell "answered" from "stopped being
  asked". A new `event_withdrawn` record names the occurrence, and like every
  other kind it has no field for the matched text.

- A prompt drawn inside an ASCII frame was never detected. The line
  `| Overwrite file? [Y/n]        |` shares its `| ` prefix with a markdown
  table row, which the generic adapter suppresses as quoted documentation, so
  the prompt was dropped at every chunk size with nothing to indicate it. A
  table row separates cells and carries more pipes than the two a frame uses to
  close its sides, and the two are now told apart.
- `docs/adapters.md` claimed that "correctness cannot depend on read
  boundaries". It does: a match must reach the active line, so a question
  followed in the same write by its own frame or option list is missed while the
  identical bytes split across reads are detected. The claim is removed and the
  limit is documented, including which patterns avoid it.

- A prompt that arrived while another one was pending was lost for good.
  Detection stops examining output while an occurrence is pending, so the second
  prompt reached the detection window and nowhere else — and answering the first
  one wiped that window. The second prompt was never evaluated by a policy,
  never audited, and still on the agent's screen, where the operator's next line
  or even their refusal of the first prompt would answer it. Claude Code's
  trust-prompt-then-tool-prompt sequence hits this every time, on both backends;
  there is no periodic reconciliation to recover it. The window is now retained
  and re-examined when the pending occurrence is answered, and dropped when
  nothing unexamined survives.

- A prompt laid out with cursor movement instead of spaces was invisible to
  every configured pattern. Claude Code 2.1.59 emits no literal space in its
  prompts, only `ESC[1C` between words, and those were stripped without
  substitution: the detector matched against `DoyouwanttousethisAPIkey?`, so
  the shipped `intercept_patterns` — and any pattern an operator writes with a
  space in it — could never fire. Deterministic, silent, and on the documented
  compatibility path. Cursor-forward escapes are now expanded to the spaces they
  produce, under a bound, since the column count comes from untrusted output.

- The audit sink took ownership of `audit.path` without checking what was
  already there. Startup truncates a partial trailing line and rotation removes
  generations beyond `max_files`, both purely by name, so pointing the setting
  at an ordinary document destroyed it: a file containing no newline at all was
  truncated to nothing, and neighbours matching `<base>.<n>` were deleted. On a
  private home directory nothing objected. A non-empty file is now only touched
  when its first line decodes as a Relayer entry with a known schema version and
  kind; `doctor` reports a foreign file as a blocker beforehand, and a journal
  interrupted while writing its first entry still recovers.

- The agent update path validated and re-read configuration files with `Load`,
  which creates a default configuration when the path is absent. Used on the
  temporary file of an in-flight save, a file that disappeared between writing
  and validating would be recreated with defaults and then renamed over the
  real configuration, replacing the user's agents, policies and audit settings
  while reporting a successful save. Every update, snapshot, revision and
  desktop profile call site now uses `LoadExisting`; only first-run bootstrap
  may create a file.

- Detection throughput was roughly 0.15 MB/s. The normalized detection window
  was rebuilt one rune at a time with `s.detectionText += ...`, copying the
  whole 16 KiB window per character, so consuming output was quadratic in chunk
  size — and it ran while holding the processor lock, serializing detection,
  operator input, and every snapshot the interfaces poll. A noisy build log
  could saturate a core and fall behind. The window now accumulates into a byte
  buffer: about 142x faster and 1650x less allocation on the same input,
  measured by a new benchmark. A differential test and a fuzz target check the
  rewrite against the original implementation.

- The tmux backend could not start a session on tmux 3.7. Machine-readable
  `-F` and `display-message` formats separated their fields with a tab, and
  tmux 3.7 rewrites an unprintable byte in rendered format output to `_`
  whenever `TMUX` is absent from the environment, which is the normal case for
  a Relayer launched from an ordinary shell. Every identity, ownership and
  snapshot response therefore failed to parse. Those formats now use a
  printable separator, guarded by a unit test that rejects an unprintable one
  on any platform and an integration matrix that pins the wire contract with
  `TMUX` absent, empty, and pointing at a foreign server. The same rewrite also
  broke `relayer-capture` and the tmux fixture capture.

- The `relayer-capture` SIGTERM test consumed the whole `go test` timeout
  instead of reporting a failure. Its single-slot wait channel could be drained
  by an early-exit path, after which the cleanup receive blocked forever. The
  cleanup now receives under a bounded select and the child has a `WaitDelay`,
  so a capture that exits early reports its real error in seconds. The test's
  capture record is also published through an atomic rename instead of a
  truncating write, so the polling reader can no longer observe a partial file
  and fail with a JSON error.

- Startup and `doctor` established tmux availability by finding the executable,
  so a tmux that could not run a session was selected anyway and failed at the
  first session start, after the readiness report had announced a healthy
  backend. Both now run a bounded functional probe: one short-lived session on
  a private socket inside a `0700` temporary directory, its identity parsed by
  the runtime parser, then removed by name. An unusable tmux blocks an
  explicitly requested tmux backend and makes `auto` fall back to PTY, each with
  a message distinct from tmux being absent. The probe never reads, attaches to,
  or modifies the user's tmux server and never calls `kill-server`; the doctor
  documentation records it as the one deliberate exception to passive checking.

### Security

- CI scans the desktop module. It carries the dependency surface — echo,
  gorilla/websocket, `x/net`, `x/crypto` all arrive with Wails — while the
  scanned root module has far fewer, so `staticcheck`, `govulncheck` and the
  `go mod tidy` diff were pointed away from the code that needed them. The
  frontend job additionally audits its locked dependency tree.

- Backend and adapter selection, policy validation, and audit initialization
  happen before agent startup.
- Partial startup and failed session-start audit writes trigger rollback.
- tmux cleanup requires immutable Relayer ownership evidence and never kills
  the tmux server.
- Terminal input values, raw output, commands, environment values, prompt
  matches, and raw errors are excluded from the audit model; direct input
  records retain only static lifecycle metadata.
- Audit storage rejects unsafe leaf symlinks and non-regular targets and checks
  private Unix ownership and permissions.

[Unreleased]: https://github.com/Hocsman/Relayer/compare/v0.4.0...main
[0.4.0]: https://github.com/Hocsman/Relayer/compare/v0.3.1...v0.4.0
[0.3.1]: https://github.com/Hocsman/Relayer/compare/v0.3.0...v0.3.1
[0.3.0]: https://github.com/Hocsman/Relayer/releases/tag/v0.3.0
[0.3.0-alpha.1]: https://github.com/Hocsman/Relayer/compare/v0.2.0...v0.3.0-alpha.1
[0.2.0]: https://github.com/Hocsman/Relayer/compare/v0.1.1-alpha...v0.2.0
[0.1.1-alpha]: https://github.com/Hocsman/Relayer/compare/v0.1.0-alpha...v0.1.1-alpha
[0.1.0-alpha]: https://github.com/Hocsman/Relayer/releases/tag/v0.1.0-alpha
