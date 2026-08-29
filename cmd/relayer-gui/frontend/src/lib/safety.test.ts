import { describe, expect, it } from "vitest";
import { promptContextLines, redactForDisplay, safeError, safeEventSummary } from "./safety";
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
    expect(safeEventSummary(sensitive)).toBe("Confidential input required");
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

// Go error strings are fragments by convention — lowercase, unpunctuated — and
// the engine's are written that way so ST1005 applies to the whole module. The
// interface is what turns one into a sentence, so that the operator never reads
// "the Relayer engine is stopped" as a paragraph.
describe("safeError sentence casing", () => {
  it("casts an engine fragment into a sentence", () => {
    expect(safeError(new Error("the Relayer engine is stopped")))
      .toBe("The Relayer engine is stopped.");
    expect(safeError(new Error("invalid terminal dimensions")))
      .toBe("Invalid terminal dimensions.");
  });

  it("leaves a message that is already a sentence alone", () => {
    expect(safeError(new Error("Configuration has changed. Reload first.")))
      .toBe("Configuration has changed. Reload first.");
    expect(safeError(new Error("Did the run stop?"))).toBe("Did the run stop?");
  });

  it("still redacts before casing, so a secret cannot be capitalised into view", () => {
    const cast = safeError(new Error("api_key=sk-live-4b91ce rejected"));
    expect(cast).not.toContain("sk-live-4b91ce");
    expect(cast).toContain("[REDACTED]");
    expect(cast.endsWith(".")).toBe(true);
  });

  it("uses the fallback when there is no message, without touching it", () => {
    expect(safeError(undefined, "The run change failed.")).toBe("The run change failed.");
    expect(safeError(new Error("   "), "The run change failed.")).toBe("The run change failed.");
  });
});
