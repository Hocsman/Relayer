import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type { PreflightReport } from "../types/relayer";
import { PreflightReportView } from "./PreflightPanel";

describe("PreflightReportView", () => {
  it("renders a structured blocked report without exposing internal check identifiers", () => {
    const report: PreflightReport = {
      schemaVersion: 1,
      status: "blocked",
      platform: { os: "darwin", arch: "arm64", supported: true },
      configuration: { version: 1, legacy: false, agentCount: 1, policyRuleCount: 2 },
      audit: {
        enabled: true,
        mode: "metadata",
        location: "default",
        maxFileSizeMB: 10,
        maxFiles: 5,
      },
      tools: [{ profileID: "codex-cli", installation: "installed" }],
      agents: [{
        ordinal: 1,
        source: "configured",
        command: "direct",
        installation: "installed",
        adapter: "codex",
        adapterMaturity: "experimental",
        backend: "pty",
      }],
      checks: [
        {
          id: "internal.config.unavailable",
          scope: "configuration",
          status: "block",
          summary: "The saved configuration is not available.",
          remediation: "Create a valid configuration before starting.",
        },
      ],
    };

    const markup = renderToStaticMarkup(<PreflightReportView report={report} />);

    expect(markup).toContain("Startup must stay blocked");
    expect(markup).toContain("The saved configuration is not available.");
    expect(markup).toContain("Create a valid configuration before starting.");
    expect(markup).toContain("Blocking");
    expect(markup).toContain("Codex CLI");
    expect(markup).toContain("Agent 1 · configured");
    expect(markup).toContain("Direct command · Installed");
    expect(markup).toContain("pty · codex (experimental)");
    expect(markup).not.toContain("internal.config.unavailable");
    expect(markup).not.toContain("argv");
  });

  it("renders a ready report with its authoritative counts", () => {
    const report: PreflightReport = {
      schemaVersion: 1,
      status: "ready",
      platform: { os: "linux", arch: "amd64", supported: true },
      configuration: { version: 1, legacy: false, agentCount: 0, policyRuleCount: 0 },
      audit: {
        enabled: false,
        mode: "off",
        location: "disabled",
        maxFileSizeMB: 10,
        maxFiles: 5,
      },
      tools: [],
      agents: [],
      checks: Array.from({ length: 5 }, (_, index) => ({
        id: `pass-${index}`,
        scope: "platform",
        status: "pass" as const,
        summary: "Check passed.",
      })),
    };

    const markup = renderToStaticMarkup(<PreflightReportView report={report} />);

    expect(markup).toContain("Relayer is ready");
    expect(markup).toContain("Schema v1");
    expect(markup).toContain(">5<");
  });
});
