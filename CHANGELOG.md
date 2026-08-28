# Changelog

All notable user-visible changes are documented here. Relayer is alpha software
and this file follows the structure of [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
without implying semantic-versioning stability before the first release.

## [Unreleased]

No release tag has been published at the time of writing.

### Added

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

- The minimum build toolchain is Go 1.25.8 so release binaries include the
  standard-library security fixes enforced by `govulncheck`.
- `--pane1` and `--pane2` remain available as deprecated direct-command
  overrides; versioned YAML is the primary interface.
- Empty version 1 agent lists retain the historical two-mock quick start.
- Legacy direct pattern lists and `intercept_patterns` wrappers remain readable,
  with conservative defaults and auditing disabled for compatibility.

### Fixed

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
  so a capture that exits early reports its real error in seconds.

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

[Unreleased]: https://github.com/Hocsman/Relayer/commits/main
