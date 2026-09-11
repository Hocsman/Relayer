import { renderToStaticMarkup } from "react-dom/server";
import { describe, expect, it } from "vitest";
import type {
  AuditEntryView,
  AuditSummaryView,
  AuditVerificationView,
  RelayerBridge,
} from "../types/relayer";
import { AuditContentView, AuditPanel } from "./AuditPanel";

function sampleSummary(): AuditSummaryView {
  return {
    path: "/home/user/.config/relayer/audit/audit.jsonl",
    totalEntries: 2,
    runsCount: 1,
    sessionsCount: 1,
    agentCounts: { "claude-code": 2 },
    kindCounts: { policy_evaluated: 1, decision: 1 },
    decisionsCount: { allow: 1, deny: 1 },
    actorsCount: { human: 1, policy: 1, system: 0 },
    outcomesCount: { in_flight: 1, applied: 1 },
    sensitiveCount: 1,
    firstTimestamp: "2026-09-11T12:00:00Z",
    lastTimestamp: "2026-09-11T12:00:25Z",
  };
}

function sampleVerification(passed = true): AuditVerificationView {
  return {
    path: "/home/user/.config/relayer/audit/audit.jsonl",
    totalLines: 2,
    totalRuns: 1,
    validLines: passed ? 2 : 1,
    passed,
    issues: passed
      ? []
      : [
          {
            line: 2,
            entryID: "ent-2",
            message: "sequence gap or reorder (expected 2, got 5)",
          },
        ],
  };
}

function sampleEntries(): AuditEntryView[] {
  return [
    {
      sequence: 1,
      timestamp: "2026-09-11T12:00:00Z",
      entryID: "ent-1",
      runID: "run-1",
      sessionID: "sess-1",
      agentID: "claude-code",
      backend: "pty",
      adapter: "claude",
      kind: "policy_evaluated",
      eventType: "permission",
      risk: "high",
      rule: "deny_curl",
      decision: "deny",
      decisionBy: "policy",
      outcome: "in_flight",
      reason: "curl command matched restricted pattern",
      summary: "curl execution intercepted",
      sensitive: true,
      metadata: { mode: "enforce" },
    },
    {
      sequence: 2,
      timestamp: "2026-09-11T12:00:25Z",
      entryID: "ent-2",
      runID: "run-1",
      sessionID: "sess-1",
      agentID: "claude-code",
      backend: "pty",
      adapter: "claude",
      kind: "decision",
      eventType: "permission",
      decision: "allow",
      decisionBy: "human",
      outcome: "applied",
      reason: "decision_selected",
      sensitive: false,
    },
  ];
}

describe("AuditContentView", () => {
  it("renders verified integrity status and journal summary metrics", () => {
    const markup = renderToStaticMarkup(
      <AuditContentView
        summary={sampleSummary()}
        verification={sampleVerification(true)}
        entries={sampleEntries()}
        selectedAgent=""
        selectedKind=""
        limit={50}
        expandedEntryKey={null}
        onSelectAgent={() => {}}
        onSelectKind={() => {}}
        onSelectLimit={() => {}}
        onToggleExpand={() => {}}
        onRefresh={() => {}}
      />,
    );

    // Verification banner
    expect(markup).toContain("Cryptographic Sequence Verified");
    expect(markup).toContain("Continuity verified across 2 record(s)");

    // Metrics
    expect(markup).toContain("2"); // total entries
    expect(markup).toContain("1 run");
    expect(markup).toContain("1 session");
    expect(markup).toContain("Human: <strong>1</strong>");
    expect(markup).toContain("Policy: <strong>1</strong>");
    expect(markup).toContain("Allow: 1");
    expect(markup).toContain("Deny: 1");
    expect(markup).toContain("sensitive events flagged");

    // Entries table
    expect(markup).toContain("claude-code");
    expect(markup).toContain("policy_evaluated");
    expect(markup).toContain("decision");
    expect(markup).toContain("deny_curl");
  });

  it("renders integrity failure with detected issues list", () => {
    const markup = renderToStaticMarkup(
      <AuditContentView
        summary={sampleSummary()}
        verification={sampleVerification(false)}
        entries={sampleEntries()}
        selectedAgent=""
        selectedKind=""
        limit={50}
        expandedEntryKey={null}
        onSelectAgent={() => {}}
        onSelectKind={() => {}}
        onSelectLimit={() => {}}
        onToggleExpand={() => {}}
        onRefresh={() => {}}
      />,
    );

    expect(markup).toContain("Integrity Issues Detected");
    expect(markup).toContain("1 verification issue(s) detected");
    expect(markup).toContain("Line 2:");
    expect(markup).toContain("sequence gap or reorder");
    expect(markup).toContain("ent-2");
  });

  it("renders empty state when no entries match filters", () => {
    const markup = renderToStaticMarkup(
      <AuditContentView
        summary={sampleSummary()}
        verification={sampleVerification(true)}
        entries={[]}
        selectedAgent="unknown-agent"
        selectedKind=""
        limit={50}
        expandedEntryKey={null}
        onSelectAgent={() => {}}
        onSelectKind={() => {}}
        onSelectLimit={() => {}}
        onToggleExpand={() => {}}
        onRefresh={() => {}}
      />,
    );

    expect(markup).toContain("No audit entries match the current filter criteria.");
  });

  it("renders loading and error states cleanly", () => {
    const loadingMarkup = renderToStaticMarkup(
      <AuditContentView
        loading={true}
        entries={[]}
        selectedAgent=""
        selectedKind=""
        limit={50}
        expandedEntryKey={null}
        onSelectAgent={() => {}}
        onSelectKind={() => {}}
        onSelectLimit={() => {}}
        onToggleExpand={() => {}}
        onRefresh={() => {}}
      />,
    );
    expect(loadingMarkup).toContain("Loading audit journal…");

    const errorMarkup = renderToStaticMarkup(
      <AuditContentView
        error={true}
        entries={[]}
        selectedAgent=""
        selectedKind=""
        limit={50}
        expandedEntryKey={null}
        onSelectAgent={() => {}}
        onSelectKind={() => {}}
        onSelectLimit={() => {}}
        onToggleExpand={() => {}}
        onRefresh={() => {}}
      />,
    );
    expect(errorMarkup).toContain("Audit journal unavailable");
  });

  it("renders expanded entry details with summary and metadata", () => {
    const markup = renderToStaticMarkup(
      <AuditContentView
        summary={sampleSummary()}
        verification={sampleVerification(true)}
        entries={sampleEntries()}
        selectedAgent=""
        selectedKind=""
        limit={50}
        expandedEntryKey="ent-1"
        onSelectAgent={() => {}}
        onSelectKind={() => {}}
        onSelectLimit={() => {}}
        onToggleExpand={() => {}}
        onRefresh={() => {}}
      />,
    );

    expect(markup).toContain("Sanitized Summary");
    expect(markup).toContain("curl execution intercepted");
    expect(markup).toContain("Sensitive Event");
    expect(markup).toContain("mode");
    expect(markup).toContain("enforce");
  });
});

describe("AuditPanel", () => {
  it("renders dialog shell with accessibility attributes and export buttons", () => {
    const dummyBridge: RelayerBridge = {
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
      getAuditSummary: async () => sampleSummary(),
      getAuditEntries: async () => sampleEntries(),
      verifyAuditJournal: async () => sampleVerification(true),
      exportAuditReport: async () => "[]",
      on: () => () => {},
    };

    const markup = renderToStaticMarkup(
      <AuditPanel bridge={dummyBridge} onClose={() => {}} />,
    );

    expect(markup).toContain('role="dialog"');
    expect(markup).toContain('aria-modal="true"');
    expect(markup).toContain('id="audit-title"');
    expect(markup).toContain("Audit Trail &amp; Integrity");
    expect(markup).toContain("Export JSON");
    expect(markup).toContain("Export CSV");
  });
});
