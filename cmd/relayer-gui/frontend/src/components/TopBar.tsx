import type { AppState } from "../types/relayer";

interface TopBarProps {
  state: AppState;
  onOpenAgents(): void;
  onOpenPreflight(): void;
  onRequestStop(): void;
}

const runLabels: Record<AppState["runStatus"], string> = {
  idle: "Prêt à démarrer",
  starting: "Initialisation",
  running: "En cours",
  restarting: "Redémarrage",
  rollback: "Restauration",
  stopping: "Arrêt en cours",
  stopped: "Arrêté",
  failed: "Erreur",
};

export function TopBar({ state, onOpenAgents, onOpenPreflight, onRequestStop }: TopBarProps) {
  const running = state.agents.filter((agent) => agent.running).length;
  const waiting = state.pendingEvents.length;
  const transitioning = ["starting", "restarting", "rollback", "stopping"].includes(
    state.runStatus,
  );
  return (
    <header className="topbar">
      <div className="brand">
        <span className="brand__mark" aria-hidden="true">
          <span />
          <span />
        </span>
        <div>
          <strong>Relayer</strong>
          <span>Human control plane</span>
        </div>
      </div>

      <div className="topbar__metrics" aria-label="État du run">
        <span className={`run-state run-state--${state.runStatus}`}>
          <i aria-hidden="true" />
          {runLabels[state.runStatus]}
        </span>
        <span className="topbar__metric"><strong>{running}</strong> actif{running > 1 ? "s" : ""}</span>
        <span className={`topbar__metric${waiting ? " topbar__metric--attention" : ""}`}>
          <strong>{waiting}</strong> en attente
        </span>
      </div>

      <div className="topbar__actions">
        {state.policy.dryRun && <span className="mode-pill">DRY RUN</span>}
        <span className="mode-pill mode-pill--quiet">POLICY {state.policy.defaultAction.toUpperCase()}</span>
        <button
          className="button button--health"
          type="button"
          onClick={onOpenPreflight}
          disabled={transitioning}
        >
          <span aria-hidden="true">＋</span> Santé
        </button>
        <button
          className="button button--agents"
          type="button"
          onClick={onOpenAgents}
          disabled={transitioning}
        >
          <span aria-hidden="true">◇</span> Agents
        </button>
        {state.runID && state.runStatus !== "idle" && (
          <button
            className="button button--ghost"
            type="button"
            disabled={transitioning}
            onClick={onRequestStop}
          >
            {state.runStatus === "failed" ? "Réessayer l’arrêt" : "Arrêter le run"}
          </button>
        )}
      </div>
    </header>
  );
}
