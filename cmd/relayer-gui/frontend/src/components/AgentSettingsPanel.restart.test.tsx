/** @vitest-environment jsdom */
import { act } from "react";
import { createRoot, type Root } from "react-dom/client";
import { afterEach, beforeEach, describe, expect, it } from "vitest";
import { AgentSettingsPanel } from "./AgentSettingsPanel";
import type { FullSettingsView, RelayerBridge } from "../types/relayer";

declare global {
  // eslint-disable-next-line no-var
  var IS_REACT_ACT_ENVIRONMENT: boolean;
}

let container: HTMLDivElement;
let root: Root;

beforeEach(() => {
  globalThis.IS_REACT_ACT_ENVIRONMENT = true;
  container = document.createElement("div");
  document.body.appendChild(container);
  root = createRoot(container);
});

afterEach(() => {
  act(() => root.unmount());
  container.remove();
});

function settings(): FullSettingsView {
  return {
    configPath: "/test/config.yaml",
    revision: "rev-1",
    editable: true,
    minProfiles: 1,
    maxProfiles: 8,
    restartRequired: false,
    catalog: [
      {
        id: "claude-code",
        name: "Claude Code",
        description: "Anthropic Claude coding agent",
        adapter: "claude-code",
        adapterStatus: "stable",
        installed: true,
        installStatus: "installed",
        requiresCustomArgv: false,
        minimumArguments: 0,
        argumentPrefix: [],
        defaultArgv: ["claude"],
      },
    ],
    profiles: [
      {
        id: "agent-1",
        name: "Agent 1",
        presetID: "claude-code",
        cwd: "/test",
        backend: "pty",
        adapter: "claude-code",
        argv: ["claude"],
        locked: false,
      },
    ],
    security: {
      profile: "developer-friendly",
      defaultAction: "ask",
      dryRun: false,
      blockDestructive: true,
      blockExfiltration: true,
      blockSensitivePaths: true,
      blockOutsideWorkspace: false,
      workspaceRoot: "/test",
      rateLimitPerMinute: 30,
      maxConsecutiveAutoDecisions: 10,
    },
    notifications: { enabled: false, bell: false, desktop: false, minSeverity: "info", webhooks: [] },
  };
}

function bridgeWhoseReload(reload: () => Promise<FullSettingsView>): RelayerBridge {
  let loads = 0;
  const full = () => (loads++ === 0 ? Promise.resolve(settings()) : reload());
  return {
    getState: async () => ({} as never),
    runPreflight: async () => ({} as never),
    submitDecision: async () => {},
    submitAutomaticDecision: async () => {},
    submitLine: async () => {},
    resizeSession: async () => {},
    stopSession: async () => {},
    startSession: async () => {},
    restartSession: async () => {},
    getAgentProfiles: full,
    saveAgentProfiles: async () => settings(),
    saveAgentProfilesAndRestart: async () => ({} as never),
    getFullSettings: full,
    saveFullSettings: async () => settings(),
    stopRun: async () => ({} as never),
    getAuditSummary: async () => ({} as never),
    getAuditEntries: async () => [],
    verifyAuditJournal: async () => ({} as never),
    exportAuditReport: async () => "",
    getTelemetrySnapshot: async () => ({} as never),
    on: () => () => {},
  };
}

async function settle() {
  await act(async () => {
    await new Promise((resolve) => setTimeout(resolve, 0));
  });
}

async function failARestart(bridge: RelayerBridge): Promise<string> {
  act(() => {
    root.render(
      <AgentSettingsPanel
        bridge={bridge}
        runID="run-1"
        runStatus="idle"
        pendingEvents={[]}
        onSave={async () => settings()}
        onSaveAndRestart={async () => {
          throw new Error("the new run did not start");
        }}
        onClose={() => {}}
      />,
    );
  });
  await settle();
  const start = Array.from(container.querySelectorAll("button")).find((button) =>
    button.textContent === "Start the agents");
  expect(start, "the start button").toBeDefined();
  expect(start!.disabled).toBe(false);
  act(() => start!.click());
  await settle();
  await settle();
  return container.querySelector(".settings-save-error")?.textContent ?? "";
}

describe("a failed Save and restart", () => {
  it("says the form was reloaded when it was", async () => {
    const error = await failARestart(bridgeWhoseReload(async () => settings()));
    expect(error).toContain("reloaded from the file");
  });

  // The form used to claim a reload it had not done, leaving the unsaved
  // drafts on screen as if they were the file.
  it("does not claim a reload that failed", async () => {
    const error = await failARestart(bridgeWhoseReload(async () => {
      throw new Error("the engine is gone");
    }));
    expect(error).not.toContain("reloaded from the file");
    expect(error).toContain("could not be reloaded");
  });
});
