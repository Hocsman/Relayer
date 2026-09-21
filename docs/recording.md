# Session recording and replay

Relayer can write each supervised session's terminal stream to a standard
[asciicast v2](https://docs.asciinema.org/manual/asciicast/v2/) `.cast` file, so
a session can be replayed later in the web interface for audit or training.

Recording is **off by default**. It captures terminal output verbatim, which is
the one thing the [audit journal](audit.md) deliberately never stores, so
enabling it is a decision about what you are willing to keep on disk.

## Enabling it

Add a `recording:` block to the version-1 configuration file:

```yaml
version: 1
recording:
  enabled: true
  path: ""                 # "" resolves to the user configuration directory
  record_input: false
  redact: true
  max_file_size_mb: 16
  max_recordings: 50
  max_total_size_mb: 512
  retention_days: 30
```

| Key | Default | Meaning |
| --- | --- | --- |
| `enabled` | `false` | Master switch. An absent block is equivalent to `false`. |
| `path` | `""` | Directory for transcripts. A relative path resolves against the configuration file's directory; `""` resolves to `<user config dir>/relayer/recordings`. |
| `record_input` | `false` | Also record what was typed into the session. See [What is never recorded](#what-is-never-recorded). |
| `redact` | `true` | Mask printable bytes in input frames. Only meaningful with `record_input`. |
| `max_file_size_mb` | `16` | Byte cap per transcript. A transcript that reaches it stops growing and is marked truncated. |
| `max_recordings` | `50` | Keep at most this many transcripts. |
| `max_total_size_mb` | `512` | Keep at most this much on disk in total. |
| `retention_days` | `30` | Delete transcripts older than this. `0` disables age-based deletion. |

Recording is not exposed in the visual settings editor. It is a security-relevant
setting, and keeping it in YAML keeps it out of reach of a browser session.

Pruning runs once at startup, oldest first, against all three limits. It never
deletes a transcript that is currently being written.

## Storage layout

```
<path>/                                     0700, owned by the current user
  <runID>/                                  0700
    <session>-<unixNano>.cast               0600   asciicast v2
    <session>-<unixNano>.meta.json          0600   sidecar
```

The store applies the same filesystem hardening as the audit journal: private
directory and file permissions, rejection of symlinks and non-regular targets,
and an ownership check on Unix. It provides no protection from the same
operating-system user, an administrator, root, malware, backups, or disk
snapshots.

> [!IMPORTANT]
> A configured `path` must either **not exist yet**, so Relayer creates it at
> `0700`, or already be private. A directory you created yourself with `mkdir`
> is `0755` on most systems — other local users could read the transcripts — so
> Relayer refuses it rather than silently widening or narrowing permissions you
> set. Recording then stays off for that run and says so on standard error.
> Either let Relayer create the directory, or `chmod 700` it first.

Each transcript has a JSON sidecar holding its identity, geometry, byte and
frame counts, and its end time. The sidecar is written when the transcript opens
and rewritten when it closes, so a transcript whose process died mid-run is
reported as truncated on the next start rather than silently looking complete.
There is no index file; listing scans the sidecars.

## Format

The file is standard asciicast v2: a JSON header object on the first line, then
one JSON array per line.

```
{"version":2,"width":120,"height":32,"timestamp":1789900000,"title":"claude"}
[0.482311,"o","[32mReady[0m\r\n"]
[1.204887,"r","100x30"]
[3.771002,"m","dropped:12"]
```

`"o"` is output, `"i"` is input, `"r"` is a resize (`COLUMNSxROWS`). `"m"` is a
marker — part of the asciicast format, used here for Relayer's own annotations:

| Marker | Meaning |
| --- | --- |
| `dropped:N` | N frames were discarded because the recorder's queue was full. |
| `truncated` | The byte cap was reached; nothing after this point was recorded. |

Any player that reads asciicast v2 will replay these files, including
`asciinema play`. A player that does not understand the markers ignores them.

## What is never recorded

**Nothing blocks the agent.** Every frame is offered to a bounded per-session
queue with a non-blocking send. If the queue is full the frame is dropped, the
loss is counted, and a `dropped:N` marker is written into the transcript when the
queue recovers. A recording is never allowed to slow down or stall the PTY read
loop, so a lossy transcript is the deliberate failure mode. Loss is visible in
the artifact rather than silent.

**Input frames are off by default.** The audit journal has no field for terminal
input and never stores a submitted value or its length. A transcript containing
`"i"` frames would defeat that, so `record_input` is opt-in. With `redact: true`
input frames keep control bytes — so a replay still shows *when* a line was
submitted or an interrupt sent — and mask every printable byte.

**Redaction is best effort.** It operates on the bytes of one chunk. A secret
split across two reads is not caught, and neither is a secret the agent printed
to its own output, which is recorded verbatim by design. **Do not treat a `.cast`
file as safe to publish.** Review it first, the same way you would review a
screen recording.

**tmux sessions record output and geometry but not input.** Sends to a tmux pane
go through `tmux send-keys` rather than a single write path, so there is no
equivalent choke point. A tmux transcript is therefore output-only regardless of
`record_input`.

## Replay

The web interface's **Recordings** panel lists the stored transcripts and
replays them in a terminal view with play, pause, scrubbing, and speed control.
Replay is strictly read-only: the player has no connection to a live session and
cannot resize, interrupt, or type into one.

Long transcripts are paged rather than loaded whole, and a very long one may be
partially loaded — the player says so rather than ending early and looking like
a session that finished there. Seeking rebuilds the screen from the start with a
bounded budget, so scrubbing a large transcript stays responsive; when the
rebuild is partial the player says that too.

A transcript that is still being written can be replayed. A transcript that is
still being written cannot be deleted.

## Audit trail

A transcript's lifecycle is journaled by the system, and what an operator does
with it is journaled with that operator:

| Kind | By | When |
| --- | --- | --- |
| `recording_started` | system | A transcript was opened for a session. |
| `recording_finished` | system | A transcript was closed, with its final size. |
| `recording_exported` | operator | A transcript was downloaded from the gateway. |
| `recording_deleted` | operator | A transcript was permanently removed. |

Entries carry the recording's identity and shape, never its content: the
`recording_id`, whether input was recorded and redacted, and on
`recording_finished` the byte and frame counts and whether the size cap
truncated it. As with all metadata, those fields survive only in the `detailed`
audit mode.

A transcript the store could not open is journaled as `recording_started` with
outcome `failed` and reason `recording_open_failed`, so a session that went
unrecorded says so in the journal as well as in the startup diagnostics. One
that could not be finalized is `recording_finished` · `failed` ·
`recording_finalize_failed`. A failed record carries no counts, since the file
it would describe is missing or unreliable.

Each record names the agent by its identifier, like every other audit record.
Transcripts written by earlier releases stored the agent's display name as its
identifier instead, so their export and delete records do not match a filter on
the agent.

Export and delete require the operator role; a viewer may list and replay.

## Limits worth knowing

- A `.cast` file is **not tamper-evident**. It is not signed or hash-chained, and
  neither is the audit journal ([docs/audit.md](audit.md)). Anyone who can write
  the directory can alter a transcript.
- A transcript records what the terminal *displayed*. It does not prove what a
  process did, and an agent can print anything it likes.
- Offsets are measured from a monotonic clock within one process. A transcript
  is internally consistent; it is not a synchronised timestamp source.
- Recording a session does not change how it is supervised. Prompt detection,
  policy, and arbitration are unaffected.
