import { useCallback, useEffect, useRef, useState } from "react";
import { useDialogKeyboard } from "../hooks/useDialogKeyboard";
import type { RelayerBridge, TelemetrySnapshotView } from "../types/relayer";

interface ObservabilityPanelProps {
  bridge: RelayerBridge;
  onClose(): void;
}

export function ObservabilityPanel({ bridge, onClose }: ObservabilityPanelProps) {
  const [snapshot, setSnapshot] = useState<TelemetrySnapshotView>();
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(false);
  const [autoRefresh, setAutoRefresh] = useState(true);
  const [copiedProm, setCopiedProm] = useState(false);

  const dialogRef = useRef<HTMLElement>(null);
  useDialogKeyboard(dialogRef, { onClose });

  const loadData = useCallback(async () => {
    try {
      const data = await bridge.getTelemetrySnapshot();
      setSnapshot(data);
      setError(false);
    } catch {
      setError(true);
    } finally {
      setLoading(false);
    }
  }, [bridge]);

  useEffect(() => {
    loadData();
  }, [loadData]);

  // Polling when autoRefresh is enabled
  useEffect(() => {
    if (!autoRefresh) return;
    const interval = setInterval(loadData, 3000);
    return () => clearInterval(interval);
  }, [autoRefresh, loadData]);

  const copyPrometheusUrl = () => {
    if (!snapshot?.prometheusAddress) return;
    const url = snapshot.prometheusAddress.startsWith("http")
      ? snapshot.prometheusAddress
      : `http://${snapshot.prometheusAddress.replace(/^:/, "localhost:")}/metrics`;
    navigator.clipboard.writeText(url).then(() => {
      setCopiedProm(true);
      setTimeout(() => setCopiedProm(false), 2000);
    });
  };

  return (
    <div className="modal-backdrop observability-layer" role="presentation">
      <section
        ref={dialogRef}
        className="modal-card observability-panel"
        role="dialog"
        aria-modal="true"
        aria-labelledby="observability-title"
      >
        {/* Header */}
        <header className="observability-panel__header">
          <div className="observability-panel__title-group">
            <div className="observability-panel__icon" aria-hidden="true">
              📊
            </div>
            <div>
              <h1 id="observability-title">Observability & Metrics</h1>
              <p>Real-time telemetry, human arbitration latency, and security guardrails</p>
            </div>
          </div>

          <div className="observability-panel__actions">
            <label className="observability-toggle">
              <input
                type="checkbox"
                checked={autoRefresh}
                onChange={(e) => setAutoRefresh(e.target.checked)}
              />
              <span>Live (3s)</span>
            </label>

            <button
              type="button"
              className="button button--ghost button--small"
              onClick={loadData}
              title="Refresh metrics now"
            >
              ↻ Refresh
            </button>

            <button
              type="button"
              className="modal-close-button"
              onClick={onClose}
              aria-label="Close observability panel"
            >
              ✕
            </button>
          </div>
        </header>

        {/* Content */}
        <div className="observability-panel__body">
          {loading && !snapshot ? (
            <div className="observability-panel__loading">
              <span className="settings-spinner" aria-hidden="true" />
              Loading telemetry metrics…
            </div>
          ) : error && !snapshot ? (
            <div className="observability-panel__failure" role="alert">
              <span aria-hidden="true">!</span>
              <div>
                <strong>Telemetry unavailable</strong>
                <p>The metrics snapshot could not be retrieved from the local engine.</p>
              </div>
            </div>
          ) : snapshot ? (
            <>
              {/* KPI Cards Grid */}
              <div className="observability-kpi-grid">
                <div className="observability-kpi-card">
                  <span className="observability-kpi-card__label">Human Decisions</span>
                  <div className="observability-kpi-card__val-row">
                    <strong className="observability-kpi-card__value">
                      {snapshot.decisionsBreakdown.allow + snapshot.decisionsBreakdown.deny + snapshot.decisionsBreakdown.custom}
                    </strong>
                    {snapshot.decisionsTotal > 0 && (
                      <span className="observability-kpi-card__sub">
                        {Math.round(
                          ((snapshot.decisionsBreakdown.allow + snapshot.decisionsBreakdown.deny) /
                            snapshot.decisionsTotal) *
                            100,
                        )}
                        % of total
                      </span>
                    )}
                  </div>
                </div>

                <div className="observability-kpi-card">
                  <span className="observability-kpi-card__label">Avg Human Latency</span>
                  <div className="observability-kpi-card__val-row">
                    <strong className="observability-kpi-card__value observability-kpi-card__value--accent">
                      {snapshot.averageReactionTime > 0
                        ? `${snapshot.averageReactionTime.toFixed(2)}s`
                        : "—"}
                    </strong>
                    <span className="observability-kpi-card__sub">detection → decision</span>
                  </div>
                </div>

                <div className="observability-kpi-card">
                  <span className="observability-kpi-card__label">Guardrail Blocks</span>
                  <div className="observability-kpi-card__val-row">
                    <strong
                      className={`observability-kpi-card__value ${
                        snapshot.guardrailsTotal > 0 ? "observability-kpi-card__value--warning" : ""
                      }`}
                    >
                      {snapshot.guardrailsTotal}
                    </strong>
                    <span className="observability-kpi-card__sub">policy interceptions</span>
                  </div>
                </div>

                <div className="observability-kpi-card">
                  <span className="observability-kpi-card__label">Active Sessions</span>
                  <div className="observability-kpi-card__val-row">
                    <strong className="observability-kpi-card__value">
                      {snapshot.sessionsActive}
                    </strong>
                    <span className="observability-kpi-card__sub">
                      of {snapshot.sessionsTotal} total
                    </span>
                  </div>
                </div>
              </div>

              {/* Main Charts Row: Decisions Donut + Latency Histogram */}
              <div className="observability-charts-row">
                {/* 1. Decision Ratio Circular Gauge */}
                <section className="observability-chart-card" aria-labelledby="chart-decisions-title">
                  <h2 id="chart-decisions-title" className="observability-chart-card__title">
                    Decisions Breakdown
                  </h2>
                  <DecisionDonutChart breakdown={snapshot.decisionsBreakdown} total={snapshot.decisionsTotal} />
                </section>

                {/* 2. Reaction Latency Histogram */}
                <section className="observability-chart-card" aria-labelledby="chart-latency-title">
                  <div className="observability-chart-card__header-row">
                    <h2 id="chart-latency-title" className="observability-chart-card__title">
                      Operator Reaction Latency
                    </h2>
                    {snapshot.averageReactionTime > 0 && (
                      <span className="observability-pill">
                        Avg: {snapshot.averageReactionTime.toFixed(2)}s
                      </span>
                    )}
                  </div>
                  <LatencyHistogramChart buckets={snapshot.decisionDurations} />
                </section>
              </div>

              {/* Bottom Row: Guardrails Breakdown & Telemetry Exporters Status */}
              <div className="observability-details-row">
                {/* Guardrails Card */}
                <section className="observability-card" aria-labelledby="guardrails-title">
                  <h2 id="guardrails-title" className="observability-card__title">
                    Security Guardrails & Interceptions
                  </h2>
                  {Object.keys(snapshot.guardrailsBreakdown).length === 0 ? (
                    <div className="observability-card__empty">
                      <span aria-hidden="true">✓</span>
                      <span>Zero guardrail violations recorded. All operations complied with security policies.</span>
                    </div>
                  ) : (
                    <div className="guardrails-list">
                      {Object.entries(snapshot.guardrailsBreakdown).map(([rule, count]) => (
                        <div key={rule} className="guardrail-item">
                          <span className="guardrail-item__icon" aria-hidden="true">🛡️</span>
                          <div className="guardrail-item__info">
                            <strong>{formatGuardrailRule(rule)}</strong>
                            <code>{rule}</code>
                          </div>
                          <span className="guardrail-item__badge">{count} blocked</span>
                        </div>
                      ))}
                    </div>
                  )}
                </section>

                {/* Exporters Status Card */}
                <section className="observability-card" aria-labelledby="exporters-title">
                  <h2 id="exporters-title" className="observability-card__title">
                    Telemetry Exporters
                  </h2>
                  <div className="exporters-grid">
                    {/* Prometheus */}
                    <div className="exporter-item">
                      <div className="exporter-item__header">
                        <span className="exporter-item__name">Prometheus HTTP</span>
                        <span
                          className={`exporter-badge ${
                            snapshot.prometheusEnabled ? "exporter-badge--active" : "exporter-badge--inactive"
                          }`}
                        >
                          {snapshot.prometheusEnabled ? "ACTIVE" : "STANDBY"}
                        </span>
                      </div>
                      <p className="exporter-item__detail">
                        {snapshot.prometheusAddress
                          ? `Listening on ${snapshot.prometheusAddress}/metrics`
                          : "Embedded server disabled in config.yaml"}
                      </p>
                      {snapshot.prometheusAddress && (
                        <button
                          type="button"
                          className="button button--ghost button--small copy-endpoint-button"
                          onClick={copyPrometheusUrl}
                        >
                          {copiedProm ? "✓ Copied!" : "📋 Copy URL"}
                        </button>
                      )}
                    </div>

                    {/* OTLP */}
                    <div className="exporter-item">
                      <div className="exporter-item__header">
                        <span className="exporter-item__name">OpenTelemetry OTLP</span>
                        <span
                          className={`exporter-badge ${
                            snapshot.otlpEnabled ? "exporter-badge--active" : "exporter-badge--inactive"
                          }`}
                        >
                          {snapshot.otlpEnabled ? "ACTIVE" : "STANDBY"}
                        </span>
                      </div>
                      <p className="exporter-item__detail">
                        {snapshot.otlpEndpoint
                          ? `Pushing to ${snapshot.otlpEndpoint}`
                          : "OTLP batch exporter disabled in config.yaml"}
                      </p>
                    </div>
                  </div>
                </section>
              </div>
            </>
          ) : null}
        </div>

        {/* Footer */}
        <footer className="observability-panel__footer">
          <span className="observability-timestamp">
            {snapshot ? `Snapshot: ${new Date(snapshot.timestamp).toLocaleTimeString()}` : ""}
          </span>
          <button type="button" className="button button--primary" onClick={onClose}>
            Close
          </button>
        </footer>
      </section>
    </div>
  );
}

// -------------------------------------------------------------
// Donut Chart Component (Pure SVG)
// -------------------------------------------------------------
function DecisionDonutChart({
  breakdown,
  total,
}: {
  breakdown: TelemetrySnapshotView["decisionsBreakdown"];
  total: number;
}) {
  const slices = [
    { label: "Allow", count: breakdown.allow, color: "#49d79a" },
    { label: "Deny", count: breakdown.deny, color: "#ff5e73" },
    { label: "Auto Allow", count: breakdown.autoAllow, color: "#42d9e8" },
    { label: "Auto Deny", count: breakdown.autoDeny, color: "#c084fc" },
    { label: "Custom / Modify", count: breakdown.custom, color: "#f59e0b" },
  ].filter((s) => s.count > 0);

  if (total === 0 || slices.length === 0) {
    return (
      <div className="donut-chart-empty">
        <span aria-hidden="true">◔</span>
        <p>No decisions applied yet in this session.</p>
      </div>
    );
  }

  const radius = 62;
  const circumference = 2 * Math.PI * radius;
  let cumulativeOffset = 0;

  return (
    <div className="donut-chart-layout">
      <div className="donut-chart-wrapper">
        <svg viewBox="0 0 160 160" className="donut-svg" aria-label="Decisions ratio chart">
          <circle
            cx="80"
            cy="80"
            r={radius}
            className="donut-track"
            fill="transparent"
            strokeWidth="16"
          />
          {slices.map((slice) => {
            const ratio = slice.count / total;
            const strokeDasharray = `${ratio * circumference} ${circumference}`;
            const strokeDashoffset = -cumulativeOffset;
            cumulativeOffset += ratio * circumference;

            return (
              <circle
                key={slice.label}
                cx="80"
                cy="80"
                r={radius}
                fill="transparent"
                stroke={slice.color}
                strokeWidth="16"
                strokeDasharray={strokeDasharray}
                strokeDashoffset={strokeDashoffset}
                strokeLinecap="round"
                className="donut-segment"
              />
            );
          })}
        </svg>
        <div className="donut-center-text" aria-hidden="true">
          <strong>{total}</strong>
          <span>decisions</span>
        </div>
      </div>

      <ul className="donut-legend" aria-label="Decisions breakdown legend">
        {slices.map((slice) => {
          const pct = Math.round((slice.count / total) * 100);
          return (
            <li key={slice.label} className="donut-legend__item">
              <span
                className="donut-legend__color"
                style={{ backgroundColor: slice.color }}
                aria-hidden="true"
              />
              <span className="donut-legend__label">{slice.label}</span>
              <strong className="donut-legend__count">{slice.count}</strong>
              <span className="donut-legend__pct">({pct}%)</span>
            </li>
          );
        })}
      </ul>
    </div>
  );
}

// -------------------------------------------------------------
// Latency Histogram Component (Pure SVG Bar Chart)
// -------------------------------------------------------------
function LatencyHistogramChart({
  buckets,
}: {
  buckets: TelemetrySnapshotView["decisionDurations"];
}) {
  const maxCount = Math.max(...buckets.map((b) => b.count), 1);
  const totalCount = buckets.reduce((acc, b) => acc + b.count, 0);

  if (totalCount === 0) {
    return (
      <div className="histogram-empty">
        <span aria-hidden="true">⏱️</span>
        <p>No human decisions recorded yet. Arbitrations will populate this distribution.</p>
      </div>
    );
  }

  const chartHeight = 110;
  const barWidth = 26;
  const gap = 12;
  const startX = 14;

  return (
    <div className="histogram-wrapper">
      <svg
        viewBox={`0 0 ${buckets.length * (barWidth + gap) + 20} 145`}
        className="histogram-svg"
        aria-label="Reaction latency histogram chart"
      >
        {/* Grid horizontal line */}
        <line x1="10" y1={chartHeight} x2={buckets.length * (barWidth + gap) + 10} y2={chartHeight} stroke="rgba(255,255,255,0.08)" />

        {buckets.map((b, i) => {
          const x = startX + i * (barWidth + gap);
          const barHeight = b.count > 0 ? Math.max((b.count / maxCount) * (chartHeight - 20), 6) : 2;
          const y = chartHeight - barHeight;

          return (
            <g key={b.label} className="histogram-bar-group">
              {/* Bar */}
              <rect
                x={x}
                y={y}
                width={barWidth}
                height={barHeight}
                rx="4"
                className={`histogram-bar ${b.count > 0 ? "histogram-bar--filled" : "histogram-bar--empty"}`}
              >
                <title>{`${b.label}: ${b.count} decision(s)`}</title>
              </rect>

              {/* Value on top of bar */}
              {b.count > 0 && (
                <text
                  x={x + barWidth / 2}
                  y={y - 5}
                  textAnchor="middle"
                  className="histogram-value-text"
                >
                  {b.count}
                </text>
              )}

              {/* Label below bar */}
              <text
                x={x + barWidth / 2}
                y={chartHeight + 16}
                textAnchor="middle"
                className="histogram-label-text"
              >
                {b.label}
              </text>
            </g>
          );
        })}
      </svg>
    </div>
  );
}

function formatGuardrailRule(rule: string): string {
  switch (rule) {
    case "destructive_command":
      return "Destructive Command Filter (rm -rf, dd, mkfs)";
    case "sensitive_paths":
      return "Sensitive File Protection (.env, ~/.ssh, keys)";
    case "rate_limit":
      return "Sliding-Window Rate Limiter";
    case "consecutive_decisions":
      return "Max Consecutive Decisions Threshold";
    default:
      return rule.replace(/_/g, " ");
  }
}
