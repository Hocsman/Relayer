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

// answerLocked reports whether this screen may send anything to the prompt at
// all: no semantic answer, no typed answer, and no keyboard shortcut.
//
// "delivering" locks whoever started it: this operator, another operator on
// another tab, or the policy. A second answer sent meanwhile is refused by the
// server with an in-flight error at best, and at worst is typed into whatever
// the agent prints once the first answer lands.
export function answerLocked(event: SupervisionEvent): boolean {
  return (
    event.deliveryStatus === "delivering" ||
    policyDecisionInProgress(event) ||
    deliveryRequiresResync(event)
  );
}
