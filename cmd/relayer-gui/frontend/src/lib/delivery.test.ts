import { describe, expect, it } from "vitest";
import { deliveryRequiresResync } from "./delivery";
import type { SupervisionEvent } from "../types/relayer";

function event(
  deliveryStatus: SupervisionEvent["deliveryStatus"],
  automatic: boolean,
): SupervisionEvent {
  return {
    runID: "run-1",
    id: "event-1",
    sessionID: "agent-a",
    agentID: "agent-a",
    adapter: "generic",
    type: "confirmation",
    summary: "Confirm?",
    sensitive: false,
    risk: "unknown",
    timestamp: "2026-01-01T00:00:00Z",
    evaluation: {
      action: "ask",
      proposedAction: "ask",
      reason: "test",
      automatic,
      dryRun: false,
    },
    deliveryStatus,
  };
}

describe("deliveryRequiresResync", () => {
  it("fails closed after uncertain delivery", () => {
    expect(deliveryRequiresResync(event("uncertain", false))).toBe(true);
  });

  it("fails closed after a failed automatic attempt", () => {
    expect(deliveryRequiresResync(event("failed", true))).toBe(true);
  });

  it("keeps a failed manual delivery locked", () => {
    expect(deliveryRequiresResync(event("failed", false))).toBe(true);
  });
});
