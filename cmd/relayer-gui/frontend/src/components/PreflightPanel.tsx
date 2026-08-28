import { useCallback, useEffect, useState } from "react";
import type { PreflightCheckStatus, PreflightReport, RelayerBridge } from "../types/relayer";

interface PreflightPanelProps {
  bridge: RelayerBridge;
  onClose(): void;
}

const overallCopy: Record<PreflightReport["status"], { title: string; detail: string }> = {
  ready: {
    title: "Relayer est prêt",
    detail: "Aucun blocage n’a été détecté pour la configuration enregistrée.",
  },
  warning: {
    title: "Relayer peut démarrer avec précaution",
    detail: "Le diagnostic a relevé un ou plusieurs points à surveiller.",
  },
  blocked: {
    title: "Le démarrage doit rester bloqué",
    detail: "Corrigez les blocages signalés avant de lancer les agents.",
  },
};

const checkLabels: Record<PreflightCheckStatus, string> = {
  pass: "Validé",
  warning: "Attention",
  block: "Bloquant",
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
            <span className="eyebrow">Diagnostic local en lecture seule</span>
            <h1 id="preflight-title">Santé du système</h1>
            <p>Configuration, politiques, audit, backends et outils configurés.</p>
          </div>
          <button
            className="icon-button"
            type="button"
            onClick={onClose}
            aria-label="Fermer le diagnostic"
            autoFocus
          >
            ×
          </button>
        </header>

        <div className="preflight-panel__body" aria-live="polite">
          {loading && !report ? (
            <div className="preflight-panel__loading">
              <span className="settings-spinner" aria-hidden="true" />
              Vérification locale en cours…
            </div>
          ) : error || !report ? (
            <div className="preflight-panel__failure" role="alert">
              <span aria-hidden="true">!</span>
              <div>
                <strong>Diagnostic indisponible</strong>
                <p>Aucun détail local n’est affiché. Le démarrage ne doit pas être considéré comme validé.</p>
              </div>
            </div>
          ) : (
            <PreflightReportView report={report} refreshing={loading} />
          )}
        </div>

        <footer className="preflight-panel__footer">
          <p>Ce rapport n’affiche ni commandes, ni variables d’environnement, ni chemins complets.</p>
          <button
            className="button button--primary"
            type="button"
            onClick={() => void refresh()}
            disabled={loading}
          >
            {loading ? "Vérification…" : "Actualiser"}
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
          <span className="eyebrow">Résultat global</span>
          <h2>{copy.title}</h2>
          <p>{copy.detail}</p>
        </div>
        <dl className="preflight-counts">
          <div><dt>Validés</dt><dd>{counts.passed}</dd></div>
          <div><dt>Avertissements</dt><dd>{counts.warnings}</dd></div>
          <div><dt>Blocages</dt><dd>{counts.blockers}</dd></div>
        </dl>
      </section>

      <dl className="preflight-facts" aria-label="Contexte du diagnostic">
        <div>
          <dt>Plateforme</dt>
          <dd>{report.platform.os} · {report.platform.arch}</dd>
        </div>
        <div>
          <dt>Configuration</dt>
          <dd>v{report.configuration.version} · {report.configuration.agentCount} agent{report.configuration.agentCount > 1 ? "s" : ""}</dd>
        </div>
        <div>
          <dt>Politiques</dt>
          <dd>{report.configuration.policyRuleCount} règle{report.configuration.policyRuleCount > 1 ? "s" : ""}</dd>
        </div>
        <div>
          <dt>Audit</dt>
          <dd>{report.audit.enabled ? report.audit.mode : "désactivé"}</dd>
        </div>
      </dl>

      {(report.tools.length > 0 || report.agents.length > 0) && (
        <section className="preflight-inventory" aria-label="Outils et agents">
          {report.tools.length > 0 && (
            <div>
              <h2>Outils détectés</h2>
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
              <h2>Plan effectif</h2>
              <ul>
                {report.agents.map((agent) => (
                  <li className="preflight-agent" key={agent.ordinal}>
                    <div>
                      <span>Agent {agent.ordinal}{agent.source === "demo" ? " · démo" : " · configuré"}</span>
                      <small>
                        {agent.command === "shell" ? "Commande shell" : "Commande directe"}
                        {` · ${installationLabel(agent.installation)}`}
                      </small>
                    </div>
                    <strong>
                      {agent.backend || "backend indéterminé"}
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
          <h2>Vérifications</h2>
          <span>Schéma v{report.schemaVersion}{refreshing ? " · actualisation…" : ""}</span>
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
            <li className="preflight-checks__empty">Aucune vérification n’a été renvoyée.</li>
          )}
        </ul>
      </div>
    </>
  );
}

function installationLabel(value: PreflightReport["tools"][number]["installation"]): string {
  if (value === "installed") return "Installé";
  if (value === "not_installed") return "Absent";
  return "Indéterminé";
}

function scopeLabel(value: string): string {
  const labels: Record<string, string> = {
    configuration: "Configuration",
    platform: "Plateforme",
    policy: "Politiques",
    audit: "Audit",
    tool: "Outil",
    agent: "Agent",
    adapter: "Adapter",
    backend: "Backend",
  };
  return labels[value] || "Système";
}

function profileLabel(value: PreflightReport["tools"][number]["profileID"]): string {
  const labels: Record<string, string> = {
    "claude-code": "Claude Code",
    "codex-cli": "Codex CLI",
    "mimo-code": "MiMo Code",
    ollama: "Ollama / DeepSeek",
    custom: "Commande personnalisée",
  };
  return labels[value] || "Outil configuré";
}

function maturityLabel(value: NonNullable<PreflightReport["agents"][number]["adapterMaturity"]>): string {
  return value === "stable" ? "stable" : "expérimental";
}
