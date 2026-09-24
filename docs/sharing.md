# Multi-operator sessions

Several people can connect to one Relayer web gateway and watch the same
interactive session at once. Exactly one connection at a time may type into a
given terminal; the others observe. This document describes how that write lock
— *the hand* — behaves, and what it does and does not guarantee.

Sharing needs no configuration. It is a property of the gateway: connect a
second operator or viewer and the roster appears.

## Roles and the hand are different things

[Role-based access control](web-gateway.md#roles) decides *what a connection may
ever do*: an `operator` token may arbitrate, type, and change lifecycle; a
`viewer` token may only watch. The hand decides *which operator may type into a
particular terminal right now*.

A viewer can never hold the hand. An operator who does not hold it can still
arbitrate prompts, stop and restart agents, and change settings — the hand
governs raw terminal input and geometry, not supervision.

A terminal nobody holds takes no keystrokes, from anybody: an operator takes
the terminal first, which the bundled interface does when you open the
interactive session. Every keystroke therefore falls within a hand whose
taking was journaled. Before v0.8.9 a terminal nobody had claimed was writable
by any operator connection, and its keystrokes could land alongside an answer
the policy was writing to the same question.

Keystrokes also wait on supervision. They share the session's one write slot
with answers and lines: they are refused while an answer or a line is being
written, and an answer or a line is refused, or for the policy's, delayed,
while they are. They are refused once the audit journal has failed, on a
session frozen after a write whose outcome is unknown, on a session that is
stopped, stopping or starting, and while the run is stopping.

## Taking, requesting, and releasing

| From | Action | By | Result |
| --- | --- | --- | --- |
| free | Take the terminal | operator | held by them |
| held by X | Take the terminal | operator Y | refused; Y is told to ask |
| held by X | Ask for the terminal | operator Y | X is prompted |
| X is prompted | Hand over | X | held by Y |
| X is prompted | Keep it | X | still held by X |
| X is prompted | no answer | — | request expires after 30s, still held by X |
| held by X | Release | X | free |
| held by X | X disconnects | — | free |
| held by X | Force takeover | operator Y | refused; still held by X (see below) |

Taking a terminal someone else holds is **refused, not queued and not stolen**.
An operator typing into an agent can be interrupted mid-command by a takeover,
so the transfer is explicit: Y asks, X answers.

A request that nobody answers expires rather than stranding the requester.
Expiry is evaluated when the state is next read, so no timer outlives the run.

Force takeover seizes a held terminal without consent. It is disabled: this
release has no setting that enables it, so the gateway refuses every attempt.
Every attempt is still journaled as `control_forced`, with outcome `failed`
while the verb stays disabled — trying to seize a colleague's terminal is the
event worth finding later, whether or not it worked.

## The hand is keyed by connection, not by identity

The same operator may have two browser tabs open. Each is a separate connection
with its own identifier, and the hand belongs to one of them:

- a second tab does **not** inherit the first tab's hand;
- releasing in one tab does **not** revoke the other;
- a second tab asking for the terminal goes through the normal request flow.

This is deliberate. Keying by identity would make "who can type" ambiguous the
moment anybody opens a second window.

## Presence

Each session shows who is watching it, who holds the terminal, and who is asking
for it. A connection joins a session's roster by observing it, which is a
read-only act available to viewers.

Presence is an indicator, not an authentication mechanism. An identity in the
roster is whatever the token it presented was named; see
[web-gateway.md](web-gateway.md#named-tokens). A roster is capped at 16
connections per session so a snapshot stays small enough to broadcast on every
change.

Presence and hand updates are sent as **complete snapshots**, never as deltas.
The gateway drops a message it cannot deliver to a slow client rather than
blocking, so a lost update must be repaired by the next one instead of leaving a
roster permanently wrong.

## One honest race

A keystroke is checked against the hand and then written to the pseudo-terminal.
Those two steps cannot share a single lock acquisition without holding that lock
across the write, which would let a saturated terminal block every other
operator. The consequence:

> After an operator releases or loses the terminal, at most one keystroke that
> was already in flight may still land.

One character, not a stream — the next keystroke is rejected, and the client is
told once per session so a stale interface resynchronises without the socket
being flooded by one rejection per character. If a deployment cannot tolerate
that window, it should not rely on the hand as a safety mechanism: it is a
coordination tool between colleagues, not an enforcement boundary against one.

Rejected keystrokes are dropped, never queued. Nothing typed while you do not
hold the terminal is delivered later.

## Resize

A resize from a connection that does not hold the terminal is silently ignored.
Two observers with different window sizes would otherwise fight over the pane
geometry and make the agent's rendering thrash. Only the holder's window size
reaches the pseudo-terminal.

## Audit trail

Every transfer is journaled with the acting operator, their role, and the
connection involved. Where a transfer has another party — the holder asked, the
operator handed to or refused, the holder displaced — the record names them as
`target_operator` and `target_conn_id`. Like all metadata, those two survive
only in the `detailed` audit mode; `metadata` mode keeps the operator, outcome
and reason.

| Kind | Outcome · reason | When |
| --- | --- | --- |
| `control_requested` | `pending` · `control_requested` | An operator asked for a terminal someone else held. |
| `control_requested` | `applied` · `control_taken_free` | The terminal was free, so asking took it at once. |
| `control_granted` | `applied` · `control_granted` | A holder handed it over. |
| `control_declined` | `applied` · `control_declined` | A holder refused. |
| `control_released` | `applied` · `control_released` | A holder released it. |
| `control_released` | `applied` · `control_released_disconnect` | A holder disconnected; recorded by the system. |
| `control_forced` | `applied` · `control_forced` | An operator seized it without consent. |
| `control_forced` | `failed` · `control_force_disabled` | An operator tried to, and the gateway refused. |
| `attach_started` / `attach_finished` | `applied` | A terminal was taken or released through the attach control. |

One action writes one record. Releasing through the attach control writes
`attach_finished`, not `control_released` as well, and asking again for a
terminal you are already waiting on refreshes the request without journaling
it twice. A request that expires or is withdrawn moves no terminal and is not
journaled. Neither is an action on a session the run never started: the gateway
refuses it.

Keystrokes themselves are never journaled. The audit model has no field for
terminal input, and sharing does not add one. To record what happened inside a
session, see [session recording](recording.md).

## What sharing is not

- It is **not an access control boundary**. Anyone holding an operator token can
  take a free terminal, and can request any held one. The boundary is the token.
- It does **not** make interactive attachment safe. An attached operator types
  directly into the agent process, bypassing prompt detection, policy, and
  arbitration exactly as a native tmux attach does. See
  [the security model](security-model.md).
- It does **not** prevent two operators from working at cross purposes. It only
  guarantees that they cannot type into the same terminal at the same time.
