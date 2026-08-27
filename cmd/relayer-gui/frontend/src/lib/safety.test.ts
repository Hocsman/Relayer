import { describe, expect, it } from "vitest";
import { redactForDisplay, safeEventSummary } from "./safety";
import type { SupervisionEvent } from "../types/relayer";

function event(overrides: Partial<SupervisionEvent> = {}): SupervisionEvent {
  return {
    id: "event-1",
    sessionID: "agent-a",
    agentID: "agent-a",
    adapter: "generic",
    type: "confirmation",
    summary: "Overwrite file? [Y/n]",
    sensitive: false,
    risk: "unknown",
    timestamp: "2026-01-01T00:00:00Z",
    evaluation: {
      action: "ask",
      proposedAction: "ask",
      reason: "default_action",
      automatic: false,
      dryRun: false,
    },
    deliveryStatus: "pending",
    ...overrides,
  };
}

describe("frontend redaction", () => {
  it.each([
    ["token=super-secret", "super-secret"],
    ["Authorization: Bearer abc.def.ghi", "abc.def.ghi"],
    ["https://alice:secret@example.test/private", "alice:secret"],
    ["eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxMjM0NTY3ODkwIn0.ABCDEFGHIJKLMNOP", "eyJhbGci"],
  ])("removes credentials from %s", (input, leakedValue) => {
    expect(redactForDisplay(input)).not.toContain(leakedValue);
  });

  it("never renders a sensitive summary", () => {
    const sensitive = event({
      type: "credential",
      sensitive: true,
      summary: "password=must-not-appear",
    });
    expect(safeEventSummary(sensitive)).toBe("Saisie confidentielle requise");
    expect(safeEventSummary(sensitive)).not.toContain("must-not-appear");
  });
});
