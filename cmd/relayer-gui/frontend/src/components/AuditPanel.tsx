import { useCallback, useEffect, useRef, useState } from "react";
import { useDialogKeyboard } from "../hooks/useDialogKeyboard";
import type {
  AuditEntryView,
  AuditFilterInput,
  AuditSummaryView,
  AuditVerificationView,
  RelayerBridge,
} from "../types/relayer";

interface AuditPanelProps {
  bridge: RelayerBridge;
  onClose(): void;
}

export function AuditPanel({ bridge, onClose }: AuditPanelProps) {
  const [summary, setSummary] = useState<AuditSummaryView>();
  const [verification, setVerification] = useState<AuditVerificationView>();
  const [entries, setEntries] = useState<AuditEntryView[]>([]);
  const [loading, setLoading] = useState(true);
  const [error, setError] = useState(false);
  const [exporting, setExporting] = useState<"json" | "csv" | null>(null);

  // Filters
  const [selectedAgent, setSelectedAgent] = useState<string>("");
  const [selectedKind, setSelectedKind] = useState<string>("");
  const [limit, setLimit] = useState<number>(50);

  // Expanded row
  const [expandedEntryKey, setExpandedEntryKey] = useState<string | null>(null);

  const dialogRef = useRef<HTMLElement>(null);
  useDialogKeyboard(dialogRef, { onClose });

  const loadData = useCallback(
    async (agentFilter = selectedAgent, kindFilter = selectedKind, limitFilter = limit) => {
      setLoading(true);
      setError(false);
      try {
        const filterInput: AuditFilterInput = {
          agentID: agentFilter || undefined,
          kind: kindFilter || undefined,
          limit: limitFilter > 0 ? limitFilter : undefined,
        };

        const [sum, ver, ent] = await Promise.all([
          bridge.getAuditSummary(),
          bridge.verifyAuditJournal(),
          bridge.getAuditEntries(filterInput),
        ]);

        setSummary(sum);
        setVerification(ver);
        setEntries(ent);
      } catch {
        setError(true);
      } finally {
        setLoading(false);
      }
    },
    [bridge, selectedAgent, selectedKind, limit],
  );

  useEffect(() => {
    void loadData();
  }, [loadData]);

  const handleExport = async (format: "json" | "csv") => {
    setExporting(format);
    try {
      const content = await bridge.exportAuditReport(format);
      const mime = format === "json" ? "application/json" : "text/csv;charset=utf-8;";
      const blob = new Blob([content], { type: mime });
      const url = URL.createObjectURL(blob);
      const anchor = document.createElement("a");
      anchor.href = url;
      const timestamp = new Date().toISOString().replace(/[:.]/g, "-");
      anchor.download = `relayer-audit-${timestamp}.${format}`;
      document.body.appendChild(anchor);
      anchor.click();
      document.body.removeChild(anchor);
      URL.revokeObjectURL(url);
    } catch {
      setError(true);
    } finally {
      setExporting(null);
    }
  };

  return (
    <div className="audit-layer" role="presentation">
      <section
        ref={dialogRef}
        className="audit-panel"
        role="dialog"
        aria-modal="true"
        aria-labelledby="audit-title"
      >
        <header className="audit-panel__header">
          <div>
            <span className="eyebrow">Cryptographic journal & compliance</span>
            <h1 id="audit-title">Audit Trail & Integrity</h1>
            <p>
              Append-only tamper-evident log · Safe records with human decisions and policy evaluations.
            </p>
          </div>
          <button
            className="icon-button"
            type="button"
            onClick={onClose}
            aria-label="Close audit panel"
            autoFocus
          >
            ×
          </button>
        </header>

        <div className="audit-panel__body">
          <AuditContentView
            summary={summary}
            verification={verification}
            entries={entries}
            loading={loading}
            error={error}
            selectedAgent={selectedAgent}
            selectedKind={selectedKind}
            limit={limit}
            expandedEntryKey={expandedEntryKey}
            onSelectAgent={(next) => {
              setSelectedAgent(next);
              void loadData(next, selectedKind, limit);
            }}
            onSelectKind={(next) => {
              setSelectedKind(next);
              void loadData(selectedAgent, next, limit);
            }}
            onSelectLimit={(next) => {
              setLimit(next);
              void loadData(selectedAgent, selectedKind, next);
            }}
            onToggleExpand={(key) =>
              setExpandedEntryKey(expandedEntryKey === key ? null : key)
            }
            onRefresh={() => void loadData()}
          />
        </div>

        <footer className="audit-panel__footer">
          <p>
            Display-safe audit journal. Passwords, shell inputs, and private tokens are never
            written to disk.
          </p>
          <div className="audit-panel__actions">
            <button
              className="button button--ghost"
              type="button"
              disabled={loading || exporting !== null}
              onClick={() => void handleExport("json")}
            >
              {exporting === "json" ? "Exporting…" : "Export JSON"}
            </button>
            <button
              className="button button--ghost"
              type="button"
              disabled={loading || exporting !== null}
              onClick={() => void handleExport("csv")}
            >
              {exporting === "csv" ? "Exporting…" : "Export CSV"}
            </button>
          </div>
        </footer>
      </section>
    </div>
  );
}

export interface AuditContentViewProps {
  summary?: AuditSummaryView;
  verification?: AuditVerificationView;
  entries: AuditEntryView[];
  loading?: boolean;
  error?: boolean;
  selectedAgent: string;
  selectedKind: string;
  limit: number;
  expandedEntryKey: string | null;
  onSelectAgent(agent: string): void;
  onSelectKind(kind: string): void;
  onSelectLimit(limit: number): void;
  onToggleExpand(entryKey: string): void;
  onRefresh(): void;
}

export function AuditContentView({
  summary,
  verification,
  entries,
  loading = false,
  error = false,
  selectedAgent,
  selectedKind,
  limit,
  expandedEntryKey,
  onSelectAgent,
  onSelectKind,
  onSelectLimit,
  onToggleExpand,
  onRefresh,
}: AuditContentViewProps) {
  if (loading && !summary) {
    return (
      <div className="audit-panel__loading">
        <span className="settings-spinner" aria-hidden="true" />
        Loading audit journal…
      </div>
    );
  }

  if (error && !summary) {
    return (
      <div className="audit-panel__failure" role="alert">
        <span aria-hidden="true">!</span>
        <div>
          <strong>Audit journal unavailable</strong>
          <p>The audit records could not be retrieved from the local engine.</p>
        </div>
      </div>
    );
  }

  const agentOptions = summary ? Object.keys(summary.agentCounts).sort() : [];

  return (
    <>
      {/* Integrity Verification Card */}
      {verification && (
        <section
          className={`audit-verification ${
            verification.passed
              ? "audit-verification--passed"
              : "audit-verification--failed"
          }`}
          aria-label="Journal integrity status"
        >
          <div className="audit-verification__header">
            <span className="audit-verification__mark" aria-hidden="true">
              {verification.passed ? "✓" : "!"}
            </span>
            <div>
              <h2>
                {verification.passed
                  ? "Cryptographic Sequence Verified"
                  : "Integrity Issues Detected"}
              </h2>
              <p>
                {verification.passed
                  ? `Continuity verified across ${verification.validLines} record(s) and ${verification.totalRuns} run(s). No gaps or regressions.`
                  : `${verification.issues.length} verification issue(s) detected in the journal stream.`}
              </p>
            </div>
          </div>

          {!verification.passed && verification.issues.length > 0 && (
            <ul className="audit-verification__issues" aria-label="Integrity issues list">
              {verification.issues.map((issue, idx) => (
                <li key={idx}>
                  <strong>Line {issue.line}:</strong> {issue.message}
                  {issue.entryID && <span> (entry: {issue.entryID})</span>}
                </li>
              ))}
            </ul>
          )}
        </section>
      )}

      {/* Summary Statistics */}
      {summary && (
        <section className="audit-metrics-grid" aria-label="Journal summary metrics">
          <div className="audit-metric-card">
            <span className="audit-metric-card__label">Records & Sessions</span>
            <strong className="audit-metric-card__value">{summary.totalEntries}</strong>
            <span className="audit-metric-card__sub">
              {summary.runsCount} run{summary.runsCount !== 1 ? "s" : ""} ·{" "}
              {summary.sessionsCount} session{summary.sessionsCount !== 1 ? "s" : ""}
            </span>
          </div>

          <div className="audit-metric-card">
            <span className="audit-metric-card__label">Decision Actors</span>
            <div className="audit-metric-card__actors">
              <span>
                Human: <strong>{summary.actorsCount["human"] ?? 0}</strong>
              </span>
              <span>
                Policy: <strong>{summary.actorsCount["policy"] ?? 0}</strong>
              </span>
              <span>
                System: <strong>{summary.actorsCount["system"] ?? 0}</strong>
              </span>
            </div>
          </div>

          <div className="audit-metric-card">
            <span className="audit-metric-card__label">Decisions</span>
            <div className="audit-metric-card__decisions">
              <span className="audit-badge audit-badge--allow">
                Allow: {summary.decisionsCount["allow"] ?? 0}
              </span>
              <span className="audit-badge audit-badge--deny">
                Deny: {summary.decisionsCount["deny"] ?? 0}
              </span>
              <span className="audit-badge audit-badge--ask">
                Ask: {summary.decisionsCount["ask"] ?? 0}
              </span>
            </div>
          </div>

          <div className="audit-metric-card">
            <span className="audit-metric-card__label">Security Flags</span>
            <strong className="audit-metric-card__value">
              {summary.sensitiveCount}
            </strong>
            <span className="audit-metric-card__sub">sensitive events flagged</span>
          </div>
        </section>
      )}

      {/* Filters Toolbar */}
      <div className="audit-filters-bar" role="search" aria-label="Filter audit entries">
        <div className="audit-filter-group">
          <label htmlFor="audit-filter-agent">Agent</label>
          <select
            id="audit-filter-agent"
            value={selectedAgent}
            onChange={(e) => onSelectAgent(e.target.value)}
          >
            <option value="">All agents</option>
            {agentOptions.map((agent) => (
              <option key={agent} value={agent}>
                {agent} ({summary?.agentCounts[agent] ?? 0})
              </option>
            ))}
          </select>
        </div>

        <div className="audit-filter-group">
          <label htmlFor="audit-filter-kind">Kind</label>
          <select
            id="audit-filter-kind"
            value={selectedKind}
            onChange={(e) => onSelectKind(e.target.value)}
          >
            <option value="">All kinds</option>
            <option value="policy_evaluated">policy_evaluated</option>
            <option value="decision">decision</option>
            <option value="delivery">delivery</option>
            <option value="operator_input">operator_input</option>
            <option value="session_started">session_started</option>
            <option value="session_finished">session_finished</option>
            <option value="run_started">run_started</option>
            <option value="run_finished">run_finished</option>
            <option value="event_withdrawn">event_withdrawn</option>
          </select>
        </div>

        <div className="audit-filter-group">
          <label htmlFor="audit-filter-limit">Limit</label>
          <select
            id="audit-filter-limit"
            value={limit}
            onChange={(e) => onSelectLimit(Number(e.target.value))}
          >
            <option value={25}>Last 25</option>
            <option value={50}>Last 50</option>
            <option value={100}>Last 100</option>
            <option value={250}>Last 250</option>
            <option value={0}>All entries</option>
          </select>
        </div>

        <button
          className="button button--ghost"
          type="button"
          onClick={onRefresh}
          disabled={loading}
          aria-label="Refresh audit data"
        >
          {loading ? "Refreshing…" : "Refresh"}
        </button>
      </div>

      {/* Entries Table */}
      <section className="audit-table-section" aria-label="Audit records list">
        {entries.length === 0 ? (
          <div className="audit-table__empty">
            <p>No audit entries match the current filter criteria.</p>
          </div>
        ) : (
          <div className="audit-table-wrapper">
            <table className="audit-table">
              <thead>
                <tr>
                  <th scope="col">#</th>
                  <th scope="col">Timestamp (UTC)</th>
                  <th scope="col">Agent</th>
                  <th scope="col">Kind</th>
                  <th scope="col">Actor</th>
                  <th scope="col">Decision</th>
                  <th scope="col">Outcome</th>
                  <th scope="col">Rule / Reason</th>
                  <th scope="col">Details</th>
                </tr>
              </thead>
              <tbody>
                {entries.map((entry) => {
                  const isExpanded = expandedEntryKey === entry.entryID;
                  return (
                    <EntryRow
                      key={entry.entryID || entry.sequence}
                      entry={entry}
                      expanded={isExpanded}
                      onToggle={() => onToggleExpand(entry.entryID)}
                    />
                  );
                })}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </>
  );
}

function EntryRow({
  entry,
  expanded,
  onToggle,
}: {
  entry: AuditEntryView;
  expanded: boolean;
  onToggle(): void;
}) {
  const formattedTime = entry.timestamp ? entry.timestamp.replace("T", " ").replace("Z", "") : "-";

  return (
    <>
      <tr className={`audit-row ${expanded ? "audit-row--expanded" : ""}`}>
        <td className="audit-cell--mono">{entry.sequence}</td>
        <td className="audit-cell--mono">{formattedTime}</td>
        <td>
          <strong>{entry.agentID || "-"}</strong>
        </td>
        <td>
          <span className={`audit-badge audit-badge--kind-${entry.kind}`}>
            {entry.kind}
          </span>
        </td>
        <td>
          <span
            className={`audit-badge audit-badge--actor-${entry.decisionBy || "unknown"}`}
          >
            {entry.decisionBy || "-"}
          </span>
        </td>
        <td>
          {entry.decision ? (
            <span className={`audit-badge audit-badge--${entry.decision}`}>
              {entry.decision}
            </span>
          ) : (
            "-"
          )}
        </td>
        <td>
          <span className="audit-cell--outcome">{entry.outcome || "-"}</span>
        </td>
        <td className="audit-cell--reason">
          {entry.rule ? <code>{entry.rule}</code> : entry.reason || "-"}
        </td>
        <td>
          <button
            type="button"
            className="audit-row__expand-button"
            onClick={onToggle}
            aria-expanded={expanded}
            aria-label={`Toggle details for entry ${entry.entryID || entry.sequence}`}
          >
            {expanded ? "▴" : "▾"}
          </button>
        </td>
      </tr>

      {expanded && (
        <tr className="audit-row-detail">
          <td colSpan={9}>
            <div className="audit-detail-content">
              <div className="audit-detail-facts">
                <div>
                  <dt>Entry ID</dt>
                  <dd><code>{entry.entryID}</code></dd>
                </div>
                <div>
                  <dt>Run ID</dt>
                  <dd><code>{entry.runID}</code></dd>
                </div>
                {entry.sessionID && (
                  <div>
                    <dt>Session ID</dt>
                    <dd><code>{entry.sessionID}</code></dd>
                  </div>
                )}
                {entry.adapter && (
                  <div>
                    <dt>Adapter</dt>
                    <dd>{entry.adapter}</dd>
                  </div>
                )}
                {entry.risk && (
                  <div>
                    <dt>Risk Level</dt>
                    <dd>
                      <span className={`audit-badge audit-badge--risk-${entry.risk}`}>
                        {entry.risk}
                      </span>
                    </dd>
                  </div>
                )}
                {entry.sensitive && (
                  <div>
                    <dt>Security</dt>
                    <dd>
                      <span className="audit-badge audit-badge--sensitive">
                        Sensitive Event
                      </span>
                    </dd>
                  </div>
                )}
              </div>

              {entry.summary && (
                <div className="audit-detail-summary">
                  <dt>Sanitized Summary</dt>
                  <dd>{entry.summary}</dd>
                </div>
              )}

              {entry.metadata && Object.keys(entry.metadata).length > 0 && (
                <div className="audit-detail-metadata">
                  <dt>Metadata</dt>
                  <dd>
                    <div className="audit-detail-meta-grid">
                      {Object.entries(entry.metadata).map(([key, val]) => (
                        <span key={key} className="audit-meta-tag">
                          <code>{key}</code>: {val}
                        </span>
                      ))}
                    </div>
                  </dd>
                </div>
              )}
            </div>
          </td>
        </tr>
      )}
    </>
  );
}
