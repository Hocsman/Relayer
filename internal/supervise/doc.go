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
//   - A human answer the adapter cannot encode never reached the agent, since
//     the runtime encodes an answer before it writes any of it. Its delivery
//     is journaled fallback_unsupported, the prompt goes back to the operator
//     pending, no longer automatic and without that answer among those it
//     offers, the caller gets ErrUnsupportedDecision, and nothing is frozen.
//   - A notice's Details is the prompt's display-safe summary, the one its
//     View shows, never the adapter's own: a notification leaves the machine.
//   - The core decides from the policy's evaluation as the engine returned it,
//     never from the bounded and redacted form a View shows, and journals the
//     policy's decision entries under the rule that made them. It asks the
//     policy again just before it journals a decision: a prompt the policy no
//     longer answers automatically, or would now answer another way, such as
//     one that reached a limit while it waited behind other answers, goes to
//     the operator, and a second policy_evaluated entry, ask, gives the new
//     reason.
//
// The package deliberately depends only on the adapter, audit, policy, session
// and terminal vocabularies and the standard library (imports_test.go enforces
// it): no notifier, no telemetry, no network and no user interface toolkit.
package supervise
