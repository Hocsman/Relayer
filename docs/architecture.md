# Architecture

This document describes the current alpha implementation. Package boundaries
and internal interfaces are intentionally allowed to change.

## Data flow

```text
strict config + CLI overrides
            │
            ▼
agent validation ── adapter registry ── policy engine ── audit initialization
            │
            ▼
backend resolution (pty / tmux / auto, per agent)
            │
            ▼
terminal backend router ───────────────► PTY manager
            │                           tmux manager
            │
            ├── bounded output ────────► TUI viewports
            └── typed adapter events ─► policy / human queue / audit
                                             │
                                             ▼
                                   encoded input to exact session
```

No adapter or policy owns a process. Backends own process/session lifecycle;
adapters turn output into typed events; the policy engine proposes an action;
the TUI coordinates human input and delivery.

## Composition and validation

`internal/app` is the composition root used by `cmd/relayer` and the historical
root entry point. Startup proceeds in this order:

1. Parse `--config` and deprecated pane overrides.
2. Load strict version 1 YAML or a compatible legacy pattern document.
3. Resolve mock or configured agent plans and validate commands, paths,
   environment names, IDs, and agent count.
4. Compile policy regexes and validate policy references to configured agents.
5. Build the adapter registry and resolve every agent adapter.
6. Resolve each backend, including `auto`, before starting an agent.
7. Open the audit recorder and durably record `run_started`.
8. Construct concrete backends and start sessions sequentially.
9. Record session startup, construct the TUI, and begin event supervision.

Configuration, adapter, policy, audit, and explicit tmux-availability failures
therefore occur before a subprocess starts. If a later session cannot start, or
its session-start audit record fails, the application rolls back sessions that
already started.

## Agent plans

`internal/agent` represents a normalized agent specification. A specification
has an ID, display name, exactly one execution mode, optional working directory
and environment overrides, adapter selection, and backend selection.

- Direct mode retains `command` as an exact argument vector. It does not invoke
  a shell or re-tokenize its arguments.
- Shell mode is explicit and uses `/bin/sh -c` on supported Unix platforms.
- Relative working directories are made absolute from the configuration
  directory and must already be directories.
- Environment maps and argument slices are copied during validation.
- Agent IDs are unique without regard to case.

The app uses IDs, not screen positions, for backend routing and event
correlation. Display names are not stable identifiers.

`internal/toolcatalog` is an optional launch-profile layer used by the desktop
configuration UI. It maps a small declarative catalogue or an explicit custom
argv into the same `agent.Spec`; it never owns a process, chooses a model,
handles authentication, or changes an adapter/backend. Executable discovery is
a passive, injectable `PATH` lookup and does not run `--version`.

GUI profile saves replace only the YAML `agents` node under an opaque revision
token and a bounded per-file/inter-process lock. Validation is repeated in Go,
the publication is an atomic same-directory rename, and stale or uncertain
results force a reload. The running desktop engine is deliberately not
restarted: the saved revision becomes active on the next application launch.

## Terminal abstraction and routing

`internal/terminal.Backend` is the context-aware boundary used by the app. Its
operations cover start, retained output, input, resize, shutdown admission, and
close. Optional attach/resync and contextual-resize capabilities are detected
through additional interfaces.

The backend router maps each normalized agent ID to one concrete backend. It
supports a mixture of PTY and tmux agents and performs case-insensitive lookup
without changing the canonical ID sent downstream. Unknown IDs fail rather
than falling through to an arbitrary backend.

Closing the router closes its distinct concrete backends concurrently under a
shared deadline. A failed close remains retryable; backends that already closed
successfully are not closed again.

## PTY backend

The PTY backend uses `github.com/creack/pty` to give a program a controlling
pseudo-terminal. It owns:

- the command and Unix process group;
- the PTY descriptor;
- a read goroutine that feeds the session processor;
- a wait goroutine that emits canonical process-exit state;
- resize and input synchronization;
- cancellation and descriptor closure during shutdown.

The application sets `TERM=xterm-256color`. Viewport geometry is converted to
PTY columns and rows. Context-aware resizing is batched asynchronously by the
TUI so a slow backend does not block Bubble Tea's update loop; older synchronous
backends use the compatibility path.

PTY sessions are process-owned. The tmux persistence configuration does not
turn them into detached sessions.

An input write that has already blocked in the Unix PTY driver is released by
session stop/close, which closes the master descriptor. The current PTY path
does not provide an independent nonblocking I/O loop capable of interrupting
that write from the request context alone; implementing one requires a bounded
poll-driven reader/writer owner rather than spawning untracked writer
goroutines.

## tmux backend

The tmux backend creates one detached tmux session per agent. A run-specific
prefix, immutable session ID, and owner marker separate its resources from
unrelated tmux use. Cleanup requires ownership verification and never calls
`kill-server`.

Launch transport uses a private runtime directory (`0700` on Unix), a launch
specification and FIFOs (`0600`), and an internal helper mode. The helper reads
and removes its specification before releasing the target process. Arguments
remain an exact vector in direct mode. For execution, the launch environment is
snapshotted from the current Relayer process, excluding stale tmux metadata
unless the agent explicitly overrides it.

`pipe-pane` streams output into the processor. A lightweight monitor observes
session/process state. Retained snapshots support startup and post-attach
reconciliation without using aggressive full-output polling as the normal
transport.

Supervisor input is loaded through tmux standard input into a temporary named
buffer, pasted into the exact pane, and removed. The sensitive value is not put
in a tmux command argument or buffer name.

Pressing Enter on an idle tmux pane launches the native tmux client. The TUI
does not intercept human input while that client is attached. After detach,
Relayer captures a bounded recent snapshot, checks process state, restores or
reconciles pending events, and reapplies pane geometry. Failure to resynchronize
freezes decisions for that pane rather than guessing.

`sessions.persist_on_exit` keeps eligible unfinished, owned tmux sessions when
Relayer stops. `sessions.cleanup_on_success` removes an owned session after a
successful process exit, independently of persistence. Explicit stop requests
still target the exact owned session.

## Adapters and event processing

`internal/adapters` separates raw terminal transport from semantic prompt
events. The per-session processor:

- stores rendered output in a 256 KiB ring buffer;
- maintains a separate 16 KiB normalized detection window;
- sanitizes fragmented ANSI sequences;
- models carriage-return line rewrites;
- calls the resolved adapter with current state and new normalized bytes;
- assigns occurrence identity and keeps at most the actionable pending state;
- reconciles replayed snapshots after attach or transport repair.

Events carry an occurrence ID, a stable signature, sequence, session and agent
IDs, adapter, type, bounded summary, match, sensitivity, risk, timestamp, and
copied metadata. The ID distinguishes two successive identical prompts; the
signature supports replay deduplication. Acknowledging an event permits a later
occurrence of the same text to become actionable again.

The stable generic adapter applies ordered regular expressions only to the
active terminal line touched by new output. See [adapters.md](adapters.md).

The older `internal/intercept` package remains as a compatibility facade around
the same bounded concepts; new app flow uses adapter events.

## Policy and delivery

`internal/policy` is immutable after construction and safe for concurrent
evaluation. It contains no process, adapter, audit, or transport side effects.
It returns both a configured proposal and an effective action.

The TUI receives actionable events and serializes human prompts. Simultaneous
events from different sessions remain identified by event and session IDs;
successive identical occurrences remain distinct. Before a delivery, the TUI
records policy and decision state. It then asks the session's adapter to encode
the action and sends bytes through the backend assigned to that ID.

Unsupported encoding, stale or resolved events, exited sessions, audit failure,
transport failure, and post-attach uncertainty do not silently become an
allow. The generic adapter currently supports manual input only.

## Bubble Tea model

`internal/tui` follows Bubble Tea's model/update/view architecture:

- backend events become `tea.Msg` values;
- commands wait for the next event or perform asynchronous delivery, attach,
  reconciliation, or resize work;
- model state owns pane focus, page, viewports, pending prompts, input masking,
  policy state, and bounded supervisor logs;
- the view renders up to four agent panes plus the supervisor.

Normal layouts devote approximately 75% of terminal height to agents and 25%
to the supervisor. One, two, three, and four visible agents use full-width,
side-by-side, two-plus-one, and two-by-two layouts. Additional agents are paged.
Small terminals use guarded minimum dimensions rather than negative geometry.

New output follows the bottom only when the user was already at the bottom;
manual keyboard or mouse scrolling preserves position.

## Audit boundaries

`internal/audit` synchronously accepts versioned entries from the app and TUI.
An accepted sequence gives a total order inside one recorder. It does not claim
distributed causality between concurrently executing agents.

The audit deliberately distinguishes:

- `supervision_finished`: Relayer stopped supervising a session;
- `session_finished`: the canonical process-exit event was observed;
- `session_cleanup`: what backend cleanup or persistence could be established.

That distinction prevents a detached persistent session from being reported as
terminated merely because the TUI exited. See [audit.md](audit.md).

## Shutdown

`Ctrl+C` first calls `BeginShutdown`, preventing new work, then exits the Bubble
Tea program. The app records the end of supervision, asks the router to close
backends, records conservative cleanup results, writes `run_finished`, and
closes the recorder.

Backends cancel readers and monitors, close descriptors, and reap or retain
only resources covered by their ownership policy. Timeouts bound external tmux
operations. Close and in-flight mutation are coordinated so a completed close
does not admit new sends, resizes, attaches, or starts.

The exact sequence is tested because clean shutdown is part of the user-visible
contract, not only process hygiene.
