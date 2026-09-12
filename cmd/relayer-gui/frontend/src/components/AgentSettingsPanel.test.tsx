import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import { AgentSettingsPanel } from "./AgentSettingsPanel";
import type { FullSettingsView, RelayerBridge } from "../types/relayer";

function sampleFullSettings(): FullSettingsView {
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
        minimumArguments: 1,
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
        argv: ["claude", "--model", "claude-3-7-sonnet"],
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
    notifications: {
      enabled: true,
      bell: true,
      desktop: true,
      minSeverity: "info",
      webhooks: [
        {
          name: "Slack Ops",
          url: "https://hooks.slack.com/services/T00/B00/X00",
          format: "slack",
          minSeverity: "warning",
          timeout: "5s",
        },
      ],
    },
  };
}

function fakeBridge(settings = sampleFullSettings()): RelayerBridge {
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
    getAgentProfiles: async () => settings,
    saveAgentProfiles: async () => settings,
    saveAgentProfilesAndRestart: async () => ({} as never),
    getFullSettings: async () => settings,
    saveFullSettings: async () => settings,
    stopRun: async () => ({} as never),
    getAuditSummary: async () => ({} as never),
    getAuditEntries: async () => [],
    verifyAuditJournal: async () =>  ({} as never),
    exportAuditReport: async () => "",
    getTelemetrySnapshot: async () =>  ({} as never),
    on: () => () => {},
  };
}

describe("AgentSettingsPanel", () => {
  it("renders the settings dialog with title and loading indicator", () => {
    const bridge = fakeBridge();
    const html = renderToStaticMarkup(
      <AgentSettingsPanel
        bridge={bridge}
        runID="run-1"
        runStatus="running"
        pendingEvents={[]}
        onSave={async () => sampleFullSettings()}
        onSaveAndRestart={async () => ({} as never)}
        onClose={() => {}}
      />,
    );

    expect(html).contain('id="agents-title"');
    expect(html).contain("Agents");
    expect(html).contain("Loading the catalog");
  });
});
