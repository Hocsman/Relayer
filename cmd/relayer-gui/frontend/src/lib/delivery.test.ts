import { describe, expect, it } from "vitest";
import {
  answerLocked,
  awaitsPerson,
  deliveryRequiresResync,
  policyDecisionInProgress,
} from "./delivery";
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

describe("policyDecisionInProgress", () => {
  it("is the policy's while the server has yet to write its answer", () => {
    expect(policyDecisionInProgress(event("pending", true))).toBe(true);
    expect(policyDecisionInProgress(event("delivering", true))).toBe(true);
  });

  it("is a person's once the server hands it back", () => {
    expect(policyDecisionInProgress(event("pending", false))).toBe(false);
  });

  // The flag stays on a failed automatic delivery, but somebody now has to
  // stop or resynchronize the session: it must reach a person, not hide.
  it("is a person's again once the policy's delivery failed or became uncertain", () => {
    expect(policyDecisionInProgress(event("failed", true))).toBe(false);
    expect(policyDecisionInProgress(event("uncertain", true))).toBe(false);
  });
});

describe("awaitsPerson", () => {
  it("waits for a person on a pending prompt the policy left to one", () => {
    expect(awaitsPerson(event("pending", false))).toBe(true);
  });

  it("does not wait for anyone while the policy answers or an answer is delivered", () => {
    expect(awaitsPerson(event("pending", true))).toBe(false);
    expect(awaitsPerson(event("delivering", true))).toBe(false);
    expect(awaitsPerson(event("delivering", false))).toBe(false);
  });

  it("waits for a person again once any delivery failed or became uncertain", () => {
    expect(awaitsPerson(event("failed", true))).toBe(true);
    expect(awaitsPerson(event("uncertain", false))).toBe(true);
  });
});

describe("answerLocked", () => {
  it("leaves a pending prompt a person must answer open", () => {
    expect(answerLocked(event("pending", false))).toBe(false);
  });

  it("locks a prompt whose answer is being delivered, whoever sent it", () => {
    expect(answerLocked(event("delivering", false))).toBe(true);
    expect(answerLocked(event("delivering", true))).toBe(true);
  });

  it("locks an automatic prompt the server has not answered yet", () => {
    expect(answerLocked(event("pending", true))).toBe(true);
  });

  it("locks a prompt whose delivery failed or is uncertain", () => {
    expect(answerLocked(event("failed", false))).toBe(true);
    expect(answerLocked(event("uncertain", true))).toBe(true);
  });
});
