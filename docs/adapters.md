# Adapters and events

Adapters translate normalized terminal output into semantic events and encode a
decision back into terminal bytes. This interface is internal and mutable
during alpha; it is not a runtime plugin protocol.

## Current registry

| ID | Registry status | Implemented | Behavior |
| --- | --- | --- | --- |
| `generic` | Stable relative to the built-ins | Yes | Ordered regex prompt detection; manual input encoding. |
| `claude` | Experimental | Yes | Claude Code 2.1.59 workspace trust and environment-key prompts; generic fallback; manual input only. |
| `codex` | Experimental | Yes | Codex CLI 0.148.0-alpha.21 directory trust and command approval; generic fallback; command allow/deny and directory deny bytes verified. |

“Stable” here is a registry maturity label, not a promise that the alpha API
will remain source-compatible.

If `agents[].adapter` is blank, the registry considers implemented executable
hints and then falls back to `generic`. A basename of `claude` or `codex`
selects the corresponding experimental adapter. Both adapters retain every
configured `intercept_pattern` as a generic compatibility fallback.

The desktop catalogue also contains generic launch profiles for MiMo Code, a
combined Ollama / DeepSeek entry, and a custom CLI. A launch profile is not an
adapter claim. In particular, no MiMo, Ollama, or DeepSeek prompt protocol is
implemented; those profiles use `generic` detection.

An unknown or unavailable explicit ID is a configuration error before a
terminal backend starts.

## Event model

An adapter event contains:

- a unique occurrence `ID` and stable replay `Signature`;
- a monotonically assigned per-state sequence;
- session and agent IDs;
- adapter ID;
- type: `confirmation`, `permission`, `credential`, or `process_exit`;
- bounded display summary and the internal matched text;
- sensitive and risk classification;
- timestamp and copied metadata.

Confirmation, permission, and credential events are actionable. Process exit
is a canonical lifecycle event and is not sent to policy for automatic
approval.

The event match is needed internally for detection, signature, and policy regex
evaluation. It is not a field in the local audit schema. Sensitive events also
omit derivative event IDs from audit records because a signature-derived value
could aid guessing low-entropy credentials.

## Streaming processor

Every session has independent adapter state. The processor consumes arbitrary
byte chunks from PTY or tmux, so correctness cannot depend on read boundaries.
It:

1. appends original output to a bounded 256 KiB ring for display;
2. incrementally removes ANSI control sequences, including escapes fragmented
   across chunks;
3. normalizes terminal line behavior, including `\r` rewrites;
4. bounds the separate detection window to 16 KiB;
5. calls the adapter with only the new normalized effect and current state;
6. stores at most the current pending actionable occurrence;
7. emits copied events through the backend session channel.

Snapshot reconciliation uses the same normalized state. Replaying the same
pending snapshot does not create a second event. After acknowledgement, seeing
only the same historical occurrence does not reblock. A genuinely new identical
prompt receives a different occurrence ID. Two identical prompts in sequence
are therefore distinguishable without inventing vendor-specific markers.

## Generic regex adapter

`intercept_patterns` is compiled before any agent starts. Pattern order is
significant: the first applicable match is emitted, and no additional
actionable event is emitted while one occurrence is pending.

A generic match must overlap the active terminal line affected by the newest
normalized chunk. This prevents old retained output from becoming actionable
simply because a new unrelated line arrived.

The adapter ignores several common non-prompt contexts on the active line:

- Markdown quote lines beginning `> `;
- table-like lines beginning `| `;
- fenced code and content while inside a code fence;
- `log:`, `previous:`, and `historique:` prefixes;
- a match enclosed in matching backticks, single quotes, or double quotes.

These rules target practical false positives such as:

```text
> The old output said Overwrite? [Y/n]
`Overwrite? [Y/n]` is the example syntax
log: Overwrite? [Y/n]
```

They are intentionally product-neutral and incomplete. Indentation, alternate
log labels, cursor-addressed interfaces, localization, or adversarial output
can still bypass or trigger the detector. Narrow expressions and a default ask
policy remain necessary.

## ANSI and carriage-return behavior

ANSI stripping is streaming rather than a per-chunk regular expression. An
escape beginning in one read and ending in another is removed as one sequence.
Control bytes are bounded so malformed escape input cannot grow state without
limit.

Carriage return is modeled as an active-line rewrite. For example, a progress
line overwritten by a prompt can become actionable, while text overwritten by
a later progress line should not survive as a current prompt. Full cursor
addressing and alternate-screen emulation are outside the text viewport's
scope; use tmux attach for such programs.

## Sensitive classification and risk

A generic pattern becomes sensitive when the configured pattern metadata or
matched text looks credential-related. Sensitive matches become `credential`
events with high risk. Other generic confirmations currently carry unknown
risk.

Policy implications:

- credential or sensitive events always ask;
- generic unknown-risk confirmations cannot be automatically allowed;
- a configured deny can be proposed automatically for a valid non-sensitive
  event, but generic encoding does not currently support it;
- unsupported automatic encoding returns control to a human ask.

## Decision encoding

The generic adapter accepts only the internal manual decision form. It rejects
automatic allow/deny and non-actionable event types. Valid manual text is
encoded exactly with a trailing carriage return for the terminal; input
containing a NUL byte is rejected.

The TUI does not log or audit the manual value. Delivery errors keep or restore
human-pending state when it is safe to do so. An uncertain automatic delivery
is not retried.

An adapter that supports automatic decisions must define exact semantic allow
and deny encodings, bind them to a pending occurrence, reject unsupported
event types, and remain safe across live output and snapshot replay.

### Claude Code 2.1.59 (experimental)

The Claude adapter recognizes only two captured prompt structures:

- workspace trust, emitted as a high-risk `permission`;
- whether to use a detected environment API key, emitted as a sensitive,
  high-risk `credential` whose match starts after the displayed key value.

Automatic allow and deny are unsupported because the observed TUI response
depends on its current highlighted choice. Manual bytes retain generic
compatibility. No Bash, file-edit, network, MCP, authentication, or other
Claude prompt is claimed.

### Codex CLI 0.148.0-alpha.21 (experimental)

The Codex adapter recognizes only two captured prompt structures:

- directory trust: human choice `1` maps to the observed default-selection
  carriage return (`0d`), while deny is the selection-independent `2` (`32`);
- command approval: allow is `y` (`79`), deny is Escape (`1b`).

Those four byte sequences were verified against disposable local sessions,
but automatic directory-trust allow remains unsupported because carriage
return depends on the current highlighted choice.
The event match is a constant question and never contains the displayed path
or command. Command approval carries unknown risk and directory trust high
risk, so the policy engine still refuses automatic allow; a verified,
non-sensitive deny may be automatic. No file-write, network, credential, MCP,
review, or other Codex prompt is claimed.

## Fixtures and test policy

`internal/adapters/testdata/generic` contains synthetic product-neutral stream
and snapshot cases. Useful cases cover:

- matches split across chunks;
- ANSI sequences split across chunks;
- active-line carriage-return rewrites;
- quoted examples, fenced code, tables, and old-log prefixes;
- same snapshot replay, acknowledgement, and a new occurrence;
- successive identical prompts;
- bounded detection and output state;
- decision bytes and unsupported decisions.

The `claude` and `codex` fixture directories contain minimal anonymized
observations plus provenance notes. They intentionally exclude account data,
personal paths, repository content, commands from real projects, credentials,
hostnames, and unrelated output.

Use the output-only capture utility for new evidence:

```sh
go run ./cmd/relayer-capture --tool example-cli --adapter generic \
  --backend pty --output /tmp/example-fixture.json -- example-cli
go run ./cmd/relayer-capture --validate /tmp/example-fixture.json
```

The same command accepts `--backend tmux`. It uses a private tmux socket,
never invokes an implicit shell, and has no stdin, environment-map, or
credential field. An explicitly selected shell remains an ordinary executable
with all of that shell's effects. Captures are bounded, redacted before
persistence, and fail closed on secret-shaped content. See
[fixture capture](fixture-capture.md).

Do not contribute real transcripts without explicit authorization and a
provenance/anonymization review. Remove credentials, account and repository
identifiers, personal paths, source code, task content, timestamps, hostnames,
and unique wording. Prefer a minimal synthetic generic fixture whenever it can
exercise the same parser contract.

## Adding an adapter

During alpha, adapters are compiled into Relayer. A contribution should include:

1. a unique lowercase ID and accurate maturity descriptor;
2. stateful detection that handles arbitrary chunking and snapshot replay;
3. occurrence identity and acknowledgement tests;
4. explicit sensitive and risk classification;
5. decision encoding tests, including unsupported actions;
6. memory bounds and malformed-input tests;
7. authorized anonymized fixtures and documented provenance;
8. TUI/backend integration tests for delivery, exit, resync, and failure;
9. configuration and user documentation without overstating vendor support.

Registering an executable hint is appropriate only after the adapter is
implemented. A placeholder must never intercept executable resolution merely
because its name exists.
