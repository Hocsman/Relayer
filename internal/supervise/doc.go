// Package supervise is the supervision core shared by Relayer's front ends.
//
// It holds what decides whether a byte reaches a supervised agent: the
// prompts a run is waiting on, the policy's automatic decisions and their
// serialisation per session, human decisions and free-text lines, the
// fail-closed audit journal, and the display-safe form in which all of it is
// shown. A front end brings the transport — Wails events on the desktop, a
// websocket on the gateway — and nothing that bears on delivery.
//
// The desktop kept this state machine in its Wails bridge, and the gateway a
// second, diverging copy of it. The code here is the desktop's, moved without
// a change in behaviour, so the desktop's tests pinned it before either front
// end was changed to rely on it. The rules it has kept since differ from the
// desktop's of v0.8.7 in these ways:
//
//   - A session takes one write at a time, and the claim a decision takes on
//     it is released only when that decision's write returns, even when the
//     agent withdraws the prompt meanwhile: the prompt is shown answered at
//     once, and the next automatic prompt is considered once the write is
//     over.
//   - An agent's status follows its prompts: a Start or a Restart that kept a
//     prompt the new process raised shows the agent waiting, not running, and
//     a prompt raised while a Stop or a Restart holds the agent leaves it
//     stopping.
//   - The exit of a process a replacement already superseded is journaled
//     with its real outcome, failed when it failed, and the reason
//     process_exit_stale (audit.ReasonProcessExitStale), which the telemetry
//     does not read as the end of the running session.
//
// The package deliberately depends only on the adapter, audit, policy, session
// and terminal vocabularies and the standard library (imports_test.go enforces
// it): no notifier, no telemetry, no network and no user interface toolkit.
package supervise
