import { describe, expect, it } from "vitest";
import { supervisionEventKey } from "./eventKey";
import { nextPromptSelection } from "./promptQueue";
import type { SupervisionEvent } from "../types/relayer";

function prompt(
  id: string,
  overrides: Partial<SupervisionEvent> = {},
  automatic = false,
): SupervisionEvent {
  return {
    runID: "run-1",
    id,
    sessionID: "agent-a",
    agentID: "agent-a",
    adapter: "codex",
    type: "permission",
    summary: "Run the build command?",
    sensitive: false,
    risk: "low",
    timestamp: "2026-01-01T00:00:00Z",
    evaluation: {
      action: automatic ? "allow" : "ask",
      proposedAction: automatic ? "allow" : "ask",
      reason: automatic ? "rule_match" : "default_action",
      automatic,
      dryRun: false,
    },
    deliveryStatus: "pending",
    ...overrides,
  };
}

const key = (event: SupervisionEvent) => supervisionEventKey(event.runID, event.sessionID, event.id);

describe("nextPromptSelection", () => {
  it("opens the modal for a prompt a person has to answer", () => {
    const human = prompt("human");
    expect(nextPromptSelection([human], undefined, new Set())).toEqual({
      selectedKey: key(human),
      open: true,
      seen: key(human),
    });
  });

  it("closes the modal once nothing is pending", () => {
    expect(nextPromptSelection([], "stale", new Set(["stale"]))).toEqual({
      selectedKey: undefined,
      open: false,
    });
  });

  it("leaves the operator's choice alone while it is still pending", () => {
    const human = prompt("human");
    expect(nextPromptSelection([human], key(human), new Set([key(human)]))).toBeUndefined();
  });

  // The server answers this prompt itself in a moment. A modal that jumps up
  // for it invites exactly the second answer the server has to refuse, and
  // covers the screen for something nobody has to do.
  it("does not open the modal for a prompt the policy is about to answer", () => {
    const automatic = prompt("auto", {}, true);
    const selection = nextPromptSelection([automatic], undefined, new Set());
    expect(selection?.open ?? false).toBe(false);
    expect(selection?.seen).toBeUndefined();
  });

  it("does not open the modal for a prompt whose policy answer is being delivered", () => {
    const automatic = prompt("auto", { deliveryStatus: "delivering" }, true);
    expect(nextPromptSelection([automatic], undefined, new Set())?.open ?? false).toBe(false);
  });

  it("opens the person's prompt rather than the policy's one before it", () => {
    const automatic = prompt("auto", {}, true);
    const human = prompt("human-later");
    expect(nextPromptSelection([automatic, human], undefined, new Set())).toEqual({
      selectedKey: key(human),
      open: true,
      seen: key(human),
    });
  });

  // An automatic prompt is not marked seen while the policy holds it, so the
  // moment the server hands it back to a person it opens like any other.
  it("opens a prompt the policy handed back to a person", () => {
    const automatic = prompt("auto", {}, true);
    const seen = new Set<string>();
    const first = nextPromptSelection([automatic], undefined, seen);
    if (first?.seen) seen.add(first.seen);
    const handedBack = prompt("auto", {
      evaluation: { ...automatic.evaluation, action: "ask", automatic: false, reason: "fallback_unsupported" },
    });
    expect(nextPromptSelection([handedBack], first?.selectedKey, seen)).toEqual({
      selectedKey: key(handedBack),
      open: true,
      seen: key(handedBack),
    });
  });

  // A failed or uncertain automatic delivery keeps its flag but needs a person
  // to stop or resynchronize the session.
  it("opens an automatic prompt whose delivery failed", () => {
    const failed = prompt("auto", { deliveryStatus: "uncertain" }, true);
    expect(nextPromptSelection([failed], undefined, new Set())?.open).toBe(true);
  });

  it("keeps a policy prompt the operator opened themselves", () => {
    const automatic = prompt("auto", {}, true);
    expect(nextPromptSelection([automatic], key(automatic), new Set())).toBeUndefined();
  });

  it("moves on to a person's prompt when the selected one was answered", () => {
    const automatic = prompt("auto", {}, true);
    const human = prompt("human");
    const seen = new Set([key(human)]);
    expect(nextPromptSelection([automatic, human], "answered", seen)).toEqual({
      selectedKey: key(human),
      open: true,
    });
  });
});
