import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { RelayerBridge, TelemetrySnapshotView } from "../types/relayer";
import { ObservabilityPanel } from "./ObservabilityPanel";

function sampleTelemetrySnapshot(): TelemetrySnapshotView {
  return {
    timestamp: "2026-09-11T19:30:00Z",
    enabled: true,
    prometheusEnabled: true,
    prometheusAddress: ":9090",
    otlpEnabled: true,
    otlpEndpoint: "http://localhost:4318/v1/metrics",
    sessionsActive: 2,
    eventsPending: 1,
    sessionsTotal: 3,
    eventsDetectedTotal: 15,
    eventsWithdrawnTotal: 1,
    decisionsTotal: 12,
    decisionsBreakdown: {
      allow: 7,
      deny: 2,
      autoAllow: 2,
      autoDeny: 1,
      custom: 0,
    },
    operatorInputsTotal: 4,
    guardrailsTotal: 2,
    guardrailsBreakdown: {
      destructive_command: 1,
      sensitive_paths: 1,
    },
    averageReactionTime: 1.75,
    decisionDurations: [
      { le: 0.5, label: "< 0.5s", count: 1 },
      { le: 1.0, label: "0.5s - 1s", count: 3 },
      { le: 2.0, label: "1s - 2s", count: 4 },
      { le: 5.0, label: "2s - 5s", count: 1 },
      { le: 10.0, label: "5s - 10s", count: 0 },
      { le: 30.0, label: "10s - 30s", count: 0 },
      { le: 60.0, label: "30s - 60s", count: 0 },
      { le: 300.0, label: "> 60s", count: 0 },
    ],
  };
}

function fakeBridge(snapshot = sampleTelemetrySnapshot()): RelayerBridge {
  return {
    getState: async () => ({} as never),
    runPreflight: async () => ({} as never),
    submitDecision: async () => {},
    submitAutomaticDecision: async () => {},
    submitLine: async () => {},
    resizeSession: async () => {},
    stopSession: async () => {},
    getAgentProfiles: async () => ({} as never),
    saveAgentProfiles: async () => ({} as never),
    saveAgentProfilesAndRestart: async () => ({} as never),
    stopRun: async () => ({} as never),
    getAuditSummary: async () => ({} as never),
    getAuditEntries: async () => [],
    verifyAuditJournal: async () => ({} as never),
    exportAuditReport: async () => "",
    getTelemetrySnapshot: async () => snapshot,
    on: () => () => {},
  };
}

describe("ObservabilityPanel", () => {
  it("renders the dialog structure with accessibility attributes and title", () => {
    const bridge = fakeBridge();
    const html = renderToStaticMarkup(
      <ObservabilityPanel bridge={bridge} onClose={() => {}} />,
    );

    expect(html).toContain('role="dialog"');
    expect(html).toContain('aria-modal="true"');
    expect(html).toContain('aria-labelledby="observability-title"');
    expect(html).toContain("Observability &amp; Metrics");
    expect(html).toContain("Live (3s)");
  });

  it("renders loading state initially before snapshot settles", () => {
    const bridge = fakeBridge();
    const html = renderToStaticMarkup(
      <ObservabilityPanel bridge={bridge} onClose={() => {}} />,
    );

    // Initial server render contains the loading skeleton
    expect(html).toContain("Loading telemetry metrics…");
  });
});
