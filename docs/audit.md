# Secure local audit log

Relayer can maintain a local JSON Lines audit trail describing lifecycle events and policy decisions. The audit is designed to answer _what Relayer observed and decided_ without recording terminal output, decision bytes, or human input.

For the surrounding trust boundaries and failure model, see the
[security model](security-model.md). Configuration semantics are documented in
[configuration.md](configuration.md), and private vulnerability reports follow
[SECURITY.md](../SECURITY.md).

Each line is one independently decodable JSON object. The current `schema_version` is `1`.


## Relayer owns the audit path

Relayer takes ownership of `audit.path` and of every rotation that shares its
base name (`<path>.1`, `<path>.2`, ...). Startup truncates a partial trailing
line so the journal stays valid JSONL, and rotation removes generations beyond
`max_files`. Point the setting at a dedicated file, never at an existing
document.

Both operations are now gated: a non-empty file is only touched when its first
line decodes as a Relayer entry carrying a known schema version and a kind from
the closed vocabulary. Anything else is refused, startup fails closed, and
`doctor` reports it as a blocker beforehand. A journal interrupted while writing
its very first entry still recovers, because an unterminated first line that
begins like one of our entries is recognized.

Without that gate, `path: notes.txt` in a private home directory truncated
`notes.txt` — to nothing when it contained no newline at all — and deleted
`notes.txt.7`.

## Configuration

```yaml
audit:
  enabled: true
  mode: metadata
  path: ""
  max_file_size_mb: 10
  max_files: 5
```

Supported modes:

- `off`: no recorder, directory, or file is created.
- `metadata`: records lifecycle and decision fields but omits summaries and free-form metadata.
- `detailed`: additionally records bounded summaries and metadata after mandatory redaction.

Set either `enabled: false` or `mode: off` to disable auditing. Version 1 configurations without an `audit` block and legacy pattern-only configurations remain disabled for compatibility. Newly generated configurations explicitly enable `metadata` mode.

`max_files` must be between 1 and 100 and counts the active file and its rotated generations. Thus `max_files: 5` retains `audit.jsonl` plus at most `audit.jsonl.1` through `audit.jsonl.4`.

## Location and permissions

With an empty `path`, Relayer uses the per-user configuration directory returned by Go's `os.UserConfigDir`:

- macOS: usually `~/Library/Application Support/relayer/audit/audit.jsonl`;
- Linux: usually `$XDG_CONFIG_HOME/relayer/audit/audit.jsonl` or `~/.config/relayer/audit/audit.jsonl`.

A relative configured path is resolved from the directory containing `config.yaml`. Relayer creates the dedicated audit directory with mode `0700` and active or rotated files with mode `0600` on Unix, and verifies that they belong to the current UID. A symlink used as the audit directory, active file, or rotated generation, non-regular files, and non-private existing audit directories are rejected.

Every complete line is appended and synchronized before `Record` succeeds. Rotation happens before a new line would cross the configured threshold; a single oversized entry remains intact rather than being split. Writes and rotation are serialized across goroutines, and shutdown synchronizes and closes the file.

If a previous process stopped during an append, reopening the sink truncates an
incomplete final JSONL fragment while preserving complete lines before it. This
is local crash recovery, not tamper detection.

## Recorded fields

Entries may contain:

- `schema_version`, `sequence`, `timestamp`, `entry_id`, and `run_id`;
- `kind` such as `run_started`, `session_started`, `supervision_finished`, `session_finished`, `event_detected`, `policy_evaluated`, `decision`, `delivery`, `operator_input`, `attach_started`, `attach_finished`, `backend_error`, or `session_cleanup`;
- session, agent, backend, and adapter identifiers;
- event ID, implemented event type, and risk level;
- selected rule, decision, actor (`human`, `policy`, or `system`), outcome, and a fixed reason code;
- `sensitive` classification;
- in `detailed` mode only, a bounded redacted `summary` and bounded filtered `metadata`.

Sequences define the order in which the synchronous recorder accepted entries. They provide a total order inside one Relayer run, but do not claim a distributed causal order between agents running concurrently.

`supervision_finished` means the TUI stopped supervising that session. It does not claim that a detached tmux process exited. `session_finished` is emitted only from the canonical `process_exit` event. Cleanup records distinguish a completed backend cleanup, a requested tmux persistence, and an incomplete/unknown aggregate cleanup; they do not claim per-session removal when the backend cannot prove it.

`operator_input` records an attempt and its terminal outcome with session,
agent, backend, adapter, actor, outcome, and a fixed reason code only. It has no
field for the submitted line or its length.

## Fields never recorded

The audit API intentionally has no field for:

- manual decision input, ordinary operator lines, passwords, passphrases,
  tokens, OTPs, PINs, API keys, private keys, or credentials;
- encoded decision bytes or terminal stdin;
- raw terminal output, raw prompt matches, or event signatures;
- commands, shell scripts, working directories, or environment-variable values;
- raw backend errors.

Relayer does not pass either text-input value to the recorder. Errors are
represented by fixed operation/outcome codes rather than `error.Error()`.

## Redaction

Redaction is mandatory in every enabled mode. `detailed` does not bypass it.

The centralized redactor removes or replaces:

- sensitive assignments whose keys contain `password`, `passphrase`, `token`, `bearer`, `authorization`, `api_key`, `api-key`, `secret`, `private_key`, `credential`, `otp`, or `pin`;
- Bearer and Basic authorization values;
- strings shaped like JWTs and common prefixed tokens;
- URL user information and sensitive query parameters or fragments;
- the remainder of the normalized summary after a detected credential label, so newline-separated values and unquoted multi-word passphrases are not partially retained;
- all metadata except a closed allowlist: policy mode/actions/automatic status and process-exit code/failure status.

For `sensitive`, `credential`, or high-risk events, `summary` is the constant `sensitive_event` and metadata is omitted. The audit never records the length of a submitted secret. Maps are copied before serialization so later mutation cannot alter an accepted entry.

Sensitive event IDs are omitted too: generic occurrence IDs are derived from a fingerprint that includes the normalized match, so persisting them could enable offline guessing of a low-entropy OTP or password. Such records remain ordered and correlated by their session and audit sequence without retaining that derivative.

## Anonymized example

This example uses `detailed` mode so the already-generic sensitive summary is visible:

```json
{"schema_version":1,"sequence":1,"timestamp":"2026-08-26T20:00:00Z","entry_id":"entry-example-1","run_id":"run-example","kind":"run_started","decision_by":"system","outcome":"started","sensitive":false}
{"schema_version":1,"sequence":2,"timestamp":"2026-08-26T20:00:01Z","entry_id":"entry-example-2","run_id":"run-example","kind":"event_detected","session_id":"reviewer","agent_id":"reviewer","backend":"pty","adapter":"generic","event_type":"credential","risk":"high","summary":"sensitive_event","sensitive":true,"outcome":"detected"}
{"schema_version":1,"sequence":3,"timestamp":"2026-08-26T20:00:02Z","entry_id":"entry-example-3","run_id":"run-example","kind":"decision","session_id":"reviewer","decision":"ask","decision_by":"human","outcome":"pending","sensitive":true}
```

No value entered by the human appears in the third record.

## Failure behavior

Audit initialization and the initial run record complete before an agent process starts. A failure at that point aborts startup cleanly. During the TUI, an audit failure is shown in the supervisor and prevents further policy or manual delivery for the affected state. A failed audit write never changes a policy result into `allow`, and Relayer never retries an uncertain automatic delivery.

The recorder is local only. There is no remote service or upload.

## Confidentiality limits

Redaction is defense in depth, not a formal data-loss-prevention engine. Previously unknown secret formats may not be recognizable in non-sensitive free-form summaries. Prefer `metadata` mode and ensure adapters mark credentials and sensitive events correctly.

Permissions protect against other ordinary local users but do not protect against the same operating-system account, an administrator, root, malware, backups, disk snapshots, or post-write tampering. The audit is not cryptographically signed and is not an authorization boundary, sandbox, or system firewall.

One recorder serializes all agents inside a Relayer run. Separate Relayer processes do not coordinate rotation with each other; configure distinct paths when running multiple instances concurrently.

Return to the [README](../README.md) or continue with
[troubleshooting](troubleshooting.md).
