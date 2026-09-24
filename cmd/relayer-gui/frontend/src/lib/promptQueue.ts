import { awaitsPerson } from "./delivery";
import { supervisionEventKey } from "./eventKey";
import type { SupervisionEvent } from "../types/relayer";

export interface PromptSelection {
  selectedKey?: string;
  open: boolean;
  // A prompt the modal has now been opened for, which it must not reopen for
  // on its own again.
  seen?: string;
}

function keyOf(event: SupervisionEvent): string {
  return supervisionEventKey(event.runID, event.sessionID, event.id);
}

// nextPromptSelection decides what the decision modal shows after the queue
// changed, or returns undefined to leave the selection as it is.
//
// The modal opens by itself only for a prompt that awaits a person. A prompt
// the policy is answering stays in the list, where the operator can watch it
// and open it on purpose, but it never jumps up over the screen: the server
// answers it in a moment, and a modal offering it invites exactly the second
// answer the server has to refuse.
//
// Such a prompt is deliberately not marked seen. If the server hands it back
// to a person (the adapter cannot encode the answer, an operator took the
// terminal, a limit tripped) or its delivery fails, it then opens like a new
// one, which is the moment somebody does have to act.
export function nextPromptSelection(
  pending: SupervisionEvent[],
  selectedKey: string | undefined,
  seen: ReadonlySet<string>,
): PromptSelection | undefined {
  if (pending.length === 0) return { selectedKey: undefined, open: false };

  const forPerson = pending.filter(awaitsPerson);
  const unseen = forPerson.find((event) => !seen.has(keyOf(event)));
  if (unseen) return { selectedKey: keyOf(unseen), open: true, seen: keyOf(unseen) };

  if (!selectedKey || !pending.some((event) => keyOf(event) === selectedKey)) {
    return forPerson.length > 0
      ? { selectedKey: keyOf(forPerson[0]), open: true }
      : { selectedKey: undefined, open: false };
  }
  return undefined;
}
