import { describe, expect, it } from "vitest";
import { reasonText } from "./reason";

describe("reasonText", () => {
  // These are the web's equivalent of the TUI's LIMIT • ASK tag: the policy
  // would have answered, and a guard handed the prompt to a person instead.
  it.each([
    ["consecutive_auto_limit", "Automatic answer limit reached"],
    ["rate_limit_exceeded", "Automatic answer rate limit reached"],
    ["repeat_after_delivery", "Same question again right after an answer"],
    ["operator_attached", "An operator holds the terminal"],
    ["fallback_unsupported", "The adapter cannot encode the policy's answer"],
    ["fallback_stale", "The prompt changed before the answer arrived"],
    ["audit_unavailable", "Audit journal unavailable"],
    ["sensitive_event", "Confidential prompt"],
    ["dry_run", "Dry run"],
    ["typed_at_terminal", "Keys were typed at the terminal"],
  ])("says in words why %s", (code, words) => {
    expect(reasonText(code)).toContain(words);
  });

  it("names every guardrail that blocked an answer", () => {
    for (const code of [
      "sensitive_path_blocked",
      "outside_workspace_blocked",
      "destructive_command_blocked",
      "exfiltration_attempt_blocked",
      "guardrail_pattern_blocked",
    ]) {
      expect(reasonText(code)).toMatch(/guardrail/i);
    }
  });

  // A server newer than this page may add a code. Showing it raw is better
  // than hiding why a prompt was asked.
  it("falls back to the raw code it does not know", () => {
    expect(reasonText("process_exit_stale")).toBe("process_exit_stale");
    expect(reasonText("unknown")).toBe("unknown");
  });

  // The server allowlists reasons, but this page does not rely on it: the
  // fallback is one line of code-shaped text, never markup or a sentence an
  // agent could have written.
  it("keeps a raw fallback to one bounded line of code characters", () => {
    expect(reasonText("line_one\nline two <b>")).toBe("line_onelinetwob");
    expect(reasonText("x".repeat(200))?.length).toBe(64);
  });

  it("shows nothing for an empty reason", () => {
    expect(reasonText("")).toBeUndefined();
    expect(reasonText("  ")).toBeUndefined();
    expect(reasonText("<>")).toBeUndefined();
  });
});
