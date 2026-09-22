import { describe, expect, it } from "vitest";
import type { SecuritySettings } from "../types/relayer";
import { limitValue, presetSettings, saveAndRestartRequest } from "./AgentSettingsPanel";

const current: SecuritySettings = {
  profile: "custom",
  defaultAction: "deny",
  dryRun: true,
  blockDestructive: false,
  blockExfiltration: false,
  blockSensitivePaths: false,
  blockOutsideWorkspace: false,
  workspaceRoot: "/home/me/project",
  rateLimitPerMinute: 0,
  maxConsecutiveAutoDecisions: 0,
};

const strictFromEngine: SecuritySettings = {
  profile: "strict",
  defaultAction: "ask",
  dryRun: false,
  blockDestructive: true,
  blockExfiltration: true,
  blockSensitivePaths: true,
  blockOutsideWorkspace: true,
  workspaceRoot: "",
  rateLimitPerMinute: 10,
  maxConsecutiveAutoDecisions: 1,
};

describe("limitValue", () => {
  it("accepts zero, which the engine treats as no limit", () => {
    expect(limitValue("0")).toBe(0);
  });

  it("reads an empty or invalid entry as zero instead of inventing a limit", () => {
    // The fields used to fall back to 30 and 10, so a limit could not be removed.
    expect(limitValue("")).toBe(0);
    expect(limitValue("abc")).toBe(0);
    expect(limitValue("-5")).toBe(0);
  });

  it("keeps a real value", () => {
    expect(limitValue("42")).toBe(42);
  });
});

describe("presetSettings", () => {
  it("fills in the engine's values for the preset, not a copy kept in the form", () => {
    const next = presetSettings(current, "strict", { strict: strictFromEngine });
    expect(next.rateLimitPerMinute).toBe(10);
    expect(next.maxConsecutiveAutoDecisions).toBe(1);
    expect(next.blockDestructive).toBe(true);
    expect(next.defaultAction).toBe("ask");
  });

  it("keeps dry-run: choosing a preset in a dry-run configuration must not go live", () => {
    // current is in dry-run; every preset the engine sends has it off.
    const next = presetSettings(current, "strict", { strict: strictFromEngine });
    expect(next.dryRun).toBe(true);
    expect(presetSettings({ ...current, dryRun: false }, "strict", { strict: strictFromEngine }).dryRun).toBe(false);
  });

  it("keeps the user's workspace path: choosing a preset does not move the workspace", () => {
    const next = presetSettings(current, "strict", { strict: strictFromEngine });
    expect(next.workspaceRoot).toBe("/home/me/project");
  });

  it("changes only the profile name when the engine sent no preset", () => {
    const next = presetSettings(current, "strict", undefined);
    expect(next).toEqual({ ...current, profile: "strict" });
  });
});

describe("saveAndRestartRequest", () => {
  it("carries every changed tab in the one transactional request", () => {
    const notifications = { enabled: true, bell: false, desktop: true, minSeverity: "info", webhooks: [] };
    const request = saveAndRestartRequest({
      runID: "run-1",
      revision: "rev-1",
      profiles: [],
      security: strictFromEngine,
      notifications,
    });
    expect(request).toEqual({
      expectedRunID: "run-1",
      expectedRevision: "rev-1",
      profiles: [],
      security: strictFromEngine,
      notifications,
    });
  });

  it("leaves an untouched tab out, so it is not rewritten", () => {
    const request = saveAndRestartRequest({ runID: "run-1", revision: "rev-1", profiles: [] });
    expect("security" in request).toBe(false);
    expect("notifications" in request).toBe(false);
  });
});
