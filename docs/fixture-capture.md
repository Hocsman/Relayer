# Secure adapter fixture capture

`relayer-capture` is a developer tool for recording a bounded, anonymized,
normalized terminal observation before it is reviewed as adapter evidence. It
is not part of the Relayer runtime and is not an agent recorder.

## Safety boundary

The capture command accepts only an exact argv vector and never wraps it in an
implicit shell. It has no separate shell-script, stdin, manual-response,
environment-override, token, provider-key, or account-data field. Explicitly
choosing `sh`, `bash`, or another interpreter still gives that process the
literal following arguments to interpret and is therefore not a safety
boundary. The child receives only a small allowlist of ordinary process
variables; common credential variables are not inherited. `HOME`, `TMPDIR`,
and the XDG config/cache/data roots point to private `0700` directories inside
the disposable capture runtime and are deleted when capture cleanup returns.

The capture harness is **not a sandbox**. The exact argv is executed as a real
local process and can still read or change any resource permitted by the
current operating-system account, use the network, or have other side effects.
Cleanup owns the original Unix process group (or tmux pane group) only. A
program that deliberately creates a new session/process group can escape that
ownership and may outlive the capture, so verify disposable targets separately.
Use only a disposable target and explicitly reviewed arguments. Redaction
protects the resulting artifact; it cannot prevent effects caused by the
captured command.

Before publication, the capture is:

- limited to 256 KiB by default and 4 MiB at the hard maximum;
- stopped after 20 seconds by default and five minutes at the hard maximum;
- rejected if centralized redaction recognizes a token, JWT, authorization
  value, API key, URL credential, password, OTP, or other secret shape;
- anonymized for home-directory prefixes and email addresses;
- stored as strict, versioned JSON with no argv, cwd, environment, input,
  timestamp, hostname, or user field;
- atomically written with a `0600` final file on Unix.

Redaction is conservative but cannot prove that arbitrary text is harmless.
Always inspect a fixture before committing it. A valid capture is evidence of
observed normalized terminal text, not proof that every version or state of a
CLI is supported.

## PTY capture

```sh
go run ./cmd/relayer-capture \
  --tool example-cli \
  --adapter generic \
  --backend pty \
  --timeout 20s \
  --max-bytes 262144 \
  --output /tmp/example-cli.json \
  -- example-cli --literal-argument
```

No input is sent. A startup prompt can therefore be captured safely, while a
dialogue that requires navigation or authentication will normally end at the
timeout. Do not put secrets in argv: the command rejects recognizable secret
shapes, and operating-system process inspection can expose arguments anyway.

## tmux capture

Replace `--backend pty` with `--backend tmux`. The tool creates a randomized
private runtime directory, starts tmux with `/dev/null` as its configuration,
uses an explicit private socket, and targets only the immutable IDs returned by
that server. Output goes to a private size-bounded sink; a separate completion
acknowledgement is required before the artifact is read. Cleanup uses only this
private server; an existing user tmux server is never selected or killed.

tmux capture is available only on supported Unix platforms and requires a
locally installed `tmux`. The owned pane process group and private server must
be confirmed stopped before a timeout or output-limit capture is returned.

## Validation and JSON schema

```sh
go run ./cmd/relayer-capture --validate /tmp/example-cli.json
```

Validation rejects unknown JSON fields, non-canonical redaction, invalid
sequence numbers, oversized chunks, unsupported schema versions, and invalid
outcome combinations. Schema version 1 contains only:

```json
{
  "schema_version": 1,
  "tool": "example-cli",
  "adapter": "generic",
  "backend": "pty",
  "outcome": "timed_out",
  "chunks": [
    {"sequence": 0, "data": "An anonymized startup prompt"}
  ]
}
```

`outcome` is `exited`, `timed_out`, or `output_limit`. An exited fixture may
also contain a numeric `exit_code`; an output-limit fixture carries only the
boolean `truncated` marker. The artifact never stores the launch command.

## Turning a capture into adapter support

A capture must be minimized and reviewed before entering
`internal/adapters/testdata`. The version 1 capture artifact contains normalized
terminal text: ANSI controls and carriage-return rewrites are canonicalized,
and its storage chunks are not the original PTY read boundaries. Raw ANSI
evidence therefore requires a separate privacy review before a minimal derived
vendor fixture can retain it. A vendor rule is acceptable only when its prompt
structure is present in reviewed evidence and tests cover arbitrary chunking,
fragmented ANSI, carriage-return rewrites, quoted/history false positives,
snapshot replay, successive prompts, sensitive classification, and memory
bounds.

Decision bytes require stronger evidence: both allow and deny must be observed
against a disposable target with the expected side effect (or absence of it).
If either response is ambiguous, `EncodeDecision` must return
`ErrDecisionUnsupported` so policy falls back to human `ask`.
