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


## Withdrawn occurrences

`event_withdrawn` records that an occurrence which was awaiting a human stopped
being pending without a decision being delivered.

Snapshot reconciliation withdraws a pending occurrence when the replayed screen
no longer shows it. That is legitimate — while attached to tmux the operator may
have answered directly, where Relayer cannot see it — but it opens the
supervision gate with nothing recorded, and the journal could not distinguish
"answered" from "stopped being asked".

The record carries the occurrence identity, adapter, event type and risk. Like
every other kind, it has no field for the matched terminal text.

The Desktop GUI and the web gateway journal every withdrawal the adapter
reports, with the reason `agent_withdrew_occurrence`, including that of a
prompt already answered, whose question left the screen once the agent read
its answer. What became of the prompt is told by the entries before it: a
withdrawal with no `decision` entry for the same occurrence is a prompt nobody
answered through Relayer. No `decision` entry follows the withdrawal of the
prompt it would answer; the `delivery` entry of an answer that was being
written when the agent withdrew the prompt does, since the write ends after.

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
- `kind`, from the closed set listed under [which front end writes what](#which-front-end-writes-what);
- session, agent, backend, and adapter identifiers;
- event ID, implemented event type, and risk level;
- selected rule, decision, actor (`human`, `policy`, or `system`), outcome, and a fixed reason code;
- `operator`, the identity of the person who acted, when the front end knows one: the web gateway's named token (`alice` for `alice:s3cret`), or `operator` or `local-operator` without one. The desktop and the TUI have a single operator and name nobody;
- `sensitive` classification;
- in `detailed` mode only, a bounded redacted `summary` and bounded filtered `metadata`.

Sequences define the order in which the synchronous recorder accepted entries. They provide a total order inside one Relayer run, but do not claim a distributed causal order between agents running concurrently.

`supervision_finished` means Relayer stopped supervising that session, when its run ended. It does not claim that a detached tmux process exited. `session_finished` means a process ended, and its reason says how:

- `process_exit`: the session's current process exited, on its own or because it was stopped; the outcome is `failed` when it failed, and in `detailed` mode `metadata` holds its exit code;
- `process_exit_stale` (Desktop GUI and web gateway): a process exited after a replacement had already started, typically the process a Restart stopped. It keeps its real outcome, but it does not end the running session;
- `operator_stop` and `operator_restart`: an operator stopped or restarted the agent, journaled as a person's (`decision_by: human`) with no `operator`, on every front end;
- `restart_stop_confirmed` (Desktop GUI and web gateway): the whole run was stopped for **Stop the run** or **Save and restart**, and the stop was confirmed.

An operator's Stop is usually followed by the stopped process's own `process_exit`, so one process can end with two `session_finished` entries. Cleanup records distinguish a completed backend cleanup, a requested tmux persistence, and an incomplete/unknown aggregate cleanup; they do not claim per-session removal when the backend cannot prove it.

`operator_input` records an attempt and its terminal outcome, as two entries,
with session, agent, backend, adapter, actor, outcome, a fixed reason code and,
on the web gateway, the `operator`, and nothing else. It has no field for the
submitted line or its length.

A person's `decision`, and its `delivery` however it ends, carry the
`operator` and, as metadata, the operator, their token's `role` and the
connection (`conn_id`) the answer came from, so that an answer can be tied to
the terminal hand-overs around it, which carry the same connection. Metadata is
kept only in `detailed` mode: the default `metadata` mode keeps the `operator`
field and drops the role and the connection. The policy's evaluations and
decisions, a prompt's detection and withdrawal, and a process's exit name
nobody.

## Which front end writes what

The TUI, the Desktop GUI and the web gateway write the same vocabulary. The
Desktop GUI and the web gateway share one supervision core, `internal/supervise`,
and write the same entries for the same prompt; the TUI has a state machine of
its own.

| Kind | TUI | Desktop GUI | Web gateway |
| --- | --- | --- | --- |
| `run_started`, `run_finished`, `session_started`, `supervision_finished`, `session_cleanup` | yes | yes | yes |
| `session_finished` | yes | yes | yes; a natural exit only since v0.8.9 |
| `event_detected`, `policy_evaluated` | yes | yes | since v0.8.9 |
| `decision`, `delivery` | yes | yes | yes; the policy's own only since v0.8.9 |
| `event_withdrawn` | yes | yes | since v0.8.9 |
| `operator_input` | yes | yes | yes; before and after the write since v0.8.9 |
| `backend_error` | yes | yes | yes; a stream failure only since v0.8.9 |
| `attach_started`, `attach_finished` | native tmux attach | — | web terminal |
| `control_requested`, `control_granted`, `control_declined`, `control_released`, `control_forced` | — | — | yes; see [sharing.md](sharing.md#audit-trail) |
| `recording_started`, `recording_finished` | — | yes | yes; see [recording.md](recording.md) |
| `recording_exported`, `recording_deleted` | — | — | yes |

Before v0.8.9 the web gateway journaled no detection, evaluation or
withdrawal, no natural exit and no stream failure, and none of the policy's
decisions, which it never delivered.

A prompt the policy would have answered on its own can go to a person all the
same. Its `policy_evaluated` entry, `ask`, then says why, and when the reason
arises after that entry was journaled, a second `policy_evaluated` entry gives
it. The Desktop GUI and the web gateway write these reasons besides the
policy's own:

| Reason | Meaning |
| --- | --- |
| `consecutive_auto_limit`, `rate_limit_exceeded` | A limit of the policy was reached, possibly while the prompt waited behind other answers: the policy is asked again just before its decision. |
| `repeat_after_delivery` | The prompt repeats one of its session answered, or being answered, less than two seconds earlier. |
| `operator_attached` | Somebody holds the session's web terminal, or took it while the prompt was being taken in. |
| `typed_at_terminal` | The terminal's holder typed while the prompt was shown; nobody answers it through Relayer. |

On those two, a decision the adapter cannot encode, the policy's or a
person's, is followed by a `delivery` entry with the outcome and reason
`fallback_unsupported`: nothing reached the agent, and the prompt went back to
a person.

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
- all metadata except a closed allowlist per kind: policy mode/actions/automatic status, process-exit code/failure status, an MCP tool call's server, tool and argument count, the operator, role and connection of a decision, delivery, attach or hand-over (plus the other party's operator and connection for a hand-over), and a recording's counters and markers.

For `sensitive`, `credential`, or high-risk events, `summary` is the constant `sensitive_event` and metadata is omitted. The audit never records the length of a submitted secret. Maps are copied before serialization so later mutation cannot alter an accepted entry.

Sensitive event IDs are omitted too: generic occurrence IDs are derived from a fingerprint that includes the normalized match, so persisting them could enable offline guessing of a low-entropy OTP or password. Such records remain ordered and correlated by their session and audit sequence without retaining that derivative. Every occurrence ID also carries a random per-process token that is never recorded, so an ID differs between two processes of the same agent; journals and webhooks that correlated a prompt's `event_id` across restarts of an agent will see a new value for each process.

## Anonymized example

This example uses `detailed` mode so the already-generic sensitive summary is visible:

```json
{"schema_version":1,"sequence":1,"timestamp":"2026-08-26T20:00:00Z","entry_id":"entry-example-1","run_id":"run-example","kind":"run_started","decision_by":"system","outcome":"started","sensitive":false}
{"schema_version":1,"sequence":2,"timestamp":"2026-08-26T20:00:01Z","entry_id":"entry-example-2","run_id":"run-example","kind":"event_detected","session_id":"reviewer","agent_id":"reviewer","backend":"pty","adapter":"generic","event_type":"credential","risk":"high","summary":"sensitive_event","sensitive":true,"outcome":"detected"}
{"schema_version":1,"sequence":3,"timestamp":"2026-08-26T20:00:02Z","entry_id":"entry-example-3","run_id":"run-example","kind":"decision","session_id":"reviewer","decision":"ask","decision_by":"human","outcome":"pending","sensitive":true}
```

No value entered by the human appears in the third record.

## Inspecting and verifying the audit log (`relayer audit`)

Relayer includes a dedicated CLI command to inspect, summarize, and verify audit records without external tools like `jq`.

```bash
# View the most recent entries in a clean tabular format
relayer audit show

# Filter by agent and limit output to 20 records
relayer audit show --agent claude --limit 20

# Output entries as JSON for downstream tooling
relayer audit show --json > audit-export.json

# Display summary statistics (total runs, decisions, human vs policy ratio)
relayer audit stats

# Verify schema compliance, monotonic sequence continuity, and security invariants
relayer audit verify
```

If `--path` or a positional file argument is omitted, `relayer audit` inspects the default user journal path automatically.

## Failure behavior

Audit initialization and the initial run record complete before an agent process starts. A failure at that point aborts startup cleanly. During a run, an audit write that fails is sticky and stops supervision from writing anything more to an agent:

- the TUI shows the failure in the supervisor and freezes every pane: no policy or manual delivery, line or attach;
- the Desktop GUI and the web gateway stop every answer, the policy's included, and every line, show each pending prompt failed with the reason `audit_unavailable`, and report the journal failed. The gateway also refuses keystrokes and new attaches. An agent can still be stopped.

Before v0.8.9 the web gateway ignored every failed audit write and went on delivering. A failed audit write never changes a policy result into `allow`, and Relayer never retries an uncertain automatic delivery.

The recorder is local only. There is no remote service or upload.

## Confidentiality limits

Redaction is defense in depth, not a formal data-loss-prevention engine. Previously unknown secret formats may not be recognizable in non-sensitive free-form summaries. Prefer `metadata` mode and ensure adapters mark credentials and sensitive events correctly.

Permissions protect against other ordinary local users but do not protect against the same operating-system account, an administrator, root, malware, backups, disk snapshots, or post-write tampering. The audit is not cryptographically signed and is not an authorization boundary, sandbox, or system firewall.

One recorder serializes all agents inside a Relayer run. Separate Relayer processes do not coordinate rotation with each other; configure distinct paths when running multiple instances concurrently.

Return to the [README](../README.md) or continue with
[troubleshooting](troubleshooting.md).
