import type { SupervisionEvent } from "../types/relayer";

export function deliveryRequiresResync(event: SupervisionEvent): boolean {
  return (
    event.deliveryStatus === "uncertain" ||
    event.deliveryStatus === "failed"
  );
}

// policyDecisionInProgress reports a prompt the server answers itself: the
// policy chose allow or deny, and the server is about to write that answer or
// is writing it now. Since v0.8.9 the gateway does this as the desktop always
// has, so the screen must not offer the same prompt to a person meanwhile.
//
// Only "pending" and "delivering" count. An automatic prompt whose delivery
// failed or is uncertain keeps its automatic flag, but it now needs a person
// to stop or resynchronize the session, so it is no longer the policy's.
// When the server hands a prompt back (the adapter cannot encode the answer,
// an operator took the terminal, a limit tripped), it clears the flag and the
// prompt is a person's again.
export function policyDecisionInProgress(event: SupervisionEvent): boolean {
  return (
    event.evaluation.automatic &&
    (event.deliveryStatus === "pending" || event.deliveryStatus === "delivering")
  );
}

// awaitsPerson reports a prompt that is waiting for a person: to answer it,
// or to stop or resynchronize its session after a failed delivery. It is what
// the "pending" counts count and what the decision modal opens for by itself.
//
// Left out are the prompts the policy is answering, and any prompt whose
// answer is being delivered: nobody has anything to do about either until the
// server says how it ended, and if it ends badly the prompt comes back as
// uncertain or failed, which counts again.
export function awaitsPerson(event: SupervisionEvent): boolean {
  return event.deliveryStatus !== "delivering" && !policyDecisionInProgress(event);
}

export interface DecisionFailure {
  code: string;
  message: string;
}

// The core's refusals, recognised by the text both bridges carry (the desktop
// returns the core's error, the gateway puts err.Error() in the reply). Only
// the fixed message beside each one is ever shown: the raw text can carry a
// transport wrapper, a request id or anything else the path added.
const decisionRefusals: Array<[RegExp, DecisionFailure]> = [
  [/decision is already in progress/i, {
    code: "decision_in_flight",
    message: "Another answer to this prompt is already being delivered. The prompt now shows the server's state.",
  }],
  [/no longer the awaited event/i, {
    code: "decision_stale",
    message: "This prompt is no longer waiting: it was answered, withdrawn or replaced. Nothing was sent.",
  }],
  [/delivery state is indeterminate/i, {
    code: "decision_delivery_uncertain",
    message: "The delivery is indeterminate. Stop or resynchronize the session before any further input.",
  }],
  [/audit journal unavailable/i, {
    code: "decision_audit_unavailable",
    message: "The audit journal is unavailable, so no answer was sent.",
  }],
  [/engine is stopped|run is no longer active/i, {
    code: "decision_run_inactive",
    message: "The run is stopping or was replaced. No answer was sent.",
  }],
  [/permission denied|read-only/i, {
    code: "decision_read_only",
    message: "This connection is read-only. No answer was sent.",
  }],
  [/does not hold the session's terminal/i, {
    code: "decision_not_holder",
    message: "Another operator holds this terminal. No answer was sent.",
  }],
  [/cannot be encoded/i, {
    code: "decision_unsupported",
    message: "The adapter cannot encode this answer for this prompt. Nothing was sent.",
  }],
  [/empty answer/i, {
    code: "decision_empty",
    message: "An empty answer is not a decision. Nothing was sent.",
  }],
  [/must be one line of text/i, {
    code: "decision_invalid",
    message: "A typed answer must be one line of text, with no control characters and at most 4096 bytes. Nothing was sent.",
  }],
  [/only its deny is accepted/i, {
    code: "decision_deny_only",
    message: "The policy denies this prompt: only Deny is accepted. Nothing was sent.",
  }],
  [/typed at this request's terminal/i, {
    code: "decision_typed_at_terminal",
    message: "Keys were typed at this prompt's terminal while it was shown, and may have answered it. Answer it at the terminal. Nothing was sent.",
  }],
];

// decisionFailure turns a failed answer into the message the operator reads.
// Anything unrecognised, a timeout or a dropped socket included, is reported
// as unconfirmed rather than failed: the answer may have reached the server,
// and only the server's state, taken again right after, says what it did.
export function decisionFailure(error: unknown): DecisionFailure {
  const text = error instanceof Error ? error.message : typeof error === "string" ? error : "";
  for (const [pattern, failure] of decisionRefusals) {
    if (pattern.test(text)) return failure;
  }
  return {
    code: "decision_unconfirmed",
    message: "The answer could not be confirmed. The prompt now shows the server's state: check it before answering again.",
  };
}

// answerLocked reports whether this screen may send anything to the prompt at
// all: no semantic answer, no typed answer, and no keyboard shortcut.
//
// "delivering" locks whoever started it: this operator, another operator on
// another tab, or the policy. A second answer sent meanwhile is refused by the
// server with an in-flight error at best, and at worst is typed into whatever
// the agent prints once the first answer lands.
//
// A prompt typed into at its terminal is locked too: keys were typed there
// while it was shown and may have answered it, so the server takes no answer
// to it from any screen, and an answer sent anyway would reach whatever the
// agent asks next. It is answered at the terminal, and still waits for a
// person there.
export function answerLocked(event: SupervisionEvent): boolean {
  return (
    event.deliveryStatus === "delivering" ||
    policyDecisionInProgress(event) ||
    deliveryRequiresResync(event) ||
    typedAtTerminal(event)
  );
}

// typedAtTerminal reports a prompt the server made its terminal's: keys were
// typed there while it was shown.
export function typedAtTerminal(event: SupervisionEvent): boolean {
  return event.evaluation.reason === "typed_at_terminal";
}
