# Configuration reference

Relayer reads `config.yaml` by default or the file passed to `--config`. A
missing file is created atomically and an existing file is never overwritten by
that creation path.

The current schema version is `1`. This is an alpha schema: the version marker
allows future incompatible formats to fail clearly rather than being guessed.

## Generated configuration

The generated file is equivalent to:

```yaml
version: 1
backend: pty
sessions:
  persist_on_exit: false
  cleanup_on_success: true
policies:
  default_action: ask
  dry_run: false
  rules: []
audit:
  enabled: true
  mode: metadata
  path: ""
  max_file_size_mb: 10
  max_files: 5
agents: []
intercept_patterns:
  - pattern: '(?i)overwrite.*\[y/n\]'
    description: confirmation d'écrasement
  - pattern: '(?i)\[[yn]/[yn]\]'
    description: confirmation oui/non
  - pattern: '(?im)password:[[:space:]]*$'
    description: saisie d'un mot de passe
  - pattern: '(?i)do you want to continue'
    description: confirmation de poursuite
```

`agents: []` is a deliberate quick-start value. The app substitutes two
synthetic Bash agents. A non-empty list must contain between one and eight
valid agents.

The generated configuration file uses mode `0644` on Unix. It is configuration,
not a secret store. Do not put passwords, tokens, API keys, or other credentials
in it.

## Strict version 1 decoding

A version 1 document rejects:

- unknown fields at any modeled level;
- omitted `version`, `backend`, `agents`, or `intercept_patterns`;
- versions other than integer `1`;
- YAML aliases and merge keys;
- more than one YAML document;
- wrong node or scalar types, including quoted booleans or integers where a
  real YAML boolean or integer is required;
- empty or invalid regular expressions;
- NUL bytes in executable agent fields;
- more than eight agents.

All validation and backend resolution complete before an agent process starts.
Semantic strings such as backend, action, mode, event type, and risk are
trimmed and normalized where their decoders define that behavior. Do not depend
on incidental whitespace normalization for IDs, paths, arguments, shell code,
or regex text.

## Top-level fields

| Field | Required | Values or meaning |
| --- | --- | --- |
| `version` | Yes | Integer `1`. |
| `backend` | Yes | Global `pty`, `tmux`, or `auto`. |
| `sessions` | No | Detached tmux persistence and success cleanup. |
| `policies` | No | First-match action rules. |
| `audit` | No | Local JSONL recorder. |
| `agents` | Yes | Zero to eight agent specifications. |
| `intercept_patterns` | Yes | One or more generic adapter regexes. |

If `sessions` is omitted, persistence is false and cleanup on success is true.
If `policies` is omitted, every actionable event defaults to `ask` and dry-run
is false. If `audit` is omitted from an existing v1 file, auditing is disabled
for compatibility. Newly generated files include and enable metadata auditing.

## Backends

### `pty`

Relayer launches the agent under a directly owned pseudo-terminal. It forwards
viewport dimensions to the PTY, retains bounded output, and closes or terminates
the owned process group during shutdown. PTY sessions cannot be natively
attached through tmux and ignore persistence settings.

### `tmux`

Relayer requires the `tmux` executable before startup. Each agent gets a
detached, Relayer-owned session that can be attached from its idle TUI pane.
Failure to locate tmux is a startup error; it does not silently change an
explicit `tmux` request to PTY.

### `auto`

Relayer looks for tmux once. If found, `auto` becomes `tmux`; otherwise it
becomes `pty`. The effective choice is reported in diagnostics. An agent's
effective backend is concrete before backend construction.

Agents may select different concrete backends in one run. A blank per-agent
`backend` inherits the global value.

## Session lifecycle

```yaml
sessions:
  persist_on_exit: false
  cleanup_on_success: true
```

- `persist_on_exit: false` asks shutdown to remove unfinished owned tmux
  sessions. The backend verifies ownership before killing a session.
- `persist_on_exit: true` permits eligible unfinished tmux sessions to remain
  after the TUI exits.
- `cleanup_on_success: true` removes an owned tmux session as soon as its
  process exits successfully, even when persistence is enabled.
- `cleanup_on_success: false` leaves that cleanup to later explicit stop or
  shutdown policy.

These settings never authorize killing unrelated sessions and never call
`tmux kill-server`. A persistent process is no longer being supervised after
Relayer exits; inspect it separately with tmux.

## Agents

The alpha desktop GUI can edit the same `agents` sequence through its
**Agents** panel. It offers launch presets for the local `claude`, `codex`, and
`mimo` executables plus a Custom CLI form. These are argv conveniences, not
vendor-specific adapters: every preset currently uses `generic` detection.
DeepSeek or another provider/model remains configuration owned by the chosen
CLI; Relayer does not infer a command, flag, credential, or model name.

Existing command vectors are masked from the WebView and remain authoritative
inside Go until the user explicitly replaces the entire argv. Shell commands,
environment overrides, and non-generic adapters are read-only in the GUI and
remain editable in YAML. A GUI save is applied on the next application launch,
not to the currently running sessions. Legacy documents and profiles with
historical IDs outside the form's conservative syntax remain read-only; Relayer
does not migrate or normalize them silently.

Each configured agent supports:

```yaml
agents:
  - id: reviewer
    name: Repository reviewer
    command: ["./bin/reviewer", "--mode", "read only", ""]
    cwd: ./workspace
    env:
      RELAYER_ROLE: reviewer
    adapter: generic
    backend: pty
```

| Field | Requirement |
| --- | --- |
| `id` | Non-blank; unique case-insensitively across agents. |
| `name` | Non-blank display name. |
| `command` | Exact string list containing a non-blank executable; mutually exclusive with `shell`. |
| `shell` | Non-blank explicit script; mutually exclusive with `command`. |
| `cwd` | Optional existing directory. Relative values use the config directory. |
| `env` | Optional string-to-string overrides with portable environment names. |
| `adapter` | Optional; blank resolves to an implemented executable hint or `generic`. |
| `backend` | Optional; blank inherits the global backend. |

Environment names must match `^[A-Za-z_][A-Za-z0-9_]*$`. Values are passed
literally and override the inherited environment. The map is not variable
expansion syntax.

### Direct commands

`command` preserves every argument exactly, including spaces, quotes,
semicolons, dollar signs, backticks, newlines, and empty arguments. The first
entry is the executable. No shell parses the vector:

```yaml
command: ["./agent", "--message", "a value with spaces", ""]
```

This is the preferred execution mode. Avoid credentials in arguments because
the operating system and local inspection tools may expose process arguments.

### Explicit shell commands

`shell` intentionally opts into `/bin/sh -c` on supported Unix systems:

```yaml
shell: 'prepare-input | exec ./agent --mode "$RELAYER_MODE"'
```

Quoting, expansion, redirection, substitutions, and injection risk are the
shell author's responsibility. Relayer displays shell execution as an explicit
shell mode rather than reproducing the script in normal diagnostics.

### Environment details

PTY agents inherit the Relayer environment plus configured overrides and use
`TERM=xterm-256color`.

tmux agents receive a launch-time snapshot of the Relayer environment rather
than relying on a potentially stale tmux server environment. `TERM`, `TMUX`,
and `TMUX_PANE` from that snapshot are excluded so the fresh tmux context can
supply correct values, unless the agent explicitly overrides one of them.

In either backend, the target process and the current operating-system user can
inspect the resulting environment. Prefer a platform secret manager or a
short-lived inherited environment over plaintext YAML.

## Adapters and interception patterns

Only `generic` is implemented. Leaving `adapter` blank resolves to an
implemented executable hint when one exists, then falls back to `generic`.
Because the current `claude` and `codex` entries are unimplemented experimental
descriptors, those executable names also fall back to `generic` when adapter is
omitted. Explicitly setting either placeholder fails before backend creation.
An unknown explicit adapter also fails.

`intercept_patterns` is an ordered, non-empty list:

```yaml
intercept_patterns:
  - pattern: '(?i)overwrite.*\[y/n\]'
    description: overwrite confirmation
```

Both fields are required and non-blank, and each expression must compile as a
Go regular expression. The first applicable generic pattern wins. Sensitive
classification is inferred conservatively from the expression and description
when they contain credential-related markers.

Patterns run against normalized terminal text, but the adapter further
requires a match to reach the active line changed by new output. See
[adapters.md](adapters.md) before designing broad expressions.

## Policies

```yaml
policies:
  default_action: ask
  dry_run: false
  rules:
    - name: ask-unknown-risk-confirmations
      match:
        event_types: [confirmation]
        risk_levels: [unknown]
        agent_ids: [builder, reviewer]
        sensitive: false
        text_regex: '(?i)delete|overwrite'
      action: ask
```

Allowed actions are `allow`, `ask`, and `deny`. Rules are checked in order and
the first match wins. Populated fields inside `match` use AND semantics; values
inside one list use OR semantics.

| Matcher | Accepted values |
| --- | --- |
| `event_types` | Non-empty list of `confirmation` or `credential`. |
| `text_regex` | Non-blank Go regular expression evaluated against summary plus match internally. |
| `agent_ids` | Non-empty list; every ID must reference a configured agent, case-insensitively. |
| `risk_levels` | Non-empty list of `low`, `unknown`, or `high`. |
| `sensitive` | YAML boolean. A credential is treated as sensitive even if its event flag is false. |

A rule needs a non-blank, case-insensitively unique name, at least one matcher,
and a valid action. The default action is subject to the same conservative
effective-action rules as a matched action:

- invalid, incomplete, or non-actionable events become `ask`;
- a credential or sensitive event becomes `ask`;
- `allow` is automatic only at explicit `low` risk;
- `deny` may remain automatic for a valid non-sensitive confirmation at
  `low`, `unknown`, or `high` risk;
- dry-run retains the proposed action for audit but makes the effective action
  `ask` and disables automatic delivery.

Policy evaluation does not guarantee delivery. The resolved adapter must be
able to encode that action for the exact pending event. The generic adapter can
currently encode only human manual input, so its automatic allow and deny both
fall back to `ask`. Deny is an adapter response, not a process kill.

## Audit

```yaml
audit:
  enabled: true
  mode: metadata
  path: ""
  max_file_size_mb: 10
  max_files: 5
```

- `mode`: `off`, `metadata`, or `detailed`.
- Empty `path`: a private per-user configuration path.
- Relative `path`: resolved from the configuration directory.
- `max_file_size_mb`: positive bounded size for each file.
- `max_files`: 1 through 100, counting the active file.

Either `enabled: false` or `mode: off` disables file creation. Full semantics
are in [audit.md](audit.md).

## Deprecated pane overrides

```bash
./relayer --pane1 './agent --label "value with spaces"' --pane2 ''
```

An explicitly supplied flag overrides the command of that configured position.
An explicitly blank value selects the mock command for that position. Omitting
the flag leaves configuration unchanged. Overriding a position that does not
exist is an error.

The compatibility tokenizer understands whitespace, single and double quotes,
and backslash escaping. It does not execute a shell or interpret environment
variables, globs, redirections, pipes, command substitutions, or other shell
syntax. Migrate these overrides into `agents[].command`.

## Legacy pattern-only files

Relayer still accepts both historical shapes:

```yaml
- pattern: '(?i)continue\? \[y/n\]'
  description: continue confirmation
```

```yaml
intercept_patterns:
  - pattern: '(?i)continue\? \[y/n\]'
    description: continue confirmation
```

Legacy files configure patterns only. They use PTY, default ask policy, default
session cleanup behavior, disabled audit, and the two mock agents. Migrate to a
version 1 file to configure real agents or tmux. Legacy parsing remains for
compatibility, not as the recommended schema.
