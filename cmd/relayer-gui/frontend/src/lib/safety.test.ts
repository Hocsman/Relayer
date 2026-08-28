import { describe, expect, it } from "vitest";
import { promptContextLines, redactForDisplay, safeEventSummary } from "./safety";
import type { SupervisionEvent } from "../types/relayer";

function event(overrides: Partial<SupervisionEvent> = {}): SupervisionEvent {
  return {
    runID: "run-1",
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

// The decision modal shows the tail of the pane so a decision is not made on a
// one-line summary. The bytes are already on the agent card, but this surface
// applies the same redaction as every other safe display.
describe("promptContextLines", () => {
  it("keeps only the last lines and drops trailing blanks", () => {
    const output = Array.from({ length: 40 }, (_, index) => `line ${index}`).join("\n") + "\n\n\n";
    const lines = promptContextLines(output, 5);
    expect(lines).toEqual(["line 35", "line 36", "line 37", "line 38", "line 39"]);
  });

  it("redacts a credential that scrolled into the tail", () => {
    const lines = promptContextLines("exporting\napi_key=sk-live-4b91ce\nready");
    expect(lines.join("\n")).not.toContain("sk-live-4b91ce");
    expect(lines.join("\n")).toContain("[REDACTED]");
  });

  it("truncates a single enormous line instead of widening the dialog", () => {
    const lines = promptContextLines("x".repeat(5000));
    expect(lines).toHaveLength(1);
    expect(lines[0].length).toBeLessThanOrEqual(201);
  });

  it("returns nothing for a pane that produced no output", () => {
    expect(promptContextLines("")).toEqual([]);
    expect(promptContextLines("\n\n  \n")).toEqual([]);
  });
});
