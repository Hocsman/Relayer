import { useCallback, useEffect, useState } from "react";
import type { PreflightCheckStatus, PreflightReport, RelayerBridge } from "../types/relayer";

interface PreflightPanelProps {
  bridge: RelayerBridge;
  onClose(): void;
}

const overallCopy: Record<PreflightReport["status"], { title: string; detail: string }> = {
  ready: {
    title: "Relayer is ready",
    detail: "No blocker was detected for the saved configuration.",
  },
  warning: {
    title: "Relayer can start, with warnings",
    detail: "Nothing blocks startup, but one or more checks need your attention.",
  },
  blocked: {
    title: "Startup must stay blocked",
    detail: "Fix the reported blockers before launching the agents.",
  },
};

const checkLabels: Record<PreflightCheckStatus, string> = {
  pass: "Passed",
  warning: "Warning",
  block: "Blocking",
};

export function PreflightPanel({ bridge, onClose }: PreflightPanelProps) {
  const [report, setReport] = useState<PreflightReport>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(false);

  const refresh = useCallback(async () => {
    setLoading(true);
    setError(false);
    try {
      setReport(await bridge.runPreflight());
    } catch {
      // Native errors can contain local details. The panel deliberately uses
      // a fixed message and never reflects the transport error.
      setReport(undefined);
      setError(true);
    } finally {
      setLoading(false);
    }
  }, [bridge]);

  useEffect(() => {
    let active = true;
    setLoading(true);
    setError(false);
    void bridge.runPreflight().then(
      (next) => {
        if (!active) return;
        setReport(next);
        setLoading(false);
      },
      () => {
        if (!active) return;
        setReport(undefined);
        setError(true);
        setLoading(false);
      },
    );
    return () => {
      active = false;
    };
  }, [bridge]);

  return (
    <div className="preflight-layer">
      <section
        className="preflight-panel"
        role="dialog"
        aria-modal="true"
        aria-labelledby="preflight-title"
      >
        <header className="preflight-panel__header">
          <div>
            <span className="eyebrow">Read-only local checks</span>
            <h1 id="preflight-title">System health</h1>
            <p>Configuration, policies, audit, backends and configured tools.</p>
          </div>
          <button
            className="icon-button"
            type="button"
            onClick={onClose}
            aria-label="Close system health"
            autoFocus
          >
            ×
          </button>
        </header>

        <div className="preflight-panel__body" aria-live="polite">
          {loading && !report ? (
            <div className="preflight-panel__loading">
              <span className="settings-spinner" aria-hidden="true" />
              Local checks running…
            </div>
          ) : error || !report ? (
            <div className="preflight-panel__failure" role="alert">
              <span aria-hidden="true">!</span>
              <div>
                <strong>Checks unavailable</strong>
                <p>No local detail is shown. Startup must not be considered validated.</p>
              </div>
            </div>
          ) : (
            <PreflightReportView report={report} refreshing={loading} />
          )}
        </div>

        <footer className="preflight-panel__footer">
          <p>This report shows no commands, no environment variables and no full paths.</p>
          <button
            className="button button--primary"
            type="button"
            onClick={() => void refresh()}
            disabled={loading}
          >
            {loading ? "Checking…" : "Refresh"}
          </button>
        </footer>
      </section>
    </div>
  );
}

export function PreflightReportView({
  report,
  refreshing = false,
}: {
  report: PreflightReport;
  refreshing?: boolean;
}) {
  const copy = overallCopy[report.status];
  const counts = report.checks.reduce(
    (current, check) => {
      if (check.status === "pass") current.passed += 1;
      if (check.status === "warning") current.warnings += 1;
      if (check.status === "block") current.blockers += 1;
      return current;
    },
    { passed: 0, warnings: 0, blockers: 0 },
  );
  return (
    <>
      <section className={`preflight-overview preflight-overview--${report.status}`}>
        <span className="preflight-overview__mark" aria-hidden="true">
          {report.status === "ready" ? "✓" : report.status === "warning" ? "!" : "×"}
        </span>
        <div>
          <span className="eyebrow">Overall result</span>
          <h2>{copy.title}</h2>
          <p>{copy.detail}</p>
        </div>
        <dl className="preflight-counts">
          <div><dt>Passed</dt><dd>{counts.passed}</dd></div>
          <div><dt>Warnings</dt><dd>{counts.warnings}</dd></div>
          <div><dt>Blockers</dt><dd>{counts.blockers}</dd></div>
        </dl>
      </section>

      <dl className="preflight-facts" aria-label="Check context">
        <div>
          <dt>Platform</dt>
          <dd>{report.platform.os} · {report.platform.arch}</dd>
        </div>
        <div>
          <dt>Configuration</dt>
          <dd>v{report.configuration.version} · {report.configuration.agentCount} agent{report.configuration.agentCount > 1 ? "s" : ""}</dd>
        </div>
        <div>
          <dt>Policies</dt>
          <dd>{report.configuration.policyRuleCount} rule{report.configuration.policyRuleCount > 1 ? "s" : ""}</dd>
        </div>
        <div>
          <dt>Audit</dt>
          <dd>{report.audit.enabled ? report.audit.mode : "disabled"}</dd>
        </div>
      </dl>

      {(report.tools.length > 0 || report.agents.length > 0) && (
        <section className="preflight-inventory" aria-label="Tools and agents">
          {report.tools.length > 0 && (
            <div>
              <h2>Detected tools</h2>
              <ul>
                {report.tools.map((tool) => (
                  <li key={tool.profileID}>
                    <span>{profileLabel(tool.profileID)}</span>
                    <strong className={`inventory-status inventory-status--${tool.installation}`}>
                      {installationLabel(tool.installation)}
                    </strong>
                  </li>
                ))}
              </ul>
            </div>
          )}
          {report.agents.length > 0 && (
            <div>
              <h2>Effective plan</h2>
              <ul>
                {report.agents.map((agent) => (
                  <li className="preflight-agent" key={agent.ordinal}>
                    <div>
                      <span>Agent {agent.ordinal}{agent.source === "demo" ? " · demo" : " · configured"}</span>
                      <small>
                        {agent.command === "shell" ? "Shell command" : "Direct command"}
                        {` · ${installationLabel(agent.installation)}`}
                      </small>
                    </div>
                    <strong>
                      {agent.backend || "indeterminate backend"}
                      {agent.adapter ? ` · ${agent.adapter}` : ""}
                      {agent.adapterMaturity ? ` (${maturityLabel(agent.adapterMaturity)})` : ""}
                    </strong>
                  </li>
                ))}
              </ul>
            </div>
          )}
        </section>
      )}

      <div className="preflight-results">
        <div className="preflight-results__heading">
          <h2>Checks</h2>
          <span>Schema v{report.schemaVersion}{refreshing ? " · refreshing…" : ""}</span>
        </div>
        <ul className="preflight-checks">
          {report.checks.map((check) => (
            <li className={`preflight-check preflight-check--${check.status}`} key={check.id}>
              <span className="preflight-check__mark" aria-hidden="true">
                {check.status === "pass" ? "✓" : check.status === "warning" ? "!" : "×"}
              </span>
              <div className="preflight-check__content">
                <div>
                  <strong>{check.summary}</strong>
                  <span>{scopeLabel(check.scope)}</span>
                </div>
                {check.remediation && <p>{check.remediation}</p>}
              </div>
              <span className="preflight-check__status">{checkLabels[check.status]}</span>
            </li>
          ))}
          {report.checks.length === 0 && (
            <li className="preflight-checks__empty">No check was returned.</li>
          )}
        </ul>
      </div>
    </>
  );
}

function installationLabel(value: PreflightReport["tools"][number]["installation"]): string {
  if (value === "installed") return "Installed";
  if (value === "not_installed") return "Missing";
  return "Indeterminate";
}

function scopeLabel(value: string): string {
  const labels: Record<string, string> = {
    configuration: "Configuration",
    platform: "Platform",
    policy: "Policies",
    audit: "Audit",
    tool: "Tool",
    agent: "Agent",
    adapter: "Adapter",
    backend: "Backend",
  };
  return labels[value] || "System";
}

function profileLabel(value: PreflightReport["tools"][number]["profileID"]): string {
  const labels: Record<string, string> = {
    "claude-code": "Claude Code",
    "codex-cli": "Codex CLI",
    "mimo-code": "MiMo Code",
    ollama: "Ollama / DeepSeek",
    custom: "Custom command",
  };
  return labels[value] || "Configured tool";
}

function maturityLabel(value: NonNullable<PreflightReport["agents"][number]["adapterMaturity"]>): string {
  return value === "stable" ? "stable" : "experimental";
}
