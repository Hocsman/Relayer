# Changelog

All notable user-visible changes are documented here. Relayer is alpha software
and this file follows the structure of [Keep a Changelog](https://keepachangelog.com/en/1.1.0/)
without implying semantic-versioning stability before the first release.

## [Unreleased]

No release tag has been published at the time of writing.

### Added

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

- The minimum build toolchain is Go 1.25.8 so release binaries include the
  standard-library security fixes enforced by `govulncheck`.
- `--pane1` and `--pane2` remain available as deprecated direct-command
  overrides; versioned YAML is the primary interface.
- Empty version 1 agent lists retain the historical two-mock quick start.
- Legacy direct pattern lists and `intercept_patterns` wrappers remain readable,
  with conservative defaults and auditing disabled for compatibility.

### Fixed

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
