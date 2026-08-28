import { describe, expect, it } from "vitest";
import {
  discardUnavailableLine,
  lineInputDisabled,
  submitUncontrolledLine,
} from "./lineInput";

describe("safe uncontrolled line input", () => {
  it("clears the field before awaiting native delivery", async () => {
    const secret = "line-secret-sentinel-4ed892";
    const input = { value: secret };
    let release: () => void = () => {};
    const pending = new Promise<void>((resolve) => { release = resolve; });
    let delivered = "";

    const submission = submitUncontrolledLine(input, async (line) => {
      expect(input.value).toBe("");
      delivered = line;
      await pending;
    });

    expect(input.value).toBe("");
    expect(JSON.stringify(input)).not.toContain(secret);
    expect(delivered).toBe(secret);
    release();
    await submission;
    expect(input.value).toBe("");
  });

  it("clears the field even when delivery rejects", async () => {
    const secret = "line-secret-sentinel-rejected";
    const input = { value: secret };
    await expect(submitUncontrolledLine(input, async () => {
      throw new Error("safe static failure");
    })).rejects.toThrow("safe static failure");
    expect(input.value).toBe("");
    expect(JSON.stringify(input)).not.toContain(secret);
  });

  it.each([
    ["prompt", { running: true, attached: false, status: "waiting" }, true],
    ["frozen", { running: true, attached: false, status: "running", inputFrozen: true }, false],
    ["attach", { running: true, attached: true, status: "attached" }, false],
    ["exit", { running: false, attached: false, status: "exited" }, false],
    ["stop", { running: false, attached: false, status: "running" }, false],
  ])("discards DOM text when the card becomes unavailable: %s", (_reason, agent, waiting) => {
    const secret = `unsent-${_reason}-secret`;
    const input = { value: secret };
    const disabled = lineInputDisabled(agent, waiting, false);
    expect(disabled).toBe(true);
    discardUnavailableLine(input, disabled, false);
    expect(input.value).toBe("");
    expect(JSON.stringify(input)).not.toContain(secret);
  });

  it("discards DOM text when the run or session identity changes", () => {
    const secret = "unsent-run-change-secret";
    const input = { value: secret };
    discardUnavailableLine(input, false, true);
    expect(input.value).toBe("");
    expect(JSON.stringify(input)).not.toContain(secret);
  });

  it("keeps unsent DOM text while the same running card remains available", () => {
    const input = { value: "ordinary draft" };
    discardUnavailableLine(input, false, false);
    expect(input.value).toBe("ordinary draft");
  });
});
